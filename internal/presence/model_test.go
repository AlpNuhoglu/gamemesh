package presence

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanTransition_RejectsOnlyOfflineToActive(t *testing.T) {
	// The only rejected transitions: an offline player (no connection) cannot
	// jump straight into queue or match.
	assert.False(t, CanTransition(StateOffline, StateInQueue))
	assert.False(t, CanTransition(StateOffline, StateInMatch))

	// Everything else is permitted so future reconnect/rematch/party flows do
	// not need to touch the state machine.
	allowed := [][2]State{
		{StateOffline, StateOnline},
		{StateOffline, StateAway},
		{StateOnline, StateInQueue},
		{StateOnline, StateInMatch},
		{StateOnline, StateAway},
		{StateOnline, StateOffline},
		{StateInQueue, StateInMatch},
		{StateInQueue, StateOnline},
		{StateInQueue, StateAway},
		{StateInMatch, StateOnline},
		{StateInMatch, StateAway},
		{StateInMatch, StateOffline},
		{StateAway, StateInQueue},
		{StateAway, StateOnline},
	}
	for _, tc := range allowed {
		assert.Truef(t, CanTransition(tc[0], tc[1]), "expected %s->%s allowed", tc[0], tc[1])
	}
}

func TestCanTransition_RejectsUnknownTargetState(t *testing.T) {
	assert.False(t, CanTransition(StateOnline, State("BOGUS")))
}

func TestStateValid(t *testing.T) {
	for _, s := range []State{StateOffline, StateOnline, StateInQueue, StateInMatch, StateAway} {
		assert.True(t, s.Valid())
	}
	assert.False(t, State("NOPE").Valid())
	assert.False(t, State("").Valid())
}
