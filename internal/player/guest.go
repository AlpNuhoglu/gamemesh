package player

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// A guest account is an ordinary account whose username and password the
// server generated. There is no second identity system: the guest renews its
// token through the same login as everyone else, with the generated password
// (the "secret") the creation answer gave it once.
//
// Why a short token and a long-lived secret: a browser sends the token in
// the WebSocket URL, where a proxy may log it, so it lives two hours. The
// secret only ever travels in a POST body.
//
// The secret is stored as a bcrypt hash, like a password. bcrypt is more
// than a random 256-bit value needs (there is nothing to slow down a guess
// of); it is used so that renewal is the existing login path and not new
// verification code. If renewal volume ever matters, this is the knob: a fast
// hash with a constant-time comparison for guest secrets.

const (
	// guestSecretBytes of crypto/rand make the secret. Encoded as unpadded
	// base64url they are 43 characters, inside bcrypt's 72-byte limit.
	guestSecretBytes = 32
	// guestEmailDomain: a guest has no e-mail, but the column is unique and
	// not null, so the row carries "<player id>@guest.invalid". ".invalid" is
	// reserved (RFC 2606); no real address can collide with it.
	guestEmailDomain = "guest.invalid"
	// guestNameAttempts bounds the retries when a generated name is taken.
	guestNameAttempts = 5
)

var (
	// ErrGuestsDisabled means guest accounts are not turned on in this service.
	ErrGuestsDisabled = errors.New("guest accounts are disabled")
	// ErrGuestName means every generated name was already taken.
	ErrGuestName = errors.New("could not find a free guest name")
	// ErrGuestProfile means a guest tried to change its generated name or to set
	// an e-mail address.
	ErrGuestProfile = errors.New("a guest account cannot change its profile")
)

// Session is a token and the player it was issued to.
type Session struct {
	Token     string
	ExpiresAt time.Time
	Player    *Player
}

// GuestSession is a new guest's first session, with the secret that renews
// it. The secret is not stored in clear and cannot be asked for again.
type GuestSession struct {
	Session
	Secret string
}

// Option configures a Service.
type Option func(*Service)

// WithGuests turns guest accounts on; ttl is the lifetime of a guest's
// token. Without it CreateGuest fails and a guest account cannot log in.
func WithGuests(ttl time.Duration) Option {
	return func(s *Service) { s.guestTTL = ttl }
}

// CreateGuest makes a guest account and logs it in.
func (s *Service) CreateGuest(ctx context.Context) (*GuestSession, error) {
	if s.guestTTL <= 0 {
		return nil, ErrGuestsDisabled
	}
	secret, err := newGuestSecret()
	if err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(secret)
	if err != nil {
		return nil, err
	}

	var p *Player
	for attempt := 0; ; attempt++ {
		if attempt == guestNameAttempts {
			return nil, ErrGuestName
		}
		name, err := s.guestName()
		if err != nil {
			return nil, err
		}
		id := uuid.New()
		now := s.now()
		p = &Player{
			ID:           id,
			Username:     name,
			Email:        id.String() + "@" + guestEmailDomain,
			PasswordHash: hash,
			IsGuest:      true,
			LastSeenAt:   &now,
			Stats:        &Stats{Rank: 1000},
		}
		e, err := events.New(events.TypePlayerRegistered, events.PlayerRegisteredPayload{
			PlayerID: id.String(),
			Username: name,
			Guest:    true,
		})
		if err != nil {
			return nil, err
		}
		err = s.repo.CreateWithOutbox(ctx, p, e, events.TopicPlayer)
		if err == nil {
			break
		}
		// The id is new every time, so a duplicate is the name.
		if !errors.Is(err, ErrDuplicate) {
			return nil, err
		}
	}

	session, err := s.guestSession(ctx, p, false)
	if err != nil {
		return nil, err
	}
	s.log.Info("guest account created", zap.String("player_id", p.ID.String()), zap.String("username", p.Username))
	return &GuestSession{Session: *session, Secret: secret}, nil
}

// guestSession issues a guest token for p and records its session. touch is
// false for a row that was written a moment ago with last_seen_at set.
func (s *Service) guestSession(ctx context.Context, p *Player, touch bool) (*Session, error) {
	issued, err := s.tokens.GenerateGuest(p.ID, p.Username, s.guestTTL)
	if err != nil {
		return nil, err
	}
	if err := s.sessions.Save(ctx, issued.JTI, p.ID.String(), s.guestTTL); err != nil {
		// Best effort, as for a registered login.
		s.log.Warn("failed to cache session", zap.Error(err), zap.String("player_id", p.ID.String()))
	}
	if touch {
		now := s.now()
		if err := s.repo.TouchLastSeen(ctx, p.ID, now); err != nil {
			// The token is good either way. What is at stake is the sweep
			// taking an active guest for an idle one, 30 days from now.
			s.log.Warn("failed to record guest activity", zap.Error(err), zap.String("player_id", p.ID.String()))
		} else {
			p.LastSeenAt = &now
		}
	}
	return &Session{Token: issued.Token, ExpiresAt: issued.ExpiresAt, Player: p}, nil
}

// newGuestSecret returns guestSecretBytes of randomness as unpadded
// base64url.
func newGuestSecret() (string, error) {
	raw := make([]byte, guestSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
