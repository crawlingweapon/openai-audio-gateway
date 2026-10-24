package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crawlingweapon/openai-audio-gateway/config"
	"github.com/crawlingweapon/openai-audio-gateway/logging"
	"github.com/crawlingweapon/openai-audio-gateway/metrics"
)

// mockWAV builds a tiny valid WAV so STT passthrough (transcode: auto)
// works without ffmpeg installed.
func mockWAV() []byte {
	pcm := make([]byte, 200)
	buf := new(bytes.Buffer)
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVEfmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))
	buf.Write([]byte{1, 0, 1, 0})
	binary.Write(buf, binary.LittleEndian, uint32(16000))
	binary.Write(buf, binary.LittleEndian, uint32(32000))
	buf.Write([]byte{2, 0, 16, 0})
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}

// fakeASR mimics gufo: WAV-only, specific model ids, OpenAI error shape.
func fakeASR(t *testing.T, acceptModel string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("asr: bad multipart: %v", err)
			w.WriteHeader(400)
			return
		}
		model := r.FormValue("model")
		fhs := r.MultipartForm.File["file"]
		if len(fhs) == 0 {
			w.WriteHeader(400)
			return
		}
		f, _ := fhs[0].Open()
		head := make([]byte, 12)
		io.ReadFull(f, head)
		f.Close()
		isWAV := string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE"
		if !isWAV {
			// gufo's real behavior: 500 RIFF WAV required
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"Qwen3-ASR input must be a RIFF WAV","type":"invalid_request_error","code":"transcription_failed"}}`)
			return
		}
		if model != acceptModel {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":{"message":"unsupported transcription model","code":"invalid_model"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"text":"mock transcript (model=%s)"}`, model)
	}))
}

// fakeTTS returns a WAV, gufo-style: accepts model+voice+language+instructions.
func fakeTTS(t *testing.T, acceptModel string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("tts: bad json: %v", err)
			w.WriteHeader(400)
			return
		}
		model, _ := body["model"].(string)
		if model != acceptModel {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":{"message":"unknown model %q","code":"invalid_model"}}`, model)
			return
		}
		if rf, _ := body["response_format"].(string); rf == "mp3" {
			// gufo: mp3 unsupported
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(mockWAV())
	}))
}

func testServer(t *testing.T, asrURL, ttsURL string, mutate func(*config.Config)) (*httptest.Server, *config.Holder) {
	t.Helper()
	cfg := &config.Config{
		Version: 1,
		Listen:  config.Listen{Host: "127.0.0.1", Port: 0},
		Upstreams: []config.Upstream{
			{Name: "asr", URL: asrURL + "/v1", TimeoutSeconds: 30},
			{Name: "tts", URL: ttsURL + "/v1", TimeoutSeconds: 30},
		},
		STT: config.STT{
			Upstreams: []string{"asr"}, DefaultModel: "default/qwen3-asr-1.7b",
			ModelAliases: map[string]string{
				"default/qwen3-asr-1.7b": "qwen3-asr-1.7b",
				"whisper-1":              "qwen3-asr-1.7b",
			},
			Transcode: "auto", SampleRate: 16000, Channels: 1,
			MaxConcurrent: 2, MaxUploadBytes: 1 << 20, TranscodeTimeoutSeconds: 30,
		},
		TTS: config.TTS{
			Upstreams: []string{"tts"}, DefaultModel: "default/qwen3-tts",
			ResponseFormatIfUnspecified: "passthrough",
			MaxInputChars:               4096, SpeedHandling: "strip",
			MaxConcurrent: 2, TranscodeTimeoutSeconds: 30,
			ModelAliases: map[string]string{"default/qwen3-tts": "qwen3-tts"},
			VoiceAliases: map[string]string{"alloy": "voicea"},
			Presets: []config.Preset{
				{ID: "default/qwen3-tts", Model: "qwen3-tts", Voice: "voicea", Language: "english"},
				{ID: "voice/female-warm", Model: "qwen3-tts", Voice: "voicea",
					Language: "english", Instructions: "a warm female voice"},
			},
		},
		Log:     config.Log{Level: "error"},
		Metrics: config.Metrics{Enabled: true, Path: "/metrics"},
	}
	if mutate != nil {
		mutate(cfg)
	}
	holder := &config.Holder{}
	holder.Set(cfg)
	log, err := logging.New(logging.Error, "", "", "local0")
	if err != nil {
		t.Fatal(err)
	}
	gw := New(holder, log, metrics.New())
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)
	return ts, holder
}

func postMultipart(t *testing.T, url, fieldFile string, fileBytes []byte, form map[string]string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range form {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile(fieldFile, "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(fileBytes)
	mw.Close()
	resp, err := http.Post(url, mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestE2ESTTModelAliasAndWAVPassthrough(t *testing.T) {
	asr := fakeASR(t, "qwen3-asr-1.7b")
	defer asr.Close()
	ts, _ := testServer(t, asr.URL, asr.URL, nil)

	// client-visible id must be rewritten to the upstream id
	resp := postMultipart(t, ts.URL+"/v1/audio/transcriptions", "file", mockWAV(),
		map[string]string{"model": "whisper-1", "language": "en"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "qwen3-asr-1.7b") {
		t.Errorf("alias not applied: %s", b)
	}
}

func TestE2ESTTDefaultModelWhenOmitted(t *testing.T) {
	asr := fakeASR(t, "qwen3-asr-1.7b")
	defer asr.Close()
	ts, _ := testServer(t, asr.URL, asr.URL, nil)

	resp := postMultipart(t, ts.URL+"/v1/audio/transcriptions", "file", mockWAV(), nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
}

func TestE2ESTTFailoverOn5xx(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer dead.Close()
	live := fakeASR(t, "qwen3-asr-1.7b")
	defer live.Close()

	ts, _ := testServer(t, live.URL, live.URL, func(c *config.Config) {
		c.Upstreams = []config.Upstream{
			{Name: "asr-dead", URL: dead.URL + "/v1", TimeoutSeconds: 10},
			c.Upstreams[0], c.Upstreams[1],
		}
		c.STT.Upstreams = []string{"asr-dead", "asr"}
	})
	resp := postMultipart(t, ts.URL+"/v1/audio/transcriptions", "file", mockWAV(), nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("failover did not happen, status %d: %s", resp.StatusCode, b)
	}
}

func TestE2ESTTRejectsBadModelFromUpstream(t *testing.T) {
	asr := fakeASR(t, "qwen3-asr-1.7b")
	defer asr.Close()
	ts, _ := testServer(t, asr.URL, asr.URL, nil)

	// unknown id passes through unmapped; upstream 400s → we relay it as-is
	resp := postMultipart(t, ts.URL+"/v1/audio/transcriptions", "file", mockWAV(),
		map[string]string{"model": "totally-unknown"})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("want upstream 400 relayed, got %d", resp.StatusCode)
	}
}

func TestE2ETTSPresetRewritesBody(t *testing.T) {
	var seen map[string]any
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(mockWAV())
	}))
	defer tts.Close()
	ts, _ := testServer(t, tts.URL, tts.URL, nil)

	body, _ := json.Marshal(map[string]any{
		"model": "voice/female-warm", "input": "hello", "voice": "alloy",
	})
	resp, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if seen["model"] != "qwen3-tts" {
		t.Errorf("preset model not applied: %v", seen["model"])
	}
	if seen["voice"] != "voicea" {
		t.Errorf("preset voice not applied: %v", seen["voice"])
	}
	if seen["instructions"] != "a warm female voice" {
		t.Errorf("preset instructions not applied: %v", seen["instructions"])
	}
	if seen["language"] != "english" {
		t.Errorf("preset language not applied: %v", seen["language"])
	}
	if resp.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("passthrough content-type = %q", resp.Header.Get("Content-Type"))
	}
}

func TestE2ETTSBuiltinPassthroughWithVoiceAlias(t *testing.T) {
	var seen map[string]any
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(mockWAV())
	}))
	defer tts.Close()
	ts, _ := testServer(t, tts.URL, tts.URL, nil)

	// default/qwen3-tts is a preset with fixed voice; alloy→voicea alias applies
	// on the non-preset path. Use an unmapped raw id for pure passthrough.
	body, _ := json.Marshal(map[string]any{
		"model": "qwen3-tts-12hz-1.7b-base", "input": "hi", "voice": "alloy",
	})
	resp, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if seen["model"] != "qwen3-tts-12hz-1.7b-base" {
		t.Errorf("unknown model mutated: %v", seen["model"])
	}
	if seen["voice"] != "voicea" {
		t.Errorf("voice alias not applied: %v", seen["voice"])
	}
}

func TestE2EAuthRejectsAndAccepts(t *testing.T) {
	asr := fakeASR(t, "qwen3-asr-1.7b")
	defer asr.Close()
	ts, _ := testServer(t, asr.URL, asr.URL, func(c *config.Config) {
		c.Auth.Keys = []config.AuthKey{{Value: "sekrit", Label: "owui"}}
	})

	// no key → 401 + OpenAI error shape
	resp := postMultipart(t, ts.URL+"/v1/audio/transcriptions", "file", mockWAV(), nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"error"`) || !strings.Contains(string(b), "invalid_api_key") {
		t.Errorf("error shape wrong: %s", b)
	}

	// wrong key → 401
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer nope")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 401 {
		t.Errorf("wrong key: want 401, got %d", r2.StatusCode)
	}

	// right key → 200
	req3, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer sekrit")
	r3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Errorf("right key: want 200, got %d", r3.StatusCode)
	}
}

func TestE2EModelsMergesPresetsAndUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"qwen3-asr-1.7b","object":"model"}]}`)
	}))
	defer up.Close()
	ts, _ := testServer(t, up.URL, up.URL, nil)

	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer ")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	ids := map[string]bool{}
	for _, d := range out.Data {
		ids[d.ID] = true
	}
	for _, want := range []string{"voice/female-warm", "default/qwen3-tts", "whisper-1", "qwen3-asr-1.7b"} {
		if !ids[want] {
			t.Errorf("models list missing %q (got %v)", want, ids)
		}
	}
}

func TestE2EHealth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	defer up.Close()
	ts, _ := testServer(t, up.URL, up.URL, nil)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "ok" {
		t.Errorf("health = %v", out)
	}
}

func TestE2EMetricsRender(t *testing.T) {
	asr := fakeASR(t, "qwen3-asr-1.7b")
	defer asr.Close()
	ts, _ := testServer(t, asr.URL, asr.URL, nil)
	resp := postMultipart(t, ts.URL+"/v1/audio/transcriptions", "file", mockWAV(), nil)
	resp.Body.Close()

	m, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Body.Close()
	b, _ := io.ReadAll(m.Body)
	if !strings.Contains(string(b), "audio_gateway_requests_total") {
		t.Errorf("metrics missing request counter: %s", b)
	}
}
