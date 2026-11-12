package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// maxRank is the highest rank POST /queue accepts.
const maxRank = 100000

type bench struct {
	cfg      *config
	api      *apiClient
	runID    string
	password string
	started  time.Time

	players []*player
	pairs   []*pair
	matches chan matchNote

	warnings []string
	mu       sync.Mutex // guards warnings

	// filled by measure
	scrapeBefore, scrapeAfter families
	scrapeErr                 []string
	beforeScrapeTook          time.Duration
	interrupted               bool

	teardown teardownResult
	cleanup  *cleanupResult
}

type teardownResult struct {
	LeftQueue     int      `json:"left_queue"`
	SocketsClosed int      `json:"sockets_closed"`
	LoggedOut     int      `json:"logged_out"`
	Errors        []string `json:"errors,omitempty"`
}

func newBench(cfg *config) *bench {
	return &bench{
		cfg:      cfg,
		api:      newAPIClient(cfg.api, cfg.setupRPS),
		runID:    randomString(6, "abcdefghijklmnopqrstuvwxyz0123456789"),
		password: randomString(24, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"),
		matches:  make(chan matchNote, 16*cfg.pairs+16),
	}
}

func randomString(n int, alphabet string) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func (b *bench) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	b.mu.Lock()
	b.warnings = append(b.warnings, msg)
	b.mu.Unlock()
	log.Print("WARNING: ", msg)
}

func (b *bench) logf(format string, args ...any) {
	if b.cfg.verbose {
		log.Printf(format, args...)
	}
}

// run executes the whole benchmark and returns the process exit code.
func (b *bench) run(ctx context.Context) int {
	b.started = time.Now()
	log.Printf("run %s: %d pair(s), %d samples (%d warmup) every %s, api=%s ws=%s",
		b.runID, b.cfg.pairs, b.cfg.samples, b.cfg.warmup, b.cfg.interval, b.cfg.api, b.cfg.ws)
	if b.cfg.interval < time.Second/60 {
		b.warnf("-interval %s exceeds the server's 60 msg/s relay limit; messages above it are dropped", b.cfg.interval)
	}

	exit := 0
	setupErr := b.setup(ctx)
	if setupErr != nil {
		log.Printf("setup failed: %v", setupErr)
		exit = 1
	} else {
		b.measure(ctx)
	}
	if ctx.Err() != nil {
		b.interrupted = true
		exit = 1
	}

	b.doTeardown()
	b.doCleanup()

	rep := b.buildReport(setupErr)
	if err := rep.print(os.Stdout); err != nil {
		log.Printf("writing summary: %v", err)
	}
	if b.cfg.out != "" {
		if err := rep.writeJSON(b.cfg.out); err != nil {
			log.Printf("writing %s: %v", b.cfg.out, err)
			exit = 1
		} else {
			log.Printf("wrote %s", b.cfg.out)
		}
	}
	if setupErr == nil && !b.interrupted && rep.EndToEnd.Samples == 0 {
		log.Print("FAIL: no samples were collected (is relay implemented by this server?)")
		exit = 1
	}
	if b.cleanup != nil && b.cleanup.Error != "" {
		exit = 1
	}
	b.loudLeftovers()
	return exit
}

// setup creates the players, connects them and pairs them into joined rooms.
func (b *bench) setup(ctx context.Context) error {
	if err := b.createPlayers(ctx); err != nil {
		return err
	}
	if err := b.connectAll(ctx); err != nil {
		return err
	}
	if err := b.pairUp(ctx); err != nil {
		return err
	}
	return b.joinRooms(ctx)
}

// forEach runs fn for every player with bounded concurrency and returns the
// first error; the others are cancelled.
func (b *bench) forEach(ctx context.Context, players []*player, conc int, fn func(context.Context, *player) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for _, p := range players {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(p *player) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, p); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}(p)
	}
	wg.Wait()
	if first == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return first
}

func (b *bench) createPlayers(ctx context.Context) error {
	n := 2 * b.cfg.pairs
	b.players = make([]*player, n)
	for i := range b.players {
		b.players[i] = &player{
			idx:      i,
			username: fmt.Sprintf("bench%s%d", b.runID, i),
			email:    fmt.Sprintf("bench_%s_%d@%s", b.runID, i, emailDomain),
		}
	}
	t0 := time.Now()
	err := b.forEach(ctx, b.players, b.cfg.setupConc, func(ctx context.Context, p *player) error {
		err := b.api.register(ctx, p, b.password)
		// Unless the server definitively refused (4xx), the account may exist.
		if st := statusOf(err); err == nil || st == 0 || st >= 500 {
			p.registered = true
		}
		if err != nil {
			return fmt.Errorf("register %s: %w", p.username, err)
		}
		if err := b.api.login(ctx, p, b.password); err != nil {
			return fmt.Errorf("login %s: %w", p.username, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	log.Printf("registered and logged in %d players in %s", n, time.Since(t0).Round(time.Millisecond))
	return nil
}

func (b *bench) connectAll(ctx context.Context) error {
	t0 := time.Now()
	var mu sync.Mutex
	err := b.forEach(ctx, b.players, 16, func(ctx context.Context, p *player) error {
		c, err := dial(ctx, b.api, b.cfg.ws, p, b.matches)
		if err != nil {
			return err
		}
		mu.Lock()
		p.conn = c
		mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}
	log.Printf("connected %d websockets in %s", len(b.players), time.Since(t0).Round(time.Millisecond))
	return nil
}

// pairUp queues the players and groups them by the room matchmaking puts
// them in.
//
// Making it deterministic: the matcher reads the queue sorted by rank and
// greedily pairs adjacent tickets whose rank gap is within the window
// (internal/matchmaking/matcher.go). Each intended couple is queued at its
// own rank, couples are rank-window+1 apart, so a couple can only pair with
// itself, even if a tick runs while only one of the two is queued. Ranks
// start at a random point high in the range, away from real players (who
// queue around 1000) and from the leftovers of earlier runs.
//
// Pairs are nevertheless taken from MatchFound, not assumed: a room holding
// two of our players is a pair whoever they are. If a room mixes one of ours
// with a foreign player (a real player or a crashed run's leftover ticket),
// ours becomes an orphan; its intended partner, if still queued alone, is
// withdrawn, and orphans are re-queued two at a time at a fresh rank.
func (b *bench) pairUp(ctx context.Context) error {
	step := b.cfg.rankWindow + 1
	need := 2 * b.cfg.pairs * step // the initial couples plus as many re-queues
	hi := maxRank - need
	lo := hi / 2
	nextRank := lo + mrand.IntN(hi-lo+1) // #nosec G404 -- placement, not security

	byID := make(map[string]*player, len(b.players))
	for _, p := range b.players {
		byID[p.id] = p
	}
	partner := map[*player]*player{}
	queueCouple := func(x, y *player) error {
		if nextRank > maxRank {
			return errors.New("ran out of ranks for re-queueing; matchmaking keeps pairing bench players with foreign tickets")
		}
		r := nextRank
		nextRank += step
		partner[x], partner[y] = y, x
		for _, p := range []*player{x, y} {
			if err := b.api.joinQueue(ctx, p, r); err != nil {
				return fmt.Errorf("queue %s: %w", p.username, err)
			}
			p.queued.Store(true)
		}
		b.logf("queued %s + %s at rank %d", x.username, y.username, r)
		return nil
	}

	t0 := time.Now()
	for i := 0; i < b.cfg.pairs; i++ {
		if err := queueCouple(b.players[2*i], b.players[2*i+1]); err != nil {
			return err
		}
	}
	log.Printf("queued %d players; waiting for MatchFound (match-timeout %s)", len(b.players), b.cfg.matchTO)

	seen := map[string]bool{}
	var orphans []*player
	timeout := time.NewTimer(b.cfg.matchTO)
	defer timeout.Stop()
	for len(b.pairs) < b.cfg.pairs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("matchmaking paired %d of %d pairs within %s", len(b.pairs), b.cfg.pairs, b.cfg.matchTO)
		case n := <-b.matches:
			if seen[n.room] {
				continue // the partner's copy, or an at-least-once redelivery
			}
			seen[n.room] = true
			var ours []*player
			for _, id := range n.players {
				if p, ok := byID[id]; ok {
					ours = append(ours, p)
					p.queued.Store(false) // the matcher dequeued it
				}
			}
			if len(ours) == 0 {
				continue
			}
			if len(ours) == 2 && len(n.players) == 2 {
				slices.SortFunc(ours, func(x, y *player) int { return x.idx - y.idx })
				b.pairs = append(b.pairs, &pair{idx: len(b.pairs), room: n.room, sender: ours[0], receiver: ours[1]})
				b.logf("room %s: %s -> %s", n.room, ours[0].username, ours[1].username)
				continue
			}
			b.warnf("room %s puts %d bench player(s) with %d foreign player(s); re-queueing ours", n.room, len(ours), len(n.players)-len(ours))
			for _, x := range ours {
				orphans = append(orphans, x)
				// Its partner would otherwise wait alone at a rank nobody else uses.
				if y := partner[x]; y != nil && y.queued.Load() {
					removed, err := b.api.leaveQueue(ctx, y)
					if err != nil {
						return fmt.Errorf("withdraw %s: %w", y.username, err)
					}
					if removed { // otherwise the matcher took it; its MatchFound is on the way
						y.queued.Store(false)
						orphans = append(orphans, y)
					}
				}
			}
			for len(orphans) >= 2 {
				if err := queueCouple(orphans[0], orphans[1]); err != nil {
					return err
				}
				orphans = orphans[2:]
			}
		}
	}
	log.Printf("matchmaking formed %d pair(s) in %s", len(b.pairs), time.Since(t0).Round(time.Millisecond))
	return nil
}

// joinRooms joins every pair: receiver first, confirmed by its own
// PlayerJoined, then the sender, so the receiver is a member before the
// first relay can arrive.
func (b *bench) joinRooms(ctx context.Context) error {
	errs := make([]error, len(b.pairs))
	var wg sync.WaitGroup
	for i, p := range b.pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, pl := range []*player{p.receiver, p.sender} {
				if err := pl.conn.sendJSON(map[string]string{"action": "join", "room": p.room}); err != nil {
					errs[i] = fmt.Errorf("%s: send join: %w", pl.username, err)
					return
				}
				if err := pl.conn.waitJoined(ctx, p.room, pl.id, 10*time.Second); err != nil {
					errs[i] = err
					return
				}
			}
			p.recv = newReceiver(p.room, p.sender.id, b.cfg.samples, b.cfg.warmup)
			p.receiver.conn.recv.Store(p.recv)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	log.Printf("all %d pair(s) joined their rooms", len(b.pairs))
	return nil
}

// measure runs the senders, pingers and the two metric scrapes.
//
// Timeline (offsets from base): pair i sends message k at
// base + i*interval/(2N) + k*interval, so all senders are spread over the
// first half of each interval. The second half is quiet, which gives the
// "before" scrape a slot between the last warmup message and the first
// counted one (at base + (warmup-1)*interval + 3/4*interval). Pings are
// spread over the counted window on every connection.
func (b *bench) measure(ctx context.Context) {
	cfg := b.cfg
	n := len(b.pairs)
	base := mono() + 300*time.Millisecond
	stagger := cfg.interval / 2 / time.Duration(n)
	countedFrom := base + time.Duration(cfg.warmup)*cfg.interval
	sendEnd := base + time.Duration(cfg.samples)*cfg.interval

	if cfg.metrics != "" && cfg.warmup == 0 {
		b.scrape(ctx, true)
	}

	var wg sync.WaitGroup
	for i, p := range b.pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSender(ctx, p, cfg, base+time.Duration(i)*stagger)
		}()
	}
	var pingWG sync.WaitGroup
	conns := 2 * n
	for i, p := range b.pairs {
		for j, pl := range []*player{p.sender, p.receiver} {
			phase := (float64(2*i+j) + 0.5) / float64(conns)
			pingWG.Add(1)
			go func() {
				defer pingWG.Done()
				runPinger(ctx, pl.conn, cfg.pings, countedFrom, sendEnd, phase)
			}()
		}
	}
	if cfg.metrics != "" && cfg.warmup > 0 {
		at := base + time.Duration(cfg.warmup-1)*cfg.interval + cfg.interval*3/4
		if sleepUntil(ctx, at) {
			b.scrape(ctx, true)
		}
	}
	log.Printf("sending: %d pair(s) x %d messages, ~%s", n, cfg.samples, (sendEnd - mono()).Round(time.Second))

	wg.Wait()
	if ctx.Err() == nil {
		sleepUntil(ctx, mono()+cfg.grace)
	}
	for _, p := range b.pairs {
		p.recv.stop()
	}
	pingWG.Wait()
	if cfg.metrics != "" && b.scrapeBefore != nil && ctx.Err() == nil {
		b.scrape(ctx, false)
	}
	for _, p := range b.pairs {
		if p.sendErr != nil {
			b.warnf("pair %d: sender stopped after %d messages: %v", p.idx, p.sent, p.sendErr)
		}
		for _, pl := range []*player{p.sender, p.receiver} {
			if !pl.conn.alive() {
				b.warnf("pair %d: %s connection closed during the run: %v", p.idx, pl.username, pl.conn.err())
			}
		}
	}
}

func (b *bench) scrape(ctx context.Context, before bool) {
	t0 := time.Now()
	f, err := scrapeMetrics(ctx, b.cfg.metrics)
	if err != nil {
		b.scrapeErr = append(b.scrapeErr, err.Error())
		b.warnf("metrics scrape failed: %v", err)
		return
	}
	if before {
		b.scrapeBefore = f
		b.beforeScrapeTook = time.Since(t0)
		if b.beforeScrapeTook > b.cfg.interval/4 {
			b.warnf("the 'before' scrape took %s, longer than the quiet slot (%s); a few counted messages may fall outside the server window",
				b.beforeScrapeTook.Round(time.Millisecond), b.cfg.interval/4)
		}
	} else {
		b.scrapeAfter = f
	}
}

// doTeardown always runs after setup started, also on error or Ctrl-C:
// withdraw queue tickets, close sockets, revoke every token.
func (b *bench) doTeardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var mu sync.Mutex
	fail := func(err error) {
		mu.Lock()
		b.teardown.Errors = append(b.teardown.Errors, err.Error())
		mu.Unlock()
	}

	var queued []*player
	for _, p := range b.players {
		if p != nil && p.token != "" && p.queued.Load() {
			queued = append(queued, p)
		}
	}
	_ = b.forEach(ctx, queued, 4, func(ctx context.Context, p *player) error {
		removed, err := b.api.leaveQueue(ctx, p)
		if err != nil {
			fail(fmt.Errorf("leave queue %s: %w", p.username, err))
			return nil
		}
		p.queued.Store(false)
		if removed {
			mu.Lock()
			b.teardown.LeftQueue++
			mu.Unlock()
		}
		return nil
	})

	var wg sync.WaitGroup
	for _, p := range b.players {
		if p == nil || p.conn == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.conn.close()
			mu.Lock()
			b.teardown.SocketsClosed++
			mu.Unlock()
		}()
	}
	wg.Wait()

	var withToken []*player
	for _, p := range b.players {
		if p != nil && p.token != "" {
			withToken = append(withToken, p)
		}
	}
	_ = b.forEach(ctx, withToken, 4, func(ctx context.Context, p *player) error {
		if err := b.api.logout(ctx, p); err != nil {
			fail(fmt.Errorf("logout %s: %w", p.username, err))
			return nil
		}
		mu.Lock()
		b.teardown.LoggedOut++
		mu.Unlock()
		return nil
	})
	if len(b.players) > 0 {
		log.Printf("teardown: left queue %d, closed %d socket(s), logged out %d token(s), %d error(s)",
			b.teardown.LeftQueue, b.teardown.SocketsClosed, b.teardown.LoggedOut, len(b.teardown.Errors))
	}
}

func (b *bench) registeredCount() int {
	n := 0
	for _, p := range b.players {
		if p != nil && p.registered {
			n++
		}
	}
	return n
}

func (b *bench) doCleanup() {
	if b.cfg.cleanupDSN == "" || b.registeredCount() == 0 {
		return
	}
	var jtis []string
	for _, p := range b.players {
		if p.jti != "" {
			jtis = append(jtis, p.jti)
		}
	}
	res, err := deleteBenchAccounts(context.Background(), cleanupOptions{
		DSN: b.cfg.cleanupDSN, Pattern: runPattern(b.runID), Limit: b.cfg.cleanupLimit, DryRun: b.cfg.cleanupDry, JTIs: jtis,
	})
	if err != nil {
		res = cleanupResult{Pattern: runPattern(b.runID), Limit: b.cfg.cleanupLimit, DryRun: b.cfg.cleanupDry, Error: err.Error()}
		log.Printf("cleanup FAILED, nothing deleted: %v", err)
	} else {
		log.Printf("cleanup: %s", res)
	}
	b.cleanup = &res
}

// loudLeftovers prints the last line when accounts may still be in Postgres.
func (b *bench) loudLeftovers() {
	n := b.registeredCount()
	if n == 0 || (b.cleanup != nil && b.cleanup.Error == "" && !b.cleanup.DryRun && b.cleanup.Remaining == 0) {
		return
	}
	dsn := `"$POSTGRES_DSN"`
	if u, err := url.Parse(b.cfg.api); err == nil {
		if h := u.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
			// #nosec G101 -- the docker-compose default, already public in docker-compose.yml
			dsn = `'postgres://gamemesh:gamemesh-dev-password@localhost:5432/gamemesh?sslmode=disable'`
		}
	}
	fmt.Fprintf(os.Stderr, "\n%s\n!!! %d bench account(s) of run %s are still in Postgres (e-mail bench_%s_*@%s, username bench%s*). Count them with -cleanup-dry-run, delete them with:\n!!!   go run ./scripts/latencybench -cleanup-only -cleanup-dsn %s   (-cleanup-limit N caps the batch, default 200; run again if some remain)\n%s\n",
		strings.Repeat("!", 78), n, b.runID, b.runID, emailDomain, b.runID, dsn, strings.Repeat("!", 78))
}
