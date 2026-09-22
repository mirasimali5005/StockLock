// Package metrics keeps duration samples in memory and summarizes them on demand.
// It exists for benchmarking: the API records how long each phase of a mutation
// takes and exposes the percentiles over HTTP.
package metrics

import (
	"slices"
	"sort"
	"sync"
	"time"
)

// maxSamples bounds memory. Beyond it, new samples overwrite the oldest.
const maxSamples = 1 << 20

// Summary describes one timer's samples. Durations are in milliseconds.
type Summary struct {
	Count int     `json:"count"`
	Mean  float64 `json:"mean_ms"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Max   float64 `json:"max_ms"`
}

// Registry holds named timers. The zero value is ready to use.
type Registry struct {
	mu     sync.Mutex
	timers map[string]*ring
}

type ring struct {
	samples []time.Duration
	next    int  // index the next sample goes to
	full    bool // whether the ring has wrapped
}

// Record adds one sample to the named timer.
func (r *Registry) Record(name string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timers == nil {
		r.timers = map[string]*ring{}
	}
	t := r.timers[name]
	if t == nil {
		t = &ring{samples: make([]time.Duration, 0, 1024)}
		r.timers[name] = t
	}
	if len(t.samples) < maxSamples {
		t.samples = append(t.samples, d)
		return
	}
	t.samples[t.next] = d
	t.next = (t.next + 1) % maxSamples
	t.full = true
}

// Reset discards every sample.
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timers = nil
}

// Summaries returns a summary per timer, computed from a copy of the samples.
func (r *Registry) Summaries() map[string]Summary {
	r.mu.Lock()
	copies := make(map[string][]time.Duration, len(r.timers))
	for name, t := range r.timers {
		copies[name] = slices.Clone(t.samples)
	}
	r.mu.Unlock()

	out := make(map[string]Summary, len(copies))
	for name, s := range copies {
		out[name] = summarize(s)
	}
	return out
}

func summarize(s []time.Duration) Summary {
	if len(s) == 0 {
		return Summary{}
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var total time.Duration
	for _, d := range s {
		total += d
	}
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	pct := func(p float64) float64 {
		i := int(p/100*float64(len(s))+0.5) - 1
		return ms(s[max(0, min(i, len(s)-1))])
	}
	return Summary{
		Count: len(s),
		Mean:  ms(total) / float64(len(s)),
		P50:   pct(50),
		P95:   pct(95),
		P99:   pct(99),
		Max:   ms(s[len(s)-1]),
	}
}
