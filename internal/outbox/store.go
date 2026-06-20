// Package outbox implements the transactional outbox pattern: domain events are
// persisted to the outbox_events table in the SAME database transaction as the
// business write, then a separate relay publishes committed rows to NATS. This
// makes the database the single source of truth for "an event must be sent" and
// eliminates the dual-write problem (state committed but event lost).
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Status values for an outbox row. A failed publish stays PENDING and is retried
// on the next poll, so a transient NATS outage self-heals with no operator
// action. A row that keeps failing past MaxAttempts is moved to FAILED (a
// dead-letter state): it is no longer polled, so a single poison row can never
// be retried forever and wedge a worker. FAILED rows are surfaced via metrics
// for an operator to inspect and replay.
const (
	StatusPending   = "PENDING"
	StatusPublished = "PUBLISHED"
	StatusFailed    = "FAILED"
)

// Event is the GORM model backing the outbox_events table. The struct fields
// mirror the migration in migrations/0003_create_outbox_events.up.sql. (Named
// Event rather than OutboxEvent to avoid the outbox.OutboxEvent stutter; it is
// distinct from events.Event, the wire format that flows over NATS.)
type Event struct {
	// ID equals events.Event.ID so the same identity flows DB -> NATS ->
	// consumer, giving consumers a natural dedup key with no extra columns.
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	EventType string    `gorm:"column:event_type;not null"`
	Topic     string    `gorm:"not null"`
	Payload   []byte    `gorm:"type:jsonb;not null"`
	// Carrier holds the W3C trace headers captured at write time so the relay
	// can continue the originating request's trace when it publishes later.
	Carrier      []byte     `gorm:"type:jsonb;not null;default:'{}'"`
	Status       string     `gorm:"not null;default:PENDING"`
	CreatedAt    time.Time  `gorm:"not null;default:now()"`
	PublishedAt  *time.Time `gorm:"column:published_at"`
	AttemptCount int        `gorm:"column:attempt_count;not null;default:0"`
	// LastError records the most recent publish failure, kept on FAILED rows so an
	// operator can see why a dead-lettered event never made it to NATS.
	LastError string `gorm:"column:last_error"`
}

// TableName pins the table name to match the SQL migrations.
func (Event) TableName() string { return "outbox_events" }

// Store provides access to outbox_events. The same Store type serves both the
// write side (Insert, on a caller-supplied tx) and the relay side (RunBatch,
// which claims/publishes/marks rows in one transaction, plus CountPending).
type Store struct {
	db *gorm.DB
}

// NewStore returns a Store bound to the given GORM handle.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Insert writes a single outbox row using the provided transaction handle. It
// MUST be called inside the business transaction (db.Transaction(...)) so the
// row commits atomically with the business state — that atomicity is the whole
// point of the pattern. The carrier map is JSON-encoded for the jsonb column.
func (s *Store) Insert(ctx context.Context, tx *gorm.DB, id uuid.UUID, eventType, topic string, payload []byte, carrier map[string]string) error {
	carrierJSON, err := json.Marshal(carrier)
	if err != nil {
		return err
	}
	if len(carrierJSON) == 0 {
		carrierJSON = []byte("{}")
	}
	row := Event{
		ID:        id,
		EventType: eventType,
		Topic:     topic,
		Payload:   payload,
		Carrier:   carrierJSON,
		Status:    StatusPending,
	}
	return tx.WithContext(ctx).Create(&row).Error
}

// PublishFunc publishes a batch of polled rows and reports which succeeded and
// which failed. The relay supplies this; the store calls it inside the batch
// transaction so the rows stay locked (FOR UPDATE SKIP LOCKED) until their
// status is updated.
type PublishFunc func(ctx context.Context, rows []Event) (published, failed []uuid.UUID, err error)

// RunBatch is the relay's poll → publish → mark cycle, all inside ONE
// transaction. It locks up to limit of the oldest PENDING rows with
// FOR UPDATE SKIP LOCKED (so concurrent relay replicas never grab the same
// rows), hands them to publish, then marks the successful ones PUBLISHED and
// bumps attempt_count on the failures. A failure whose bumped attempt_count
// reaches maxAttempts is moved to FAILED (dead-lettered) instead of staying
// PENDING, so a poison row stops being retried; maxAttempts <= 0 disables this
// and rows retry forever (the original behaviour). Returns the number of rows
// polled and the number dead-lettered in this batch. Keeping the transaction
// logic here is the only place that knows about gorm, which keeps the relay
// unit-testable behind the Batcher interface.
func (s *Store) RunBatch(ctx context.Context, limit, maxAttempts int, publish PublishFunc) (polledN, deadLetteredN int, err error) {
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []Event
		if err := tx.
			Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", StatusPending).
			Order("created_at ASC").
			Limit(limit).
			Find(&rows).Error; err != nil {
			return err
		}
		polledN = len(rows)
		if polledN == 0 {
			return nil
		}

		published, failed, err := publish(ctx, rows)
		if err != nil {
			return err
		}

		markCtx, span := tracing.Tracer().Start(ctx, "outbox.mark_published",
			trace.WithAttributes(attribute.Int("outbox.published", len(published)), attribute.Int("outbox.failed", len(failed))))
		defer span.End()
		if err := markPublished(markCtx, tx, published); err != nil {
			tracing.RecordError(span, err)
			return err
		}
		dead, err := incrementAttempt(markCtx, tx, failed, maxAttempts)
		if err != nil {
			tracing.RecordError(span, err)
			return err
		}
		deadLetteredN = dead
		span.SetAttributes(attribute.Int("outbox.dead_lettered", dead))
		return nil
	})
	if txErr != nil {
		// A failed transaction commits nothing, so no rows were dead-lettered.
		return polledN, 0, txErr
	}
	return polledN, deadLetteredN, nil
}

func markPublished(ctx context.Context, tx *gorm.DB, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return tx.WithContext(ctx).
		Model(&Event{}).
		Where("id IN ?", ids).
		Updates(map[string]any{"status": StatusPublished, "published_at": now}).Error
}

// incrementAttempt bumps attempt_count on every failed row and, when maxAttempts
// is positive, dead-letters the rows whose bumped count reaches it: those flip to
// FAILED so the PENDING-only poll skips them on the next cycle. Both updates run
// in one statement via a CASE so the bump and the status flip stay atomic and a
// row is never read back between them. Returns the number of rows dead-lettered.
func incrementAttempt(ctx context.Context, tx *gorm.DB, ids []uuid.UUID, maxAttempts int) (deadLettered int, err error) {
	if len(ids) == 0 {
		return 0, nil
	}

	updates := map[string]any{
		"attempt_count": gorm.Expr("attempt_count + 1"),
	}
	// maxAttempts <= 0 disables dead-lettering: rows just keep retrying (the
	// original self-healing behaviour for indefinite transient outages).
	if maxAttempts > 0 {
		updates["status"] = gorm.Expr(
			"CASE WHEN attempt_count + 1 >= ? THEN ? ELSE status END",
			maxAttempts, StatusFailed,
		)
		updates["last_error"] = gorm.Expr(
			"CASE WHEN attempt_count + 1 >= ? THEN ? ELSE last_error END",
			maxAttempts, "publish failed: max attempts exceeded",
		)
	}

	if err := tx.WithContext(ctx).
		Model(&Event{}).
		Where("id IN ?", ids).
		Updates(updates).Error; err != nil {
		return 0, err
	}

	if maxAttempts <= 0 {
		return 0, nil
	}
	// Count how many of these rows ended up FAILED so the relay can record the
	// dead-letter metric. Scoped to the just-touched ids inside the same tx.
	var n int64
	if err := tx.WithContext(ctx).
		Model(&Event{}).
		Where("id IN ? AND status = ?", ids, StatusFailed).
		Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}

// CountPending returns the number of PENDING rows, used to publish the backlog
// gauge each poll tick.
func (s *Store) CountPending(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).
		Model(&Event{}).
		Where("status = ?", StatusPending).
		Count(&n).Error
	return n, err
}

// DB exposes the underlying handle so the relay can open its own transaction
// around a poll/publish/mark cycle.
func (s *Store) DB() *gorm.DB { return s.db }
