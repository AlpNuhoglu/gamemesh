package player

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/alpnuhoglu/gamemesh/internal/outbox"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// Domain errors. The service layer maps these to HTTP statuses; the
// repository never leaks driver-specific errors upward.
var (
	ErrNotFound  = errors.New("player not found")
	ErrDuplicate = errors.New("username or email already taken")
)

// Repository abstracts player persistence (repository pattern). The GORM
// implementation below is the only one in production; tests substitute mocks.
//
// The *WithOutbox methods are the transactional-outbox write path: they perform
// the business write AND insert the domain event into outbox_events inside a
// single Postgres transaction, so the event can never be lost once the write
// commits (and is never visible before it commits). The plain Create/Update are
// retained for callers that do not emit events.
type Repository interface {
	Create(ctx context.Context, p *Player) error
	CreateWithOutbox(ctx context.Context, p *Player, e events.Event, topic string) error
	GetByID(ctx context.Context, id uuid.UUID) (*Player, error)
	GetByUsername(ctx context.Context, username string) (*Player, error)
	GetByEmail(ctx context.Context, email string) (*Player, error)
	Update(ctx context.Context, p *Player) error
	UpdateWithOutbox(ctx context.Context, p *Player, e events.Event, topic string) error
	// PublishEvent writes an event to the outbox on its own, for events with no
	// accompanying row change (a logout).
	PublishEvent(ctx context.Context, e events.Event, topic string) error
	// TouchLastSeen records that the player took a token at the given time.
	TouchLastSeen(ctx context.Context, id uuid.UUID, at time.Time) error
	// DeleteWithOutbox deletes the player, stats included, and writes the
	// event in the same transaction. ErrNotFound if there is no such player.
	DeleteWithOutbox(ctx context.Context, id uuid.UUID, e events.Event, topic string) error
	// DeleteSwept deletes at most limit accounts that rule selects as of
	// cutoff, oldest first, and writes event(id) to the outbox for each, all
	// in one transaction. It returns the ids it deleted. A limit of zero or
	// less deletes nothing.
	DeleteSwept(ctx context.Context, rule SweepRule, cutoff time.Time, limit int, topic string, event func(uuid.UUID) (events.Event, error)) ([]uuid.UUID, error)
}

// SweepRule names a set of accounts the sweep may delete. The rule is a
// fixed predicate in the repository, never a caller's condition: what the
// sweep can reach is readable in one place.
type SweepRule int

const (
	// SweepInactiveGuests selects guest accounts whose last token was taken before
	// the cutoff.
	SweepInactiveGuests SweepRule = iota + 1
	// SweepBenchAccounts selects accounts latencybench registered and left behind,
	// created before the cutoff. Both conditions of its own clean-up apply:
	// the e-mail is under latencybench.invalid (a reserved TLD, RFC 2606, so
	// no real player has one) and the username starts with "bench".
	SweepBenchAccounts
)

// sweepPredicates are the WHERE clauses of the rules; $1 is the cutoff.
var sweepPredicates = map[SweepRule]string{
	SweepInactiveGuests: `is_guest AND COALESCE(last_seen_at, created_at) < ?`,
	SweepBenchAccounts:  `NOT is_guest AND email LIKE '%@latencybench.invalid' AND username LIKE 'bench%' AND created_at < ?`,
}

type gormRepository struct {
	db  *gorm.DB
	out *outbox.Publisher
}

// NewRepository returns the PostgreSQL-backed repository. out may be nil for
// callers (e.g. tests) that never use the *WithOutbox methods.
func NewRepository(db *gorm.DB, out *outbox.Publisher) Repository {
	return &gormRepository{db: db, out: out}
}

func (r *gormRepository) Create(ctx context.Context, p *Player) error {
	return translate(r.db.WithContext(ctx).Create(p).Error)
}

// CreateWithOutbox inserts the player (and cascaded stats) and the outbox row in
// one transaction. The outbox.Publisher is rebound to the tx handle so its
// insert joins this transaction — if either write fails, both roll back.
func (r *gormRepository) CreateWithOutbox(ctx context.Context, p *Player, e events.Event, topic string) error {
	return translate(r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(p).Error; err != nil {
			return err
		}
		return r.out.WithTx(tx).Publish(ctx, topic, e)
	}))
}

// UpdateWithOutbox saves the player and inserts the outbox row in one transaction.
func (r *gormRepository) UpdateWithOutbox(ctx context.Context, p *Player, e events.Event, topic string) error {
	return translate(r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(p).Error; err != nil {
			return err
		}
		return r.out.WithTx(tx).Publish(ctx, topic, e)
	}))
}

func (r *gormRepository) PublishEvent(ctx context.Context, e events.Event, topic string) error {
	return r.out.Publish(ctx, topic, e)
}

func (r *gormRepository) GetByID(ctx context.Context, id uuid.UUID) (*Player, error) {
	var p Player
	err := r.db.WithContext(ctx).Preload("Stats").First(&p, "id = ?", id).Error
	if err != nil {
		return nil, translate(err)
	}
	return &p, nil
}

func (r *gormRepository) GetByUsername(ctx context.Context, username string) (*Player, error) {
	var p Player
	err := r.db.WithContext(ctx).Preload("Stats").First(&p, "username = ?", username).Error
	if err != nil {
		return nil, translate(err)
	}
	return &p, nil
}

func (r *gormRepository) GetByEmail(ctx context.Context, email string) (*Player, error) {
	var p Player
	err := r.db.WithContext(ctx).Preload("Stats").First(&p, "email = ?", email).Error
	if err != nil {
		return nil, translate(err)
	}
	return &p, nil
}

// TouchLastSeen sets last_seen_at alone: it is not a profile change, so
// updated_at stays what it was.
func (r *gormRepository) TouchLastSeen(ctx context.Context, id uuid.UUID, at time.Time) error {
	res := r.db.WithContext(ctx).Model(&Player{}).Where("id = ?", id).UpdateColumn("last_seen_at", at)
	if res.Error != nil {
		return translate(res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *gormRepository) DeleteWithOutbox(ctx context.Context, id uuid.UUID, e events.Event, topic string) error {
	return translate(r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// player_stats goes with the row: its foreign key cascades.
		res := tx.Where("id = ?", id).Delete(&Player{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return r.out.WithTx(tx).Publish(ctx, topic, e)
	}))
}

// DeleteSwept selects and deletes in one statement, so an account is judged
// by the rule at the moment it is deleted (a guest who renews between a
// separate select and delete would otherwise lose the account), and the
// LIMIT is in the statement itself. SKIP LOCKED lets two replicas sweep at
// once without waiting on each other's rows.
func (r *gormRepository) DeleteSwept(ctx context.Context, rule SweepRule, cutoff time.Time, limit int, topic string, event func(uuid.UUID) (events.Event, error)) ([]uuid.UUID, error) {
	predicate, ok := sweepPredicates[rule]
	if !ok {
		return nil, fmt.Errorf("unknown sweep rule %d", rule)
	}
	if limit <= 0 {
		return nil, nil
	}
	var ids []uuid.UUID
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := `DELETE FROM players WHERE id IN (
			SELECT id FROM players WHERE ` + predicate + `
			ORDER BY created_at LIMIT ? FOR UPDATE SKIP LOCKED
		) RETURNING id`
		if err := tx.Raw(query, cutoff, limit).Scan(&ids).Error; err != nil {
			return err
		}
		for _, id := range ids {
			e, err := event(id)
			if err != nil {
				return err
			}
			if err := r.out.WithTx(tx).Publish(ctx, topic, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, translate(err)
	}
	return ids, nil
}

func (r *gormRepository) Update(ctx context.Context, p *Player) error {
	return translate(r.db.WithContext(ctx).Save(p).Error)
}

// translate maps driver errors to domain errors. 23505 is PostgreSQL's
// unique_violation code.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicate
	}
	return err
}
