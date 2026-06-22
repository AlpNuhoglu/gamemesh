package presence

import (
	"context"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
)

// Service is the presence domain logic. It turns connection lifecycle calls
// (connect/disconnect/heartbeat) and explicit state changes into atomic Redis
// mutations, then publishes the resulting transitions as events. Reads are
// pure: Get/GetMany never mutate or publish.
type Service struct {
	repo *Repository
	bus  events.Publisher
	m    *metrics.Metrics
	log  *zap.Logger
}

// NewService wires the repository, event publisher and metrics together. bus
// may publish to NATS or Redis (the events.Publisher abstraction); consumers
// are optional, so a publish failure is logged but never fails the operation —
// presence is re-derived from the next heartbeat.
func NewService(repo *Repository, bus events.Publisher, m *metrics.Metrics, log *zap.Logger) *Service {
	return &Service{repo: repo, bus: bus, m: m, log: log}
}

// Connect registers a new connection for the player and returns the resulting
// record. The player becomes ONLINE on the first connection; additional
// connections only bump the count.
func (s *Service) Connect(ctx context.Context, playerID string) (Record, error) {
	ctx, span := tracing.Tracer().Start(ctx, "presence.connect")
	defer span.End()
	span.SetAttributes(attribute.String("presence.player_id", playerID))

	mut, err := s.repo.connect(ctx, playerID)
	if err != nil {
		tracing.RecordError(span, err)
		return Record{}, err
	}
	s.publishTransition(ctx, playerID, mut)
	return mut.Current, nil
}

// Disconnect drops one of the player's connections. The player goes OFFLINE
// only when the last connection closes (connection_count reaches 0).
func (s *Service) Disconnect(ctx context.Context, playerID string) (Record, error) {
	ctx, span := tracing.Tracer().Start(ctx, "presence.transition")
	defer span.End()
	span.SetAttributes(
		attribute.String("presence.player_id", playerID),
		attribute.String("presence.op", "disconnect"),
	)

	mut, err := s.repo.disconnect(ctx, playerID)
	if err != nil {
		tracing.RecordError(span, err)
		return Record{}, err
	}
	s.publishTransition(ctx, playerID, mut)
	return mut.Current, nil
}

// Heartbeat refreshes the player's presence TTL. If the record had expired it
// is re-created (ONLINE), so presence self-heals after a crash without an
// explicit reconnect.
func (s *Service) Heartbeat(ctx context.Context, playerID string) (Record, error) {
	ctx, span := tracing.Tracer().Start(ctx, "presence.heartbeat")
	defer span.End()
	span.SetAttributes(attribute.String("presence.player_id", playerID))

	mut, err := s.repo.heartbeat(ctx, playerID)
	if err != nil {
		tracing.RecordError(span, err)
		return Record{}, err
	}
	if s.m != nil {
		s.m.PresenceHeartbeatTotal.Inc()
	}
	// A heartbeat that re-created an expired record is a real ONLINE transition.
	s.publishTransition(ctx, playerID, mut)
	return mut.Current, nil
}

// HeartbeatMany refreshes presence for a batch of players — the bulk fast-path
// for the WS replica's heartbeat ticker. It counts the heartbeats once for the
// whole batch and publishes PresenceOnline only for players whose record had
// expired and was re-created; the steady-state majority refresh silently.
func (s *Service) HeartbeatMany(ctx context.Context, playerIDs []string) error {
	if len(playerIDs) == 0 {
		return nil
	}
	ctx, span := tracing.Tracer().Start(ctx, "presence.heartbeat")
	defer span.End()
	span.SetAttributes(attribute.Int("presence.batch_size", len(playerIDs)))

	healed, err := s.repo.HeartbeatMany(ctx, playerIDs)
	if err != nil {
		tracing.RecordError(span, err)
		return err
	}

	if s.m != nil {
		// One Add for the whole batch instead of N Inc.
		s.m.PresenceHeartbeatTotal.Add(float64(len(playerIDs)))
	}

	// Only re-created (OFFLINE->ONLINE) players changed state → publish those.
	nowSec := now().Unix()
	for _, id := range healed {
		s.publishTransition(ctx, id, mutation{
			Previous: Record{State: StateOffline},
			Current:  Record{State: StateOnline, ConnectionCount: 1, LastSeen: nowSec},
		})
	}
	return nil
}

// SetState applies an explicit transition (e.g. ONLINE->IN_QUEUE,
// IN_QUEUE->IN_MATCH). The rule is permissive (see CanTransition); only
// OFFLINE->IN_QUEUE / OFFLINE->IN_MATCH are rejected.
func (s *Service) SetState(ctx context.Context, playerID string, to State) (Record, error) {
	ctx, span := tracing.Tracer().Start(ctx, "presence.transition")
	defer span.End()
	span.SetAttributes(
		attribute.String("presence.player_id", playerID),
		attribute.String("presence.to", string(to)),
	)

	if !to.Valid() {
		if s.m != nil {
			s.m.PresenceInvalidTransitions.Inc()
		}
		return Record{}, ErrInvalidTransition
	}

	current, err := s.repo.Get(ctx, playerID)
	if err != nil {
		tracing.RecordError(span, err)
		return Record{}, err
	}
	if !CanTransition(current.State, to) {
		if s.m != nil {
			s.m.PresenceInvalidTransitions.Inc()
		}
		span.SetAttributes(attribute.String("presence.from", string(current.State)))
		return Record{}, ErrInvalidTransition
	}

	mut, err := s.repo.setState(ctx, playerID, to)
	if err != nil {
		tracing.RecordError(span, err)
		return Record{}, err
	}
	s.publishTransition(ctx, playerID, mut)
	return mut.Current, nil
}

// Get returns a single player's current presence (OFFLINE if absent). Pure read.
func (s *Service) Get(ctx context.Context, playerID string) (Record, error) {
	return s.repo.Get(ctx, playerID)
}

// Friends returns presence for a batch of players in one pipelined round trip.
// Optimised for friend lists: O(N) commands, one round trip, no N+1. Pure read.
func (s *Service) Friends(ctx context.Context, playerIDs []string) ([]Friend, error) {
	recs, err := s.repo.GetMany(ctx, playerIDs)
	if err != nil {
		return nil, err
	}
	out := make([]Friend, 0, len(playerIDs))
	for _, id := range playerIDs {
		rec := recs[id]
		out = append(out, Friend{PlayerID: id, State: rec.State, LastSeen: rec.LastSeen})
	}
	return out, nil
}

// publishTransition records metrics and emits the appropriate presence event
// when a mutation actually changed the player's state. A no-op mutation (e.g. a
// second device connecting to an already-ONLINE player) records the heartbeat
// effect but emits no state-change event.
func (s *Service) publishTransition(ctx context.Context, playerID string, mut mutation) {
	prev, cur := mut.Previous.State, mut.Current.State
	if prev == cur {
		return
	}

	if s.m != nil {
		s.m.PresenceStateTransitionsTotal.WithLabelValues(string(prev), string(cur)).Inc()
		switch {
		case prev == StateOffline && cur != StateOffline:
			s.m.PresenceOnlinePlayers.Inc()
		case prev != StateOffline && cur == StateOffline:
			s.m.PresenceOnlinePlayers.Dec()
		}
	}

	eventType := events.TypePresenceStateChanged
	switch {
	case prev == StateOffline && cur != StateOffline:
		eventType = events.TypePresenceOnline
	case cur == StateOffline:
		eventType = events.TypePresenceOffline
	}

	e, err := events.New(eventType, events.PresenceChangedPayload{
		PlayerID:        playerID,
		State:           string(cur),
		PreviousState:   string(prev),
		ConnectionCount: mut.Current.ConnectionCount,
		LastSeen:        mut.Current.LastSeen,
	})
	if err != nil {
		s.log.Warn("presence: build event failed", zap.Error(err), zap.String("player_id", playerID))
		return
	}
	if err := s.bus.Publish(ctx, events.TopicPresence, e); err != nil {
		// Non-fatal: presence is re-derived from the next heartbeat, so a dropped
		// event never corrupts the source of truth (Redis).
		s.log.Warn("presence: publish event failed", zap.Error(err),
			zap.String("player_id", playerID), zap.String("type", eventType))
	}
}
