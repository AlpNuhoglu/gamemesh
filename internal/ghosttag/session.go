package ghosttag

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// Events the session reports besides those of ports.go and room.go.
const (
	// EventRanked: a match was settled and produced points. With
	// EventMatchStarted it gives ranked_match_ratio of section 5.
	EventRanked = "ranked"
	// EventRoomPanic: the room's goroutine panicked. The match was ended as
	// void and unranked and the store was not called (Session.Run).
	EventRoomPanic = "room_panic"
)

// DefaultBotID is the bot's player ID when Spec.BotID is empty.
const DefaultBotID = "bot"

// BotName is the bot's display name in snap (section 6: a fixed name).
const BotName = "Bot"

// Spec describes one room, from matchmaking's record.
type Spec struct {
	RoomID string
	// Players are the human players: one in a bot match, two otherwise.
	Players []string
	// Bot adds the bot as the second player.
	Bot bool
	// Unranked rooms are never settled (bench rooms, section 9).
	Unranked bool
	// BotID is the bot's player ID, DefaultBotID if empty. It must not be a
	// human's.
	BotID string
	// Rand is the bot's random source (its reaction delays). It is used by
	// the session's goroutine only and must not be shared. If nil, it is
	// seeded from the room ID, so a room replays the same. The first-half
	// roles never come from it (see firstHunter).
	Rand *rand.Rand
	// Logf, if set, receives the rare events worth a log line: a settle
	// failure with the full result, a cancellation, a panic. Never a
	// packet.
	Logf func(format string, args ...any)
}

// inboxItem is one payload Post handed the session.
type inboxItem struct {
	player  string
	data    []byte
	arrived time.Time
}

// Session runs one room: its goroutine (Run) owns the room, and Post,
// Notify and Done are the only methods other goroutines may call.
type Session struct {
	cfg   Config
	spec  Spec
	t     Transport
	store ScoreStore // nil: every match is unranked
	obs   Observer
	logf  func(format string, args ...any)

	inbox    chan inboxItem
	presence chan string
	done     chan struct{}
	started  atomic.Bool
	closing  atomic.Bool // the match is over; Post refuses quietly

	// Owned by Run's goroutine.
	room    *room
	created time.Time
	last    time.Time // the latest time the loop has used: ServerTs never goes back
	humans  []string
	all     []string // humans, then the bot
	ended   bool     // a match_end has been broadcast
}

// NewSession validates cfg and spec and builds the room. Nothing runs until
// Run is called. A nil ScoreStore makes every match unranked; a nil
// Observer counts nothing.
func NewSession(cfg Config, spec Spec, t Transport, s ScoreStore, o Observer) (*Session, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errors.New("ghosttag: nil Transport")
	}
	if spec.RoomID == "" {
		return nil, errors.New("ghosttag: empty room ID")
	}
	want := 2
	if spec.Bot {
		want = 1
		if spec.BotID == "" {
			spec.BotID = DefaultBotID
		}
	} else {
		spec.BotID = ""
	}
	if len(spec.Players) != want {
		return nil, fmt.Errorf("ghosttag: room %s: want %d human players, got %d", spec.RoomID, want, len(spec.Players))
	}
	seen := map[string]bool{spec.BotID: spec.Bot}
	for _, id := range spec.Players {
		if id == "" || seen[id] {
			return nil, fmt.Errorf("ghosttag: room %s: empty or repeated player ID %q", spec.RoomID, id)
		}
		seen[id] = true
	}
	if o == nil {
		o = nopObserver{}
	}
	if spec.Rand == nil {
		seed := roomSeed(spec.RoomID)
		spec.Rand = rand.New(rand.NewPCG(seed, ^seed)) //nolint:gosec // G404: the bot's behaviour, not a secret
	}
	s2 := &Session{
		cfg:      cfg,
		spec:     spec,
		t:        t,
		store:    s,
		obs:      o,
		logf:     spec.Logf,
		inbox:    make(chan inboxItem, cfg.InboxSize),
		presence: make(chan string, cfg.PresenceBuffer),
		done:     make(chan struct{}),
		created:  time.Now(),
		humans:   append([]string(nil), spec.Players...),
	}
	if s2.logf == nil {
		s2.logf = func(string, ...any) {}
	}
	s2.all = append(s2.all, s2.humans...)
	if spec.Bot {
		s2.all = append(s2.all, spec.BotID)
	}
	s2.last = s2.created
	s2.room = newRoom(&s2.cfg, spec.RoomID, s2.humans, spec.BotID, t, o, spec.Rand, s2.created)
	return s2, nil
}

type nopObserver struct{}

func (nopObserver) Count(string) {}

// Done is closed when Run has returned.
func (s *Session) Done() <-chan struct{} { return s.done }

// Post hands the session one inbound payload ({"t":"pos",...} or
// {"t":"ping",...}) read from playerID's socket at arrived. It never
// blocks: false means the payload was dropped, because the inbox was full
// (counted as EventInboxDropped) or the match is already over (not
// counted). The session keeps data; the caller must not modify it
// afterwards. Decoding happens on the session's goroutine.
func (s *Session) Post(playerID string, data []byte, arrived time.Time) bool {
	if s.closing.Load() {
		return false
	}
	select {
	case s.inbox <- inboxItem{player: playerID, data: data, arrived: arrived}:
		return true
	default:
		s.obs.Count(EventInboxDropped)
		return false
	}
}

// Notify says playerID's presence may have changed (a connection joined or
// left the room). It is never lost: it blocks until the session has taken
// it (into a buffer of Config.PresenceBuffer) or is done. The session then
// asks Transport.Connected for the truth, and a player who is connected
// gets a snap, so a connection that just joined always learns the state.
func (s *Session) Notify(playerID string) {
	select {
	case s.presence <- playerID:
	case <-s.done:
	}
}

// Run is the room's one goroutine. It returns when the match is over or ctx
// is cancelled, and closes Done either way. It must be called once; a
// second call returns at once.
//
// Cancelling ctx (server shutdown, including every rollout) ends the room
// at once with nothing written and nothing sent: no forfeit, no settle, no
// match_end. Otherwise every deploy would record both players as having
// abandoned (section 2).
//
// If the room's goroutine panics, Run recovers: it counts EventRoomPanic,
// logs the panic and its stack through Spec.Logf, ends the match as void
// and unranked without calling the store (the room's state is not to be
// trusted with a score), and makes one guarded attempt to broadcast a void
// match_end. The process stays up and Done still closes.
func (s *Session) Run(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	defer close(s.done)
	defer s.closing.Store(true)
	defer func() {
		if v := recover(); v != nil {
			s.panicked(v, debug.Stack())
		}
	}()
	if s.loop(ctx) {
		s.finish(ctx)
	}
}

// loop runs the room until it finishes (true) or ctx is cancelled (false).
// Everything that changes the room arrives here, one at a time: payloads
// from the lossy inbox, presence from the lossless channel, and time from
// the one ticker. There is no other timer.
func (s *Session) loop(ctx context.Context) bool {
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()

	// Players may have joined before Run: start from the truth.
	for _, id := range s.humans {
		s.presenceChanged(id)
	}
	for !s.room.finished() {
		select {
		case <-ctx.Done():
			s.cancelled(ctx)
			return false
		case in := <-s.inbox:
			s.handle(in)
		case id := <-s.presence:
			s.presenceChanged(id)
		case <-ticker.C:
			s.room.tick(s.clock(time.Now()))
		}
		if ctx.Err() != nil {
			// A cancellation that raced the last event still writes nothing,
			// even if that event ended the match.
			s.cancelled(ctx)
			return false
		}
	}
	return true
}

// clock returns t, or the latest time already used if t is earlier, so the
// room's ServerTs never goes backwards: two connections of one player, or
// two readers racing into the inbox, cannot reorder time.
func (s *Session) clock(t time.Time) time.Time {
	if t.Before(s.last) {
		return s.last
	}
	s.last = t
	return t
}

func (s *Session) presenceChanged(id string) {
	if id == s.spec.BotID && s.spec.Bot {
		return
	}
	now := s.clock(time.Now())
	connected := s.t.Connected(id)
	if connected {
		s.room.setName(id, s.t.Name(id))
	}
	s.room.setPresence(now, id, connected)
}

func (s *Session) handle(in inboxItem) {
	now := s.clock(in.arrived)
	msg, err := decodeInbound(in.data)
	if err != nil {
		s.obs.Count(EventInvalidPayload)
		return
	}
	switch msg.t {
	case msgPos:
		s.room.handlePos(now, in.player, msg.pos, in.arrived)
	case msgPing:
		if p := s.room.byID[in.player]; p == nil || p.bot {
			s.obs.Count(EventInvalidPayload)
			return
		}
		s.t.SendTo(in.player, marshal(pongMsg{T: "pong", Cts: msg.cts, Sts: s.room.sts(now)}))
	}
}

func (s *Session) cancelled(ctx context.Context) {
	s.logf("ghosttag: room %s: cancelled in %s: %v", s.spec.RoomID, s.room.state, context.Cause(ctx))
}

// ranks says whether a finished match of this room may reach the store.
func (s *Session) ranks() bool {
	return s.store != nil && !s.spec.Bot && !s.spec.Unranked && s.room.result.settles()
}

// finish settles a ranked match (the only call the loop blocks on, under
// SettleTimeout) and broadcasts match_end.
func (s *Session) finish(ctx context.Context) {
	s.closing.Store(true)
	res := s.room.result
	v := decide(&s.cfg, res)
	ranked := false
	deltas := s.zeroDeltas()
	if s.ranks() {
		sctx, cancel := context.WithTimeout(ctx, s.cfg.SettleTimeout)
		st, err := s.store.Settle(sctx, Match{RoomID: s.spec.RoomID, Players: s.humans, Names: s.room.names(), EndedAt: s.room.endedAt}, scorer(&s.cfg, res))
		cancel()
		switch {
		case err != nil && ctx.Err() != nil:
			// Shutdown during the settle: as for any cancellation, nothing
			// is sent. Whether the store wrote is the store's to say.
			s.logf("ghosttag: room %s: cancelled during settle: %v; result %+v", s.spec.RoomID, err, res)
			return
		case err != nil:
			s.obs.Count(EventSettleFailed)
			s.logf("ghosttag: room %s: settle failed, reported unranked: %v; result %+v, verdict %+v", s.spec.RoomID, err, res, v)
		case st.Ranked:
			ranked = true
			for id, a := range st.Awards {
				if _, ok := deltas[id]; ok {
					deltas[id] = a.Delta
				}
			}
			s.obs.Count(EventRanked)
		}
	}
	s.ended = true
	s.t.Broadcast(marshal(matchEndMsg{T: "match_end", Winner: v.winner, IsDraw: v.draw, Ranked: ranked, Deltas: deltas}))
}

func (s *Session) zeroDeltas() map[string]int {
	d := make(map[string]int, len(s.all))
	for _, id := range s.all {
		d[id] = 0
	}
	return d
}

// panicked is the panic policy of Run. Each step is guarded on its own: the
// Observer, the log or the Transport may be what panicked.
func (s *Session) panicked(v any, stack []byte) {
	s.closing.Store(true)
	guard := func(f func()) {
		defer func() { _ = recover() }()
		f()
	}
	guard(func() { s.obs.Count(EventRoomPanic) })
	guard(func() {
		s.logf("ghosttag: room %s: panic, match void and unranked: %v\n%s", s.spec.RoomID, v, stack)
	})
	if s.ended {
		return // the match_end is out; a second one would contradict it
	}
	s.ended = true
	guard(func() {
		s.t.Broadcast(marshal(matchEndMsg{T: "match_end", Deltas: s.zeroDeltas()}))
	})
}
