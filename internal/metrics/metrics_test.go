package metrics

import (
	"testing"
	"time"
)

// 100 samples of 1ms..100ms: the percentiles must land where a person would put them.
func TestSummaryPercentiles(t *testing.T) {
	var r Registry
	for i := 1; i <= 100; i++ {
		r.Record("t", time.Duration(i)*time.Millisecond)
	}
	s := r.Summaries()["t"]
	want := Summary{Count: 100, Mean: 50.5, P50: 50, P95: 95, P99: 99, Max: 100}
	if s != want {
		t.Errorf("summary = %+v, want %+v", s, want)
	}
	r.Reset()
	if len(r.Summaries()) != 0 {
		t.Error("reset left timers behind")
	}
}
