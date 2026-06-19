package outbox

import (
	"context"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Publisher implements events.Publisher by writing the event to the outbox_events
// table instead of sending it to NATS. Because it inserts on a transaction handle
// supplied by the caller, the row commits atomically with the business write.
//
// Service code keeps depending only on events.Publisher, so swapping a direct
// NATS publisher for this one requires no change to business logic — the
// reliability guarantee is purely an infrastructure concern.
type Publisher struct {
	store *Store
	// tx is the transaction handle the row is written on. A request-scoped
	// Publisher is created via WithTx inside db.Transaction so the insert shares
	// the business transaction. The zero-tx Publisher (store's db) is only used
	// for tests/standalone inserts.
	tx *gorm.DB
}

// NewPublisher returns a Publisher bound to a store. Callers wrap a transaction
// with WithTx before publishing so the insert joins the business transaction.
func NewPublisher(store *Store) *Publisher {
	return &Publisher{store: store, tx: store.DB()}
}

// WithTx returns a Publisher that writes on the given transaction handle. Used
// inside db.Transaction(func(tx *gorm.DB) error {...}) so the outbox insert and
// the business writes commit together (or roll back together).
func (p *Publisher) WithTx(tx *gorm.DB) *Publisher {
	return &Publisher{store: p.store, tx: tx}
}

// Publish writes the event to the outbox. It captures the current trace context
// into the event's Carrier (mirroring NATSBus.Publish) so the relay can resume
// the originating request's trace when it later publishes to NATS. This does NOT
// talk to NATS — the relay does that asynchronously after the transaction commits.
func (p *Publisher) Publish(ctx context.Context, topic string, e events.Event) error {
	ctx, span := tracing.Tracer().Start(ctx, "outbox.insert "+topic,
		trace.WithAttributes(
			attribute.String("messaging.event.type", e.Type),
			attribute.String("messaging.destination.name", topic),
		),
	)
	defer span.End()

	e.Carrier = tracing.InjectCarrier(ctx, e.Carrier)

	id, err := uuid.Parse(e.ID)
	if err != nil {
		// Event.ID is always a generated UUID (events.New); fall back to a fresh
		// one rather than failing the business transaction on a malformed ID.
		id = uuid.New()
	}

	err = p.store.Insert(ctx, p.tx, id, e.Type, topic, e.Payload, e.Carrier)
	tracing.RecordError(span, err)
	return err
}

// Close satisfies events.Publisher. The DB lifecycle is owned by main, so this
// is a no-op.
func (p *Publisher) Close() error { return nil }
