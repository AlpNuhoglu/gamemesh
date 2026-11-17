package matchmaking

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/health"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Config tunes the matcher.
type Config struct {
	RankWindow  int
	BatchSize   int64
	MaxQueueAge time.Duration
	// Heartbeat, when set, is beaten after every match tick (failed or not)
	// so /livez can tell a stuck loop from a quiet one.
	Heartbeat *health.Heartbeat

	// GhostTag and GhostTagUnranked are the Ghost Tag queues
	// (NewModeQueue(rdb, QueueGhostTag) and QueueGhostTagUnranked). Nil
	// turns the mode off: JoinGhostTag fails with ErrModeDisabled.
	GhostTag, GhostTagUnranked *Queue
	// BotWait is how long a Ghost Tag ticket waits for a human opponent
	// before it gets a room with the bot. Zero or less: never.
	BotWait time.Duration
}

// ErrModeDisabled is returned for a join to a game mode this service does
// not run.
var ErrModeDisabled = errors.New("game mode not available")

// Service orchestrates the queue, the pairing algorithm and room creation.
type Service struct {
	queue *Queue
	rooms *RoomStore
	pub   events.Publisher
	m     *metrics.Metrics
	log   *zap.Logger
	cfg   Config
}

// NewService wires the service's dependencies.
func NewService(queue *Queue, rooms *RoomStore, pub events.Publisher, m *metrics.Metrics, log *zap.Logger, cfg Config) *Service {
	return &Service{queue: queue, rooms: rooms, pub: pub, m: m, log: log, cfg: cfg}
}

// JoinQueue enqueues a player at the given rank, in the general queue.
func (s *Service) JoinQueue(ctx context.Context, playerID string, rank int) error {
	return s.join(ctx, s.queue, playerID, rank)
}

// JoinGhostTag enqueues a player for a Ghost Tag match. An unranked ticket
// goes to the unranked queue and is paired only with other unranked
// tickets. It fails with ErrModeDisabled if the mode is not configured.
func (s *Service) JoinGhostTag(ctx context.Context, playerID string, rank int, unranked bool) error {
	q := s.cfg.GhostTag
	if unranked {
		q = s.cfg.GhostTagUnranked
	}
	if q == nil {
		return ErrModeDisabled
	}
	return s.join(ctx, q, playerID, rank)
}

// queues returns every queue this service pairs: the general one first.
func (s *Service) queues() []*Queue {
	qs := []*Queue{s.queue}
	for _, q := range []*Queue{s.cfg.GhostTag, s.cfg.GhostTagUnranked} {
		if q != nil {
			qs = append(qs, q)
		}
	}
	return qs
}

// join puts the player in q and takes them out of every other queue: a
// player in two queues could be put in two rooms by the same tick.
func (s *Service) join(ctx context.Context, q *Queue, playerID string, rank int) error {
	ctx, span := tracing.Tracer().Start(ctx, "matchmaking.JoinQueue")
	defer span.End()
	// Low-cardinality only: bucket the rank instead of recording player_id.
	span.SetAttributes(attribute.Int("matchmaking.rank_bucket", rank/100))

	for _, other := range s.queues() {
		if other == q {
			continue
		}
		if err := other.RemoveBatch(ctx, []string{playerID}); err != nil {
			tracing.RecordError(span, err)
			return err
		}
	}
	err := q.Enqueue(ctx, playerID, rank)
	tracing.RecordError(span, err)
	return err
}

// LeaveQueue removes a player from whichever queue they are in; returns
// ErrNotQueued if they are in none.
func (s *Service) LeaveQueue(ctx context.Context, playerID string) error {
	left := false
	for _, q := range s.queues() {
		err := q.Remove(ctx, playerID)
		switch {
		case err == nil:
			left = true
		case !errors.Is(err, ErrNotQueued):
			return err
		}
	}
	if !left {
		return ErrNotQueued
	}
	return nil
}

// QueueStatus reports whether the player is queued, in any queue, and the
// depth of the queue they are in (of the general queue if none).
func (s *Service) QueueStatus(ctx context.Context, playerID string) (queued bool, size int64, err error) {
	in := s.queue
	for _, q := range s.queues() {
		ok, err := q.Contains(ctx, playerID)
		if err != nil {
			return false, 0, err
		}
		if ok {
			queued, in = true, q
			break
		}
	}
	size, err = in.Size(ctx)
	return queued, size, err
}

// GetRoom fetches a created room.
func (s *Service) GetRoom(ctx context.Context, id string) (*Room, error) {
	return s.rooms.Get(ctx, id)
}

// tickTimeout bounds one match tick. A tick runs on a context detached from
// the loop's, so a shutdown that starts mid-tick lets the tick finish (rooms
// saved, players dequeued, MatchFound published) instead of cutting it
// between steps.
const tickTimeout = 10 * time.Second

// RunMatchLoop ticks every interval until ctx is cancelled; a tick already
// running when that happens completes first. Note: with
// multiple replicas this loop needs a leader lock (e.g. Redis SET NX with
// TTL); the K8s deployment therefore runs a single matchmaking replica, which
// comfortably handles thousands of matches per tick.
func (s *Service) RunMatchLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.log.Info("match loop started", zap.Duration("interval", interval))
	for {
		select {
		case <-ctx.Done():
			s.log.Info("match loop stopped")
			return
		case <-ticker.C:
			tickCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tickTimeout)
			if _, err := s.MatchOnce(tickCtx); err != nil {
				s.log.Error("match tick failed", zap.Error(err))
			}
			cancel()
			s.cfg.Heartbeat.Beat()
		}
	}
}

// MatchOnce performs a single matchmaking pass: evict stale tickets, pair
// players within the rank window, create rooms and publish MatchFound events.
// Returns the number of matches created.
func (s *Service) MatchOnce(ctx context.Context) (int, error) {
	// Each tick is its own root trace (it is not request-driven), so the Jaeger
	// UI shows one trace per tick rather than one unbounded trace for the loop.
	ctx, span := tracing.Tracer().Start(ctx, "matchmaking.tick")
	defer span.End()

	func() {
		_, evictSpan := tracing.Tracer().Start(ctx, "matchmaking.evict_stale")
		defer evictSpan.End()
		if evicted, err := s.queue.EvictStale(ctx, s.cfg.MaxQueueAge); err != nil {
			tracing.RecordError(evictSpan, err)
			s.log.Warn("stale eviction failed", zap.Error(err))
		} else if len(evicted) > 0 {
			evictSpan.SetAttributes(attribute.Int("matchmaking.evicted", len(evicted)))
			s.log.Info("evicted stale players", zap.Int("count", len(evicted)))
		}
	}()

	snapCtx, snapSpan := tracing.Tracer().Start(ctx, "matchmaking.snapshot")
	tickets, err := s.queue.Snapshot(snapCtx, s.cfg.BatchSize)
	snapSpan.SetAttributes(attribute.Int("matchmaking.tickets", len(tickets)))
	tracing.RecordError(snapSpan, err)
	snapSpan.End()
	if err != nil {
		tracing.RecordError(span, err)
		return 0, err
	}

	_, pairSpan := tracing.Tracer().Start(ctx, "matchmaking.pair")
	pairs := Pair(tickets, s.cfg.RankWindow)
	pairSpan.SetAttributes(
		attribute.Int("matchmaking.tickets_in", len(tickets)),
		attribute.Int("matchmaking.pairs_out", len(pairs)),
	)
	pairSpan.End()

	if len(pairs) > 0 {
		if err := s.createRooms(ctx, s.queue, roomsFor(pairs, Room{})); err != nil {
			tracing.RecordError(span, err)
			s.log.Error("batch room creation failed", zap.Error(err))
		}
	}
	matches := len(pairs)

	// The Ghost Tag queues, each on its own: an unranked ticket is never
	// paired with a ranked one.
	if s.cfg.GhostTag != nil {
		matches += s.matchGhostTag(ctx, s.cfg.GhostTag, false)
	}
	if s.cfg.GhostTagUnranked != nil {
		matches += s.matchGhostTag(ctx, s.cfg.GhostTagUnranked, true)
	}

	span.SetAttributes(attribute.Int("matchmaking.matches", matches))

	if size, err := s.queue.Size(ctx); err == nil {
		s.m.MatchmakingQueueSize.Set(float64(size))
	}
	return matches, nil
}

// matchGhostTag runs one tick of a Ghost Tag queue: the same stale eviction
// and the same rank-window pairing as the general queue, into rooms that
// name the game; then a room with the bot for every ticket left unpaired
// that has waited at least BotWait. It returns the rooms created. A failure
// is logged and leaves the tickets queued for the next tick.
func (s *Service) matchGhostTag(ctx context.Context, q *Queue, unranked bool) int {
	ctx, span := tracing.Tracer().Start(ctx, "matchmaking.ghosttag")
	defer span.End()
	span.SetAttributes(attribute.Bool("matchmaking.unranked", unranked))

	if _, err := q.EvictStale(ctx, s.cfg.MaxQueueAge); err != nil {
		s.log.Warn("ghost tag stale eviction failed", zap.Error(err))
	}
	tickets, err := q.Snapshot(ctx, s.cfg.BatchSize)
	if err != nil {
		tracing.RecordError(span, err)
		s.log.Error("ghost tag snapshot failed", zap.Error(err))
		return 0
	}
	template := Room{Mode: ModeGhostTag, Unranked: unranked}
	pairs := Pair(tickets, s.cfg.RankWindow)
	rooms := roomsFor(pairs, template)

	if s.cfg.BotWait > 0 {
		bots, err := s.botRooms(ctx, q, tickets, pairs, template)
		if err != nil {
			s.log.Warn("ghost tag bot rooms skipped this tick", zap.Error(err))
		}
		rooms = append(rooms, bots...)
	}
	span.SetAttributes(attribute.Int("matchmaking.pairs", len(pairs)), attribute.Int("matchmaking.rooms", len(rooms)))
	if len(rooms) == 0 {
		return 0
	}
	if err := s.createRooms(ctx, q, rooms); err != nil {
		tracing.RecordError(span, err)
		s.log.Error("ghost tag room creation failed", zap.Error(err))
		return 0
	}
	return len(rooms)
}

// botRooms returns a room with the bot for every ticket that Pair left
// alone and that joined at least BotWait ago. Join times are kept in whole
// seconds, so a ticket may get its bot up to a second early.
func (s *Service) botRooms(ctx context.Context, q *Queue, tickets []Ticket, pairs [][2]Ticket, template Room) ([]*Room, error) {
	paired := make(map[string]bool, 2*len(pairs))
	for _, p := range pairs {
		paired[p[0].PlayerID], paired[p[1].PlayerID] = true, true
	}
	var alone []Ticket
	var ids []string
	for _, t := range tickets {
		if !paired[t.PlayerID] {
			alone = append(alone, t)
			ids = append(ids, t.PlayerID)
		}
	}
	if len(alone) == 0 {
		return nil, nil
	}
	joined, err := q.JoinedAt(ctx, ids)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-s.cfg.BotWait)
	var rooms []*Room
	for _, t := range alone {
		at, ok := joined[t.PlayerID]
		if !ok || at.After(cutoff) {
			continue
		}
		r := template
		r.Players = []string{t.PlayerID}
		r.Ranks = map[string]int{t.PlayerID: t.Rank}
		r.Bot = true
		rooms = append(rooms, &r)
	}
	return rooms, nil
}

// roomsFor builds a room per pair, each a copy of template (its Mode and
// Unranked) with the pair's players.
func roomsFor(pairs [][2]Ticket, template Room) []*Room {
	rooms := make([]*Room, len(pairs))
	for i, pair := range pairs {
		r := template
		r.Players = []string{pair[0].PlayerID, pair[1].PlayerID}
		r.Ranks = map[string]int{
			pair[0].PlayerID: pair[0].Rank,
			pair[1].PlayerID: pair[1].Rank,
		}
		rooms[i] = &r
	}
	return rooms
}

// createRooms persists rooms (players, ranks and mode already set; ID,
// status and creation time are set here), dequeues their players from q and
// publishes MatchFound for each — in a fixed, small number of round trips per
// tick instead of two per pair. This is the hot path's core optimization:
// O(pairs) sequential round trips collapse into O(1) pipelined batches, and the
// per-pair span is replaced by one span for the whole batch.
//
// Partial-failure is tolerated by design (Cluster-ready, non-atomic): if the
// dequeue fails after rooms are saved, the still-queued players are simply
// re-paired on the next tick and the orphaned rooms are reclaimed by their TTL.
// A "ghost room" is far cheaper than a failed match.
func (s *Service) createRooms(ctx context.Context, q *Queue, rooms []*Room) error {
	ctx, span := tracing.Tracer().Start(ctx, "matchmaking.create_rooms")
	defer span.End()
	span.SetAttributes(attribute.Int("matchmaking.pairs", len(rooms)))

	// Pre-allocated for the usual two players per room (a bot room has one).
	dequeue := make([]string, 0, len(rooms)*2)
	createdAt := time.Now().UTC()

	for _, room := range rooms {
		room.ID = uuid.NewString()
		room.Status = "waiting"
		room.CreatedAt = createdAt
		dequeue = append(dequeue, room.Players...)
	}

	// 1) Persist all rooms in one pipeline.
	if err := s.rooms.SaveMany(ctx, rooms); err != nil {
		return err
	}
	// 2) Dequeue all matched players in one pipeline. If this fails the rooms are
	// already saved (ghost rooms, reclaimed by TTL); the still-queued players are
	// re-paired next tick — idempotent, matching the old per-room behaviour.
	if err := q.RemoveBatch(ctx, dequeue); err != nil {
		return err
	}

	// 3) Metrics + events. Count once for the whole batch (one atomic Add instead
	// of N Inc), then publish per room since each match is a distinct downstream
	// notification.
	s.m.MatchesCreated.Add(float64(len(rooms)))
	for _, room := range rooms {
		e, err := events.New(events.TypeMatchFound, events.MatchFoundPayload{
			RoomID:  room.ID,
			Players: room.Players,
		})
		if err != nil {
			s.log.Warn("failed to build MatchFound", zap.Error(err), zap.String("room_id", room.ID))
			continue
		}
		if err := s.pub.Publish(ctx, events.TopicMatchmaking, e); err != nil {
			s.log.Warn("failed to publish MatchFound", zap.Error(err), zap.String("room_id", room.ID))
		}
	}
	return nil
}
