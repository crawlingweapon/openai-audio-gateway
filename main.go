// Command audio-gateway is an OpenAI-compatible audio gateway: it normalizes
// client audio (ffmpeg built in), applies named voice presets from config,
// and forwards STT/TTS to configurable upstreams with ordered failover.
//
// Usage:
//
//	audio-gateway serve    -config config.yaml   (default when no subcommand)
//	audio-gateway keygen   [-n 32]               print a new CSRNG key
//	audio-gateway configtest -config config.yaml validate and exit
//	audio-gateway version
//
// Signals: SIGHUP reloads config (except listen); SIGINT/SIGTERM drain.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crawlingweapon/openai-audio-gateway/config"
	"github.com/crawlingweapon/openai-audio-gateway/logging"
	"github.com/crawlingweapon/openai-audio-gateway/metrics"
	"github.com/crawlingweapon/openai-audio-gateway/server"
	"github.com/crawlingweapon/openai-audio-gateway/transcode"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "keygen":
		keygenCmd(args)
	case "version":
		fmt.Println(server.Version)
	case "configtest":
		os.Exit(configtestCmd(args, false))
	case "serve":
		serveCmd(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (serve|keygen|configtest|version)\n", cmd)
		os.Exit(2)
	}
}

func keygenCmd(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	n := fs.Int("n", 32, "bytes of entropy")
	fs.Parse(args)
	buf := make([]byte, *n)
	if _, err := rand.Read(buf); err != nil {
		fmt.Fprintln(os.Stderr, "crypto/rand failed:", err)
		os.Exit(1)
	}
	fmt.Println(base64.RawURLEncoding.EncodeToString(buf))
}

func configtestCmd(args []string, verbose bool) int {
	fs := flag.NewFlagSet("configtest", flag.ExitOnError)
	path := fs.String("config", "config.yaml", "path to config file")
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		return 1
	}
	fmt.Printf("OK: %s\n", cfg)
	if verbose {
		fmt.Printf("  generated keys file: %q\n", cfg.KeysFilePath())
	}
	return 0
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "config.yaml", "path to config file")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		fatal("load config", err)
	}

	if err := transcode.Available(); err != nil {
		// Not fatal: STT passthrough (already-WAV) and TTS-only still work,
		// but shout about it — every non-WAV upload will fail.
		fmt.Fprintf(os.Stderr, "WARNING: ffmpeg not found: %v (non-WAV STT input will fail)\n", err)
	}

	lvl := logging.ParseLevel(cfg.Log.Level)
	sysAddr := ""
	if cfg.Log.Syslog.Enabled {
		sysAddr = cfg.Log.Syslog.Address
	}
	log, err := logging.New(lvl, sysAddr, cfg.Log.Syslog.Tag, cfg.Log.Syslog.Facility)
	if err != nil {
		fatal("init logger", err)
	}

	holder := &config.Holder{}
	holder.Set(cfg)
	m := metrics.New()
	gw := server.New(holder, log, m)

	srv := &http.Server{
		Addr:              cfg.Endpoint(),
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: audio responses stream at arbitrary length.
	}

	// ---- reload machinery: SIGHUP + config file watch (debounced)
	reloadCh := make(chan os.Signal, 1)
	signal.Notify(reloadCh, syscall.SIGHUP)
	watchDone := make(chan struct{})
	go func() {
		for range reloadCh {
			doReload(holder, log, cfg)
		}
	}()

	// file watcher as convenience alongside SIGHUP
	go watchFile(*path, func() { doReload(holder, log, cfg) }, watchDone)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening",
			logging.F("addr", cfg.Endpoint()),
			logging.F("version", server.Version),
			logging.F("keys", len(cfg.Auth.Keys)),
			logging.F("presets", len(cfg.TTS.Presets)),
			logging.F("syslog", sysAddr != ""),
		)
		errCh <- srv.ListenAndServe()
	}()

	// ---- graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fatal("serve", err)
		}
	case sig := <-stop:
		log.Info("shutting down", logging.F("signal", sig.String()))
		close(watchDone)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Error("shutdown error", logging.F("error", err.Error()))
		}
	}
	log.Info("stopped")
}

func doReload(holder *config.Holder, log *logging.Logger, original *config.Config) {
	current := holder.Get()
	next, err := config.Reload(current)
	if err != nil {
		log.Error("config reload rejected, keeping running config",
			logging.F("error", err.Error()))
		return
	}
	next = config.PreservePath(original, next)
	holder.Set(next)
	log.SetLevel(logging.ParseLevel(next.Log.Level))
	log.Info("config reloaded", logging.F("config", next.String()))
}

// watchFile fires fn debounce-window after the config file stops changing.
func watchFile(path string, fn func(), done <-chan struct{}) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	lastMod := info.ModTime()
	lastSize := info.Size()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.ModTime().After(lastMod) || info.Size() != lastSize {
				lastMod, lastSize = info.ModTime(), info.Size()
				time.Sleep(300 * time.Millisecond) // debounce partial writes
				fn()
			}
		}
	}
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	os.Exit(1)
}
