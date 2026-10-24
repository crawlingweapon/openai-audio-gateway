// Package metrics exposes a dependency-free Prometheus text-format endpoint:
// request counters, latency histograms, transcode counters, upstream
// failovers, auth failures, and rate-limit drops — all labeled minimally
// (route, code, upstream, key label) so cardinality stays bounded.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Buckets for request/transcode latency in seconds.
var buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

type histogram struct {
	counts []int
	sum    float64
	count  uint64
}

func newHistogram() *histogram {
	return &histogram{counts: make([]int, len(buckets))}
}

func (h *histogram) observe(v float64) {
	h.sum += v
	h.count++
	for i, b := range buckets {
		if v <= b {
			h.counts[i]++
		}
	}
}

type labeled struct {
	key   string
	hist  *histogram
	total uint64
}

type Metrics struct {
	mu sync.Mutex

	requests   map[string]*labeled // route|code
	latency    map[string]*histogram
	transcodes map[string]*uint64 // direction|kind
	failovers  map[string]*uint64 // route
	authFails  uint64
	rateDrops  uint64
	inFlight   map[string]int
	start      time.Time
}

func New() *Metrics {
	return &Metrics{
		requests:   map[string]*labeled{},
		latency:    map[string]*histogram{},
		transcodes: map[string]*uint64{},
		failovers:  map[string]*uint64{},
		inFlight:   map[string]int{},
		start:      time.Now(),
	}
}

func (m *Metrics) ObserveRequest(route string, code int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := fmt.Sprintf("%s|%d", route, code)
	if e, ok := m.requests[k]; ok {
		e.total++
	} else {
		m.requests[k] = &labeled{key: k, hist: newHistogram(), total: 1}
	}
	if h, ok := m.latency[route]; ok {
		h.observe(d.Seconds())
	} else {
		h = newHistogram()
		h.observe(d.Seconds())
		m.latency[route] = h
	}
}

func (m *Metrics) IncTranscode(direction, kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := direction + "|" + kind
	if v, ok := m.transcodes[k]; ok {
		*v++
	} else {
		var n uint64 = 1
		m.transcodes[k] = &n
	}
}

func (m *Metrics) IncFailover(route string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.failovers[route]; ok {
		*v++
	} else {
		var n uint64 = 1
		m.failovers[route] = &n
	}
}

func (m *Metrics) IncAuthFailure() { m.mu.Lock(); m.authFails++; m.mu.Unlock() }
func (m *Metrics) IncRateDrop()    { m.mu.Lock(); m.rateDrops++; m.mu.Unlock() }

func (m *Metrics) AddInFlight(route string, delta int) {
	m.mu.Lock()
	m.inFlight[route] += delta
	if m.inFlight[route] <= 0 {
		delete(m.inFlight, route)
	}
	m.mu.Unlock()
}

// Render writes the Prometheus text exposition format.
func (m *Metrics) Render() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder

	b.WriteString("# HELP audio_gateway_requests_total Completed requests by route and status.\n")
	b.WriteString("# TYPE audio_gateway_requests_total counter\n")
	for _, e := range sortedValues(m.requests, func(e *labeled) string { return e.key }) {
		parts := strings.SplitN(e.key, "|", 2)
		fmt.Fprintf(&b, "audio_gateway_requests_total{route=%q,code=%q} %d\n", parts[0], parts[1], e.total)
	}

	b.WriteString("# HELP audio_gateway_request_duration_seconds Request latency by route.\n")
	b.WriteString("# TYPE audio_gateway_request_duration_seconds histogram\n")
	for _, k := range sortedKeys(m.latency) {
		h := m.latency[k]
		for i, bound := range buckets {
			fmt.Fprintf(&b, "audio_gateway_request_duration_seconds_bucket{route=%q,le=\"%g\"} %d\n", k, bound, h.counts[i])
		}
		fmt.Fprintf(&b, "audio_gateway_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", k, h.count)
		fmt.Fprintf(&b, "audio_gateway_request_duration_seconds_sum{route=%q} %g\n", k, h.sum)
		fmt.Fprintf(&b, "audio_gateway_request_duration_seconds_count{route=%q} %d\n", k, h.count)
	}

	b.WriteString("# HELP audio_gateway_transcodes_total Audio transcodes by direction and kind.\n")
	b.WriteString("# TYPE audio_gateway_transcodes_total counter\n")
	for _, k := range sortedKeys(m.transcodes) {
		parts := strings.SplitN(k, "|", 2)
		fmt.Fprintf(&b, "audio_gateway_transcodes_total{direction=%q,kind=%q} %d\n", parts[0], parts[1], *m.transcodes[k])
	}

	b.WriteString("# HELP audio_gateway_upstream_failovers_total Failovers to a secondary upstream.\n")
	b.WriteString("# TYPE audio_gateway_upstream_failovers_total counter\n")
	for _, k := range sortedKeys(m.failovers) {
		fmt.Fprintf(&b, "audio_gateway_upstream_failovers_total{route=%q} %d\n", k, *m.failovers[k])
	}

	b.WriteString("# HELP audio_gateway_auth_failures_total Rejected requests (bad/missing key).\n")
	b.WriteString("# TYPE audio_gateway_auth_failures_total counter\n")
	fmt.Fprintf(&b, "audio_gateway_auth_failures_total %d\n", m.authFails)

	b.WriteString("# HELP audio_gateway_rate_limit_drops_total Requests dropped by the rate limiter.\n")
	b.WriteString("# TYPE audio_gateway_rate_limit_drops_total counter\n")
	fmt.Fprintf(&b, "audio_gateway_rate_limit_drops_total %d\n", m.rateDrops)

	b.WriteString("# HELP audio_gateway_inflight_requests Currently in-flight requests by route.\n")
	b.WriteString("# TYPE audio_gateway_inflight_requests gauge\n")
	for _, k := range sortedKeys(m.inFlight) {
		fmt.Fprintf(&b, "audio_gateway_inflight_requests{route=%q} %d\n", k, m.inFlight[k])
	}

	b.WriteString("# HELP audio_gateway_up_seconds Uptime in seconds.\n")
	b.WriteString("# TYPE audio_gateway_up_seconds counter\n")
	fmt.Fprintf(&b, "audio_gateway_up_seconds %g\n", time.Since(m.start).Seconds())
	return b.String()
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedValues[T any](m map[string]T, key func(T) string) []T {
	vals := make([]T, 0, len(m))
	for _, v := range m {
		vals = append(vals, v)
	}
	sort.Slice(vals, func(i, j int) bool { return key(vals[i]) < key(vals[j]) })
	return vals
}
