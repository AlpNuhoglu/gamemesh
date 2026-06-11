package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.NotEqual(t, "correct horse battery staple", hash)

	assert.True(t, CheckPassword(hash, "correct horse battery staple"))
	assert.False(t, CheckPassword(hash, "wrong password"))
	assert.False(t, CheckPassword("not-a-hash", "anything"))
}

func TestHashIsSalted(t *testing.T) {
	h1, err := HashPassword("same-password")
	require.NoError(t, err)
	h2, err := HashPassword("same-password")
	require.NoError(t, err)
	assert.NotEqual(t, h1, h2, "bcrypt must salt: identical inputs produce distinct hashes")
}
