package player

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
)

// The repository's CRUD paths run against real PostgreSQL in
// tests/integration; here we cover the driver-error translation that turns
// database errors into domain errors.
func TestTranslate(t *testing.T) {
	assert.NoError(t, translate(nil))

	assert.ErrorIs(t, translate(gorm.ErrRecordNotFound), ErrNotFound)

	uniqueViolation := &pgconn.PgError{Code: "23505"}
	assert.ErrorIs(t, translate(uniqueViolation), ErrDuplicate)

	otherPgErr := &pgconn.PgError{Code: "23503"} // FK violation passes through
	assert.NotErrorIs(t, translate(otherPgErr), ErrDuplicate)

	plain := errors.New("connection refused")
	assert.Equal(t, plain, translate(plain))
}
