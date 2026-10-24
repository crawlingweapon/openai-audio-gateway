// Package server is the gateway itself: OpenAI-compatible /v1/audio/* routes,
// API-key auth, rate limiting, upstream failover, and request logging.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crawlingweapon/openai-audio-gateway/config"
	"github.com/crawlingweapon/openai-audio-gateway/logging"
	"github.com/crawlingweapon/openai-audio-gateway/metrics"
)

type Server struct {
	cfg       *config.Holder
	log       *logging.Logger
	metrics   *metrics.Metrics
	limiters  *limiterSet
	limiterMu sync.Mutex
}

func New(cfg *config.Holder, log *logging.Logger, m *metrics.Metrics) *Server {
	s := &Server{cfg: cfg, log: log, metrics: m}
	s.limiters = newLimiterSet()
	return s
}

// Handler returns the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/audio/transcriptions", s.handleTranscriptions)
	mux.HandleFunc("POST /v1/audio/speech", s.handleSpeech)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return s.withMiddleware(mux)
}

// ------------------------------------------------------------------ middleware

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}
		w.Header().Set("X-Request-Id", reqID)

		cfg := s.cfg.Get()
		if cfg.CORS.Enabled {
			origin := r.Header.Get("Origin")
			if origin != "" && (contains(cfg.CORS.Origins, "*") || contains(cfg.CORS.Origins, origin)) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		keyLabel := ""
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			label, ok := s.authenticate(r, cfg)
			if !ok {
				s.metrics.IncAuthFailure()
				s.log.Warn("auth rejected", logging.F("req_id", reqID),
					logging.F("path", r.URL.Path), logging.F("remote", clientIP(r)),
					logging.F("reason", label))
				writeError(w, http.StatusUnauthorized, "invalid_api_key",
					"Missing or invalid API key.")
				s.finish(r, start, reqID, http.StatusUnauthorized)
				return
			}
			keyLabel = label
			if !s.allow(cfg, keyLabel, clientIP(r)) {
				s.metrics.IncRateDrop()
				s.log.Warn("rate limited", logging.F("req_id", reqID),
					logging.F("path", r.URL.Path), logging.F("key_label", keyLabel))
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
					"Rate limit exceeded.")
				s.finish(r, start, reqID, http.StatusTooManyRequests)
				return
			}
		}
		r.Header.Set("X-Gateway-Key-Label", keyLabel)
		r.Header.Set("X-Request-Id", reqID)

		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.finish(r, start, reqID, rw.status)
	})
}

func (s *Server) finish(r *http.Request, start time.Time, reqID string, status int) {
	d := time.Since(start)
	route := routeName(r.URL.Path)
	s.metrics.ObserveRequest(route, status, d)
	s.log.Info("request",
		logging.F("req_id", reqID),
		logging.F("route", route),
		logging.F("method", r.Method),
		logging.F("status", status),
		logging.F("dur_ms", d.Milliseconds()),
		logging.F("key_label", r.Header.Get("X-Gateway-Key-Label")),
		logging.F("remote", clientIP(r)),
	)
}

func routeName(path string) string {
	switch path {
	case "/v1/audio/transcriptions":
		return "stt"
	case "/v1/audio/speech":
		return "tts"
	case "/v1/models":
		return "models"
	case "/health":
		return "health"
	case "/metrics":
		return "metrics"
	}
	return path
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// ------------------------------------------------------------------ auth

// authenticate returns (key label, ok). ok=true with label "anon" means auth
// is disabled in config.
func (s *Server) authenticate(r *http.Request, cfg *config.Config) (string, bool) {
	if len(cfg.Auth.Keys) == 0 {
		return "anon", true
	}
	authz := r.Header.Get("Authorization")
	if authz == "" {
		// Also accept ?api_key= for clients that cannot set headers (browsers).
		if v := r.URL.Query().Get("api_key"); v != "" {
			authz = "Bearer " + v
		} else {
			return "missing", false
		}
	}
	token, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok {
		return "malformed", false
	}
	token = strings.TrimSpace(token)
	for _, k := range cfg.Auth.Keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(k.Value)) == 1 {
			return k.Label, true
		}
	}
	return "unknown_key", false
}

// ------------------------------------------------------------------ rate limit

// limiterSet is a per-(label|ip) token bucket, reconfigured on reload.
type limiterSet struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
	enabled bool
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiterSet() *limiterSet {
	return &limiterSet{buckets: map[string]*bucket{}}
}

// reconfigure applies current config values (cheap, called per request).
func (l *limiterSet) reconfigure(cfg *config.Config) {
	l.mu.Lock()
	l.enabled = cfg.RateLimit.Enabled
	l.rate = float64(cfg.RateLimit.RequestsPerMinute) / 60.0
	l.burst = float64(cfg.RateLimit.Burst)
	l.mu.Unlock()
}

func (s *Server) allow(cfg *config.Config, keyLabel, ip string) bool {
	l := s.limiters
	l.reconfigure(cfg)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled {
		return true
	}
	id := keyLabel + "|" + ip
	b, ok := l.buckets[id]
	now := time.Now()
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[id] = b
	}
	// refill
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	// opportunistic cleanup: keep the map from growing unbounded
	if len(l.buckets) > 4096 {
		for k, v := range l.buckets {
			if now.Sub(v.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	return true
}

// ------------------------------------------------------------------ helpers

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// writeError emits an OpenAI-shaped error body.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error","code":%q}}`+"\n", message, code)
}
