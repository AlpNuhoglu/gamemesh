package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateAndValidate(t *testing.T) {
	tm := NewTokenManager("test-secret", time.Hour, "gamemesh")
	playerID := uuid.New()

	token, jti, err := tm.Generate(playerID, "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.NotEmpty(t, jti)

	claims, err := tm.Validate(token)
	require.NoError(t, err)
	assert.Equal(t, playerID.String(), claims.PlayerID)
	assert.Equal(t, "alice", claims.Username)
	assert.Equal(t, jti, claims.ID)
	assert.Equal(t, "gamemesh", claims.Issuer)
}

func TestValidateExpiredToken(t *testing.T) {
	tm := NewTokenManager("test-secret", -time.Minute, "gamemesh")
	token, _, err := tm.Generate(uuid.New(), "alice")
	require.NoError(t, err)

	_, err = tm.Validate(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestValidateWrongSecret(t *testing.T) {
	issuer := NewTokenManager("secret-a", time.Hour, "gamemesh")
	verifier := NewTokenManager("secret-b", time.Hour, "gamemesh")

	token, _, err := issuer.Generate(uuid.New(), "alice")
	require.NoError(t, err)

	_, err = verifier.Validate(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestValidateWrongIssuer(t *testing.T) {
	issuer := NewTokenManager("test-secret", time.Hour, "other-system")
	verifier := NewTokenManager("test-secret", time.Hour, "gamemesh")

	token, _, err := issuer.Generate(uuid.New(), "alice")
	require.NoError(t, err)

	_, err = verifier.Validate(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestValidateGarbage(t *testing.T) {
	tm := NewTokenManager("test-secret", time.Hour, "gamemesh")
	for _, tok := range []string{"", "not-a-jwt", "a.b.c"} {
		_, err := tm.Validate(tok)
		assert.ErrorIs(t, err, ErrInvalidToken)
	}
}
