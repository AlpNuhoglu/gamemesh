package player

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
)

// ErrInvalidCredentials is returned on any login failure. It is deliberately
// identical for "no such user" and "wrong password" to prevent user
// enumeration.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Service implements player business logic on top of the repository and
// session store (service layer pattern; dependencies injected via the
// constructor).
type Service struct {
	repo     Repository
	sessions SessionStore
	tokens   *auth.TokenManager
	log      *zap.Logger
}

// NewService wires the service's dependencies.
func NewService(repo Repository, sessions SessionStore, tokens *auth.TokenManager, log *zap.Logger) *Service {
	return &Service{repo: repo, sessions: sessions, tokens: tokens, log: log}
}

// RegisterInput is validated at the handler; the service trusts its shape but
// still owns hashing and uniqueness handling.
type RegisterInput struct {
	Username string
	Email    string
	Password string
}

// Register creates a player with default stats and a bcrypt-hashed password.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*Player, error) {
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	p := &Player{
		Username:     in.Username,
		Email:        in.Email,
		PasswordHash: hash,
		Stats:        &Stats{Rank: 1000},
	}
	if err := s.repo.Create(ctx, p); err != nil {
		return nil, err
	}
	s.log.Info("player registered", zap.String("player_id", p.ID.String()), zap.String("username", p.Username))
	return p, nil
}

// Login verifies credentials, issues a JWT and caches the session in Redis so
// it can be revoked before natural expiry.
func (s *Service) Login(ctx context.Context, identifier, password string) (string, *Player, error) {
	p, err := s.repo.GetByUsername(ctx, identifier)
	if errors.Is(err, ErrNotFound) {
		p, err = s.repo.GetByEmail(ctx, identifier)
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, err
	}
	if !auth.CheckPassword(p.PasswordHash, password) {
		return "", nil, ErrInvalidCredentials
	}

	token, jti, err := s.tokens.Generate(p.ID, p.Username)
	if err != nil {
		return "", nil, err
	}
	if err := s.sessions.Save(ctx, jti, p.ID.String(), s.tokens.Expiry()); err != nil {
		// Session caching is best-effort: a Redis blip must not block logins.
		s.log.Warn("failed to cache session", zap.Error(err), zap.String("player_id", p.ID.String()))
	}
	s.log.Info("player logged in", zap.String("player_id", p.ID.String()))
	return token, p, nil
}

// Logout revokes the session for the given JTI.
func (s *Service) Logout(ctx context.Context, jti string) error {
	return s.sessions.Delete(ctx, jti)
}

// GetProfile fetches a player with stats.
func (s *Service) GetProfile(ctx context.Context, id uuid.UUID) (*Player, error) {
	return s.repo.GetByID(ctx, id)
}

// UpdateInput carries optional profile changes.
type UpdateInput struct {
	Username *string
	Email    *string
}

// UpdateProfile applies partial updates to a player's own profile.
func (s *Service) UpdateProfile(ctx context.Context, id uuid.UUID, in UpdateInput) (*Player, error) {
	p, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Username != nil {
		p.Username = *in.Username
	}
	if in.Email != nil {
		p.Email = *in.Email
	}
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}
