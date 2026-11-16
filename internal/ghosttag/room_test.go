package ghosttag

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sent is one message the room handed its sender.
type sent struct {
	how  string // "all", "except" or "to"
	who  string // the excluded player for "except", the recipient for "to"
	raw  string
	body map[string]any
}

func (s sent) typ() string { t, _ := s.body["t"].(string); return t }

// fakeOut records what the room sends. Room tests run on one goroutine; it
// has no lock.
type fakeOut struct{ msgs []sent }

func (f *fakeOut) record(how, who string, payload []byte) {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		panic(fmt.Sprintf("the room sent invalid JSON %q: %v", payload, err))
	}
	f.msgs = append(f.msgs, sent{how: how, who: who, raw: string(payload), body: body})
}
func (f *fakeOut) Broadcast(p []byte) { f.record("all", "", p) }
func (f *fakeOut) BroadcastExcept(id string, p []byte, _ time.Time) {
	f.record("except", id, p)
}
func (f *fakeOut) SendTo(id string, p []byte) { f.record("to", id, p) }

// ofType returns the recorded messages of type t, and forgets nothing.
func (f *fakeOut) ofType(t string) []sent {
	var out []sent
	for _, m := range f.msgs {
		if m.typ() == t {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeOut) lastSnap(t *testing.T) map[string]any {
	t.Helper()
	snaps := f.ofType("snap")
	require.NotEmpty(t, snaps)
	return snaps[len(snaps)-1].body
}

// counts is a single-goroutine Observer.
type counts map[string]int

func (c counts) Count(e string) { c[e]++ }

// harness drives one room with a fake clock: t0 is when the room was made.
type harness struct {
	t   *testing.T
	cfg Config
	r   *room
	out *fakeOut
	obs counts
	seq map[string]int64
}

func newHarness(t *testing.T, cfg Config, roomID string, humans []string, botID string) *harness {
	t.Helper()
	require.NoError(t, cfg.Validate())
	h := &harness{t: t, cfg: cfg, out: &fakeOut{}, obs: counts{}, seq: map[string]int64{}}
	h.r = newRoom(&h.cfg, roomID, humans, botID, h.out, h.obs, rand.New(rand.NewPCG(1, 2)), t0)
	return h
}

func twoPlayers(t *testing.T) *harness {
	return newHarness(t, DefaultConfig(), "room-1", []string{"a", "b"}, "")
}

func at(d time.Duration) time.Time { return t0.Add(d) }

// started connects everyone at 1 s and runs the countdown out: HALF_1 is
// active from 3 s.
func (h *harness) started() time.Time {
	h.r.setPresence(at(time.Second), "a", true)
	h.r.setPresence(at(time.Second), "b", true)
	require.Equal(h.t, stateCountdown, h.r.state)
	start := at(time.Second + h.cfg.TCountdown)
	h.r.tick(start)
	require.Equal(h.t, stateHalf1, h.r.state)
	return start
}

func (h *harness) hunter() *player { return h.r.withRole(roleHunter) }
func (h *harness) runner() *player { return h.r.withRole(roleRunner) }

// send posts the next packet of p, asking to be at (x, y).
func (h *harness) send(now time.Time, p *player, x, y float64) {
	h.seq[p.id]++
	h.r.handlePos(now, p.id, posPacket{seq: h.seq[p.id], raw: vec{x, y}}, now)
}

// walk sends n packets of p, 40 ms apart from start, each asking for
// target; validation turns them into a walk at top speed. It returns the
// time of the last packet.
func (h *harness) walk(start time.Time, p *player, target vec, n int) time.Time {
	now := start
	for i := 0; i < n && !h.r.finished(); i++ {
		now = start.Add(time.Duration(i) * 40 * time.Millisecond)
		h.send(now, p, target.X, target.Y)
	}
	return now
}

// TestRoomWaitsForEveryoneThenCountsDown: WAITING ignores positions and
// starts the countdown only when every human is connected; the countdown
// ends in HALF_1_ACTIVE with both players on their spawn points.
func TestRoomWaitsForEveryoneThenCountsDown(t *testing.T) {
	h := twoPlayers(t)
	h.r.setPresence(at(ms(100)), "a", true)
	assert.Equal(t, stateWaiting, h.r.state)
	assert.Equal(t, "WAITING", h.out.lastSnap(t)["state"])
	h.send(at(ms(200)), h.r.byID["a"], 50, 50)
	assert.Empty(t, h.out.ofType("pos"), "positions are ignored in WAITING")

	h.r.setPresence(at(ms(300)), "b", true)
	snap := h.out.lastSnap(t)
	assert.Equal(t, "COUNTDOWN_1", snap["state"])
	assert.Equal(t, 2000.0, snap["rem_ms"])
	assert.Equal(t, 1, h.obs[EventMatchStarted])

	h.r.tick(at(ms(2299)))
	assert.Equal(t, stateCountdown, h.r.state)
	h.r.tick(at(ms(2300)))
	assert.Equal(t, stateHalf1, h.r.state)
	snap = h.out.lastSnap(t)
	assert.Equal(t, 45000.0, snap["rem_ms"])
	assert.Equal(t, 500.0, snap["r"])
	assert.Equal(t, vec{-225, 0}, h.hunter().mot.pos)
	assert.Equal(t, vec{225, 0}, h.runner().mot.pos)
}

// TestFirstHunterComesFromTheRoomID: the same room ID always gives the same
// first hunter, whatever the room's own random source, and across room IDs
// both players get to hunt first.
func TestFirstHunterComesFromTheRoomID(t *testing.T) {
	firstOf := func(roomID string, seed uint64) string {
		h := newHarness(t, DefaultConfig(), roomID, []string{"a", "b"}, "")
		h.r.rng = rand.New(rand.NewPCG(seed, seed))
		h.r.setPresence(at(0), "a", true)
		h.r.setPresence(at(0), "b", true)
		return h.hunter().id
	}
	want := firstOf("room-42", 0)
	for seed := uint64(1); seed <= 20; seed++ {
		assert.Equal(t, want, firstOf("room-42", seed), "room-42, seed %d", seed)
	}
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		seen[firstOf(fmt.Sprintf("room-%d", i), 7)] = true
	}
	assert.Equal(t, map[string]bool{"a": true, "b": true}, seen)
}

// TestBotHuntsFirst: in a bot match the human is the runner in the first
// half, for any room ID.
func TestBotHuntsFirst(t *testing.T) {
	for i := 0; i < 16; i++ {
		h := newHarness(t, DefaultConfig(), fmt.Sprintf("bot-room-%d", i), []string{"human"}, "bot")
		h.r.setPresence(at(0), "human", true)
		require.Equal(t, stateCountdown, h.r.state)
		assert.Equal(t, "bot", h.hunter().id)
		assert.Equal(t, "human", h.runner().id)
	}
}

// TestRoleSwapResetsState: at the end of half 1 the roles swap, both
// players go back to their spawn points with no previous motion and empty
// rings; half 2 starts with the spawn as each ring's only sample, and its
// first packet is measured with DtInitial.
func TestRoleSwapResetsState(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	firstHunter, firstRunner := h.hunter(), h.runner()
	h.walk(start.Add(ms(40)), firstHunter, vec{0, 300}, 10)
	h.walk(start.Add(ms(60)), firstRunner, vec{300, 300}, 10)
	require.True(t, firstHunter.mot.hasPrev)

	end := start.Add(h.cfg.THalf)
	h.r.tick(end)

	require.Equal(t, stateRoleSwap, h.r.state)
	halfEnd := h.out.ofType("half_end")
	require.Len(t, halfEnd, 1)
	assert.Equal(t, "TIMEOUT", halfEnd[0].body["reason"])
	assert.Equal(t, 45000.0, halfEnd[0].body["dur_ms"])
	assert.Equal(t, firstHunter.id, halfEnd[0].body["hunter"])
	assert.Equal(t, roleRunner, firstHunter.role)
	assert.Equal(t, roleHunter, firstRunner.role)
	for _, p := range h.r.players {
		assert.False(t, p.mot.hasPrev, p.id)
		assert.Equal(t, 0, p.hist.len(), p.id)
	}
	assert.Equal(t, vec{-225, 0}, firstRunner.mot.pos, "the new hunter spawns on the hunter's point")
	assert.Equal(t, vec{225, 0}, firstHunter.mot.pos)
	assert.Equal(t, "ROLE_SWAP", h.out.lastSnap(t)["state"])

	half2 := end.Add(h.cfg.TRoleSwap)
	h.r.tick(half2)
	require.Equal(t, stateHalf2, h.r.state)
	assert.Equal(t, time.Duration(0), h.r.elapsed(half2))
	for _, p := range h.r.players {
		require.Equal(t, 1, p.hist.len(), p.id)
		assert.Equal(t, sample{ts: half2, p: p.mot.pos}, p.hist.at(0))
	}
	// The first packet of the half, 1 s after it began, moves the new
	// hunter at most 225·0.04 + the full slack of a new half, 25 = 34 units.
	h.send(half2.Add(time.Second), firstRunner, 0, 0)
	assert.InDelta(t, -225+34, firstRunner.mot.pos.X, 1e-9)
}

// TestReconnectResetsMotion: a player who drops and comes back within the
// grace keeps their position (P_anchor) but not their motion, and so does
// the player who stayed: the first packet of each after the return moves at
// most one DtInitial step, although more than 4 s passed since their last.
// The slack both spent walking before the drop is not refilled by the
// return: it stays empty, so the step is V·DtInitial and no more (9 and
// 10.4, not 34 and 35.4).
func TestReconnectResetsMotion(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	hunter, runner := h.hunter(), h.runner()
	last := h.walk(start.Add(ms(40)), hunter, vec{-225, 200}, 5)
	h.walk(start.Add(ms(60)), runner, vec{225, -200}, 5)
	anchorH, anchorR := hunter.mot.pos, runner.mot.pos
	require.Zero(t, hunter.mot.slack)
	require.Zero(t, runner.mot.slack)

	h.r.setPresence(last.Add(ms(10)), runner.id, false)
	require.Equal(t, stateGrace, h.r.state)
	back := last.Add(4 * time.Second)
	h.r.setPresence(back, runner.id, true)
	require.Equal(t, stateHalf1, h.r.state)
	assert.Equal(t, anchorH, hunter.mot.pos)
	assert.Equal(t, anchorR, runner.mot.pos)
	assert.False(t, hunter.mot.hasPrev)
	assert.False(t, runner.mot.hasPrev)
	assert.Zero(t, hunter.mot.slack, "not refilled by the return")
	assert.Zero(t, runner.mot.slack)

	h.send(back.Add(ms(500)), hunter, anchorH.X, anchorH.Y+500)
	h.send(back.Add(ms(500)), runner, anchorR.X, anchorR.Y-500)
	assert.InDelta(t, 225*0.04, hunter.mot.pos.dist(anchorH), 1e-9)
	assert.InDelta(t, 260*0.04, runner.mot.pos.dist(anchorR), 1e-9)
}

// TestGracePausesTheClockCaptureAndPressure: in DISCONNECT_GRACE the half
// clock does not run (the snap's rem_ms is frozen and the half does not end
// on time), positions are not taken so nobody can be caught, and no
// pressure is sampled. After the return the half resumes from where it
// stopped.
func TestGracePausesTheClockCaptureAndPressure(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	hunter, runner := h.hunter(), h.runner()

	gone := start.Add(10 * time.Second)
	h.r.tick(gone)
	samples := hunter.tally.pressureN
	require.Equal(t, 50, samples, "10 s of samples every 200 ms")
	h.r.setPresence(gone, runner.id, false)
	require.Equal(t, stateGrace, h.r.state)
	assert.Equal(t, 35000.0, h.out.lastSnap(t)["rem_ms"])

	// The hunter walks onto the absent runner's anchor: nothing happens.
	posBefore := len(h.out.ofType("pos"))
	h.walk(gone.Add(ms(40)), hunter, runner.mot.pos, 100)
	assert.Equal(t, vec{-225, 0}, hunter.mot.pos)
	assert.Len(t, h.out.ofType("pos"), posBefore)
	assert.Empty(t, h.out.ofType("half_end"))

	for d := ms(40); d < h.cfg.TGrace; d += ms(40) {
		h.r.tick(gone.Add(d))
	}
	assert.Equal(t, samples, hunter.tally.pressureN)
	assert.Equal(t, 0, runner.tally.pressureN)
	h.r.setPresence(gone.Add(4900*time.Millisecond), runner.id, true)
	require.Equal(t, stateHalf1, h.r.state)
	snap := h.out.lastSnap(t)
	assert.Equal(t, "HALF_1_ACTIVE", snap["state"])
	assert.Equal(t, 35000.0, snap["rem_ms"])

	// 35 s of half are left from the return, not from the drop.
	back := gone.Add(4900 * time.Millisecond)
	h.r.tick(back.Add(35*time.Second - time.Millisecond))
	assert.Equal(t, stateHalf1, h.r.state)
	h.r.tick(back.Add(35 * time.Second))
	assert.Equal(t, stateRoleSwap, h.r.state)
	assert.Equal(t, 225, hunter.tally.pressureN, "45 s of samples, none in the grace")
}

// TestPlayerGoneWhenTheCountdownEnds: the half does not start; the room goes
// to DISCONNECT_GRACE, and when the player is back the half starts from its
// beginning.
func TestPlayerGoneWhenTheCountdownEnds(t *testing.T) {
	h := twoPlayers(t)
	h.r.setPresence(at(0), "a", true)
	h.r.setPresence(at(0), "b", true)
	h.r.setPresence(at(ms(500)), "b", false)
	require.Equal(t, stateCountdown, h.r.state)

	h.r.tick(at(h.cfg.TCountdown))

	assert.Equal(t, stateGrace, h.r.state)
	for _, s := range h.out.ofType("snap") {
		assert.NotEqual(t, "HALF_1_ACTIVE", s.body["state"])
	}
	back := at(h.cfg.TCountdown + 3*time.Second)
	h.r.setPresence(back, "b", true)
	require.Equal(t, stateHalf1, h.r.state)
	assert.Equal(t, 45000.0, h.out.lastSnap(t)["rem_ms"])
	assert.Equal(t, time.Duration(0), h.r.elapsed(back))
}

// TestPlayerGoneWhenTheRoleSwapEnds: the same at the start of half 2.
func TestPlayerGoneWhenTheRoleSwapEnds(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	end := start.Add(h.cfg.THalf)
	h.r.tick(end)
	require.Equal(t, stateRoleSwap, h.r.state)
	h.r.setPresence(end.Add(time.Second), "a", false)

	h.r.tick(end.Add(h.cfg.TRoleSwap))

	assert.Equal(t, stateGrace, h.r.state)
	for _, s := range h.out.ofType("snap") {
		assert.NotEqual(t, "HALF_2_ACTIVE", s.body["state"])
	}
	h.r.setPresence(end.Add(h.cfg.TRoleSwap+time.Second), "a", true)
	assert.Equal(t, stateHalf2, h.r.state)
}

// TestJoinTimeoutCancels: a player who never joins within T_join cancels the
// match: no forfeit, no winner, EventJoinTimeout.
func TestJoinTimeoutCancels(t *testing.T) {
	h := twoPlayers(t)
	h.r.setPresence(at(time.Second), "a", true)
	h.r.tick(at(h.cfg.TJoin - time.Millisecond))
	require.Equal(t, stateWaiting, h.r.state)

	h.r.tick(at(h.cfg.TJoin))

	require.True(t, h.r.finished())
	assert.Equal(t, outcomeCancelled, h.r.result.outcome)
	assert.Empty(t, h.r.result.leaver)
	assert.Equal(t, 1, h.obs[EventJoinTimeout])
	assert.Zero(t, h.obs[EventForfeit])
	assert.Equal(t, verdict{}, decide(&h.cfg, h.r.result))
}

// TestBothGoneAtGraceExpiryIsVoid: no forfeit, no winner,
// EventBothDisconnected.
func TestBothGoneAtGraceExpiryIsVoid(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	h.r.setPresence(start.Add(time.Second), "a", false)
	h.r.setPresence(start.Add(2*time.Second), "b", false)

	h.r.tick(start.Add(time.Second + h.cfg.TGrace))

	require.True(t, h.r.finished())
	assert.Equal(t, outcomeVoid, h.r.result.outcome)
	assert.Empty(t, h.r.result.leaver)
	assert.Equal(t, 1, h.obs[EventBothDisconnected])
	assert.Zero(t, h.obs[EventForfeit])
	assert.False(t, h.r.result.settles())
}

// TestOneGoneAtGraceExpiryForfeits: the grace runs from the drop, and the
// player still gone when it ends forfeits.
func TestOneGoneAtGraceExpiryForfeits(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	h.r.setPresence(start.Add(time.Second), "b", false)
	h.r.tick(start.Add(time.Second + h.cfg.TGrace - time.Millisecond))
	require.Equal(t, stateGrace, h.r.state)

	h.r.tick(start.Add(time.Second + h.cfg.TGrace))

	require.True(t, h.r.finished())
	assert.Equal(t, outcomeForfeit, h.r.result.outcome)
	assert.Equal(t, "b", h.r.result.leaver)
	assert.Equal(t, 1, h.obs[EventForfeit])
	assert.Zero(t, h.obs[EventBothDisconnected])
	assert.Equal(t, "a", decide(&h.cfg, h.r.result).winner)
}

// TestPressureGoesToTheHunterOnly: every 200 ms the hunter-runner distance
// is added to the total of whoever hunts in that half, and not to the
// runner's. At the spawn points the distance is 450.
func TestPressureGoesToTheHunterOnly(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	first, second := h.hunter(), h.runner()

	h.r.tick(start.Add(time.Second))
	assert.Equal(t, 5, first.tally.pressureN)
	assert.InDelta(t, 5*450, first.tally.pressureSum, 1e-9)
	assert.Zero(t, second.tally.pressureN)
	assert.Zero(t, second.tally.pressureSum)

	h.r.tick(start.Add(h.cfg.THalf))
	h.r.tick(start.Add(h.cfg.THalf + h.cfg.TRoleSwap + 2*time.Second))
	require.Equal(t, stateHalf2, h.r.state)
	assert.Equal(t, 225, first.tally.pressureN, "nothing more once they run")
	assert.Equal(t, 10, second.tally.pressureN)
	assert.InDelta(t, 10*450, second.tally.pressureSum, 1e-9)
}

// capture runs a capture in half 1 where mover walks into the other player,
// who keeps sending packets where they stand. It returns the half_end.
func captureBy(t *testing.T, moverIsHunter bool) (*harness, sent) {
	h := twoPlayers(t)
	start := h.started()
	mover, still := h.runner(), h.hunter()
	if moverIsHunter {
		mover, still = h.hunter(), h.runner()
	}
	now := start
	for i := 0; i < 80 && !h.r.finished() && h.r.state == stateHalf1; i++ {
		now = now.Add(ms(20))
		h.send(now, still, still.mot.pos.X, still.mot.pos.Y)
		now = now.Add(ms(20))
		h.send(now, mover, still.mot.pos.X, still.mot.pos.Y)
	}
	ends := h.out.ofType("half_end")
	require.Len(t, ends, 1, "a capture")
	// The packet that decided it is the mover's: the one that brought the
	// two within D_capture, not the other player's next one.
	var last sent
	for _, m := range h.out.msgs {
		if m.typ() == "half_end" {
			break
		}
		if m.typ() == "pos" {
			last = m
		}
	}
	require.Equal(t, mover.id, last.body["from"], "the capturing packet")
	return h, ends[0]
}

// TestCaptureIsSymmetric: the half ends in a capture whichever player walks
// into the other, so whose packet arrives last does not matter; the
// capture is always the hunter's, timed in half time.
func TestCaptureIsSymmetric(t *testing.T) {
	for _, moverIsHunter := range []bool{true, false} {
		h, end := captureBy(t, moverIsHunter)
		assert.Equal(t, "CAPTURE", end.body["reason"], "mover is hunter: %v", moverIsHunter)
		assert.Equal(t, stateRoleSwap, h.r.state)
		first := h.r.byID[end.body["hunter"].(string)]
		assert.True(t, first.tally.captured)
		assert.Equal(t, float64(first.tally.captureMs), end.body["dur_ms"])
		assert.Positive(t, first.tally.captureMs)
		assert.False(t, h.r.byID[end.body["runner"].(string)].tally.captured)
	}
}

// TestSeqFilter: a packet with a seq not above the last accepted one is
// dropped; a player whose presence is notified while connected (a new
// connection: a new client instance) may start again from a low seq.
func TestSeqFilter(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	p := h.hunter()
	h.r.handlePos(start.Add(ms(40)), p.id, posPacket{seq: 10, raw: vec{-215, 0}}, time.Time{})
	h.r.handlePos(start.Add(ms(80)), p.id, posPacket{seq: 10, raw: vec{-205, 0}}, time.Time{})
	h.r.handlePos(start.Add(ms(120)), p.id, posPacket{seq: 9, raw: vec{-205, 0}}, time.Time{})
	assert.Equal(t, vec{-215, 0}, p.mot.pos)

	h.r.setPresence(start.Add(ms(150)), p.id, true) // a second tab, or a reconnect
	h.r.handlePos(start.Add(ms(160)), p.id, posPacket{seq: 1, raw: vec{-205, 0}}, time.Time{})
	assert.InDelta(t, -205, p.mot.pos.X, 1e-9)
}

// TestPresenceSnap: a change goes to everyone; a notification that changes
// nothing sends the state to that player alone.
func TestPresenceSnap(t *testing.T) {
	h := twoPlayers(t)
	h.r.setPresence(at(0), "a", true)
	require.Equal(t, "all", h.out.msgs[len(h.out.msgs)-1].how)
	h.r.setPresence(at(ms(10)), "a", true)
	last := h.out.msgs[len(h.out.msgs)-1]
	assert.Equal(t, "to", last.how)
	assert.Equal(t, "a", last.who)
	assert.Equal(t, "snap", last.typ())
}

// TestSilentPlayerIsKeptInTheArena: a runner who reaches the edge and stops
// sending is moved in as R(t) shrinks, so the hunter can still reach them;
// the correction is broadcast and counted as a clamp.
func TestSilentPlayerIsKeptInTheArena(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	runner := h.runner()
	h.walk(start.Add(ms(40)), runner, vec{499, 0}, 60)
	require.InDelta(t, 499, runner.mot.pos.X, 1e-9)

	late := start.Add(40 * time.Second) // R = 500 − 220·(10/15)
	h.r.tick(late)

	assert.InDelta(t, h.r.radius(late), runner.mot.pos.norm(), 1e-9)
	assert.Less(t, runner.mot.pos.norm(), 354.0)
	last, _ := runner.hist.last()
	assert.Equal(t, late, last.ts)
	assert.Positive(t, h.obs[EventClamp])
}

// TestClampMessagesAreThrottled: every correction counts, but a player is
// told at most once per ClampInterval.
func TestClampMessagesAreThrottled(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	p := h.hunter()
	for i := 1; i <= 10; i++ {
		h.send(start.Add(time.Duration(i)*ms(40)), p, 400, 0) // always too far
	}
	assert.Equal(t, 10, h.obs[EventClamp])
	clamps := h.out.ofType("clamp")
	require.Len(t, clamps, 1, "packets 1-10 span 360 ms")
	assert.Equal(t, "to", clamps[0].how)
	assert.Equal(t, p.id, clamps[0].who)
	h.send(start.Add(ms(440)), p, 400, 0)
	assert.Len(t, h.out.ofType("clamp"), 2)
}

// TestOutboundPosCarriesNoClientClaim: the broadcast pos goes to the others
// only, with the server's identity of the sender, the server's time, the
// validated position, no seq or cts, and the velocity hint bounded.
func TestOutboundPosCarriesNoClientClaim(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	p := h.hunter()
	now := start.Add(ms(40))
	h.r.handlePos(now, p.id, posPacket{seq: 1, raw: vec{0, 0}, vx: 9e300, vy: 0}, now)

	pos := h.out.ofType("pos")
	require.Len(t, pos, 1)
	assert.Equal(t, "except", pos[0].how)
	assert.Equal(t, p.id, pos[0].who)
	assert.Equal(t, map[string]any{
		"t": "pos", "from": p.id, "sts": float64(h.r.sts(now)),
		"x": -191.0, "y": 0.0, "vx": 225.0, "vy": 0.0,
	}, pos[0].body)
}

// TestRoleOrderIsCounted: a completed match between humans counts whether
// the first or the second hunter won; a forfeit and a bot match do not.
func TestRoleOrderIsCounted(t *testing.T) {
	// The first hunter catches in half 1; nobody catches in half 2.
	h, _ := captureBy(t, true)
	swap := h.r.since
	h.r.tick(swap.Add(h.cfg.TRoleSwap))
	h.r.tick(swap.Add(h.cfg.TRoleSwap + h.cfg.THalf))
	require.True(t, h.r.finished())
	assert.Equal(t, 1, h.obs[EventFirstHunterWon])
	assert.Zero(t, h.obs[EventSecondHunterWon]+h.obs[EventRolesDraw])

	// No captures, equal pressure: a draw.
	h = twoPlayers(t)
	start := h.started()
	h.r.tick(start.Add(h.cfg.THalf))
	h.r.tick(start.Add(h.cfg.THalf + h.cfg.TRoleSwap))
	h.r.tick(start.Add(2*h.cfg.THalf + h.cfg.TRoleSwap))
	require.True(t, h.r.finished())
	assert.Equal(t, 1, h.obs[EventRolesDraw])

	// A forfeit counts nothing.
	h = twoPlayers(t)
	start = h.started()
	h.r.setPresence(start, "a", false)
	h.r.tick(start.Add(h.cfg.TGrace))
	require.True(t, h.r.finished())
	assert.Zero(t, h.obs[EventFirstHunterWon]+h.obs[EventSecondHunterWon]+h.obs[EventRolesDraw])

	// Nor does a bot match played to the end.
	h = newHarness(t, DefaultConfig(), "room-b", []string{"human"}, "bot")
	h.r.setPresence(at(0), "human", true)
	h.r.tick(at(h.cfg.TCountdown))
	h.r.tick(at(h.cfg.TCountdown + h.cfg.THalf))
	h.r.tick(at(h.cfg.TCountdown + h.cfg.THalf + h.cfg.TRoleSwap))
	h.r.tick(at(h.cfg.TCountdown + 2*h.cfg.THalf + h.cfg.TRoleSwap))
	require.True(t, h.r.finished())
	assert.Equal(t, outcomeCompleted, h.r.result.outcome)
	assert.Zero(t, h.obs[EventFirstHunterWon]+h.obs[EventSecondHunterWon]+h.obs[EventRolesDraw])
}
