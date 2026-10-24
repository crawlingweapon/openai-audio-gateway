// Package logging provides one structured line format written to stdout
// (journald) and, when configured, to a local syslog socket for rsyslog ->
// centralized logging. Never logs payloads, transcripts, or key material —
// only request metadata (route, model, upstream, status, timing, sizes).
package logging

import (
	"fmt"
	"log/syslog"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

func ParseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug":
		return Debug
	case "warn", "warning":
		return Warn
	case "error":
		return Error
	default:
		return Info
	}
}

func (l Level) String() string {
	switch l {
	case Debug:
		return "debug"
	case Warn:
		return "warn"
	case Error:
		return "error"
	default:
		return "info"
	}
}

// Field is a structured key/value pair. Values are printed as-is; callers must
// never pass secrets or audio content.
type Field struct {
	Key string
	Val any
}

func F(k string, v any) Field { return Field{Key: k, Val: v} }

type Logger struct {
	mu   sync.Mutex
	min  Level
	out  *os.File
	sysl *syslog.Writer
	tag  string
}

// New builds a logger. syslogAddr == "" disables syslog.
func New(level Level, syslogAddr, syslogTag, facility string) (*Logger, error) {
	l := &Logger{min: level, out: os.Stdout, tag: syslogTag}
	if syslogAddr != "" {
		pri := facilityPriority(facility)
		w, err := syslog.New(pri, syslogTag)
		if err != nil {
			// syslog.New dials /dev/log; fall back to the configured address
			// when it differs, otherwise degrade to stdout-only with a warning.
			if syslogAddr == "/dev/log" || syslogAddr == "" {
				fmt.Fprintf(os.Stderr, "syslog unavailable, continuing with stdout only: %v\n", err)
			} else {
				w2, err2 := syslog.Dial("unixgram", syslogAddr, pri, syslogTag)
				if err2 != nil {
					return nil, fmt.Errorf("syslog dial %s: %w", syslogAddr, err2)
				}
				w = w2
			}
		}
		if w != nil {
			l.sysl = w
		}
	}
	return l, nil
}

func facilityPriority(facility string) syslog.Priority {
	fac := map[string]syslog.Priority{
		"auth": syslog.LOG_AUTH, "daemon": syslog.LOG_DAEMON, "user": syslog.LOG_USER,
		"local0": syslog.LOG_LOCAL0, "local1": syslog.LOG_LOCAL1, "local2": syslog.LOG_LOCAL2,
		"local3": syslog.LOG_LOCAL3, "local4": syslog.LOG_LOCAL4, "local5": syslog.LOG_LOCAL5,
		"local6": syslog.LOG_LOCAL6, "local7": syslog.LOG_LOCAL7,
	}[facility]
	return fac | syslog.LOG_INFO
}

// SetLevel changes the threshold at runtime (config reload).
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	l.min = level
	l.mu.Unlock()
}

func (l *Logger) log(lv Level, msg string, fields ...Field) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lv < l.min {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ts=%s level=%s msg=%q", time.Now().UTC().Format(time.RFC3339Nano), lv.String(), msg)
	for _, f := range fields {
		fmt.Fprintf(&b, " %s=%v", f.Key, f.Val)
	}
	line := b.String()
	fmt.Fprintln(l.out, line)
	if l.sysl != nil {
		switch {
		case lv >= Error:
			l.sysl.Err(line)
		case lv == Warn:
			l.sysl.Warning(line)
		case lv == Debug:
			l.sysl.Debug(line)
		default:
			l.sysl.Info(line)
		}
	}
}

func (l *Logger) Debug(msg string, fields ...Field) { l.log(Debug, msg, fields...) }
func (l *Logger) Info(msg string, fields ...Field)  { l.log(Info, msg, fields...) }
func (l *Logger) Warn(msg string, fields ...Field)  { l.log(Warn, msg, fields...) }
func (l *Logger) Error(msg string, fields ...Field) { l.log(Error, msg, fields...) }
