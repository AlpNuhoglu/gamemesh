// latencybench measures player-to-player WebSocket relay latency.
//
// It registers 2×N bench players, pairs them through matchmaking, joins each
// pair into its room and has one player of each pair relay -samples messages
// to the other. Both ends live in this process, so latency is measured on one
// monotonic clock: the send offset since process start travels in the
// payload and is subtracted from the receive offset. It also measures
// WebSocket ping RTT on every connection (how much of the number is network)
// and, with -metrics, the server's own stage histograms over the same window.
//
// Bench accounts have e-mails bench_<run>_<n>@latencybench.invalid. Sockets
// are closed, queue tickets withdrawn and tokens logged out at the end of
// every run. Postgres is touched only with -cleanup-dsn (no default): it
// deletes this run's accounts at the end, and -cleanup-only deletes the
// leftovers of earlier runs. A bench account must match BOTH the e-mail
// pattern and username LIKE 'bench%'. The matches are counted and printed
// first; -cleanup-dry-run stops there, -cleanup-limit (default 200) caps how
// many players one call deletes (run again if some remain).
//
// Usage:
//
//	go run ./scripts/latencybench -pairs 10 -metrics http://localhost:8084/metrics \
//	    -cleanup-dsn 'postgres://gamemesh:gamemesh-dev-password@localhost:5432/gamemesh?sslmode=disable'
//
//	go run ./scripts/latencybench -cleanup-only -cleanup-dry-run -cleanup-dsn '…'   # count only
//	go run ./scripts/latencybench -cleanup-only -cleanup-limit 100 -cleanup-dsn '…'
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// processStart anchors every timestamp the tool takes. time.Since reads the
// monotonic clock, so offsets from it never jump with wall-clock changes.
var processStart = time.Now()

func mono() time.Duration { return time.Since(processStart) }

type config struct {
	api, ws, metrics, out    string
	cleanupDSN               string
	cleanupOnly, cleanupDry  bool
	cleanupLimit             int
	pairs, samples, warmup   int
	payload, pings           int
	rankWindow               int
	interval, grace, matchTO time.Duration
	setupRPS                 float64
	setupConc                int
	verbose                  bool
}

func parseFlags() (*config, error) {
	c := &config{}
	flag.StringVar(&c.api, "api", "http://localhost:8080", "HTTP API base URL (gateway)")
	flag.StringVar(&c.ws, "ws", "ws://localhost:8084", "WebSocket base URL; /ws is appended")
	flag.StringVar(&c.metrics, "metrics", "", "Prometheus endpoint of the websocket service, e.g. http://localhost:8084/metrics (optional)")
	flag.StringVar(&c.out, "out", "", "write the full result as JSON to this file")
	flag.StringVar(&c.cleanupDSN, "cleanup-dsn", "", "Postgres DSN (no default; Postgres is never touched without it); delete this run's bench accounts at the end")
	flag.BoolVar(&c.cleanupOnly, "cleanup-only", false, "only delete every bench account (e-mail %@"+emailDomain+" AND username LIKE 'bench%') via -cleanup-dsn, then exit")
	flag.BoolVar(&c.cleanupDry, "cleanup-dry-run", false, "with -cleanup-dsn: count the matching bench accounts and delete nothing (needs -cleanup-dsn)")
	flag.IntVar(&c.cleanupLimit, "cleanup-limit", 200, "delete at most this many bench players per cleanup (> 0); run again if some remain")
	flag.IntVar(&c.pairs, "pairs", 1, "number of concurrent sender/receiver pairs")
	flag.IntVar(&c.samples, "samples", 550, "messages each sender sends (including warmup)")
	flag.IntVar(&c.warmup, "warmup", 50, "leading messages per pair excluded from the results")
	flag.DurationVar(&c.interval, "interval", 50*time.Millisecond, "send interval per sender")
	flag.DurationVar(&c.grace, "grace", 2*time.Second, "wait after the last send before unreceived messages count as lost")
	flag.IntVar(&c.payload, "payload", 120, "size in bytes of the relayed data object (max 1024)")
	flag.IntVar(&c.pings, "pings", 20, "WebSocket pings per connection, spread over the measured window")
	flag.IntVar(&c.rankWindow, "rank-window", 100, "matchmaking rank window; must be >= the server's MATCH_RANK_WINDOW")
	flag.DurationVar(&c.matchTO, "match-timeout", 60*time.Second, "how long to wait for matchmaking to pair every player")
	flag.Float64Var(&c.setupRPS, "setup-rps", 20, "HTTP request rate during setup/teardown (gateway allows 50/s per IP)")
	flag.IntVar(&c.setupConc, "setup-concurrency", 8, "concurrent register/login calls (login is bcrypt-bound)")
	flag.BoolVar(&c.verbose, "v", false, "verbose progress logging")
	flag.Parse()

	if flag.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	if c.cleanupLimit <= 0 {
		return nil, errors.New("-cleanup-limit must be > 0")
	}
	if c.cleanupDSN == "" && (c.cleanupOnly || c.cleanupDry) {
		return nil, errors.New("-cleanup-only and -cleanup-dry-run need -cleanup-dsn")
	}
	if c.cleanupOnly {
		return c, nil
	}
	switch {
	case c.pairs < 1:
		return nil, errors.New("-pairs must be >= 1")
	case c.warmup < 0 || c.samples <= c.warmup:
		return nil, errors.New("-samples must be greater than -warmup (and -warmup >= 0)")
	case c.interval <= 0 || c.grace < 0:
		return nil, errors.New("-interval must be > 0 and -grace >= 0")
	case c.payload < 48 || c.payload > 1024:
		return nil, errors.New("-payload must be between 48 and 1024 bytes")
	case c.pings < 0 || c.rankWindow < 0 || c.setupRPS <= 0 || c.setupConc < 1:
		return nil, errors.New("-pings, -rank-window must be >= 0; -setup-rps, -setup-concurrency > 0")
	}
	for _, u := range []string{c.api, c.ws} {
		if _, err := url.Parse(u); err != nil {
			return nil, fmt.Errorf("bad URL %q: %w", u, err)
		}
	}
	if need := 2 * c.pairs * (c.rankWindow + 1); need > maxRank {
		return nil, fmt.Errorf("-pairs %d with -rank-window %d needs %d distinct ranks; the API allows %d", c.pairs, c.rankWindow, need, maxRank)
	}
	return c, nil
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("latencybench ")
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "latencybench:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // a second Ctrl-C kills the process outright
	}()

	if cfg.cleanupOnly {
		res, err := deleteBenchAccounts(ctx, cleanupOptions{DSN: cfg.cleanupDSN, Pattern: allBenchPattern, Limit: cfg.cleanupLimit, DryRun: cfg.cleanupDry})
		if err != nil {
			fmt.Fprintln(os.Stderr, "latencybench: cleanup failed (nothing deleted):", err)
			os.Exit(1)
		}
		fmt.Printf("bench accounts with e-mail matching %s: %s\n", res.Pattern, res)
		os.Exit(0)
	}
	os.Exit(newBench(cfg).run(ctx))
}
