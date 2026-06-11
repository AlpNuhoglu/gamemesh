// Package player implements registration, login and profile management.
package player

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Player is the core identity entity. PasswordHash is never serialised.
type Player struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	// "unique" (constraint) rather than "uniqueIndex": GORM names these
	// uni_players_*, which the SQL migrations mirror so AutoMigrate is a
	// no-op when the schema was applied via schema.sql first.
	Username     string       `gorm:"size:32;not null;unique" json:"username"`
	Email        string       `gorm:"size:255;not null;unique" json:"email"`
	PasswordHash string       `gorm:"size:255;not null" json:"-"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
	Stats        *PlayerStats `gorm:"foreignKey:PlayerID;constraint:OnDelete:CASCADE" json:"stats,omitempty"`
}

// BeforeCreate assigns a UUID client-side so the ID is known before the
// INSERT round-trip (and works on databases without uuid extensions).
func (p *Player) BeforeCreate(_ *gorm.DB) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return nil
}

// PlayerStats holds gameplay-derived state, separated from identity so that
// hot writes (rank/score changes) never contend with the players row.
type PlayerStats struct {
	PlayerID    uuid.UUID `gorm:"type:uuid;primaryKey" json:"player_id"`
	Rank        int       `gorm:"not null;default:1000;index" json:"rank"`
	Score       int64     `gorm:"not null;default:0" json:"score"`
	GamesPlayed int       `gorm:"not null;default:0" json:"games_played"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TableName pins the table name to match the SQL migrations.
func (PlayerStats) TableName() string { return "player_stats" }
