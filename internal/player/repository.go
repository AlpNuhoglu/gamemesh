package player

import (
	"context"
	"errors"

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
