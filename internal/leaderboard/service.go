package leaderboard

import (
	"context"

	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// Service layers event publishing and metrics on top of the store.
type Service struct {
	store *Store
	pub   events.Publisher
	m     *metrics.Metrics
	log   *zap.Logger
}

// NewService wires the service's dependencies.
func NewService(store *Store, pub events.Publisher, m *metrics.Metrics, log *zap.Logger) *Service {
	return &Service{store: store, pub: pub, m: m, log: log}
}

// SubmitScore applies a score update (absolute or incremental), then
// publishes LeaderboardUpdated so the WebSocket gateway can push it to
// connected clients in real time.
func (s *Service) SubmitScore(ctx context.Context, playerID string, score float64, increment bool) (Entry, error) {
	var err error
	if increment {
		_, err = s.store.IncrScore(ctx, playerID, score)
	} else {
		err = s.store.SetScore(ctx, playerID, score)
	}
	if err != nil {
		return Entry{}, err
	}
	s.m.LeaderboardUpdates.Inc()

	entry, err := s.store.Rank(ctx, playerID)
	if err != nil {
		return Entry{}, err
	}

	e, err := events.New(events.TypeLeaderboardUpdated, events.LeaderboardUpdatedPayload{
		PlayerID: entry.PlayerID,
		Score:    entry.Score,
		Rank:     entry.Rank,
	})
	if err == nil {
		err = s.pub.Publish(ctx, events.TopicLeaderboard, e)
	}
	if err != nil {
		// Realtime fan-out is best-effort; the authoritative state is the
		// sorted set, which clients can always re-query.
		s.log.Warn("failed to publish leaderboard event", zap.Error(err))
	}
	return entry, nil
}

// Top returns the n best players.
func (s *Service) Top(ctx context.Context, n int64) ([]Entry, error) {
	return s.store.Top(ctx, n)
}

// Page returns a paginated leaderboard view with the total player count.
func (s *Service) Page(ctx context.Context, offset, limit int64) ([]Entry, int64, error) {
	return s.store.Page(ctx, offset, limit)
}

// Rank returns one player's standing.
func (s *Service) Rank(ctx context.Context, playerID string) (Entry, error) {
	return s.store.Rank(ctx, playerID)
}
