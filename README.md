# openai-audio-gateway

A small OpenAI-compatible gateway for self-hosted speech-to-text and
text-to-speech backends. It owns the three things client apps get wrong —
audio format, model naming, and voice text — so every client just sets a URL.

```
client (Open WebUI / Hermes / anything OpenAI-audio)
   │  POST /v1/audio/transcriptions   (mp3, webm, m4a, flac, wav…)
   │  POST /v1/audio/speech           (any model id, any voice name)
   ▼
┌─────────────────────────────────────────────────────────┐
│  audio-gateway                                          │
│   • ffmpeg transcode-in  (anything → 16 kHz mono WAV)   │
│   • ffmpeg transcode-out (WAV → mp3/opus/flac/aac)      │
│   • preset models: named voices + instructions in YAML  │
│   • model/voice aliases (whisper-1, tts-1, alloy…)      │
│   • sentence chunking for long input                    │
│   • ordered upstream failover                           │
│   • API keys, rate limits, metrics, syslog              │
└─────────────────────────────────────────────────────────┘
   ▼
WAV-only STT upstream          TTS upstream
(e.g. Qwen3-ASR)               (e.g. Qwen3-TTS)
```

**Why:** every client reimplements the same glue. Open WebUI converts all
audio to MP3 before upload; a WAV-only upstream rejects it. Hermes needs an
ffmpeg shell command as its STT provider. That's two clients paying a tax the
server should own. Put ffmpeg and the naming layer in one gateway and clients
degrade to "set the base URL".

## Quick start

```bash
go build -o audio-gateway .
cp config.example.yaml config.yaml
./audio-gateway configtest -config config.yaml
./audio-gateway serve -config config.yaml
```

Then point any OpenAI-compatible client at `http://host:8086/v1`.

Requires `ffmpeg` on PATH (checked at startup; a warning is printed if it's
missing — WAV-only STT input still works, nothing else does).

## What goes in config (nothing here needs a rebuild)

Everything except `listen` hot-reloads: `kill -HUP $(pidof audio-gateway)`
or just edit the file (watched with debounce). Invalid reloads are rejected
and the running config stays active.

| Want to… | Change |
|---|---|
| add/reword a voice | `tts.presets[].instructions` (multiline YAML) |
| rename a model id | `tts.presets[].id` |
| accept a client's stock name | `tts.model_aliases` / `tts.voice_aliases` / `stt.model_aliases` |
| add/remove an API key | `auth.keys` (labels show up in logs + metrics) |
| auto-generate a key on first boot | `auth.auto_generate: true` → CSRNG key written `0600` |
| change upstream URL/key/timeout | `upstreams[]` |
| fail over to a second upstream | append to `stt.upstreams` / `tts.upstreams` |
| change transcode rate/limits | `stt.sample_rate`, `max_concurrent`, `max_upload_bytes` |
| log to rsyslog instead of/in addition to journald | `log.syslog.enabled: true` |
| turn on metrics/rate limits/CORS | `metrics`, `rate_limit`, `cors` |

Generate a key by hand:

```bash
./audio-gateway keygen -n 32
```

### Presets vs pass-through

Clients pick a **model id**. What happens next:

1. **Matches a preset** (`tts.presets[].id`) → preset fills `model`, `voice`,
   `language`, and re-feeds `instructions` on every request. A preset with
   `allow_body_override: true` lets a client-supplied `voice`/`instructions`
   win when present.
2. **Matches an alias** → rewritten, everything else passes through.
3. **Matches nothing** → forwarded untouched, so built-in upstream ids
   (`qwen3-tts`, `qwen3-asr-1.7b`, …) work with zero configuration.

`GET /v1/models` merges presets, aliases, and every upstream's own list, so
client dropdowns are populated in one call.

### Why instructions live in config

Voice-design-style TTS models take an `instructions` string per request that
determines the persona. Hardcoding that in a client means a redeploy to reword
a voice; here it's a YAML block you edit while the gateway runs. Cloned/fixed
voices ignore `instructions` — the field is harmless there, and presets carry
both `voice` and `instructions` so switching upstream flavor never touches the
gateway.

## API

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/audio/transcriptions` | multipart, any ffmpeg-readable audio in |
| POST | `/v1/audio/speech` | JSON `{model,input,voice,response_format,speed}` |
| GET | `/v1/models` | presets ∪ aliases ∪ upstreams |
| GET | `/health` | gateway + upstream reachability + ffmpeg status |
| GET | `/metrics` | Prometheus text format (when enabled) |

Errors are OpenAI-shaped: `{"error":{"message":…,"type":…,"code":…}}`.

Auth: `Authorization: Bearer <key>` on `/v1/*` (empty `auth.keys` = disabled).
`?api_key=` is accepted for clients that can't set headers.

### Upstream quirks handled

- **WAV-only STT**: input normalized to 16 kHz mono PCM16 before forwarding.
- **`response_format`**: upstreams that only produce WAV get asked for WAV;
  if the client asked for `mp3`/`opus`/`flac`/`aac`, ffmpeg converts the
  response. `passthrough` (default) returns the upstream bytes untouched.
- **`speed`**: upstreams that only accept `1.0` get it stripped (logged);
  `speed_handling: atempo` is reserved for real rescaling.
- **`language`**: presets stamp full names (`english`) because some upstreams
  reject bare `en`.
- **4096-char input cap**: longer input is split on sentence boundaries,
  synthesized sequentially, and concatenated (`tts.max_input_chars`, 0 = off).

## Failover

Each route has an ordered upstream list. Connection errors and 5xx advance to
the next candidate (counted in `audio_gateway_upstream_failovers_total`);
4xx returns to the client as-is, because that's not a server problem.

```yaml
stt:
  upstreams: [gufo-asr, speaches-fallback]
```

## Deployment

systemd unit with hardening: [`deploy/audio-gateway.service`](deploy/audio-gateway.service).

```bash
sudo install -m755 audio-gateway /usr/local/bin/
sudo install -m644 config.example.yaml /etc/audio-gateway/config.yaml
sudo systemctl enable --now audio-gateway
```

Logs are single-line `key=value` on stdout (journald). With `log.syslog`
enabled the same lines go to `/dev/log` for rsyslog → your central logger.
Nothing sensitive is ever logged: no audio, no transcripts, no key material
— keys appear only as their config `label`.

## Build

```bash
go test ./...
go build -ldflags "-X github.com/crawlingweapon/openai-audio-gateway/server.Version=v1.2.3" .
```

CI runs vet + tests on every push and attaches binaries to tagged releases.

## License

MIT — see [LICENSE](LICENSE).
