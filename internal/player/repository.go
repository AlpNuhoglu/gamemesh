package player

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// Domain errors. The service layer maps these to HTTP statuses; the
// repository never leaks driver-specific errors upward.
var (
	ErrNotFound  = errors.New("player not found")
	ErrDuplicate = errors.New("username or email already taken")
)

// Repository abstracts player persistence (repository pattern). The GORM
// implementation below is the only one in production; tests substitute mocks.
type Repository interface {
	Create(ctx context.Context, p *Player) error
	GetByID(ctx context.Context, id uuid.UUID) (*Player, error)
	GetByUsername(ctx context.Context, username string) (*Player, error)
	GetByEmail(ctx context.Context, email string) (*Player, error)
	Update(ctx context.Context, p *Player) error
}

type gormRepository struct {
	db *gorm.DB
}

// NewRepository returns the PostgreSQL-backed repository.
func NewRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

func (r *gormRepository) Create(ctx context.Context, p *Player) error {
	return translate(r.db.WithContext(ctx).Create(p).Error)
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
