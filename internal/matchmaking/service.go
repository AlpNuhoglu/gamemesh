package matchmaking

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Config tunes the matcher.
type Config struct {
	RankWindow  int
	BatchSize   int64
	MaxQueueAge time.Duration
}

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

// JoinQueue enqueues a player at the given rank.
func (s *Service) JoinQueue(ctx context.Context, playerID string, rank int) error {
	ctx, span := tracing.Tracer().Start(ctx, "matchmaking.JoinQueue")
	defer span.End()
	// Low-cardinality only: bucket the rank instead of recording player_id.
	span.SetAttributes(attribute.Int("matchmaking.rank_bucket", rank/100))

	err := s.queue.Enqueue(ctx, playerID, rank)
	tracing.RecordError(span, err)
	return err
}

// LeaveQueue removes a player; returns ErrNotQueued if absent.
func (s *Service) LeaveQueue(ctx context.Context, playerID string) error {
	return s.queue.Remove(ctx, playerID)
}

// QueueStatus reports whether the player is queued and the current depth.
func (s *Service) QueueStatus(ctx context.Context, playerID string) (queued bool, size int64, err error) {
	queued, err = s.queue.Contains(ctx, playerID)
	if err != nil {
		return false, 0, err
	}
	size, err = s.queue.Size(ctx)
	return queued, size, err
}

// GetRoom fetches a created room.
func (s *Service) GetRoom(ctx context.Context, id string) (*Room, error) {
	return s.rooms.Get(ctx, id)
}

// RunMatchLoop ticks every interval until ctx is cancelled. Note: with
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
			if _, err := s.MatchOnce(ctx); err != nil {
				s.log.Error("match tick failed", zap.Error(err))
			}
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

	for _, pair := range pairs {
		s.createRoom(ctx, pair)
	}

	span.SetAttributes(attribute.Int("matchmaking.matches", len(pairs)))

	if size, err := s.queue.Size(ctx); err == nil {
		s.m.MatchmakingQueueSize.Set(float64(size))
	}
	return len(pairs), nil
}

// createRoom persists a room for one matched pair, dequeues the players and
// publishes MatchFound. Errors are logged and the room is skipped (the next
// tick re-pairs the still-queued players).
func (s *Service) createRoom(ctx context.Context, pair [2]Ticket) {
	ctx, span := tracing.Tracer().Start(ctx, "matchmaking.create_room")
	defer span.End()

	room := &Room{
		ID:      uuid.NewString(),
		Players: []string{pair[0].PlayerID, pair[1].PlayerID},
		Ranks: map[string]int{
			pair[0].PlayerID: pair[0].Rank,
			pair[1].PlayerID: pair[1].Rank,
		},
		Status:    "waiting",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.rooms.Save(ctx, room); err != nil {
		tracing.RecordError(span, err)
		s.log.Error("failed to save room", zap.Error(err), zap.String("room_id", room.ID))
		return
	}
	if err := s.queue.RemoveBatch(ctx, room.Players); err != nil {
		tracing.RecordError(span, err)
		s.log.Error("failed to dequeue matched players", zap.Error(err))
		return
	}
	s.m.MatchesCreated.Inc()

	e, err := events.New(events.TypeMatchFound, events.MatchFoundPayload{
		RoomID:  room.ID,
		Players: room.Players,
	})
	if err == nil {
		err = s.pub.Publish(ctx, events.TopicMatchmaking, e)
	}
	if err != nil {
		tracing.RecordError(span, err)
		s.log.Warn("failed to publish MatchFound", zap.Error(err), zap.String("room_id", room.ID))
	}
	s.log.Info("match created",
		append([]zap.Field{
			zap.String("room_id", room.ID),
			zap.Strings("players", room.Players),
		}, tracing.LogFields(ctx)...)...)
}
