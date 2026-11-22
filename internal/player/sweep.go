package player

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// benchGrace is how old a bench account must be before the sweep takes it,
// so a measurement that is running keeps its accounts.
const benchGrace = 24 * time.Hour

// SweepResult is what one run of the sweep deleted.
type SweepResult struct {
	Guests int
	Bench  int
	// Capped is true when the run stopped at its limit: there may be more
	// for the next run.
	Capped bool
}

// Sweep deletes guest accounts that have not taken a token for retention,
// and the accounts latencybench left behind, at most maxPerRun in all.
//
// The limit is the safety of the sweep: whatever is wrong with a rule or
// with the data, one run cannot delete more than maxPerRun accounts. It is
// part of the DELETE statement itself, and the second rule only gets what
// the first left of it. Every run logs what it deleted, zeros included, so
// that a run which deleted much is seen and a sweep that stopped running is
// too.
func (s *Service) Sweep(ctx context.Context, retention time.Duration, maxPerRun int) (SweepResult, error) {
	var out SweepResult
	now := s.now()
	budget := maxPerRun

	guests, err := s.sweepRule(ctx, SweepInactiveGuests, now.Add(-retention), budget)
	out.Guests = len(guests)
	budget -= len(guests)
	if err == nil {
		var bench []uuid.UUID
		bench, err = s.sweepRule(ctx, SweepBenchAccounts, now.Add(-benchGrace), budget)
		out.Bench = len(bench)
		budget -= len(bench)
	}
	out.Capped = maxPerRun > 0 && budget <= 0

	fields := []zap.Field{
		zap.Int("guests_deleted", out.Guests),
		zap.Int("bench_deleted", out.Bench),
		zap.Int("max_per_run", maxPerRun),
		zap.Bool("capped", out.Capped),
	}
	if err != nil {
		s.log.Error("account sweep failed", append(fields, zap.Error(err))...)
		return out, err
	}
	s.log.Info("account sweep", fields...)
	return out, nil
}

// sweepRule deletes up to budget accounts of one rule and ends their
// sessions. The rows and their PlayerDeleted events are one transaction; the
// sessions are revoked after it, best effort (a swept guest has had no
// session for weeks, a bench account's expired within a day).
func (s *Service) sweepRule(ctx context.Context, rule SweepRule, cutoff time.Time, budget int) ([]uuid.UUID, error) {
	if budget <= 0 {
		return nil, nil
	}
	ids, err := s.repo.DeleteSwept(ctx, rule, cutoff, budget, events.TopicPlayer, deletedEvent)
	if err != nil {
		return nil, err
	}
	if len(ids) > budget {
		// The repository promised at most budget. If it ever returns more,
		// the rows are already gone; say so loudly rather than hide it.
		s.log.Error("account sweep deleted more than its limit",
			zap.Int("deleted", len(ids)), zap.Int("limit", budget))
	}
	for _, id := range ids {
		if err := s.revokeAll(ctx, id); err != nil {
			s.log.Warn("failed to revoke a swept account's sessions", zap.Error(err), zap.String("player_id", id.String()))
		}
	}
	return ids, nil
}

// RunSweep runs the sweep once now and then every interval, until ctx is
// done. It runs at start because a service that is redeployed more often
// than the interval would otherwise never reach its first tick.
func (s *Service) RunSweep(ctx context.Context, interval, retention time.Duration, maxPerRun int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// A run is not cut short by shutdown: it is one or two statements.
		_, _ = s.Sweep(context.WithoutCancel(ctx), retention, maxPerRun)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
