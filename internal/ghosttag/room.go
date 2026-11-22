package ghosttag

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"time"
)

// Events the room reports besides those of ports.go.
const (
	// EventMatchStarted: every human joined and the countdown began.
	EventMatchStarted = "match_started"
	// EventMatchCompleted: both halves were played. With EventMatchStarted
	// it gives the completion rate of section 5.
	EventMatchCompleted = "match_completed"
	// EventForfeit: a player did not come back within TGrace.
	EventForfeit = "forfeit"
	// EventBotClamp: the validation pipeline corrected a bot's step. It is
	// kept apart from EventClamp so the human clamp rate stays a signal of
	// cheating or bad networks, and this one a live test of the bot.
	EventBotClamp = "bot_clamp"
	// EventFirstHunterWon, EventSecondHunterWon and EventRolesDraw: who won a
	// human-vs-human match decided by play over both halves, by the half
	// they hunted, so the owner can see whether the role order gives an
	// advantage (the second hunter knows the time to beat). Counted once per
	// such match; never for a forfeit, a void, a cancellation or a bot
	// match.
	EventFirstHunterWon  = "first_hunter_won"
	EventSecondHunterWon = "second_hunter_won"
	EventRolesDraw       = "roles_draw"
	// EventInvalidPayload: an inbound payload was malformed, of an unknown
	// type, from a player not in the room, or a non-finite position.
	EventInvalidPayload = "invalid_payload"
)

// state is the room state of section 7.
type state uint8

const (
	stateWaiting state = iota
	stateCountdown
	stateHalf1
	stateRoleSwap
	stateHalf2
	stateGrace
	stateFinished
)

// String is the state as snap spells it.
func (s state) String() string {
	return [...]string{
		stateWaiting:   "WAITING",
		stateCountdown: "COUNTDOWN_1",
		stateHalf1:     "HALF_1_ACTIVE",
		stateRoleSwap:  "ROLE_SWAP",
		stateHalf2:     "HALF_2_ACTIVE",
		stateGrace:     "DISCONNECT_GRACE",
		stateFinished:  "FINISHED",
	}[s]
}

// sender is the part of Transport the room uses. The room cannot ask who is
// connected: the session asks Transport.Connected and tells the room.
type sender interface {
	Broadcast(payload []byte)
	BroadcastExcept(playerID string, payload []byte, arrived time.Time)
	SendTo(playerID string, payload []byte)
}

// player is one player's state in the room (section 12: a list of players
// with their own role, not Player1 and Player2).
type player struct {
	id     string
	quoted []byte // id as a JSON string, for the outbound pos
	bot    bool
	// name is the display name snap shows: BotName for the bot, the one
	// the session last learned from the transport for a human, "" before.
	name string

	role      role
	connected bool // as the session last learned it; a bot always is

	mot  motion
	hist ring

	// seq is the newest accepted packet's seq. The filter restarts
	// (hasSeq = false) whenever the player's presence is notified while
	// they are connected: a new connection is a new client instance that
	// may count from zero again.
	seq    int64
	hasSeq bool

	lastClamp time.Time // when the last clamp message went to this player

	// deadline is when this player forfeits if still gone, zero when the
	// room is not waiting for them (section 2: per player, not per room).
	// expired says it passed: the player has forfeited, and coming back
	// does not undo it.
	deadline time.Time
	expired  bool
	// graceUsed is how long the room has waited for this player over the
	// match, from the start of each deadline to the return; graceFrom is
	// when the current wait began. A wait that ends in expiry is not
	// added: the player has forfeited and their usage no longer matters.
	graceUsed time.Duration
	graceFrom time.Time

	tally tally
}

// room is the state machine of section 7. It belongs to one goroutine (the
// session's loop) and has no lock. Every entry point takes the time as an
// argument and the room never reads the clock, so a test drives it with a
// fake one.
type room struct {
	cfg     *Config
	roomID  string
	out     sender
	obs     Observer
	turnout Turnout
	rng     *rand.Rand // the bot's; the roles do not come from it
	created time.Time  // sts counts from here; T_join too

	players []*player // the humans in Spec order, then the bot
	byID    map[string]*player

	// bot is the bot player, nil without one. Its two brains get their
	// inputs from stepBot and from nowhere else (bot.go).
	bot       *player
	botRunner botRunner
	botHunter botHunter

	state state
	since time.Time // when the current state began
	half  int       // the half being played, or the next one to be

	// The half clock counts active time only: played is the time before the
	// last (re)start at resumed. DISCONNECT_GRACE stops it (section 2).
	played  time.Duration
	resumed time.Time
	// nextSample is the half time of the next pressure sample.
	nextSample time.Duration
	// halfStarted says, in DISCONNECT_GRACE, whether the half it interrupts
	// had begun; it had not when a countdown or role swap ended with a
	// player gone (section 2).
	halfStarted bool

	firstHunter string // who hunted in the first half

	result  matchResult
	endedAt time.Time
}

func newRoom(cfg *Config, roomID string, humans []string, botID string, out sender, obs Observer, rng *rand.Rand, created time.Time) *room {
	r := &room{
		cfg:     cfg,
		roomID:  roomID,
		out:     out,
		obs:     obs,
		turnout: nopTurnout{},
		rng:     rng,
		created: created,
		byID:    make(map[string]*player),
		state:   stateWaiting,
		since:   created,
		half:    1,
	}
	add := func(id string, bot bool) {
		p := &player{id: id, quoted: quoteID(id), bot: bot, connected: bot, hist: newRing(cfg.RingSize)}
		if bot {
			p.name = BotName
		}
		p.tally.id = id
		r.players = append(r.players, p)
		r.byID[id] = p
	}
	for _, id := range humans {
		add(id, false)
	}
	if botID != "" {
		add(botID, true)
		r.bot = r.byID[botID]
		r.botRunner = botRunner{cfg: cfg, rng: rng}
		r.botHunter = botHunter{cfg: cfg}
	}
	return r
}

// firstHunter picks, by index into players, who hunts in the first half
// (the owner's decision of 2026-11-16; the design document is silent):
//
//   - In a bot match the bot hunts first. As runner the newcomer never sees
//     the hunter, only its trail, so their first 45 seconds are the half in
//     which the bot looks most natural.
//   - Between humans it is a fair coin whose seed is a stable hash (FNV-1a)
//     of the room ID: the same room always gives the same order, so a log
//     line can be checked against it.
func firstHunter(roomID string, players []*player) int {
	for i, p := range players {
		if p.bot {
			return i
		}
	}
	seed := roomSeed(roomID)
	coin := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // G404: a game coin toss, not a secret
	return coin.IntN(len(players))
}

// roomSeed is the FNV-1a hash of a room ID.
func roomSeed(roomID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(roomID)) // a hash.Hash never returns an error
	return h.Sum64()
}

func (r *room) finished() bool { return r.state == stateFinished }

func (r *room) active() bool { return r.state == stateHalf1 || r.state == stateHalf2 }

// elapsed is the active time played in the current half at now.
func (r *room) elapsed(now time.Time) time.Duration {
	if r.active() {
		return r.played + now.Sub(r.resumed)
	}
	return r.played
}

func (r *room) radius(now time.Time) float64 { return arenaRadius(r.cfg, r.elapsed(now)) }

// sts is the ServerTs of section 6: milliseconds since the room was created.
func (r *room) sts(now time.Time) int64 { return now.Sub(r.created).Milliseconds() }

func (r *room) withRole(want role) *player {
	for _, p := range r.players {
		if p.role == want {
			return p
		}
	}
	return nil
}

func (r *room) opponent(p *player) *player {
	for _, q := range r.players {
		if q != p {
			return q
		}
	}
	return nil
}

// humanIDs are the human players, a new slice each call.
func (r *room) humanIDs() []string {
	ids := make([]string, 0, len(r.players))
	for _, p := range r.players {
		if !p.bot {
			ids = append(ids, p.id)
		}
	}
	return ids
}

func (r *room) allConnected() bool {
	for _, p := range r.players {
		if !p.connected {
			return false
		}
	}
	return true
}

// tick is the room's part of the session's one ticker: it runs every
// deadline that has passed, then whatever happens once per tick in a half.
func (r *room) tick(now time.Time) {
	r.advance(now)
	if !r.active() {
		return
	}
	r.stepBot(now)
	if !r.active() {
		return // the bot's step was a capture
	}
	r.keepInArena(now)
}

// stepBot moves the bot once: its brain picks a target from what it may
// know, and the target goes through the same pipeline as a human packet.
// The bot's ServerTs is the tick's time; it has no seq and no packet, so
// the transport gets the zero arrival time.
func (r *room) stepBot(now time.Time) {
	b := r.bot
	if b == nil {
		return
	}
	radius := r.radius(now)
	opp := r.opponent(b)
	var target vec
	if b.role == roleRunner {
		spawn := vec{-r.cfg.SpawnDistance, 0} // where every hunter starts
		target = r.botRunner.next(b.mot, now, radius, delayedSight(&opp.hist, now, r.cfg.BotSightDelay, spawn))
	} else {
		pos, vel := liveView(&opp.hist)
		target = r.botHunter.next(b.mot, now, radius, pos, vel)
	}
	var hint vec // the dead-reckoning hint: the bot's velocity in this step
	if dt := botDt(r.cfg, b.mot, now); dt > 0 {
		hint = target.sub(b.mot.pos).scale(1 / dt)
	}
	r.move(now, b, target, hint, time.Time{})
}

// advance runs every transition whose deadline is not after now, each at
// its own deadline rather than at now, so a late tick does not stretch the
// next state. Every entry point calls it first, so a packet or a
// presence change is never applied to a state that has already run out.
func (r *room) advance(now time.Time) {
	for {
		var end time.Time
		switch r.state {
		case stateWaiting:
			end = r.created.Add(r.cfg.TJoin)
		case stateCountdown:
			end = r.since.Add(r.cfg.TCountdown)
		case stateRoleSwap:
			end = r.since.Add(r.cfg.TRoleSwap)
		case stateHalf1, stateHalf2:
			r.samplePressure(now)
			end = r.resumed.Add(r.cfg.THalf - r.played)
		case stateGrace:
			p := r.nextDeadline()
			if p == nil {
				return
			}
			end = p.deadline
		default:
			return
		}
		if now.Before(end) {
			return
		}
		switch r.state {
		case stateWaiting:
			// Section 7: the game never started, so nobody forfeits; the
			// players who are there are told, unranked and with no winner.
			r.obs.Count(EventJoinTimeout)
			r.finish(end, outcomeCancelled, "")
		case stateCountdown, stateRoleSwap:
			r.startHalfOrGrace(end)
		case stateHalf1, stateHalf2:
			r.endHalf(end, reasonTimeout, int(r.cfg.THalf/time.Millisecond))
		case stateGrace:
			p := r.nextDeadline()
			p.expired, p.deadline = true, time.Time{}
			r.settleGrace(end)
		}
	}
}

// samplePressure takes the 200 ms samples of section 1.4 that fall due up
// to now, from the positions as they stand. Each adds the hunter-runner
// distance to the HUNTER's total only: added to both, the two totals of a
// match would always be equal and every match without a capture a draw.
func (r *room) samplePressure(now time.Time) {
	upTo := min(r.elapsed(now), r.cfg.THalf)
	hunter, runner := r.withRole(roleHunter), r.withRole(roleRunner)
	for r.nextSample <= upTo {
		hunter.tally.pressureSum += hunter.mot.pos.dist(runner.mot.pos)
		hunter.tally.pressureN++
		r.nextSample += r.cfg.PressureInterval
	}
}

// enterCountdown starts the match: roles, spawn points, COUNTDOWN_1.
func (r *room) enterCountdown(now time.Time) {
	h := firstHunter(r.roomID, r.players)
	for i, p := range r.players {
		p.role = roleRunner
		if i == h {
			p.role = roleHunter
		}
	}
	r.firstHunter = r.players[h].id
	r.placeAtSpawn(now, false)
	r.state, r.since, r.half = stateCountdown, now, 1
	r.obs.Count(EventMatchStarted)
	r.turnout.Started(r.humanIDs(), now)
	r.broadcastSnap(now)
}

// placeAtSpawn is the half-start reset of section 1.2: both players on
// their symmetric spawn points, heading for the centre, with no previous
// motion, a full slack budget and empty rings. With seed, the spawn point is written into the
// ring as the half's first validated position, so a player who never sends
// a packet is still somewhere the capture check can find.
func (r *room) placeAtSpawn(at time.Time, seed bool) {
	for _, p := range r.players {
		p.mot = motion{pos: vec{r.cfg.SpawnDistance, 0}, theta: math.Pi, slack: r.cfg.SlackMax}
		if p.role == roleHunter {
			p.mot = motion{pos: vec{-r.cfg.SpawnDistance, 0}, theta: 0, slack: r.cfg.SlackMax}
		}
		p.hist.reset()
		if seed {
			p.hist.push(sample{ts: at, p: p.mot.pos})
		}
	}
}

// startHalfOrGrace ends a countdown or role swap. If a player is gone the
// half does not start: the room goes to DISCONNECT_GRACE, or the half would
// begin with a player who cannot move and no grace would ever run
// (section 2).
func (r *room) startHalfOrGrace(at time.Time) {
	if !r.allConnected() {
		r.enterGrace(at, false)
		return
	}
	r.startHalf(at)
}

func (r *room) startHalf(at time.Time) {
	r.placeAtSpawn(at, true)
	r.state = stateHalf1
	if r.half == 2 {
		r.state = stateHalf2
	}
	r.played, r.resumed, r.nextSample = 0, at, r.cfg.PressureInterval
	r.since = at
	r.broadcastSnap(at)
}

// endHalf closes the current half at at, by capture or time.
func (r *room) endHalf(at time.Time, reason string, durMs int) {
	hunter, runner := r.withRole(roleHunter), r.withRole(roleRunner)
	r.out.Broadcast(marshal(halfEndMsg{
		T: "half_end", Half: r.half, Reason: reason, DurMs: durMs,
		Hunter: hunter.id, Runner: runner.id,
	}))
	if r.half == 2 {
		r.played += at.Sub(r.resumed)
		r.obs.Count(EventMatchCompleted)
		r.turnout.Completed(r.humanIDs(), at)
		r.finish(at, outcomeCompleted, "")
		r.countRoleOrder()
		return
	}
	// ROLE_SWAP: swap, reset, spawn, clear the rings.
	hunter.role, runner.role = roleRunner, roleHunter
	r.half, r.played = 2, 0
	r.placeAtSpawn(at, false)
	r.state, r.since = stateRoleSwap, at
	r.broadcastSnap(at)
}

// capture ends the half: the hunter caught the runner at now.
func (r *room) capture(now time.Time) {
	h := r.withRole(roleHunter)
	h.tally.captured = true
	h.tally.captureMs = int(r.elapsed(now) / time.Millisecond)
	r.endHalf(now, reasonCapture, h.tally.captureMs)
}

// enterGrace stops the half clock, the capture check and the pressure
// sample, all of which run only in an active half, and starts the TGrace
// deadline of every player found gone (section 2: the deadline is per
// player and starts when the room starts waiting for them).
func (r *room) enterGrace(at time.Time, halfStarted bool) {
	if r.active() {
		r.played += at.Sub(r.resumed)
	}
	r.state, r.since, r.halfStarted = stateGrace, at, halfStarted
	for _, p := range r.players {
		if !p.connected {
			r.await(p, at)
		}
	}
	r.broadcastSnap(at)
}

// await starts the room waiting for p at at. Their deadline is TGrace away,
// or what is left of their TGraceTotal if that is less; with nothing left
// it is at itself, and advance forfeits them at that instant.
func (r *room) await(p *player, at time.Time) {
	left := max(0, min(r.cfg.TGrace, r.cfg.TGraceTotal-p.graceUsed))
	p.graceFrom, p.deadline = at, at.Add(left)
}

// returned ends the wait for p, who came back before their deadline, and
// charges it to their match total.
func (r *room) returned(p *player, at time.Time) {
	p.graceUsed += at.Sub(p.graceFrom)
	p.graceFrom, p.deadline = time.Time{}, time.Time{}
}

// nextDeadline is the gone player whose grace deadline comes first, or nil.
func (r *room) nextDeadline() *player {
	var next *player
	for _, p := range r.players {
		if !p.deadline.IsZero() && (next == nil || p.deadline.Before(next.deadline)) {
			next = p
		}
	}
	return next
}

// settleGrace decides what DISCONNECT_GRACE does next, after a return or an
// expiry at at. While anyone is gone and not yet expired, the room waits
// for them. Then: nobody expired, everyone is back, and the half resumes;
// every player expired, and the match is void; otherwise the expired player
// forfeits, at once if the opponent is there, or when the opponent comes
// back in time.
func (r *room) settleGrace(at time.Time) {
	var expired []*player
	for _, p := range r.players {
		switch {
		case p.expired:
			expired = append(expired, p)
		case !p.connected:
			return // still waiting for p
		}
	}
	switch {
	case len(expired) == 0:
		r.resume(at)
	case len(expired) == len(r.players): // a bot never expires
		r.obs.Count(EventBothDisconnected)
		r.finish(at, outcomeVoid, "")
	default:
		r.obs.Count(EventForfeit)
		r.finish(at, outcomeForfeit, expired[0].id)
	}
}

// resume leaves DISCONNECT_GRACE with everyone back. A half that had begun
// continues from where its clock stopped, after the return reset of
// section 1.2: positions stay (P_anchor) but there is no previous motion,
// so the first packet is measured from the anchor with DtInitial. It is
// applied to both players: the one who stayed was frozen for the grace too,
// and their next packet would otherwise be measured against a ServerTs
// seconds old (and clamped up to DtMax). The slack budget is kept as it was
// at the drop, not refilled: it fills with time only, so dropping and coming
// back buys no distance. A half that had not begun starts now.
func (r *room) resume(now time.Time) {
	if !r.halfStarted {
		r.startHalf(now)
		return
	}
	for _, p := range r.players {
		p.mot.hasPrev = false
	}
	r.state = stateHalf1
	if r.half == 2 {
		r.state = stateHalf2
	}
	r.resumed, r.since = now, now
	r.broadcastSnap(now)
}

// countRoleOrder reports whether the first or the second hunter won a
// completed match between humans.
func (r *room) countRoleOrder() {
	for _, p := range r.players {
		if p.bot {
			return
		}
	}
	switch v := decide(r.cfg, r.result); {
	case v.draw:
		r.obs.Count(EventRolesDraw)
	case v.winner == r.firstHunter:
		r.obs.Count(EventFirstHunterWon)
	default:
		r.obs.Count(EventSecondHunterWon)
	}
}

func (r *room) finish(at time.Time, o outcome, leaver string) {
	r.state, r.since, r.endedAt = stateFinished, at, at
	r.result = matchResult{outcome: o, leaver: leaver}
	for _, p := range r.players {
		r.result.players = append(r.result.players, p.tally)
	}
}

// setName records a human's display name. An empty name changes nothing:
// a name once learned stays until another replaces it. The next snap shows
// it.
func (r *room) setName(id, name string) {
	if p := r.byID[id]; p != nil && !p.bot && name != "" {
		p.name = name
	}
}

// names are the humans' names that the room knows, by ID.
func (r *room) names() map[string]string {
	out := make(map[string]string, len(r.players))
	for _, p := range r.players {
		if !p.bot && p.name != "" {
			out[p.id] = p.name
		}
	}
	return out
}

// setPresence applies what the session learned from Transport.Connected
// about one player. Everyone hears of a change; a player who is connected
// always gets the state, so a connection that just joined learns it even
// when nothing changed.
func (r *room) setPresence(now time.Time, id string, connected bool) {
	p := r.byID[id]
	if p == nil || p.bot {
		return
	}
	r.advance(now)
	if r.finished() {
		return
	}
	changed := p.connected != connected
	p.connected = connected
	if connected {
		p.hasSeq = false
	}
	before := r.state
	switch r.state {
	case stateWaiting:
		if r.allConnected() {
			r.enterCountdown(now)
		}
	case stateHalf1, stateHalf2:
		if !connected {
			r.enterGrace(now, true)
		}
	case stateGrace:
		// A player's own deadline starts at their drop and is cleared by
		// their return; an expired player's return changes nothing.
		switch {
		case p.expired:
		case !connected && changed:
			r.await(p, now)
		case connected && !p.deadline.IsZero():
			r.returned(p, now)
		}
		r.settleGrace(now)
	}
	if r.state == stateGrace {
		// A deadline set just now may already be due: a player whose total
		// is used up forfeits at the instant of the drop.
		r.advance(now)
	}
	switch {
	case r.state != before:
		// The transition told everyone.
	case changed:
		r.broadcastSnap(now)
	case connected:
		r.out.SendTo(id, r.snap(now))
	}
}

// handlePos is the inbound position pipeline of section 7 for a human.
// arrived is when the packet was read from the socket, passed on to the
// transport for timing; now is its ServerTs.
func (r *room) handlePos(now time.Time, id string, pk posPacket, arrived time.Time) {
	p := r.byID[id]
	if p == nil || p.bot {
		r.obs.Count(EventInvalidPayload)
		return
	}
	r.advance(now)
	if !r.active() {
		return // 1. only an active half takes positions
	}
	if p.hasSeq && pk.seq <= p.seq {
		return // 2. reordered or duplicated in the network
	}
	if r.move(now, p, pk.raw, vec{pk.vx, pk.vy}, arrived) {
		p.seq, p.hasSeq = pk.seq, true
	}
}

// step is the per-packet core shared by humans and the bot: validate,
// write the ring, and check for a capture against the opponent's window.
// It changes p's motion and ring and nothing else; acting on the result is
// the caller's.
func (r *room) step(now time.Time, p *player, raw vec) (clipped, captured bool, err error) {
	next, clipped, err := validateMove(r.cfg, p.mot, p.role, raw, now, r.radius(now))
	if err != nil {
		return false, false, err
	}
	p.mot = next
	p.hist.push(sample{ts: now, p: next.pos})
	// The symmetric check of section 1.3: whoever's packet this is, its
	// validated position against the other player's recent path.
	d, ok := minDistance(&r.opponent(p).hist, next.pos, now, r.cfg.CaptureWindow)
	return clipped, ok && d <= r.cfg.DCapture, nil
}

// move takes p to raw at now through the pipeline, tells the room, and ends
// the half on a capture. It reports whether the position was accepted.
func (r *room) move(now time.Time, p *player, raw, hint vec, arrived time.Time) bool {
	clipped, captured, err := r.step(now, p, raw)
	if err != nil {
		r.obs.Count(EventInvalidPayload)
		return false
	}
	if clipped {
		r.clamped(now, p)
	}
	r.out.BroadcastExcept(p.id, encodePos(p.quoted, r.sts(now), p.mot.pos, r.boundHint(p, hint)), arrived)
	if captured {
		r.capture(now)
	}
	return true
}

// clamped counts a correction and tells a human about it, at most once per
// ClampInterval (section 1.2), so a cheating client cannot map the limits by
// trial and error at packet rate.
func (r *room) clamped(now time.Time, p *player) {
	if p.bot {
		r.obs.Count(EventBotClamp)
		return
	}
	r.obs.Count(EventClamp)
	if !p.lastClamp.IsZero() && now.Sub(p.lastClamp) < r.cfg.ClampInterval {
		return
	}
	p.lastClamp = now
	r.out.SendTo(p.id, marshal(clampMsg{
		T: "clamp", X: round2(p.mot.pos.X), Y: round2(p.mot.pos.Y), Theta: round4(p.mot.theta),
	}))
}

// boundHint limits the client's velocity hint to the sender's top speed. It
// stays a hint for the other client's dead reckoning, but an absurd one
// (or one with a thousand digits) is not passed on.
func (r *room) boundHint(p *player, v vec) vec {
	if !isFinite(v.X) || !isFinite(v.Y) {
		return vec{}
	}
	limit := r.cfg.VHunter
	if p.role == roleRunner {
		limit = r.cfg.VRunner
	}
	if n := v.norm(); n > limit {
		return v.scale(limit / n)
	}
	return v
}

// keepInArena applies R(t) to a player who has stopped sending. Validation
// scales a position onto the arena only when a packet arrives; a runner
// standing silent at the edge as the arena shrinks would otherwise stay
// outside it, out of the hunter's reach, for the rest of the half. A player
// who sent within DtMax is left alone: their next packet is validated
// against R(t) anyway. The correction goes through the pipeline as a
// stationary step.
func (r *room) keepInArena(now time.Time) {
	radius := r.radius(now)
	for _, p := range r.players {
		if p.bot || (p.mot.hasPrev && now.Sub(p.mot.ts) < r.cfg.DtMax) || p.mot.pos.norm() <= radius {
			continue
		}
		r.move(now, p, p.mot.pos, vec{}, time.Time{})
		if !r.active() {
			return // the correction ended the half with a capture
		}
	}
}

func (r *room) broadcastSnap(now time.Time) { r.out.Broadcast(r.snap(now)) }

// snap is the room's state as section 6's snap message.
func (r *room) snap(now time.Time) []byte {
	var rem time.Duration
	switch r.state {
	case stateWaiting:
		rem = r.created.Add(r.cfg.TJoin).Sub(now)
	case stateCountdown:
		rem = r.since.Add(r.cfg.TCountdown).Sub(now)
	case stateRoleSwap:
		rem = r.since.Add(r.cfg.TRoleSwap).Sub(now)
	case stateHalf1, stateHalf2, stateGrace:
		rem = r.cfg.THalf - r.elapsed(now)
	}
	m := snapMsg{
		T:       "snap",
		State:   r.state.String(),
		Sts:     r.sts(now),
		RemMs:   max(0, rem).Milliseconds(),
		R:       round2(r.radius(now)),
		Players: make([]snapPlayer, 0, len(r.players)),
		Cfg:     clientConfigOf(r.cfg),
	}
	for _, p := range r.players {
		m.Players = append(m.Players, snapPlayer{
			ID: p.id, Name: p.name, Role: p.role.String(),
			X: round2(p.mot.pos.X), Y: round2(p.mot.pos.Y), Theta: round4(p.mot.theta),
			Connected: p.connected,
		})
	}
	return marshal(m)
}
