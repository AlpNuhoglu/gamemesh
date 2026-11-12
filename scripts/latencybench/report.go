package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"
)

// report is everything a run produced; -out writes it as JSON so the docs
// tables can be generated from it. Durations are milliseconds.
type report struct {
	Tool        string    `json:"tool"`
	RunID       string    `json:"run_id"`
	StartedAt   time.Time `json:"started_at"`
	DurationS   float64   `json:"duration_s"`
	GoVersion   string    `json:"go_version"`
	OS          string    `json:"os_arch"`
	Interrupted bool      `json:"interrupted,omitempty"`
	SetupError  string    `json:"setup_error,omitempty"`

	Targets struct {
		API     string `json:"api"`
		WS      string `json:"ws"`
		Metrics string `json:"metrics,omitempty"`
	} `json:"targets"`
	Params struct {
		Pairs            int     `json:"pairs"`
		Samples          int     `json:"samples"`
		Warmup           int     `json:"warmup"`
		IntervalMs       float64 `json:"interval_ms"`
		GraceMs          float64 `json:"grace_ms"`
		PayloadBytes     int     `json:"payload_bytes"`
		PingsPerConn     int     `json:"pings_per_connection"`
		RankWindow       int     `json:"rank_window"`
		PercentileMethod string  `json:"percentile_method"`
	} `json:"params"`

	PairsFormed int          `json:"pairs_formed"`
	EndToEnd    e2eStats     `json:"end_to_end"`
	PerPair     []pairReport `json:"per_pair"`
	Ping        pingStats    `json:"ping_rtt"`
	Server      serverReport `json:"server"`

	Teardown teardownResult `json:"teardown"`
	Cleanup  *cleanupResult `json:"cleanup,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
}

type e2eStats struct {
	Samples    int     `json:"samples"`
	Expected   int     `json:"expected"`
	Lost       int     `json:"lost"`
	LossPct    float64 `json:"loss_pct"`
	WarmupLost int     `json:"warmup_lost"`
	Duplicates int     `json:"duplicates"`
	OutOfOrder int     `json:"out_of_order"`
	Late       int     `json:"arrived_after_grace"`
	Foreign    int     `json:"unattributable_frames,omitempty"`
	MinMs      float64 `json:"min_ms"`
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	MaxMs      float64 `json:"max_ms"`
	MeanMs     float64 `json:"mean_ms"`
}

type pairReport struct {
	Pair       int     `json:"pair"`
	Room       string  `json:"room"`
	Sent       int     `json:"sent"`
	Samples    int     `json:"samples"`
	Lost       int     `json:"lost"`
	Duplicates int     `json:"duplicates,omitempty"`
	OutOfOrder int     `json:"out_of_order,omitempty"`
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	MaxMs      float64 `json:"max_ms"`
}

type pingStats struct {
	Connections int     `json:"connections"`
	Sent        int     `json:"sent"`
	Received    int     `json:"received"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	MaxMs       float64 `json:"max_ms"`
	MeanMs      float64 `json:"mean_ms"`
}

func (b *bench) buildReport(setupErr error) *report {
	cfg := b.cfg
	r := &report{
		Tool:        "latencybench",
		RunID:       b.runID,
		StartedAt:   b.started.UTC(),
		DurationS:   time.Since(b.started).Round(time.Millisecond).Seconds(),
		GoVersion:   runtime.Version(),
		OS:          runtime.GOOS + "/" + runtime.GOARCH,
		Interrupted: b.interrupted,
		PairsFormed: len(b.pairs),
		Teardown:    b.teardown,
		Cleanup:     b.cleanup,
		Warnings:    b.warnings,
	}
	if setupErr != nil {
		r.SetupError = setupErr.Error()
	}
	r.Targets.API, r.Targets.WS, r.Targets.Metrics = cfg.api, cfg.ws, cfg.metrics
	r.Params.Pairs, r.Params.Samples, r.Params.Warmup = cfg.pairs, cfg.samples, cfg.warmup
	r.Params.IntervalMs, r.Params.GraceMs = ms(cfg.interval), ms(cfg.grace)
	r.Params.PayloadBytes, r.Params.PingsPerConn, r.Params.RankWindow = cfg.payload, cfg.pings, cfg.rankWindow
	r.Params.PercentileMethod = "exact nearest-rank over all counted samples (sorted)"

	var all []time.Duration
	for _, p := range b.pairs {
		if p.recv == nil {
			continue
		}
		res := p.recv.result(cfg.warmup, p.sent)
		all = append(all, res.samples...)
		e := &r.EndToEnd
		e.Expected += res.expected
		e.Lost += res.lost
		e.WarmupLost += res.warmupLost
		e.Duplicates += res.dup
		e.OutOfOrder += res.ooo
		e.Late += res.late
		e.Foreign += res.foreign
		d := summarize(res.samples)
		r.PerPair = append(r.PerPair, pairReport{
			Pair: p.idx, Room: p.room, Sent: p.sent, Samples: d.N, Lost: res.lost,
			Duplicates: res.dup, OutOfOrder: res.ooo,
			P50Ms: ms(d.P50), P95Ms: ms(d.P95), P99Ms: ms(d.P99), MaxMs: ms(d.Max),
		})
	}
	d := summarize(all)
	e := &r.EndToEnd
	e.Samples = d.N
	if e.Expected > 0 {
		e.LossPct = round3(100 * float64(e.Lost) / float64(e.Expected))
	}
	e.MinMs, e.P50Ms, e.P95Ms, e.P99Ms, e.MaxMs, e.MeanMs = ms(d.Min), ms(d.P50), ms(d.P95), ms(d.P99), ms(d.Max), ms(d.Mean)

	var rtts []time.Duration
	for _, p := range b.pairs {
		for _, pl := range []*player{p.sender, p.receiver} {
			sent, got := pl.conn.pingStats()
			r.Ping.Connections++
			r.Ping.Sent += sent
			rtts = append(rtts, got...)
		}
	}
	pd := summarize(rtts)
	r.Ping.Received = pd.N
	r.Ping.P50Ms, r.Ping.P95Ms, r.Ping.MaxMs, r.Ping.MeanMs = ms(pd.P50), ms(pd.P95), ms(pd.Max), ms(pd.Mean)

	switch {
	case cfg.metrics == "":
		r.Server = serverReport{Note: "no -metrics URL given; server stage breakdown skipped"}
	case b.scrapeBefore == nil || b.scrapeAfter == nil:
		note := "metrics not scraped before and after the measured window"
		if len(b.scrapeErr) > 0 {
			note += ": " + strings.Join(b.scrapeErr, "; ")
		}
		r.Server = serverReport{MetricsURL: cfg.metrics, Note: note}
	default:
		r.Server = compareScrapes(cfg.metrics, b.scrapeBefore, b.scrapeAfter)
	}
	return r
}

func (r *report) writeJSON(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// printer writes formatted lines and remembers the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

func (r *report) print(out io.Writer) error {
	w := &printer{w: out}
	w.f("\n=== latencybench run %s  %s  (%s, %s)\n", r.RunID, r.StartedAt.Format(time.RFC3339), r.GoVersion, r.OS)
	w.f("target  api=%s  ws=%s\n", r.Targets.API, r.Targets.WS)
	w.f("params  pairs=%d (formed %d)  samples=%d  warmup=%d  interval=%gms  payload=%dB  grace=%gms\n",
		r.Params.Pairs, r.PairsFormed, r.Params.Samples, r.Params.Warmup, r.Params.IntervalMs, r.Params.PayloadBytes, r.Params.GraceMs)
	if r.SetupError != "" {
		w.f("SETUP FAILED: %s\n", r.SetupError)
	}
	if r.Interrupted {
		w.f("INTERRUPTED: results are partial\n")
	}

	e := r.EndToEnd
	w.f("\nEnd-to-end, player -> player (monotonic clock, exact percentiles, all pairs pooled)\n")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	tp := &printer{w: tw}
	tp.f("samples\tlost\tp50\tp95\tp99\tmax\tmean\t\n")
	tp.f("%d\t%d (%.2f%%)\t%.3fms\t%.3fms\t%.3fms\t%.3fms\t%.3fms\t\n",
		e.Samples, e.Lost, e.LossPct, e.P50Ms, e.P95Ms, e.P99Ms, e.MaxMs, e.MeanMs)
	if err := tw.Flush(); err != nil && w.err == nil {
		w.err = err
	}
	if e.Duplicates+e.OutOfOrder+e.Late+e.Foreign+e.WarmupLost > 0 {
		w.f("  duplicates=%d  out-of-order=%d  arrived-after-grace=%d  unattributable=%d  warmup-lost=%d\n",
			e.Duplicates, e.OutOfOrder, e.Late, e.Foreign, e.WarmupLost)
	}
	if len(r.PerPair) > 1 && e.Samples > 0 {
		lo, hi := r.PerPair[0], r.PerPair[0]
		for _, p := range r.PerPair {
			if p.P95Ms < lo.P95Ms {
				lo = p
			}
			if p.P95Ms > hi.P95Ms {
				hi = p
			}
		}
		w.f("  per-pair p95 ranges %.3fms (pair %d) .. %.3fms (pair %d); see -out for every pair\n", lo.P95Ms, lo.Pair, hi.P95Ms, hi.Pair)
	}

	p := r.Ping
	w.f("\nNetwork: WebSocket ping -> pong RTT (%d connections, %d/%d pongs)\n", p.Connections, p.Received, p.Sent)
	w.f("  p50 %.3fms  p95 %.3fms  max %.3fms  mean %.3fms\n", p.P50Ms, p.P95Ms, p.MaxMs, p.MeanMs)
	w.f("  (one-way network ~ RTT/2 per hop; end-to-end crosses two hops: sender -> server -> receiver)\n")

	s := r.Server
	w.f("\nServer stages: %s, delta over the counted window (BUCKET ESTIMATES)\n", relayHistogram)
	if len(s.Stages) > 0 {
		tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
		tp = &printer{w: tw}
		tp.f("stage\tcount\tmean\t~p50\t~p95\t~p99\t\n")
		for _, st := range s.Stages {
			if st.Error != "" {
				tp.f("%s\t%s\t\t\t\t\t\n", st.Stage, st.Error)
				continue
			}
			mark := ""
			if st.Overflow {
				mark = "+"
			}
			tp.f("%s\t%.0f\t%.3fms\t%.3fms\t%.3fms\t%.3fms%s\t\n", st.Stage, st.Count, st.MeanMs, st.P50Ms, st.P95Ms, st.P99Ms, mark)
		}
		if err := tw.Flush(); err != nil && w.err == nil {
			w.err = err
		}
		w.f("  (client sent %d counted messages; mean is exact, percentiles interpolated within buckets; '+' = beyond the top bucket)\n", e.Expected)
	}
	for _, c := range s.Counters {
		w.f("  %s%s  +%.0f\n", c.Metric, c.Labels, c.Delta)
	}
	if s.Note != "" {
		w.f("  note: %s\n", s.Note)
	}

	t := r.Teardown
	w.f("\nTeardown: left queue %d, closed %d socket(s), logged out %d token(s)", t.LeftQueue, t.SocketsClosed, t.LoggedOut)
	if len(t.Errors) > 0 {
		w.f(", %d error(s): %s", len(t.Errors), strings.Join(t.Errors, "; "))
	}
	w.f("\n")
	if r.Cleanup != nil {
		if r.Cleanup.Error != "" {
			w.f("Postgres cleanup FAILED (rolled back): %s\n", r.Cleanup.Error)
		} else {
			w.f("Postgres cleanup (%s): %s\n", r.Cleanup.Pattern, r.Cleanup)
		}
	}
	if len(r.Warnings) > 0 {
		w.f("Warnings (%d):\n", len(r.Warnings))
		for _, msg := range r.Warnings {
			w.f("  - %s\n", msg)
		}
	}
	return errors.Join(w.err, tp.err)
}
