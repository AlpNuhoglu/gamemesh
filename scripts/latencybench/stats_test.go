package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	// 1..100 ms: nearest-rank p50 is the 50th value, p95 the 95th, p99 the 99th.
	var s []time.Duration
	for i := 100; i >= 1; i-- { // unsorted on purpose
		s = append(s, time.Duration(i)*time.Millisecond)
	}
	d := summarize(s)
	want := map[string][2]time.Duration{
		"min":  {d.Min, 1 * time.Millisecond},
		"p50":  {d.P50, 50 * time.Millisecond},
		"p95":  {d.P95, 95 * time.Millisecond},
		"p99":  {d.P99, 99 * time.Millisecond},
		"max":  {d.Max, 100 * time.Millisecond},
		"mean": {d.Mean, 50500 * time.Microsecond},
	}
	for name, v := range want {
		if v[0] != v[1] {
			t.Errorf("%s = %v, want %v", name, v[0], v[1])
		}
	}
	if d.N != 100 {
		t.Errorf("N = %d", d.N)
	}
	if s[0] != 100*time.Millisecond {
		t.Error("summarize must not reorder its input")
	}
}

func TestPercentileSmallSets(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty = %v", got)
	}
	one := []time.Duration{7}
	for _, q := range []float64{0.01, 0.5, 0.99, 1} {
		if got := percentile(one, q); got != 7 {
			t.Errorf("q=%v of one sample = %v", q, got)
		}
	}
	// 500 samples (the default counted count): p99 is the 495th value.
	s := make([]time.Duration, 500)
	for i := range s {
		s[i] = time.Duration(i + 1)
	}
	if got := percentile(s, 0.99); got != 495 {
		t.Errorf("p99 of 1..500 = %v, want 495", got)
	}
	if got := percentile(s, 0.95); got != 475 {
		t.Errorf("p95 of 1..500 = %v, want 475", got)
	}
}

func bk(pairs ...float64) []bucket {
	var out []bucket
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, bucket{Upper: pairs[i], Cum: pairs[i+1]})
	}
	return out
}

func TestHistDelta(t *testing.T) {
	inf := math.Inf(1)
	before := histSnap{Count: 10, Sum: 0.05, Buckets: bk(0.001, 4, 0.005, 9, inf, 10)}
	after := histSnap{Count: 110, Sum: 0.35, Buckets: bk(0.001, 54, 0.005, 99, inf, 110)}
	d, err := histDelta(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if d.Count != 100 || math.Abs(d.Sum-0.30) > 1e-12 {
		t.Fatalf("count/sum = %v/%v", d.Count, d.Sum)
	}
	wantCum := []float64{50, 90, 100}
	for i, b := range d.Buckets {
		if b.Cum != wantCum[i] {
			t.Errorf("bucket %d cum = %v, want %v", i, b.Cum, wantCum[i])
		}
	}

	// A series that appeared during the window counts from zero.
	if d2, err := histDelta(histSnap{}, after); err != nil || d2.Count != 110 {
		t.Errorf("missing before: %v %v", d2.Count, err)
	}
	// Server restart inside the window.
	if _, err := histDelta(after, before); err == nil {
		t.Error("expected a counter-reset error")
	}
	// Bucket layout change.
	if _, err := histDelta(before, histSnap{Count: 20, Buckets: bk(0.002, 4, inf, 20)}); err == nil {
		t.Error("expected a layout error")
	}
}

func TestBucketQuantile(t *testing.T) {
	inf := math.Inf(1)
	// 100 observations: 50 in (0,1ms], 40 in (1ms,5ms], 10 in (5ms,+Inf).
	b := bk(0.001, 50, 0.005, 90, inf, 100)
	cases := []struct {
		q        float64
		want     float64
		overflow bool
	}{
		{0.25, 0.0005, false}, // rank 25 of 50 in [0,1ms] -> halfway
		{0.50, 0.001, false},  // rank 50 = top of first bucket
		{0.70, 0.003, false},  // rank 70: 20 of 40 into (1ms,5ms] -> 1 + 4*0.5
		{0.95, 0.005, true},   // in +Inf bucket -> highest finite bound
	}
	for _, c := range cases {
		got, of := bucketQuantile(c.q, b)
		if math.Abs(got-c.want) > 1e-12 || of != c.overflow {
			t.Errorf("q=%v: got %v (overflow %v), want %v (%v)", c.q, got, of, c.want, c.overflow)
		}
	}
	if v, _ := bucketQuantile(0.5, bk(0.001, 0, inf, 0)); !math.IsNaN(v) {
		t.Errorf("empty histogram = %v, want NaN", v)
	}
	// Empty buckets below the target must be skipped, not interpolated into.
	got, _ := bucketQuantile(0.5, bk(0.001, 0, 0.002, 0, 0.004, 10, inf, 10))
	if math.Abs(got-0.003) > 1e-12 {
		t.Errorf("sparse buckets: got %v, want 0.003", got)
	}
}

func TestCompareScrapesFromText(t *testing.T) {
	before := `# TYPE gamemesh_ws_relay_duration_seconds histogram
gamemesh_ws_relay_duration_seconds_bucket{stage="total",le="0.001"} 1
gamemesh_ws_relay_duration_seconds_bucket{stage="total",le="0.01"} 2
gamemesh_ws_relay_duration_seconds_bucket{stage="total",le="+Inf"} 2
gamemesh_ws_relay_duration_seconds_sum{stage="total"} 0.004
gamemesh_ws_relay_duration_seconds_count{stage="total"} 2
# TYPE gamemesh_ws_relay_rejected_total counter
gamemesh_ws_relay_rejected_total{reason="rate_limit"} 3
`
	after := `# TYPE gamemesh_ws_relay_duration_seconds histogram
gamemesh_ws_relay_duration_seconds_bucket{stage="total",le="0.001"} 101
gamemesh_ws_relay_duration_seconds_bucket{stage="total",le="0.01"} 102
gamemesh_ws_relay_duration_seconds_bucket{stage="total",le="+Inf"} 102
gamemesh_ws_relay_duration_seconds_sum{stage="total"} 0.054
gamemesh_ws_relay_duration_seconds_count{stage="total"} 102
gamemesh_ws_relay_duration_seconds_bucket{stage="queue",le="0.001"} 100
gamemesh_ws_relay_duration_seconds_bucket{stage="queue",le="0.01"} 100
gamemesh_ws_relay_duration_seconds_bucket{stage="queue",le="+Inf"} 100
gamemesh_ws_relay_duration_seconds_sum{stage="queue"} 0.01
gamemesh_ws_relay_duration_seconds_count{stage="queue"} 100
# TYPE gamemesh_ws_relay_rejected_total counter
gamemesh_ws_relay_rejected_total{reason="rate_limit"} 5
gamemesh_ws_relay_rejected_total{reason="too_large"} 1
`
	fb, fa := parseText(t, before), parseText(t, after)
	r := compareScrapes("x", fb, fa)
	if !r.Available || len(r.Stages) != 2 {
		t.Fatalf("report = %+v", r)
	}
	if r.Stages[0].Stage != "queue" || r.Stages[1].Stage != "total" {
		t.Errorf("stage order = %s, %s", r.Stages[0].Stage, r.Stages[1].Stage)
	}
	tot := r.Stages[1]
	if tot.Count != 100 || tot.MeanMs != 0.5 || tot.P50Ms != 0.5 {
		t.Errorf("total row = %+v", tot)
	}
	if len(r.Counters) != 2 || r.Counters[0].Delta != 2 || r.Counters[1].Delta != 1 {
		t.Errorf("counters = %+v", r.Counters)
	}
	if !strings.Contains(r.Note, relayDropped) {
		t.Errorf("note should mention the missing dropped counter: %q", r.Note)
	}

	missing := compareScrapes("x", families{}, families{})
	if missing.Available || !strings.Contains(missing.Note, "not exported") {
		t.Errorf("missing metric report = %+v", missing)
	}
}

func parseText(t *testing.T, s string) families {
	t.Helper()
	p := newTextParser()
	f, err := p.TextToMetricFamilies(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRunPatternIsStrict(t *testing.T) {
	if got := runPattern("ab12cd"); got != `bench\_ab12cd\_%@latencybench.invalid` {
		t.Errorf("runPattern = %q", got)
	}
	if allBenchPattern != `%@latencybench.invalid` {
		t.Errorf("allBenchPattern = %q", allBenchPattern)
	}
}

func TestBenchPredicateHasBothConditions(t *testing.T) {
	if want := `email LIKE $1 ESCAPE '\' AND username LIKE 'bench%'`; benchPredicate != want {
		t.Errorf("benchPredicate = %q, want %q", benchPredicate, want)
	}
	got := describePredicate(allBenchPattern)
	if want := `email LIKE %@latencybench.invalid AND username LIKE 'bench%'`; got != want {
		t.Errorf("describePredicate = %q, want %q", got, want)
	}
	if _, err := deleteBenchAccounts(context.Background(), cleanupOptions{Limit: 0}); err == nil {
		t.Error("limit 0 must be rejected before any connection is made")
	}
}

func TestJWTID(t *testing.T) {
	// header.payload.signature with payload {"pid":"p","jti":"abc"}
	tok := "eyJhbGciOiJIUzI1NiJ9.eyJwaWQiOiJwIiwianRpIjoiYWJjIn0.sig"
	if got := jwtID(tok); got != "abc" {
		t.Errorf("jwtID = %q", got)
	}
	if got := jwtID("garbage"); got != "" {
		t.Errorf("jwtID(garbage) = %q", got)
	}
}
