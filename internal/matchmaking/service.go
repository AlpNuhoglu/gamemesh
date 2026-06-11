package matchmaking

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
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
	return s.queue.Enqueue(ctx, playerID, rank)
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
	if evicted, err := s.queue.EvictStale(ctx, s.cfg.MaxQueueAge); err != nil {
		s.log.Warn("stale eviction failed", zap.Error(err))
	} else if len(evicted) > 0 {
		s.log.Info("evicted stale players", zap.Int("count", len(evicted)))
	}

	tickets, err := s.queue.Snapshot(ctx, s.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	pairs := Pair(tickets, s.cfg.RankWindow)

	for _, pair := range pairs {
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
			s.log.Error("failed to save room", zap.Error(err), zap.String("room_id", room.ID))
			continue
		}
		if err := s.queue.RemoveBatch(ctx, room.Players); err != nil {
			s.log.Error("failed to dequeue matched players", zap.Error(err))
			continue
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
			s.log.Warn("failed to publish MatchFound", zap.Error(err), zap.String("room_id", room.ID))
		}
		s.log.Info("match created",
			zap.String("room_id", room.ID),
			zap.Strings("players", room.Players))
	}

	if size, err := s.queue.Size(ctx); err == nil {
		s.m.MatchmakingQueueSize.Set(float64(size))
	}
	return len(pairs), nil
}
