package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `
version: 1
listen:
  port: 8086
upstreams:
  - name: asr
    url: http://127.0.0.1:8084/v1
  - name: tts
    url: http://127.0.0.1:8085/v1
stt:
  upstreams: [asr]
  default_model: default/asr
tts:
  upstreams: [tts]
  default_model: default/tts
  presets:
    - id: voice/warm
      model: qwen3-tts
      voice: voicea
`

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMinimal(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen.Port != 8086 {
		t.Errorf("port = %d", cfg.Listen.Port)
	}
	if cfg.STT.Transcode != "auto" {
		t.Errorf("stt.transcode default = %q, want auto", cfg.STT.Transcode)
	}
	if cfg.STT.SampleRate != 16000 || cfg.STT.Channels != 1 {
		t.Errorf("ffmpeg defaults = %d/%d", cfg.STT.SampleRate, cfg.STT.Channels)
	}
	if cfg.TTS.ResponseFormatIfUnspecified != "passthrough" {
		t.Errorf("response format default = %q", cfg.TTS.ResponseFormatIfUnspecified)
	}
	if cfg.Upstreams[0].TimeoutSeconds != 600 {
		t.Errorf("timeout default = %d", cfg.Upstreams[0].TimeoutSeconds)
	}
	if len(cfg.TTS.Presets) != 1 || cfg.TTS.Presets[0].ID != "voice/warm" {
		t.Errorf("presets = %+v", cfg.TTS.Presets)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	bad := strings.Replace(minimal, "listen:", "listne:\n  port: 8086\nlisten_was:", 1)
	// typo'd top-level key must fail loudly
	if _, err := Load(write(t, "version: 1\nupstrems: []\n")); err == nil {
		t.Fatal("expected unknown-key error, got nil")
	}
	_ = bad
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"no upstreams":  "version: 1\nstt: {upstreams: [x], default_model: m}\ntts: {upstreams: [x], default_model: m}\n",
		"bad url":       strings.Replace(minimal, "http://127.0.0.1:8084/v1", "ftp://nope", 1),
		"unknown ref":   strings.Replace(minimal, "upstreams: [asr]", "upstreams: [ghost]", 1),
		"bad transcode": strings.Replace(minimal, "  default_model: default/asr", "  default_model: default/asr\n  transcode: sometimes", 1),
		"dup preset":    minimal + "      - id: voice/warm\n        model: x\n",
		"bad version":   strings.Replace(minimal, "version: 1", "version: 99", 1),
	}
	for name, content := range cases {
		if _, err := Load(write(t, content)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestGeneratedKeyLifecycle(t *testing.T) {
	path := write(t, strings.Replace(minimal, "listen:", "auth:\n  auto_generate: true\n  keys_file: gen.yaml\nlisten:", 1))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Auth.Keys) != 1 {
		t.Fatalf("expected generated key, got %d", len(cfg.Auth.Keys))
	}
	if len(cfg.Auth.Keys[0].Value) < 32 {
		t.Errorf("generated key too short: %d", len(cfg.Auth.Keys[0].Value))
	}
	// key file must be 0600 and reload stable (same key, not regenerated)
	genPath := filepath.Join(filepath.Dir(path), "gen.yaml")
	st, err := os.Stat(genPath)
	if err != nil {
		t.Fatalf("keys file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("keys file mode = %v, want 0600", st.Mode().Perm())
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg2.Auth.Keys[0].Value != cfg.Auth.Keys[0].Value {
		t.Error("key regenerated instead of reloaded")
	}
}

func TestHardcodedKeysWithLabels(t *testing.T) {
	content := strings.Replace(minimal, "listen:", `auth:
  keys:
    - value: aaa
      label: owui
    - value: bbb
listen:
`, 1)
	cfg, err := Load(write(t, content))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Keys[1].Label != "key1" {
		t.Errorf("default label = %q, want key1", cfg.Auth.Keys[1].Label)
	}
}

func TestEndpoint(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Endpoint(); got != "0.0.0.0:8086" {
		t.Errorf("Endpoint() = %q", got)
	}
}
