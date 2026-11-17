package main

// The ghosttag mode: the same measurement through a validated room.
//
// In a validated room the server opens each position packet, validates it
// and broadcasts its own pos, keeping only the position (seq and cts are
// dropped, x and y rounded to 0.01). A timestamp in the payload cannot
// survive, so the position itself identifies the message: the sender moves
// in a straight line from its spawn along +y, ghostStep units per message,
// and keeps each message's send time here, keyed by index. The receiver
// recovers the index from the y it is told and looks the send time up. Both
// ends are in this process, so it is still one monotonic clock.
//
// The path must be the validated path as intended: a position that the
// server corrected (clipped) maps to no index, is counted, and fails the
// run. The schedule is checked up front to stay inside the arena, away from
// a capture, and before the arena starts to shrink.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Modes of -mode.
const (
	modeRelay = "relay"
	modeGhost = "ghosttag"
)

const (
	// ghostStep is how far the sender moves per message, in arena units,
	// along +y. At 40 ms that is 12.5 units/s, far under either role's top
	// speed (225 and 260), in a straight line, so the server never clips it.
	ghostStep = 0.5
	// ghostGridTol is how far a received coordinate may lie from the grid
	// and still be the position that was sent: the server rounds to 0.01.
	ghostGridTol = 0.01
	// ghostDefaultInterval is the game's packet rate (25 Hz), the default
	// -interval in ghosttag mode.
	ghostDefaultInterval = 40 * time.Millisecond

	// ghostShrinkStart is when the arena starts to shrink, measured from the
	// start of the half (THalf − TShrink = 45 s − 15 s). Every message must
	// have been sent by then, less ghostShrinkMargin.
	ghostShrinkStart = 30 * time.Second
	// ghostShrinkMargin absorbs what the client cannot see of the half's
	// start: the snap's one-way trip, and the moments between the check
	// and the first send.
	ghostShrinkMargin = time.Second

	// The server's defaults (internal/ghosttag DefaultConfig), used only to
	// refuse impossible flags before any account is created. The run itself
	// checks against the spawn points and radius the room reports.
	ghostDefaultSpawn = 225.0 // hunter at (−225, 0), runner at (+225, 0)
	ghostDefaultR0    = 500.0
	// ghostCaptureDistance is the capture radius (DCapture). The snap does
	// not report it.
	ghostCaptureDistance = 36.0
	// ghostArenaMargin keeps the last position this far inside the arena.
	ghostArenaMargin = 1.0

	// ghostActiveTimeout bounds the wait for HALF_1_ACTIVE after both
	// players joined: T_join (10 s) and the countdown (2 s), with room.
	ghostActiveTimeout = 15 * time.Second
	// stateActive is the snap state of the first half.
	stateActive = "HALF_1_ACTIVE"
)

// ghostY is the y of message k for a sender whose spawn y is y0.
func ghostY(y0 float64, k int) float64 { return y0 + float64(k)*ghostStep }

// ghostIndex recovers the message index from a received position: the
// index whose grid point lies within ghostGridTol of (x, y) in both
// coordinates and is in [0, n). ok is false for anything else, a NaN
// included.
func ghostIndex(x, y, x0, y0 float64, n int) (k int, ok bool) {
	f := math.Round((y - y0) / ghostStep)
	// Written as !(… <= …) so that a NaN fails every test.
	if !(math.Abs(x-x0) <= ghostGridTol) || !(math.Abs(y-(y0+f*ghostStep)) <= ghostGridTol) ||
		!(f >= 0 && f < float64(n)) {
		return 0, false
	}
	return int(f), true
}

// checkGhostSchedule refuses a schedule whose last message would go out
// after the arena starts to shrink. first is how long after the start of
// the half the first message goes out.
func checkGhostSchedule(first time.Duration, samples int, interval time.Duration) error {
	last := first + time.Duration(samples-1)*interval
	if limit := ghostShrinkStart - ghostShrinkMargin; last > limit {
		return fmt.Errorf("-samples %d every %s would send the last message %s into the half; it must be done by %s "+
			"(the arena shrinks from %s into the half, %s margin): lower -samples or -interval",
			samples, interval, last.Round(time.Millisecond), limit, ghostShrinkStart, ghostShrinkMargin)
	}
	return nil
}

// checkGhostArena refuses a path from (x0, y0) along +y for samples messages
// that would leave an arena of the given radius, or come within capture
// distance of the other player standing at (ox, oy).
func checkGhostArena(x0, y0, ox, oy, radius float64, samples int) error {
	yEnd := ghostY(y0, samples-1)
	if r := math.Hypot(x0, yEnd); !(r <= radius-ghostArenaMargin) {
		return fmt.Errorf("-samples %d at %g units each ends at (%g, %g), %.1f from the centre; the arena radius is %g: lower -samples",
			samples, ghostStep, x0, yEnd, r, radius)
	}
	// The closest point of the vertical segment to the other player.
	cy := math.Max(y0, math.Min(yEnd, oy))
	if d := math.Hypot(x0-ox, cy-oy); !(d > ghostCaptureDistance) {
		return fmt.Errorf("the sender's path passes %.1f from the other player, within capture distance (%g)", d, ghostCaptureDistance)
	}
	return nil
}

// checkGhostFlags refuses, before any account is created, a ghosttag run
// that is not local or whose schedule cannot fit the half. The arena check
// here assumes the server's default spawn and radius; waitActive repeats it
// against the room's own.
func checkGhostFlags(c *config) error {
	for _, u := range []string{c.api, c.ws, c.metrics} {
		if u != "" && !isLocalURL(u) {
			return fmt.Errorf("-mode %s runs against a local stack only, never production (design doc section 9, Note B); %q is not local", modeGhost, u)
		}
	}
	// The latest the first message can go out: the lead-in plus the largest
	// stagger. The run checks again with the half's real start.
	if err := checkGhostSchedule(leadIn+c.interval/2, c.samples, c.interval); err != nil {
		return err
	}
	return checkGhostArena(ghostDefaultSpawn, 0, -ghostDefaultSpawn, 0, ghostDefaultR0, c.samples)
}

// isBotRoom reports whether a MatchFound is a bot room for one of ours: a
// Ghost Tag room lists only its human players, so a room of one is a room
// with the server's bot.
func isBotRoom(players []string, ours map[string]*player) bool {
	if len(players) != 1 {
		return false
	}
	_, ok := ours[players[0]]
	return ok
}

// isLocalURL reports whether u points at this machine.
func isLocalURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	h := p.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// gameData is the data of a Game frame, as far as the bench reads it: pos
// (from, x, y) and snap (state, players), plus t for the rest.
type gameData struct {
	T       string       `json:"t"`
	From    string       `json:"from"`
	X       float64      `json:"x"`
	Y       float64      `json:"y"`
	State   string       `json:"state"`
	R       float64      `json:"r"`
	Players []snapPlayer `json:"players"`
}

type snapPlayer struct {
	ID   string  `json:"id"`
	Role string  `json:"role"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
}

// decodeGame decodes a Game frame's data. A coordinate the frame does not
// carry stays NaN, so it can never map to an index.
func decodeGame(raw []byte) (gameData, error) {
	g := gameData{X: math.NaN(), Y: math.NaN()}
	err := json.Unmarshal(raw, &g)
	return g, err
}

// gameNote is a Game frame other than pos, seen by one connection.
type gameNote struct {
	room string
	at   time.Duration // mono() when it was read
	data gameData
}

// ghostReceiver is the receiving side of one pair in ghosttag mode. The
// bookkeeping (duplicates, order, loss, lateness) is the relay receiver's;
// this adds the index recovery and the clipped count.
type ghostReceiver struct {
	*receiver
	x0, y0 float64
	// sentAt[k] is message k's send time (mono), 0 until it is sent. The
	// sender stores it before the write, so a frame can never arrive for an
	// index whose time is not there yet.
	sentAt []atomic.Int64

	cmu     sync.Mutex
	clipped int
}

func newGhostReceiver(room, from string, samples, warmup int, x0, y0 float64) *ghostReceiver {
	return &ghostReceiver{
		receiver: newReceiver(room, from, samples, warmup),
		x0:       x0, y0: y0,
		sentAt: make([]atomic.Int64, samples),
	}
}

// record stores one pos arrival. now was taken right after ReadMessage
// returned.
func (g *ghostReceiver) record(room string, d gameData, now time.Duration) {
	if room != g.room || d.From != g.from {
		g.mu.Lock()
		g.foreign++
		g.mu.Unlock()
		return
	}
	k, ok := ghostIndex(d.X, d.Y, g.x0, g.y0, len(g.sentAt))
	var sent int64
	if ok {
		sent = g.sentAt[k].Load()
	}
	if sent == 0 { // off the grid, out of range, or never sent: corrected
		g.cmu.Lock()
		g.clipped++
		g.cmu.Unlock()
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.storeLocked(k, now-time.Duration(sent))
}

func (g *ghostReceiver) clippedCount() int {
	g.cmu.Lock()
	defer g.cmu.Unlock()
	return g.clipped
}

// runGhostSender sends pos k at start + k*interval, at (x0, y0 + k*ghostStep),
// on runSender's absolute schedule. The send time is stored under k before
// the write.
func runGhostSender(ctx context.Context, p *pair, cfg *config, start time.Duration) {
	ws := p.sender.conn.ws
	g := p.ghost
	prefix := []byte(`{"action":"relay","room":` + strconv.Quote(p.room) + `,"data":{"t":"pos","seq":`)
	buf := make([]byte, 0, len(prefix)+128)
	for k := 0; k < cfg.samples; k++ {
		if !sleepUntil(ctx, start+time.Duration(k)*cfg.interval) {
			return
		}
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		t := mono()
		g.sentAt[k].Store(int64(t))
		buf = append(buf[:0], prefix...)
		buf = strconv.AppendInt(buf, int64(k), 10)
		buf = append(buf, `,"cts":`...)
		buf = strconv.AppendInt(buf, t.Milliseconds(), 10)
		buf = append(buf, `,"x":`...)
		buf = strconv.AppendFloat(buf, g.x0, 'f', -1, 64)
		buf = append(buf, `,"y":`...)
		buf = strconv.AppendFloat(buf, ghostY(g.y0, k), 'f', -1, 64)
		buf = append(buf, `,"vx":0,"vy":0}}`...)
		if err := ws.WriteMessage(websocket.TextMessage, buf); err != nil {
			p.sendErr = err
			return
		}
		p.sent = k + 1
	}
}

// errNotUnranked: the server took the ticket but did not confirm it as an
// unranked Ghost Tag ticket. Bench matches must never score.
var errNotUnranked = errors.New("the server did not confirm an unranked ghosttag ticket (bench matches must never score); is this server older than the ghosttag queue?")

// waitActive waits until every pair's room is in HALF_1_ACTIVE, takes the
// sender's spawn from that snap, checks the path and the schedule against
// it, and installs the pair's receiver.
func (b *bench) waitActive(ctx context.Context) error {
	n := len(b.pairs)
	activeAt := make([]time.Duration, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, p := range b.pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, at, err := p.sender.conn.waitState(ctx, p.room, stateActive, ghostActiveTimeout)
			if err != nil {
				errs[i] = err
				return
			}
			var me, other *snapPlayer
			for j := range snap.Players {
				switch snap.Players[j].ID {
				case p.sender.id:
					me = &snap.Players[j]
				case p.receiver.id:
					other = &snap.Players[j]
				}
			}
			if me == nil || other == nil || len(snap.Players) != 2 {
				errs[i] = fmt.Errorf("pair %d: the %s snap of room %s does not list exactly this pair's two players", p.idx, stateActive, p.room)
				return
			}
			if err := checkGhostArena(me.X, me.Y, other.X, other.Y, snap.R, b.cfg.samples); err != nil {
				errs[i] = fmt.Errorf("pair %d: %w", p.idx, err)
				return
			}
			activeAt[i] = at
			p.ghost = newGhostReceiver(p.room, p.sender.id, b.cfg.samples, b.cfg.warmup, me.X, me.Y)
			p.ghostRole = me.Role
			b.logf("room %s: %s is %s from (%g, %g)", p.room, p.sender.username, me.Role, me.X, me.Y)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	// measure starts pair i at base + i*stagger, base = mono() + leadIn.
	base := mono() + leadIn
	stagger := b.cfg.interval / 2 / time.Duration(n)
	for i, p := range b.pairs {
		first := base + time.Duration(i)*stagger - activeAt[i]
		if err := checkGhostSchedule(first, b.cfg.samples, b.cfg.interval); err != nil {
			return fmt.Errorf("pair %d (room %s, active %s before the first send): %w", p.idx, p.room, first.Round(time.Millisecond), err)
		}
	}
	for _, p := range b.pairs {
		p.recv = p.ghost.receiver
		p.receiver.conn.ghost.Store(p.ghost)
		// What the room said before its half began (the WAITING and
		// COUNTDOWN_1 snaps, which waitState read only on the sender's
		// connection) is not a sign of anything; roomEvents reports only
		// what arrives from here on.
		p.sender.conn.drainGames()
		p.receiver.conn.drainGames()
	}
	log.Printf("all %d room(s) in %s", n, stateActive)
	return nil
}

// roomEvents drains what the pair's two connections were told besides pos
// after the half started. In a clean run that is nothing: a clamp means the
// sender was corrected, anything else (a half_end, a snap of another state)
// that the room left the active half during the measurement.
func (p *pair) roomEvents() []string {
	var out []string
	for _, pl := range []*player{p.sender, p.receiver} {
		for _, n := range pl.conn.drainGames() {
			if n.room != p.room || (n.data.T == "snap" && n.data.State == stateActive) {
				continue
			}
			what := n.data.T
			if n.data.State != "" {
				what += " " + n.data.State
			}
			out = append(out, fmt.Sprintf("%s got %s", pl.username, what))
		}
	}
	return out
}
