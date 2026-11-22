package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// channelSub is a transport without acknowledgements.
type channelSub struct {
	ch     chan Event
	topics []string
	err    error
}

func (s *channelSub) Subscribe(_ context.Context, topics ...string) (<-chan Event, error) {
	s.topics = topics
	return s.ch, s.err
}
func (s *channelSub) Close() error { return nil }

// ackSub is a transport with them: it hands the handler one event and
// records what the handler answered, as JetStream would to decide on an ACK.
type ackSub struct {
	channelSub
	event    Event
	answered chan error
}

func (s *ackSub) SubscribeAck(ctx context.Context, handler Handler, topics ...string) error {
	s.topics = topics
	s.answered <- handler(ctx, s.event)
	<-ctx.Done()
	return nil
}

func TestConsumeUsesAcknowledgementsWhenTheTransportHasThem(t *testing.T) {
	boom := errors.New("redis down")
	sub := &ackSub{event: Event{Type: TypePlayerDeleted}, answered: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Consume(ctx, sub, func(_ context.Context, e Event) error {
			assert.Equal(t, TypePlayerDeleted, e.Type)
			return boom
		}, TopicPlayer)
	}()

	select {
	case err := <-sub.answered:
		assert.ErrorIs(t, err, boom, "the transport was not told the handler failed, so it would not redeliver")
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was never called")
	}
	assert.Equal(t, []string{TopicPlayer}, sub.topics)
	cancel()
	require.NoError(t, <-done)
}

func TestConsumeFallsBackToTheChannel(t *testing.T) {
	sub := &channelSub{ch: make(chan Event, 2)}
	handled := make(chan string, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Consume(ctx, sub, func(_ context.Context, e Event) error {
			handled <- e.Type
			return errors.New("ignored: this transport cannot redeliver")
		}, TopicPlayer)
	}()

	// A handler error does not stop the loop: the second event still arrives.
	sub.ch <- Event{Type: TypePlayerDeleted}
	sub.ch <- Event{Type: TypePlayerUpdated}
	for _, want := range []string{TypePlayerDeleted, TypePlayerUpdated} {
		select {
		case got := <-handled:
			assert.Equal(t, want, got)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not delivered", want)
		}
	}
	cancel()
	require.NoError(t, <-done)
}

func TestConsumeReportsASubscribeFailure(t *testing.T) {
	boom := errors.New("no broker")
	err := Consume(context.Background(), &channelSub{err: boom}, func(context.Context, Event) error { return nil }, TopicPlayer)
	assert.ErrorIs(t, err, boom)
}
