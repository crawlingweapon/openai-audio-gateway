// Package transcode wraps ffmpeg for the two directions the gateway needs:
// input normalization (anything → WAV for WAV-only STT upstreams) and
// output formatting (upstream WAV → whatever the client asked for).
//
// Everything runs through stdin/stdout pipes — no temp files, no disk writes.
package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// DetectFormat sniffs the container from magic bytes. Returns a lowercase
// extension-ish name: wav, mp3, webm, m4a/mp4, flac, ogg, mp4, or "".
func DetectFormat(header []byte) string {
	switch {
	case len(header) >= 12 && string(header[0:4]) == "RIFF" && string(header[8:12]) == "WAVE":
		return "wav"
	case len(header) >= 3 && string(header[0:3]) == "ID3":
		return "mp3"
	case len(header) >= 2 && header[0] == 0xFF && header[1]&0xE0 == 0xE0:
		return "mp3"
	case len(header) >= 4 && string(header[0:4]) == "fLaC":
		return "flac"
	case len(header) >= 4 && string(header[0:4]) == "OggS":
		return "ogg"
	case len(header) >= 8 && string(header[4:8]) == "ftyp":
		brand := string(header[8:12])
		if brand == "M4A " || brand == "M4B " || brand == "F4A " {
			return "m4a"
		}
		return "mp4"
	case len(header) >= 4 && string(header[0:4]) == "\x1A\x45\xDF\xA3":
		return "webm"
	}
	return ""
}

// Pool bounds concurrent ffmpeg processes so a burst queues instead of forking.
type Pool struct {
	sem chan struct{}
}

func NewPool(n int) *Pool {
	if n < 1 {
		n = 1
	}
	return &Pool{sem: make(chan struct{}, n)}
}

// Acquire blocks until a slot is free or ctx is done.
func (p *Pool) Acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) Release() { <-p.sem }

func (p *Pool) InFlight() int { return len(p.sem) }

// ffmpegBin resolves the binary once.
var (
	ffmpegOnce sync.Once
	ffmpegPath string
	ffmpegErr  error
)

func ffmpeg() (string, error) {
	ffmpegOnce.Do(func() {
		ffmpegPath, ffmpegErr = exec.LookPath("ffmpeg")
	})
	return ffmpegPath, ffmpegErr
}

// Available reports whether ffmpeg is on PATH (checked at startup).
func Available() error {
	_, err := ffmpeg()
	return err
}

// ToWAV normalizes any input audio to PCM WAV at the given rate/channels.
// src is streamed; the returned bytes are the full WAV file.
func ToWAV(ctx context.Context, src io.Reader, sampleRate, channels int) ([]byte, error) {
	bin, err := ffmpeg()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not available: %w", err)
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-vn",
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-ac", fmt.Sprintf("%d", channels),
		"-c:a", "pcm_s16le",
		"-f", "wav",
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = src
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := bytes.TrimSpace(errb.Bytes())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return nil, fmt.Errorf("ffmpeg transcode failed: %v: %s", err, msg)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg produced no output")
	}
	return out.Bytes(), nil
}

// ConvertWAV converts a WAV byte stream to the named container/codec.
// Supported targets: mp3, opus (ogg container), flac, aac, wav, pcm (s16le raw).
func ConvertWAV(ctx context.Context, src io.Reader, target string) (io.Reader, error) {
	bin, err := ffmpeg()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not available: %w", err)
	}
	var args []string
	switch target {
	case "mp3":
		args = []string{"-c:a", "libmp3lame", "-q:a", "4", "-f", "mp3"}
	case "opus":
		args = []string{"-c:a", "libopus", "-b:a", "64k", "-f", "ogg"}
	case "flac":
		args = []string{"-c:a", "flac", "-f", "flac"}
	case "aac":
		args = []string{"-c:a", "aac", "-b:a", "96k", "-f", "adts"}
	case "wav":
		args = []string{"-c:a", "pcm_s16le", "-f", "wav"}
	case "pcm":
		args = []string{"-c:a", "pcm_s16le", "-f", "s16le"}
	default:
		return nil, fmt.Errorf("unsupported target format %q", target)
	}
	full := append([]string{"-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-vn"}, args...)
	full = append(full, "pipe:1")

	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Stdin = src
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Close the write pipe when ffmpeg exits; surface stderr on failure.
	go func() {
		err := cmd.Wait()
		if err != nil {
			msg := bytes.TrimSpace(errb.Bytes())
			if len(msg) > 400 {
				msg = msg[:400]
			}
			pw.CloseWithError(fmt.Errorf("ffmpeg: %v: %s", err, msg))
			return
		}
		pw.Close()
	}()
	return pr, nil
}

// ContentTypeFor maps a response_format value to its MIME type.
func ContentTypeFor(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	case "wav":
		return "audio/wav"
	case "pcm":
		return "application/octet-stream"
	}
	return "application/octet-stream"
}
