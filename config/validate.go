package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func (c *Config) applyDefaultsAndValidate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d (want 1)", c.Version)
	}
	if c.Listen.Port == 0 {
		c.Listen.Port = 8086
	}
	if c.Listen.Host == "" {
		c.Listen.Host = "0.0.0.0"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level: unknown %q", c.Log.Level)
	}
	if c.Log.Syslog.Enabled {
		if c.Log.Syslog.Address == "" {
			c.Log.Syslog.Address = "/dev/log"
		}
		if c.Log.Syslog.Tag == "" {
			c.Log.Syslog.Tag = "audio-gateway"
		}
		if c.Log.Syslog.Facility == "" {
			c.Log.Syslog.Facility = "local0"
		}
		if !validFacilities[c.Log.Syslog.Facility] {
			return fmt.Errorf("log.syslog.facility: unknown %q", c.Log.Syslog.Facility)
		}
	}
	if c.Metrics.Path == "" {
		c.Metrics.Path = "/metrics"
	}
	if c.RateLimit.RequestsPerMinute == 0 {
		c.RateLimit.RequestsPerMinute = 300
	}
	if c.RateLimit.Burst == 0 {
		c.RateLimit.Burst = 60
	}

	if err := c.validateUpstreams(); err != nil {
		return err
	}
	if err := c.validateSTT(); err != nil {
		return err
	}
	if err := c.validateTTS(); err != nil {
		return err
	}
	for i, k := range c.Auth.Keys {
		if k.Value == "" {
			return fmt.Errorf("auth.keys[%d]: empty value", i)
		}
		if k.Label == "" {
			c.Auth.Keys[i].Label = fmt.Sprintf("key%d", i)
		}
	}
	if len(c.Auth.Keys) == 0 && c.Auth.AutoGenerate {
		if err := c.ensureGeneratedKey(); err != nil {
			return err
		}
	}
	return nil
}

var validFacilities = map[string]bool{
	"auth": true, "daemon": true, "user": true, "local0": true,
	"local1": true, "local2": true, "local3": true, "local4": true,
	"local5": true, "local6": true, "local7": true,
}

func (c *Config) validateUpstreams() error {
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("upstreams: at least one required")
	}
	seen := map[string]bool{}
	for i, u := range c.Upstreams {
		if u.Name == "" {
			return fmt.Errorf("upstreams[%d]: name required", i)
		}
		if seen[u.Name] {
			return fmt.Errorf("upstreams: duplicate name %q", u.Name)
		}
		seen[u.Name] = true
		parsed, err := url.Parse(u.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("upstream %q: bad url %q", u.Name, u.URL)
		}
		if u.TimeoutSeconds == 0 {
			c.Upstreams[i].TimeoutSeconds = 600
		}
	}
	for _, name := range c.STT.Upstreams {
		if !seen[name] {
			return fmt.Errorf("stt.upstreams: unknown upstream %q", name)
		}
	}
	for _, name := range c.TTS.Upstreams {
		if !seen[name] {
			return fmt.Errorf("tts.upstreams: unknown upstream %q", name)
		}
	}
	return nil
}

func (c *Config) validateSTT() error {
	s := &c.STT
	if len(s.Upstreams) == 0 {
		return fmt.Errorf("stt.upstreams: at least one required")
	}
	if s.DefaultModel == "" {
		return fmt.Errorf("stt.default_model required")
	}
	if s.Transcode == "" {
		s.Transcode = "auto"
	}
	switch s.Transcode {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("stt.transcode: unknown %q (auto|always|never)", s.Transcode)
	}
	if s.SampleRate == 0 {
		s.SampleRate = 16000
	}
	if s.Channels == 0 {
		s.Channels = 1
	}
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = 4
	}
	if s.MaxUploadBytes == 0 {
		s.MaxUploadBytes = 100 << 20
	}
	if s.TranscodeTimeoutSeconds == 0 {
		s.TranscodeTimeoutSeconds = 120
	}
	return nil
}

func (c *Config) validateTTS() error {
	t := &c.TTS
	if len(t.Upstreams) == 0 {
		return fmt.Errorf("tts.upstreams: at least one required")
	}
	if t.DefaultModel == "" {
		return fmt.Errorf("tts.default_model required")
	}
	if t.ResponseFormatIfUnspecified == "" {
		t.ResponseFormatIfUnspecified = "passthrough"
	}
	switch t.ResponseFormatIfUnspecified {
	case "passthrough", "mp3", "wav", "opus", "flac", "aac", "pcm":
	default:
		return fmt.Errorf("tts.response_format_if_unspecified: unknown %q", t.ResponseFormatIfUnspecified)
	}
	if t.SpeedHandling == "" {
		t.SpeedHandling = "strip"
	}
	switch t.SpeedHandling {
	case "strip", "atempo", "reject":
	default:
		return fmt.Errorf("tts.speed_handling: unknown %q (strip|atempo|reject)", t.SpeedHandling)
	}
	if t.MaxConcurrent <= 0 {
		t.MaxConcurrent = 4
	}
	if t.TranscodeTimeoutSeconds == 0 {
		t.TranscodeTimeoutSeconds = 120
	}
	seen := map[string]bool{}
	for i, p := range t.Presets {
		if p.ID == "" {
			return fmt.Errorf("tts.presets[%d]: id required", i)
		}
		if seen[p.ID] {
			return fmt.Errorf("tts.presets: duplicate id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Model == "" {
			return fmt.Errorf("tts.presets[%q]: model required", p.ID)
		}
	}
	return nil
}

// EnsureGeneratedKey persists a fresh CSRNG key when none exist. Idempotent.
func (c *Config) ensureGeneratedKey() error {
	path := c.keysFilePath()
	if path == "" {
		return nil
	}
	if raw, err := os.ReadFile(path); err == nil {
		var existing Config
		if err := yaml.Unmarshal(raw, &existing); err == nil && len(existing.Auth.Keys) > 0 {
			c.Auth.Keys = existing.Auth.Keys
			return nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	key := AuthKey{
		Value: base64.RawURLEncoding.EncodeToString(buf),
		Label: "generated",
	}
	out, err := yaml.Marshal(struct {
		Auth Auth `yaml:"auth"`
	}{Auth: Auth{Keys: []AuthKey{key}}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("persist generated key: %w", err)
	}
	c.Auth.Keys = []AuthKey{key}
	return nil
}

func (c *Config) keysFilePath() string {
	if c.Auth.KeysFile == "" {
		return ""
	}
	if filepath.IsAbs(c.Auth.KeysFile) {
		return c.Auth.KeysFile
	}
	return filepath.Join(c.dir, c.Auth.KeysFile)
}

// KeysFilePath exposes the resolved generated-keys path ("" if none).
func (c *Config) KeysFilePath() string { return c.keysFilePath() }

// Fingerprint returns a short stable id for a key value (never the value itself).
func Fingerprint(v string) string {
	if len(v) <= 8 {
		return v
	}
	return v[:8]
}

// Endpoint returns host:port for the listen block.
func (c *Config) Endpoint() string {
	return fmt.Sprintf("%s:%d", c.Listen.Host, c.Listen.Port)
}

// PreservePath returns a copy of newcfg with the original path/dir set, so a
// reload keeps pointing at the same file.
func PreservePath(original, newcfg *Config) *Config {
	if original != nil {
		newcfg.path = original.path
		newcfg.dir = original.dir
	}
	return newcfg
}

// String renders a redacted dump for logging on reload (no key material).
func (c *Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "listen=%s stt_upstreams=%v tts_upstreams=%v presets=%d keys=%d",
		c.Endpoint(), c.STT.Upstreams, c.TTS.Upstreams, len(c.TTS.Presets), len(c.Auth.Keys))
	return b.String()
}
