package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/crawlingweapon/openai-audio-gateway/config"
	"github.com/crawlingweapon/openai-audio-gateway/logging"
)

// upstreamResult is a successful or final response from the chain.
type upstreamResult struct {
	name      string
	status    int
	header    http.Header
	body      io.ReadCloser
	failovers int
}

// transport is shared: keep-alive pooling to the LAN upstreams.
var transport = &http.Transport{
	MaxIdleConns:        32,
	MaxIdleConnsPerHost: 16,
	IdleConnTimeout:     90 * time.Second,
	TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
}

// doUpstream tries each named upstream in order. Connection errors and 5xx
// responses advance to the next candidate; anything else (2xx/4xx) returns
// immediately — a 400 from upstream is the client's problem, not a failover.
// The returned result carries which upstream answered and how many failovers
// happened before it.
func (s *Server) doUpstream(ctx context.Context, cfg *config.Config, names []string,
	route string, build func(u config.Upstream) (*http.Request, error)) (*upstreamResult, error) {

	var lastErr error
	fails := 0
	for i, name := range names {
		u, ok := findUpstream(cfg, name)
		if !ok {
			lastErr = fmt.Errorf("unknown upstream %q", name)
			continue
		}
		req, err := build(u)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req = req.WithContext(ctx)
		if u.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+u.APIKey)
		}
		client := &http.Client{Transport: transport, Timeout: time.Duration(u.TimeoutSeconds) * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("upstream %s: %w", name, err)
			fails++
			if i < len(names)-1 {
				s.metrics.IncFailover(route)
				s.log.Warn("upstream failed, trying next",
					logging.F("upstream", name), logging.F("route", route),
					logging.F("error", errText(err)))
			}
			continue
		}
		if resp.StatusCode >= 500 && i < len(names)-1 {
			resp.Body.Close()
			fails++
			s.metrics.IncFailover(route)
			s.log.Warn("upstream 5xx, trying next",
				logging.F("upstream", name), logging.F("route", route),
				logging.F("status", resp.StatusCode))
			continue
		}
		return &upstreamResult{
			name: name, status: resp.StatusCode,
			header: resp.Header, body: resp.Body, failovers: fails,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no upstreams configured")
	}
	return nil, lastErr
}

func findUpstream(cfg *config.Config, name string) (config.Upstream, bool) {
	for _, u := range cfg.Upstreams {
		if u.Name == name {
			return u, true
		}
	}
	return config.Upstream{}, false
}

// copyHeader carries upstream response headers (content-type, etc.) through.
func copyHeader(dst http.Header, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Transfer-Encoding", "Connection":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// drainAndClose lets the idle pool reuse the connection.
func drainAndClose(body io.ReadCloser) {
	if body != nil {
		io.Copy(io.Discard, io.LimitReader(body, 1<<20))
		body.Close()
	}
}

func errText(err error) string {
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

// readBody enforces the upload cap while reading.
func readBody(r io.ReadCloser, max int64) ([]byte, error) {
	defer r.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, fmt.Errorf("body exceeds %d bytes", max)
	}
	return buf.Bytes(), nil
}
