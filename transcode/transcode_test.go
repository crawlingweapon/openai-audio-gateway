package transcode

import "testing"

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		want   string
	}{
		{"wav", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), "wav"},
		{"mp3-id3", []byte("ID3\x04\x00\x00\x00"), "mp3"},
		{"mp3-frame", []byte{0xFF, 0xFB, 0x90, 0x00}, "mp3"},
		{"flac", []byte("fLaC\x00\x00\x00\x22"), "flac"},
		{"ogg", []byte("OggS\x00\x02\x00\x00"), "ogg"},
		{"m4a", []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '}, "m4a"},
		{"mp4", []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, "mp4"},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x01, 0x00, 0x00, 0x00}, "webm"},
		{"empty", []byte{}, ""},
		{"garbage", []byte("hello world this is not audio"), ""},
	}
	for _, c := range cases {
		if got := DetectFormat(c.header); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestPoolBoundsConcurrency(t *testing.T) {
	p := NewPool(2)
	ctx := t.Context()
	for i := 0; i < 2; i++ {
		if err := p.Acquire(ctx); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	if p.InFlight() != 2 {
		t.Errorf("in flight = %d", p.InFlight())
	}
	// third acquire must block until released (test with a cancelled ctx)
	blocked := make(chan error, 1)
	go func() { blocked <- p.Acquire(ctx) }()
	select {
	case err := <-blocked:
		t.Errorf("acquire should have blocked, got %v", err)
	default:
	}
	p.Release()
	p.Release()
	if p.InFlight() != 0 {
		t.Errorf("after release in flight = %d", p.InFlight())
	}
}

func TestContentTypeFor(t *testing.T) {
	if ContentTypeFor("mp3") != "audio/mpeg" {
		t.Error("mp3 content type")
	}
	if ContentTypeFor("wav") != "audio/wav" {
		t.Error("wav content type")
	}
	if ContentTypeFor("nope") != "application/octet-stream" {
		t.Error("fallback content type")
	}
}

// TestToWAVRealFFmpeg only runs where ffmpeg is installed (CI + deploy host).
func TestToWAVRealFFmpeg(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("ffmpeg not installed: %v", err)
	}
	// 0.1s of 440Hz sine as WAV, feed through re-encode
	sine := buildTestWAV(t)
	out, err := ToWAV(t.Context(), bytesReader(sine), 16000, 1)
	if err != nil {
		t.Fatalf("ToWAV: %v", err)
	}
	if len(out) < 44 || string(out[0:4]) != "RIFF" {
		t.Fatalf("not a wav: %d bytes", len(out))
	}
	// 16kHz mono 16-bit: header sample rate must be 16000
	rate := int(out[24]) | int(out[25])<<8 | int(out[26])<<16 | int(out[27])<<24
	if rate != 16000 {
		t.Errorf("sample rate = %d, want 16000", rate)
	}
}
