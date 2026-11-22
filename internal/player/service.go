package player

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
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

	// guestTTL is the lifetime of a guest's token; zero means guest accounts
	// are off (see WithGuests).
	guestTTL time.Duration
	// now and guestName are replaced in tests.
	now       func() time.Time
	guestName func() (string, error)
}

// NewService wires the service's dependencies.
func NewService(repo Repository, sessions SessionStore, tokens *auth.TokenManager, log *zap.Logger, opts ...Option) *Service {
	s := &Service{
		repo: repo, sessions: sessions, tokens: tokens, log: log,
		now: time.Now, guestName: newGuestName,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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
	// Assign the ID up front (the model's BeforeCreate also would) so the event
	// payload can reference it before the INSERT round-trip.
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	e, err := events.New(events.TypePlayerRegistered, events.PlayerRegisteredPayload{
		PlayerID: p.ID.String(),
		Username: p.Username,
		Email:    p.Email,
	})
	if err != nil {
		return nil, err
	}
	// Business row + outbox event commit atomically: the PlayerRegistered event
	// can never be lost once registration succeeds (no dual-write).
	if err := s.repo.CreateWithOutbox(ctx, p, e, events.TopicPlayer); err != nil {
		return nil, err
	}
	s.log.Info("player registered", zap.String("player_id", p.ID.String()), zap.String("username", p.Username))
	return p, nil
}

// Login verifies credentials, issues a JWT and caches the session in Redis so
// it can be revoked before natural expiry.
//
// A guest account logs in here too, with its generated name and secret; that
// is how it renews its token. What it gets back is again a guest's token:
// the short lifetime and the guest claim follow the account, not the route.
// With guest accounts off, a guest account cannot log in.
func (s *Service) Login(ctx context.Context, identifier, password string) (*Session, error) {
	p, err := s.repo.GetByUsername(ctx, identifier)
	if errors.Is(err, ErrNotFound) {
		p, err = s.repo.GetByEmail(ctx, identifier)
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if !auth.CheckPassword(p.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}
	if p.IsGuest {
		if s.guestTTL <= 0 {
			return nil, ErrInvalidCredentials
		}
		return s.guestSession(ctx, p, true)
	}

	issued, err := s.tokens.Issue(p.ID, p.Username)
	if err != nil {
		return nil, err
	}
	if err := s.sessions.Save(ctx, issued.JTI, p.ID.String(), s.tokens.Expiry()); err != nil {
		// Session caching is best-effort: a Redis blip must not block logins.
		s.log.Warn("failed to cache session", zap.Error(err), zap.String("player_id", p.ID.String()))
	}
	s.log.Info("player logged in", zap.String("player_id", p.ID.String()))
	return &Session{Token: issued.Token, ExpiresAt: issued.ExpiresAt, Player: p}, nil
}

// Logout revokes the session for the given JTI.
// Logout revokes the session and announces it so open WebSocket connections
// opened with it are closed at once. The revocation is the Redis delete; if
// the announcement fails, the WebSocket service's periodic session sweep
// still closes those connections, so the logout itself succeeds.
func (s *Service) Logout(ctx context.Context, jti string) error {
	if err := s.sessions.Delete(ctx, jti); err != nil {
		return err
	}
	s.announceRevoked(ctx, jti)
	return nil
}

// announceRevoked tells the WebSocket service to close the connections
// opened with the session now. Best effort: its periodic session sweep closes
// them within seconds anyway.
func (s *Service) announceRevoked(ctx context.Context, jti string) {
	e, err := events.New(events.TypeSessionRevoked, events.SessionRevokedPayload{JTI: jti})
	if err == nil {
		err = s.repo.PublishEvent(ctx, e, events.TopicPlayer)
	}
	if err != nil {
		s.log.Warn("failed to announce session revocation", zap.Error(err))
	}
}

// DeleteAccount deletes the player for good: the row with its stats, every
// session, and (through the PlayerDeleted event, written in the row's own
// transaction) its entries on the leaderboards.
//
// The sessions are revoked even when the row is already gone, and a failure
// to revoke is returned: an attempt that deleted the row and then lost Redis
// is finished by repeating it, which answers ErrNotFound once the sessions
// are gone too.
func (s *Service) DeleteAccount(ctx context.Context, id uuid.UUID) error {
	e, err := deletedEvent(id)
	if err != nil {
		return err
	}
	deleteErr := s.repo.DeleteWithOutbox(ctx, id, e, events.TopicPlayer)
	if deleteErr != nil && !errors.Is(deleteErr, ErrNotFound) {
		return deleteErr
	}
	if err := s.revokeAll(ctx, id); err != nil {
		return err
	}
	if deleteErr != nil {
		return deleteErr
	}
	s.log.Info("account deleted", zap.String("player_id", id.String()))
	return nil
}

func deletedEvent(id uuid.UUID) (events.Event, error) {
	return events.New(events.TypePlayerDeleted, events.PlayerDeletedPayload{PlayerID: id.String()})
}

// revokeAll ends every session of the player.
func (s *Service) revokeAll(ctx context.Context, id uuid.UUID) error {
	jtis, err := s.sessions.DeleteAll(ctx, id.String())
	if err != nil {
		return err
	}
	for _, jti := range jtis {
		s.announceRevoked(ctx, jti)
	}
	return nil
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
	// A guest's name is the server's; letting it be changed would bring back
	// the moderation the generated names exist to avoid. The row decides,
	// whatever the caller's token says.
	if p.IsGuest {
		return nil, ErrGuestProfile
	}
	if in.Username != nil {
		p.Username = *in.Username
	}
	if in.Email != nil {
		p.Email = *in.Email
	}
	e, err := events.New(events.TypePlayerUpdated, events.PlayerUpdatedPayload{
		PlayerID: p.ID.String(),
		Username: p.Username,
		Email:    p.Email,
	})
	if err != nil {
		return nil, err
	}
	if err := s.repo.UpdateWithOutbox(ctx, p, e, events.TopicPlayer); err != nil {
		return nil, err
	}
	return p, nil
}
