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
	jwt.RegisteredClaims
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
	now := time.Now()
	jti = uuid.NewString()
	claims := Claims{
		PlayerID: playerID.String(),
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Subject:   playerID.String(),
			Issuer:    m.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.expiry)),
		},
	}
	token, err = jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	return token, jti, err
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
