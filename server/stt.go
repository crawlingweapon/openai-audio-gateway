package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/crawlingweapon/openai-audio-gateway/config"
	"github.com/crawlingweapon/openai-audio-gateway/logging"
	"github.com/crawlingweapon/openai-audio-gateway/transcode"
)

// handleTranscriptions implements POST /v1/audio/transcriptions.
//
// Pipeline: parse multipart → sniff format → normalize to WAV when needed
// (ffmpeg, bounded by a concurrency pool) → rebuild multipart with the
// resolved upstream model → forward through the failover chain → stream the
// response back untouched.
func (s *Server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	route := "stt"
	s.metrics.AddInFlight(route, 1)
	defer s.metrics.AddInFlight(route, -1)

	r.Body = http.MaxBytesReader(w, r.Body, cfg.STT.MaxUploadBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_multipart",
			fmt.Sprintf("could not parse multipart body: %v", err))
		return
	}
	defer r.MultipartForm.RemoveAll()

	// --- extract the file part (first file field wins; OpenAI uses "file")
	var fileHeader *multipart.FileHeader
	var fieldName string
	for name, files := range r.MultipartForm.File {
		if len(files) > 0 {
			fileHeader, fieldName = files[0], name
			break
		}
	}
	if fileHeader == nil {
		writeError(w, http.StatusBadRequest, "missing_file", "No audio file in request.")
		return
	}
	src, err := fileHeader.Open()
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable_file", "Could not read upload.")
		return
	}

	// --- read magic bytes for sniffing, then rejoin the two readers
	head := make([]byte, 32)
	n, _ := io.ReadFull(src, head)
	head = head[:n]
	body, err := io.ReadAll(src)
	src.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_error", "Could not read upload.")
		return
	}
	full := append(head, body...)

	format := transcode.DetectFormat(head)
	needsWAV := cfg.STT.Transcode == "always" ||
		(cfg.STT.Transcode == "auto" && format != "wav")

	var audio []byte = full
	if needsWAV {
		if err := s.transcodePool().Acquire(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "busy",
				"Transcode pool saturated, retry shortly.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(),
			time.Duration(cfg.STT.TranscodeTimeoutSeconds)*time.Second)
		start := time.Now()
		audio, err = transcode.ToWAV(ctx, bytes.NewReader(full),
			cfg.STT.SampleRate, cfg.STT.Channels)
		cancel()
		s.transcodePool().Release()
		if err != nil {
			s.log.Error("stt transcode failed",
				logging.F("req_id", r.Header.Get("X-Request-Id")),
				logging.F("format", format), logging.F("error", errText(err)))
			writeError(w, http.StatusBadRequest, "transcode_failed",
				"Could not decode audio input.")
			return
		}
		s.metrics.IncTranscode("in", format)
		s.log.Debug("stt transcoded",
			logging.F("req_id", r.Header.Get("X-Request-Id")),
			logging.F("from", format), logging.F("in_bytes", len(full)),
			logging.F("out_bytes", len(audio)), logging.F("ms", time.Since(start).Milliseconds()))
	}

	// --- resolve the model the client asked for
	clientModel := r.FormValue("model")
	if clientModel == "" {
		clientModel = cfg.STT.DefaultModel
	}
	upstreamModel := cfg.STT.DefaultModel
	if resolved, ok := cfg.STT.ModelAliases[clientModel]; ok {
		upstreamModel = resolved
	} else if _, ok := findSTTPreset(cfg, clientModel); ok {
		upstreamModel = clientModel // STT presets resolve to their own id upstream
	} else {
		upstreamModel = clientModel // unknown id: pass through untouched
	}

	// --- forward
	res, err := s.doUpstream(r.Context(), cfg, cfg.STT.Upstreams, route,
		func(u config.Upstream) (*http.Request, error) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			// copy every form field except model (replaced) and the file
			for k, vs := range r.MultipartForm.Value {
				if k == "model" {
					continue
				}
				for _, v := range vs {
					if err := mw.WriteField(k, v); err != nil {
						return nil, err
					}
				}
			}
			if err := mw.WriteField("model", upstreamModel); err != nil {
				return nil, err
			}
			fw, err := mw.CreateFormFile(fieldName, fileHeader.Filename)
			if err != nil {
				return nil, err
			}
			if _, err := fw.Write(audio); err != nil {
				return nil, err
			}
			if err := mw.Close(); err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
				fmt.Sprintf("%s/audio/transcriptions", u.URL), &buf)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", mw.FormDataContentType())
			req.Header.Set("X-Request-Id", r.Header.Get("X-Request-Id"))
			return req, nil
		})
	if err != nil {
		s.log.Error("stt upstream failure",
			logging.F("req_id", r.Header.Get("X-Request-Id")),
			logging.F("error", errText(err)))
		writeError(w, http.StatusBadGateway, "upstream_unavailable",
			"No speech-to-text upstream available.")
		return
	}
	defer drainAndClose(res.body)

	copyHeader(w.Header(), res.header)
	w.WriteHeader(res.status)
	io.Copy(w, res.body)

	s.log.Info("stt forwarded",
		logging.F("req_id", r.Header.Get("X-Request-Id")),
		logging.F("upstream", res.name),
		logging.F("client_model", clientModel),
		logging.F("upstream_model", upstreamModel),
		logging.F("input_format", format),
		logging.F("transcoded", needsWAV),
		logging.F("failovers", res.failovers),
		logging.F("status", res.status),
	)
}

// findSTTPreset lets STT presets exist later without schema change: today it
// always returns ok=false and the alias/passthrough path handles everything.
func findSTTPreset(cfg *config.Config, model string) (string, bool) {
	return "", false
}

// handleHealth reports gateway + upstream reachability.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	up := map[string]string{}
	for _, u := range cfg.Upstreams {
		up[u.Name] = s.probeUpstream(r, u)
	}
	status := "ok"
	for _, v := range up {
		if v != "ok" {
			status = "degraded"
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":%q,"version":%q,"ffmpeg":%t,"upstreams":{`,
		status, Version, transcode.Available() == nil)
	first := true
	for name, st := range up {
		if !first {
			io.WriteString(w, ",")
		}
		first = false
		fmt.Fprintf(w, "%q:%q", name, st)
	}
	io.WriteString(w, "}}\n")
}

func (s *Server) probeUpstream(r *http.Request, u config.Upstream) string {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL+"/models", nil)
	if err != nil {
		return "bad_url"
	}
	if u.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.APIKey)
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return "unreachable"
	}
	drainAndClose(resp.Body)
	if resp.StatusCode >= 500 {
		return fmt.Sprintf("error_%d", resp.StatusCode)
	}
	return "ok"
}

// handleMetrics serves Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	if !cfg.Metrics.Enabled {
		writeError(w, http.StatusNotFound, "not_found", "Metrics disabled.")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	io.WriteString(w, s.metrics.Render())
}

// transcodePool builds pools lazily from the current config each call is
// cheap: pools are recreated only when capacity changes.

func (s *Server) transcodePool() *transcode.Pool {
	return s.poolFor("stt", s.cfg.Get().STT.MaxConcurrent)
}
