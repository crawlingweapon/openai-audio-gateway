// Package config loads and validates the gateway's YAML configuration.
//
// Everything except `listen` is designed to be hot-reloaded: the server swaps
// the whole config atomically on SIGHUP (or file change), validates first, and
// keeps the previous config if the new one is bad.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version   int        `yaml:"version"`
	Listen    Listen     `yaml:"listen"`
	Auth      Auth       `yaml:"auth"`
	Log       Log        `yaml:"log"`
	CORS      CORS       `yaml:"cors"`
	Metrics   Metrics    `yaml:"metrics"`
	RateLimit RateLimit  `yaml:"rate_limit"`
	Upstreams []Upstream `yaml:"upstreams"`
	STT       STT        `yaml:"stt"`
	TTS       TTS        `yaml:"tts"`
	path      string     `yaml:"-"`
	dir       string     `yaml:"-"`
}

type Listen struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type Auth struct {
	AutoGenerate bool      `yaml:"auto_generate"`
	KeysFile     string    `yaml:"keys_file"`
	Keys         []AuthKey `yaml:"keys"`
}

type AuthKey struct {
	Value string `yaml:"value"`
	Label string `yaml:"label"`
}

type Log struct {
	Level  string `yaml:"level"`
	Syslog Syslog `yaml:"syslog"`
}

type Syslog struct {
	Enabled  bool   `yaml:"enabled"`
	Address  string `yaml:"address"`
	Tag      string `yaml:"tag"`
	Facility string `yaml:"facility"`
}

type CORS struct {
	Enabled bool     `yaml:"enabled"`
	Origins []string `yaml:"origins"`
}

type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

type RateLimit struct {
	Enabled           bool `yaml:"enabled"`
	RequestsPerMinute int  `yaml:"requests_per_minute"`
	Burst             int  `yaml:"burst"`
}

type Upstream struct {
	Name           string `yaml:"name"`
	URL            string `yaml:"url"`
	APIKey         string `yaml:"api_key"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	TLSSkipVerify  bool   `yaml:"tls_skip_verify"`
}

type STT struct {
	Upstreams               []string          `yaml:"upstreams"`
	DefaultModel            string            `yaml:"default_model"`
	ModelAliases            map[string]string `yaml:"model_aliases"`
	Transcode               string            `yaml:"transcode"`
	SampleRate              int               `yaml:"sample_rate"`
	Channels                int               `yaml:"channels"`
	MaxConcurrent           int               `yaml:"max_concurrent"`
	MaxUploadBytes          int64             `yaml:"max_upload_bytes"`
	TranscodeTimeoutSeconds int               `yaml:"transcode_timeout_seconds"`
}

type TTS struct {
	Upstreams                   []string          `yaml:"upstreams"`
	DefaultModel                string            `yaml:"default_model"`
	ResponseFormatIfUnspecified string            `yaml:"response_format_if_unspecified"`
	MaxInputChars               int               `yaml:"max_input_chars"`
	SpeedHandling               string            `yaml:"speed_handling"`
	MaxConcurrent               int               `yaml:"max_concurrent"`
	TranscodeTimeoutSeconds     int               `yaml:"transcode_timeout_seconds"`
	ModelAliases                map[string]string `yaml:"model_aliases"`
	VoiceAliases                map[string]string `yaml:"voice_aliases"`
	Presets                     []Preset          `yaml:"presets"`
}

// Preset is a named TTS model clients can select. The full id is user-defined.
type Preset struct {
	ID                string `yaml:"id"`
	Model             string `yaml:"model"`
	Voice             string `yaml:"voice"`
	Language          string `yaml:"language"`
	Instructions      string `yaml:"instructions"`
	AllowBodyOverride bool   `yaml:"allow_body_override"`
}

// atomic holder so request handlers can swap configs without locks.
type Holder struct {
	v atomic.Pointer[Config]
}

func (h *Holder) Get() *Config { return h.v.Load() }

func (h *Holder) Set(c *Config) { h.v.Store(c) }

// Load reads, parses (strictly), and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // unknown keys are errors: typos fail loudly
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.path = abs
	c.dir = filepath.Dir(abs)
	if err := c.applyDefaultsAndValidate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Reload re-reads the file this config was loaded from.
func Reload(current *Config) (*Config, error) {
	if current == nil || current.path == "" {
		return nil, fmt.Errorf("no previous config to reload from")
	}
	return Load(current.path)
}
