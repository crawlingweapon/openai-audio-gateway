package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression: upstream TTS 400s must be relayed to the client as errors —
// never re-wrapped as WAV audio with a 200. Found by round-trip ASR on the
// deploy box: gufo 400s on response_format="", the gateway read the error
// JSON, wrapped it in a RIFF header via buildWAV, and served 172 bytes of
// "audio" (44-byte header + the JSON body as fake PCM).
func TestE2ETTSUpstreamErrorRelayedNotWrapped(t *testing.T) {
	// upstream behaves like gufo: only explicit wav is accepted
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		rf, _ := body["response_format"].(string)
		if rf != "wav" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"unsupported response format","type":"invalid_request_error","code":"bad_format"}}`)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(mockWAV())
	}))
	defer upstream.Close()

	ts, _ := testServer(t, upstream.URL, upstream.URL, nil)

	// client omits response_format entirely — gateway must force wav upstream
	resp := doJSON(t, ts.URL+"/v1/audio/speech",
		`{"model":"voice/female-warm","input":"hello there"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("want 200 (gateway must force wav upstream), got %d: %s", resp.StatusCode, b)
	}
	if string(b[:4]) != "RIFF" {
		t.Fatalf("not audio: %s", b)
	}
	if len(b) <= 44 {
		t.Fatalf("suspiciously small payload (%d b) — likely an error body wearing a WAV header", len(b))
	}
}

// A gufo-style hard reject (400 with OpenAI error body) must reach the client
// with its status and body intact — the original bug returned 200 + garbage.
func TestE2ETTSUpstream400RelayedVerbatim(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"Qwen3-TTS unsupported response format","type":"invalid_request_error","code":"bad_format"}}`)
	}))
	defer upstream.Close()

	ts, _ := testServer(t, upstream.URL, upstream.URL, nil)

	resp := doJSON(t, ts.URL+"/v1/audio/speech",
		`{"model":"voice/female-warm","input":"hello"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 400 {
		t.Errorf("want upstream 400 relayed, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(b), "unsupported response format") {
		t.Errorf("upstream body not relayed: %s", b)
	}
	if len(b) >= 4 && string(b[:4]) == "RIFF" {
		t.Error("error body was wrapped as WAV audio")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("content-type = %q, want json", ct)
	}
}

// Upstream 5xx that survives the failover chain → relayed, not converted.
func TestE2ETTSUpstream5xxRelayed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"message":"model loading","code":"unavailable"}}`)
	}))
	defer upstream.Close()

	ts, _ := testServer(t, upstream.URL, upstream.URL, nil)

	resp := doJSON(t, ts.URL+"/v1/audio/speech",
		`{"model":"voice/female-warm","input":"hello"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 503 {
		t.Errorf("want 503 relayed, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(b), "model loading") {
		t.Errorf("body not relayed: %s", b)
	}
}

// doJSON POSTs a JSON body with auth off (test configs have no keys).
func doJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
