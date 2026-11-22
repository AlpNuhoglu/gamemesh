// Package auth implements JWT issuing/validation and password hashing.
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidToken is returned for any token that fails validation; callers
// must not leak the underlying parse error to clients.
var ErrInvalidToken = errors.New("invalid or expired token")

// Claims carries the player identity inside the JWT.
type Claims struct {
	PlayerID string `json:"pid"`
	Username string `json:"username"`
	// Guest is true in a token issued to a guest account. It lets the
	// gateway refuse a guest early on the routes guests may not use; the
	// player's row in the database, not this claim, is what says an account
	// is a guest.
	Guest bool `json:"guest,omitempty"`
	jwt.RegisteredClaims
}

// Issued is a signed token and what its issuer needs to know about it.
type Issued struct {
	Token string
	// JTI keys the session, so it can be revoked before the token expires.
	JTI       string
	ExpiresAt time.Time
}

// TokenManager issues and validates HS256-signed JWTs.
type TokenManager struct {
	secret []byte
	expiry time.Duration
	issuer string
}

// NewTokenManager constructs a TokenManager. The secret must come from the
// environment, never from source code.
func NewTokenManager(secret string, expiry time.Duration, issuer string) *TokenManager {
	return &TokenManager{secret: []byte(secret), expiry: expiry, issuer: issuer}
}

// Generate returns a signed token plus its JTI (used as the session-cache key
// so individual sessions can be revoked before the JWT itself expires).
func (m *TokenManager) Generate(playerID uuid.UUID, username string) (token, jti string, err error) {
	out, err := m.issue(playerID, username, false, m.expiry)
	return out.Token, out.JTI, err
}

// GenerateGuest returns a token for a guest account: it carries the guest
// claim and lives for ttl instead of the manager's own lifetime.
func (m *TokenManager) GenerateGuest(playerID uuid.UUID, username string, ttl time.Duration) (Issued, error) {
	return m.issue(playerID, username, true, ttl)
}

func (m *TokenManager) issue(playerID uuid.UUID, username string, guest bool, ttl time.Duration) (Issued, error) {
	now := time.Now()
	out := Issued{JTI: uuid.NewString(), ExpiresAt: now.Add(ttl)}
	claims := Claims{
		PlayerID: playerID.String(),
		Username: username,
		Guest:    guest,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        out.JTI,
			Subject:   playerID.String(),
			Issuer:    m.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(out.ExpiresAt),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return Issued{}, err
	}
	out.Token = token
	return out, nil
}

// Validate parses and verifies a token, returning its claims.
func (m *TokenManager) Validate(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims,
		func(_ *jwt.Token) (any, error) { return m.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(m.issuer),
	)
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// Expiry exposes the configured token lifetime (used to TTL session entries).
func (m *TokenManager) Expiry() time.Duration { return m.expiry }
