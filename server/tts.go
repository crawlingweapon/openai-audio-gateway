package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crawlingweapon/openai-audio-gateway/config"
	"github.com/crawlingweapon/openai-audio-gateway/logging"
	"github.com/crawlingweapon/openai-audio-gateway/transcode"
)

// Version is stamped at build time via -ldflags "-X server.Version=...".
var Version = "dev"

// ------------------------------------------------------------------ pools

type poolSet struct {
	mu    sync.Mutex
	pools map[string]*poolEntry
}

type poolEntry struct {
	n int
	p *transcode.Pool
}

var pools = &poolSet{pools: map[string]*poolEntry{}}

func (s *Server) poolFor(kind string, n int) *transcode.Pool {
	pools.mu.Lock()
	defer pools.mu.Unlock()
	e, ok := pools.pools[kind]
	if !ok || e.n != n {
		e = &poolEntry{n: n, p: transcode.NewPool(n)}
		pools.pools[kind] = e
	}
	return e.p
}

// ------------------------------------------------------------------ speech

// speechRequest is the OpenAI /v1/audio/speech body (superset kept loose —
// unknown fields are passed through to the upstream).
type speechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float64 `json:"speed"`
	Instructions   string  `json:"instructions,omitempty"`
	Language       string  `json:"language,omitempty"`
}

// upstreamHTTPError carries a non-200 upstream response so handlers can
// relay it verbatim instead of misreading the error body as audio.
type upstreamHTTPError struct {
	Status      int
	Body        []byte
	ContentType string
}

func (e *upstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream status %d", e.Status)
}

// handleSpeech implements POST /v1/audio/speech.
//
// Pipeline: parse body → resolve preset (or alias/passthrough) → normalize
// speed/language per config → split long input on sentence boundaries and
// synthesize sequentially → concatenate WAVs → transcode to the requested
// response format if the upstream cannot produce it → stream back.
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	route := "tts"
	s.metrics.AddInFlight(route, 1)
	defer s.metrics.AddInFlight(route, -1)

	body, err := readBody(r.Body, 10<<20)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Could not read request body.")
		return
	}
	var req speechRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json",
			fmt.Sprintf("Could not parse JSON: %v", err))
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeError(w, http.StatusBadRequest, "missing_input", "`input` is required.")
		return
	}
	if req.Model == "" {
		req.Model = cfg.TTS.DefaultModel
	}

	// --- preset resolution: presets are the interface, ids are yours to name
	preset, isPreset := findPreset(cfg, req.Model)
	upstreamModel := req.Model
	if isPreset {
		upstreamModel = preset.Model
		if preset.Voice != "" && (!preset.AllowBodyOverride || req.Voice == "") {
			req.Voice = preset.Voice
		}
		req.Instructions = mergeInstructions(preset.Instructions, req.Instructions, preset.AllowBodyOverride)
		// gufo rejects its implicit "en" on some builds; presets carry full names
		if preset.Language != "" && (req.Language == "" || !preset.AllowBodyOverride) {
			req.Language = preset.Language
		}
	} else if resolved, ok := cfg.TTS.ModelAliases[req.Model]; ok {
		upstreamModel = resolved
	} // else: unknown id passes through untouched (built-in models "just work")

	// voice aliasing for OpenAI-standard names when no preset supplied one
	if !isPreset && req.Voice != "" {
		if a, ok := cfg.TTS.VoiceAliases[req.Voice]; ok {
			req.Voice = a
		}
	}

	// --- speed normalization (gufo 400s anything != 1.0)
	speedNote := ""
	switch {
	case req.Speed == 0 || req.Speed == 1.0:
		req.Speed = 1.0
	case cfg.TTS.SpeedHandling == "strip":
		speedNote = "stripped"
		req.Speed = 1.0
	case cfg.TTS.SpeedHandling == "reject":
		writeError(w, http.StatusBadRequest, "unsupported_speed",
			"This gateway does not accept speed != 1.0.")
		return
	} // "atempo" falls through unmodified (future: ffmpeg rescale)

	// --- chunking for OpenAI's 4096-char input cap
	chunks := []string{req.Input}
	chunked := false
	if cfg.TTS.MaxInputChars > 0 && len(req.Input) > cfg.TTS.MaxInputChars {
		chunks = splitSentences(req.Input, cfg.TTS.MaxInputChars)
		chunked = len(chunks) > 1
	}

	// --- synthesize sequentially, concatenate PCM
	var pcm []byte
	upstreamName := ""
	for i, text := range chunks {
		partReq := req
		partReq.Model = upstreamModel // preset/alias resolution lands here
		partReq.Input = text
		res, wav, err := s.synthesize(r, cfg, upstreamModel, partReq, route)
		if err != nil {
			var upErr *upstreamHTTPError
			if errors.As(err, &upErr) {
				// relay the upstream's own OpenAI-shaped error verbatim
				s.log.Warn("tts upstream rejected request",
					logging.F("req_id", r.Header.Get("X-Request-Id")),
					logging.F("upstream", res.name), logging.F("status", upErr.Status),
					logging.F("chunk", i))
				w.Header().Set("Content-Type", upErr.ContentType)
				if w.Header().Get("Content-Type") == "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(upErr.Status)
				w.Write(upErr.Body)
				return
			}
			s.log.Error("tts upstream failure",
				logging.F("req_id", r.Header.Get("X-Request-Id")),
				logging.F("chunk", i), logging.F("error", errText(err)))
			writeError(w, http.StatusBadGateway, "upstream_unavailable",
				"No text-to-speech upstream available.")
			return
		}
		upstreamName = res.name
		drainAndClose(res.body)
		pcm = append(pcm, stripWAVHeader(wav)...)
	}

	// --- response format: transcode only when the client asked for something
	// upstream can't produce; wav/pcm from gufo passes straight through.
	clientFormat := req.ResponseFormat
	target := cfg.TTS.ResponseFormatIfUnspecified
	if clientFormat != "" {
		target = clientFormat
	}
	wavBytes := buildWAV(pcm, 24000, 1)

	contentType := "audio/wav"
	var outBytes []byte
	if target == "" || target == "passthrough" || target == "wav" || target == "pcm" {
		outBytes = wavBytes
		if target == "pcm" {
			outBytes = pcm
			contentType = transcode.ContentTypeFor("pcm")
		} else {
			contentType = "audio/wav"
		}
	} else {
		ctx, cancel := context.WithTimeout(r.Context(),
			time.Duration(cfg.TTS.TranscodeTimeoutSeconds)*time.Second)
		defer cancel()
		if err := s.poolFor("tts-out", cfg.TTS.MaxConcurrent).Acquire(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "busy", "Transcode pool saturated.")
			return
		}
		rd, err := transcode.ConvertWAV(ctx, bytes.NewReader(wavBytes), target)
		if err != nil {
			s.poolFor("tts-out", cfg.TTS.MaxConcurrent).Release()
			s.log.Error("tts out-transcode failed", logging.F("error", errText(err)))
			writeError(w, http.StatusInternalServerError, "transcode_failed",
				"Could not encode requested audio format.")
			return
		}
		outBytes, err = io.ReadAll(rd)
		s.poolFor("tts-out", cfg.TTS.MaxConcurrent).Release()
		if err != nil {
			s.log.Error("tts out-transcode read failed", logging.F("error", errText(err)))
			writeError(w, http.StatusInternalServerError, "transcode_failed",
				"Could not encode requested audio format.")
			return
		}
		contentType = transcode.ContentTypeFor(target)
		s.metrics.IncTranscode("out", target)
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(outBytes)))
	w.WriteHeader(http.StatusOK)
	w.Write(outBytes)

	s.log.Info("tts forwarded",
		logging.F("req_id", r.Header.Get("X-Request-Id")),
		logging.F("upstream", upstreamName),
		logging.F("client_model", req.Model),
		logging.F("upstream_model", upstreamModel),
		logging.F("voice", req.Voice),
		logging.F("preset", isPreset),
		logging.F("chunked", chunked),
		logging.F("chunks", len(chunks)),
		logging.F("speed", speedNote),
		logging.F("out_format", target),
		logging.F("out_bytes", len(outBytes)),
	)
}

// synthesize one chunk against the failover chain; returns WAV bytes.
// On a non-200 upstream response it returns *upstreamHTTPError so the caller
// relays the real error instead of wrapping it as audio.
func (s *Server) synthesize(r *http.Request, cfg *config.Config, model string,
	part speechRequest, route string) (*upstreamResult, []byte, error) {

	// Upstream always gets an explicit wav request: gufo rejects other
	// values (and empty strings), wav is universally valid, and any client
	// format we can't pass through is transcoded locally anyway.
	part.ResponseFormat = "wav"

	payload, err := json.Marshal(part)
	if err != nil {
		return nil, nil, err
	}
	res, err := s.doUpstream(r.Context(), cfg, cfg.TTS.Upstreams, route,
		func(u config.Upstream) (*http.Request, error) {
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
				fmt.Sprintf("%s/audio/speech", u.URL), bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-Id", r.Header.Get("X-Request-Id"))
			return req, nil
		})
	if err != nil {
		return nil, nil, err
	}
	body, err := readBody(res.body, 64<<20)
	if err != nil {
		return res, nil, err
	}
	if res.status != http.StatusOK {
		ct := res.header.Get("Content-Type")
		return res, nil, &upstreamHTTPError{Status: res.status, Body: body, ContentType: ct}
	}
	return res, body, nil
}

// ------------------------------------------------------------------ helpers

func findPreset(cfg *config.Config, id string) (config.Preset, bool) {
	for _, p := range cfg.TTS.Presets {
		if p.ID == id {
			return p, true
		}
	}
	return config.Preset{}, false
}

// mergeInstructions: preset text wins unless the preset opts into client
// overrides and the client actually sent something.
func mergeInstructions(preset, client string, allowOverride bool) string {
	if allowOverride && strings.TrimSpace(client) != "" {
		return client
	}
	if preset != "" {
		return preset
	}
	return client
}

// splitSentences splits text into chunks of at most max bytes on sentence
// boundaries (falls back to hard cuts for pathological input).
func splitSentences(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	var out []string
	var cur strings.Builder
	start := 0
	runes := []rune(text)
	// walk by byte offsets using the original string for slicing
	for i := 0; i < len(text); {
		// find next sentence end within max window
		limit := i + max
		if limit > len(text) {
			limit = len(text)
		}
		cut := strings.LastIndexAny(text[i:limit], ".!?;:\n")
		if cut < 0 {
			cut = limit // no boundary in window: hard cut at window end (absolute)
		} else {
			cut += i + 1 // include the punctuation (absolute)
		}
		piece := text[start:cut]
		if strings.TrimSpace(piece) != "" {
			out = append(out, strings.TrimSpace(piece))
		}
		i = cut
		start = cut
		for i < len(text) && text[i] == ' ' {
			i++
			start++
		}
	}
	_ = runes
	_ = cur
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

// stripWAVHeader returns only the PCM payload of a RIFF/WAVE byte slice.
func stripWAVHeader(b []byte) []byte {
	if len(b) < 44 || string(b[0:4]) != "RIFF" {
		return b // not WAV (pcm passthrough): treat whole as payload
	}
	// walk chunks to find "data"
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		sz := int(uint32(b[off+4]) | uint32(b[off+5])<<8 | uint32(b[off+6])<<16 | uint32(b[off+7])<<24)
		off += 8
		if id == "data" {
			end := off + sz
			if end > len(b) {
				end = len(b)
			}
			return b[off:end]
		}
		off += sz + sz%2 // chunks are word-aligned
	}
	return b[44:] // fallback: classic 44-byte header
}

// buildWAV assembles a canonical 44-byte-header PCM16 mono WAV.
func buildWAV(pcm []byte, rate, channels int) []byte {
	byteRate := rate * channels * 2
	blockAlign := channels * 2
	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	putU32(h[4:8], uint32(36+len(pcm)))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	putU32(h[16:20], 16)
	h[20] = 1 // PCM
	h[22] = byte(channels)
	putU32(h[24:28], uint32(rate))
	putU32(h[28:32], uint32(byteRate))
	h[32] = byte(blockAlign)
	h[34] = 16
	copy(h[36:40], "data")
	putU32(h[40:44], uint32(len(pcm)))
	return append(h, pcm...)
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

// handleModels merges preset ids, aliases, and the upstream model lists so
// client dropdowns show everything in one place.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Created int64  `json:"created"`
	}
	seen := map[string]bool{}
	var out []modelEntry
	add := func(id, owner string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, modelEntry{ID: id, Object: "model", OwnedBy: owner, Created: 1770000000})
	}
	for _, p := range cfg.TTS.Presets {
		add(p.ID, "audio-gateway")
	}
	for a := range cfg.TTS.ModelAliases {
		add(a, "audio-gateway-alias")
	}
	for a := range cfg.STT.ModelAliases {
		add(a, "audio-gateway-alias")
	}
	add(cfg.STT.DefaultModel, "audio-gateway")
	add(cfg.TTS.DefaultModel, "audio-gateway")

	// upstream listings (best-effort; failures never break the endpoint)
	for _, u := range cfg.Upstreams {
		list, err := s.fetchUpstreamModels(r, u)
		if err != nil {
			s.log.Debug("upstream models unavailable",
				logging.F("upstream", u.Name), logging.F("error", errText(err)))
			continue
		}
		for _, id := range list {
			add(id, u.Name)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   out,
	})
}

func (s *Server) fetchUpstreamModels(r *http.Request, u config.Upstream) ([]string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if u.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.APIKey)
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range parsed.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}
