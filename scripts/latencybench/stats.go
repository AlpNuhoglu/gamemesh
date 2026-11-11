package main

import (
	"math"
	"slices"
	"time"
)

// dist summarises a set of durations with exact, sort-based percentiles.
type dist struct {
	N    int
	Min  time.Duration
	P50  time.Duration
	P95  time.Duration
	P99  time.Duration
	Max  time.Duration
	Mean time.Duration
}

// percentile returns the nearest-rank percentile of an ascending slice: the
// smallest sample such that at least q of all samples are <= it (index
// ceil(q*n)-1). It never interpolates, so every reported value is a latency
// that was actually observed. q is in (0, 1]; an empty slice yields 0.
func percentile(sorted []time.Duration, q float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(q * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// summarize sorts a copy of samples and computes the distribution.
func summarize(samples []time.Duration) dist {
	if len(samples) == 0 {
		return dist{}
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	var sum float64
	for _, v := range s {
		sum += float64(v)
	}
	return dist{
		N:    len(s),
		Min:  s[0],
		P50:  percentile(s, 0.50),
		P95:  percentile(s, 0.95),
		P99:  percentile(s, 0.99),
		Max:  s[len(s)-1],
		Mean: time.Duration(sum / float64(len(s))),
	}
}

// ms converts a duration to milliseconds rounded to the microsecond, the unit
// every table in the report uses.
func ms(d time.Duration) float64 {
	return math.Round(float64(d)/1e3) / 1e3
}
