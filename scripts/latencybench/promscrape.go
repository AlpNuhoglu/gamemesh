package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Server-side metric names the relay path exports (see pkg/metrics).
const (
	relayHistogram = "gamemesh_ws_relay_duration_seconds"
	relayDropped   = "gamemesh_ws_relay_dropped_total"
	relayRejected  = "gamemesh_ws_relay_rejected_total"
	stageLabel     = "stage"
)

// stageOrder is the order stages are printed in; unknown stages follow.
var stageOrder = []string{"process", "queue", "write", "total"}

type families map[string]*dto.MetricFamily

// scrapeMetrics fetches and parses a Prometheus text exposition.
func scrapeMetrics(ctx context.Context, url string) (families, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Ask for the classic text format: counters keep their _total suffix and
	// histograms come as _bucket/_sum/_count lines.
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	parser := newTextParser()
	fams, err := parser.TextToMetricFamilies(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", url, err)
	}
	return fams, nil
}

func newTextParser() expfmt.TextParser { return expfmt.NewTextParser(model.UTF8Validation) }

// lookup finds a family by name, tolerating the OpenMetrics convention of
// dropping the _total suffix from counter family names.
func (f families) lookup(name string) *dto.MetricFamily {
	if mf, ok := f[name]; ok {
		return mf
	}
	if trimmed, ok := strings.CutSuffix(name, "_total"); ok {
		return f[trimmed]
	}
	return nil
}

// bucket is one cumulative histogram bucket.
type bucket struct {
	Upper float64 // upper bound (le); +Inf for the last
	Cum   float64 // cumulative count of observations <= Upper
}

// histSnap is one histogram series (or a sum of series) at one instant, or
// the difference between two instants.
type histSnap struct {
	Count   float64
	Sum     float64
	Buckets []bucket // ascending by Upper, always ending in +Inf
}

// snapFromDTO converts a parsed histogram, making sure the +Inf bucket is
// present (it equals the sample count by definition).
func snapFromDTO(h *dto.Histogram) histSnap {
	s := histSnap{Count: float64(h.GetSampleCount()), Sum: h.GetSampleSum()}
	if h.GetSampleCountFloat() != 0 {
		s.Count = h.GetSampleCountFloat()
	}
	for _, b := range h.GetBucket() {
		c := float64(b.GetCumulativeCount())
		if b.GetCumulativeCountFloat() != 0 {
			c = b.GetCumulativeCountFloat()
		}
		s.Buckets = append(s.Buckets, bucket{Upper: b.GetUpperBound(), Cum: c})
	}
	sort.Slice(s.Buckets, func(i, j int) bool { return s.Buckets[i].Upper < s.Buckets[j].Upper })
	if n := len(s.Buckets); n == 0 || !math.IsInf(s.Buckets[n-1].Upper, 1) {
		s.Buckets = append(s.Buckets, bucket{Upper: math.Inf(1), Cum: s.Count})
	}
	return s
}

// add merges another series with the same bucket layout into s (used to sum
// series that differ only in labels other than stage, e.g. per-pod).
func (s histSnap) add(o histSnap) (histSnap, error) {
	if len(s.Buckets) == 0 {
		return o, nil
	}
	if len(s.Buckets) != len(o.Buckets) {
		return s, errors.New("bucket layouts differ")
	}
	out := histSnap{Count: s.Count + o.Count, Sum: s.Sum + o.Sum, Buckets: make([]bucket, len(s.Buckets))}
	for i := range s.Buckets {
		if s.Buckets[i].Upper != o.Buckets[i].Upper {
			return s, errors.New("bucket bounds differ")
		}
		out.Buckets[i] = bucket{Upper: s.Buckets[i].Upper, Cum: s.Buckets[i].Cum + o.Buckets[i].Cum}
	}
	return out, nil
}

// errCounterReset reports that a series went backwards between the scrapes,
// i.e. the process restarted inside the measured window.
var errCounterReset = errors.New("counter reset between scrapes (server restarted?)")

// histDelta returns after - before. A zero-value before (series did not exist
// yet) is treated as all zeros.
func histDelta(before, after histSnap) (histSnap, error) {
	if len(before.Buckets) == 0 {
		return after, nil
	}
	if after.Count < before.Count {
		return histSnap{}, errCounterReset
	}
	if len(before.Buckets) != len(after.Buckets) {
		return histSnap{}, errors.New("bucket layout changed between scrapes")
	}
	d := histSnap{Count: after.Count - before.Count, Sum: after.Sum - before.Sum, Buckets: make([]bucket, len(after.Buckets))}
	for i := range after.Buckets {
		if before.Buckets[i].Upper != after.Buckets[i].Upper {
			return histSnap{}, errors.New("bucket bounds changed between scrapes")
		}
		c := after.Buckets[i].Cum - before.Buckets[i].Cum
		if c < 0 {
			return histSnap{}, errCounterReset
		}
		d.Buckets[i] = bucket{Upper: after.Buckets[i].Upper, Cum: c}
	}
	return d, nil
}

// bucketQuantile estimates the q-quantile from cumulative buckets the way
// PromQL's histogram_quantile does: find the bucket holding rank q*count and
// interpolate linearly inside it, assuming observations are spread evenly
// between its bounds. If the rank falls in the +Inf bucket the upper bound of
// the highest finite bucket is returned and overflow is true (the true value
// is "at least this"). The lowest bucket's lower bound is 0. Returns NaN for
// an empty histogram.
func bucketQuantile(q float64, buckets []bucket) (v float64, overflow bool) {
	if len(buckets) == 0 {
		return math.NaN(), false
	}
	total := buckets[len(buckets)-1].Cum
	if total <= 0 || math.IsNaN(q) {
		return math.NaN(), false
	}
	if q <= 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	rank := q * total
	i := sort.Search(len(buckets), func(i int) bool { return buckets[i].Cum >= rank })
	if i >= len(buckets) {
		i = len(buckets) - 1
	}
	if math.IsInf(buckets[i].Upper, 1) {
		if i == 0 {
			return math.NaN(), true
		}
		return buckets[i-1].Upper, true
	}
	lower, below := 0.0, 0.0
	if i > 0 {
		lower, below = buckets[i-1].Upper, buckets[i-1].Cum
	} else if buckets[0].Upper <= 0 {
		return buckets[0].Upper, false
	}
	upper := buckets[i].Upper
	inBucket := buckets[i].Cum - below
	if inBucket <= 0 {
		return upper, false
	}
	return lower + (upper-lower)*(rank-below)/inBucket, false
}

// stageHists sums the relay histogram's series per stage label.
func stageHists(f families) (map[string]histSnap, error) {
	mf := f.lookup(relayHistogram)
	if mf == nil {
		return nil, nil
	}
	if mf.GetType() != dto.MetricType_HISTOGRAM {
		return nil, fmt.Errorf("%s is a %s, not a histogram", relayHistogram, mf.GetType())
	}
	out := map[string]histSnap{}
	for _, m := range mf.GetMetric() {
		stage := labelValue(m, stageLabel)
		if stage == "" {
			stage = "(none)"
		}
		merged, err := out[stage].add(snapFromDTO(m.GetHistogram()))
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", stage, err)
		}
		out[stage] = merged
	}
	return out, nil
}

// counterValues returns a counter family's values keyed by a rendered label
// set ("" when the series has no labels). ok is false if the family is absent.
func counterValues(f families, name string) (vals map[string]float64, ok bool) {
	mf := f.lookup(name)
	if mf == nil {
		return nil, false
	}
	vals = map[string]float64{}
	for _, m := range mf.GetMetric() {
		var v float64
		switch {
		case m.GetCounter() != nil:
			v = m.GetCounter().GetValue()
		case m.GetUntyped() != nil:
			v = m.GetUntyped().GetValue()
		case m.GetGauge() != nil:
			v = m.GetGauge().GetValue()
		default:
			continue
		}
		vals[renderLabels(m)] += v
	}
	return vals, true
}

func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

func renderLabels(m *dto.Metric) string {
	if len(m.GetLabel()) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		parts = append(parts, fmt.Sprintf("%s=%q", lp.GetName(), lp.GetValue()))
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}"
}

// stageResult is one row of the server stage table.
type stageResult struct {
	Stage    string  `json:"stage"`
	Count    float64 `json:"count"`
	MeanMs   float64 `json:"mean_ms"`
	P50Ms    float64 `json:"p50_ms_estimate"`
	P95Ms    float64 `json:"p95_ms_estimate"`
	P99Ms    float64 `json:"p99_ms_estimate"`
	Overflow bool    `json:"p99_in_top_bucket,omitempty"`
	Error    string  `json:"error,omitempty"`
}

type counterDelta struct {
	Metric string  `json:"metric"`
	Labels string  `json:"labels,omitempty"`
	Delta  float64 `json:"delta"`
}

// serverReport is the server-side breakdown over the measured window.
type serverReport struct {
	MetricsURL string         `json:"metrics_url,omitempty"`
	Available  bool           `json:"available"`
	Note       string         `json:"note,omitempty"`
	Stages     []stageResult  `json:"stages,omitempty"`
	Counters   []counterDelta `json:"counters,omitempty"`
}

// compareScrapes computes the stage table and counter deltas. It never fails:
// whatever cannot be computed is explained in Note or in a row's Error.
func compareScrapes(url string, before, after families) serverReport {
	r := serverReport{MetricsURL: url}
	hb, errB := stageHists(before)
	ha, errA := stageHists(after)
	var notes []string
	switch {
	case errB != nil || errA != nil:
		notes = append(notes, fmt.Sprintf("cannot read %s: %v", relayHistogram, errors.Join(errB, errA)))
	case ha == nil:
		notes = append(notes, relayHistogram+" not exported by this server (relay instrumentation missing or old image)")
	default:
		r.Available = true
		stages := make([]string, 0, len(ha))
		for s := range ha {
			stages = append(stages, s)
		}
		sort.Slice(stages, func(i, j int) bool {
			return stageRank(stages[i]) < stageRank(stages[j]) || (stageRank(stages[i]) == stageRank(stages[j]) && stages[i] < stages[j])
		})
		for _, s := range stages {
			row := stageResult{Stage: s}
			d, err := histDelta(hb[s], ha[s])
			if err != nil {
				row.Error = err.Error()
				r.Stages = append(r.Stages, row)
				continue
			}
			row.Count = d.Count
			if d.Count > 0 {
				row.MeanMs = round3(d.Sum / d.Count * 1e3)
				p50, _ := bucketQuantile(0.50, d.Buckets)
				p95, _ := bucketQuantile(0.95, d.Buckets)
				p99, of := bucketQuantile(0.99, d.Buckets)
				row.P50Ms, row.P95Ms, row.P99Ms, row.Overflow = round3(p50*1e3), round3(p95*1e3), round3(p99*1e3), of
			}
			r.Stages = append(r.Stages, row)
		}
	}
	for _, name := range []string{relayDropped, relayRejected} {
		vb, _ := counterValues(before, name)
		va, ok := counterValues(after, name)
		if !ok {
			notes = append(notes, name+" not exported")
			continue
		}
		keys := make([]string, 0, len(va))
		for k := range va {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			d := va[k] - vb[k]
			if d < 0 {
				notes = append(notes, fmt.Sprintf("%s%s: %v", name, k, errCounterReset))
				continue
			}
			r.Counters = append(r.Counters, counterDelta{Metric: name, Labels: k, Delta: d})
		}
	}
	r.Note = strings.Join(notes, "; ")
	return r
}

func stageRank(s string) int {
	for i, v := range stageOrder {
		if v == s {
			return i
		}
	}
	return len(stageOrder)
}

func round3(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e3) / 1e3
}
