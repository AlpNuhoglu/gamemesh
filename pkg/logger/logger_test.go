package logger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDevelopmentAndProduction(t *testing.T) {
	dev, err := New("test", "development")
	require.NoError(t, err)
	assert.NotNil(t, dev)

	prod, err := New("test", "production")
	require.NoError(t, err)
	assert.NotNil(t, prod)
}

func TestMust(t *testing.T) {
	assert.NotPanics(t, func() { Must("test", "development") })
}
