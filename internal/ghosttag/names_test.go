package ghosttag

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snapNames returns the names in a snap, by player ID.
func snapNames(t *testing.T, snap map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, p := range snap["players"].([]any) {
		p := p.(map[string]any)
		require.Contains(t, p, "name", "every player in a snap has a name field")
		out[p["id"].(string)] = p["name"]
	}
	return out
}

// TestSessionNamesReachSnapAndStore: the names the transport knows for the
// players' connections are in the snaps, from the presence notice that
// brought each player in, and in the Match the store settles.
func TestSessionNamesReachSnapAndStore(t *testing.T) {
	tr := newFakeTransport("a", "b")
	tr.setName("a", "SwiftFox4821")
	tr.setName("b", "alice")
	store := &fakeStore{}
	rig := newRig(t, fastConfig(), pvp(), tr, store)
	rig.run(context.Background())
	rig.waitDone(t)

	snaps := tr.ofType("snap")
	require.NotEmpty(t, snaps)
	for i, s := range snaps {
		want := map[string]any{"a": "SwiftFox4821", "b": "alice"}
		if i == 0 {
			// The room heard of a first and is waiting for b.
			require.Equal(t, "WAITING", s.body["state"])
			want["b"] = ""
		}
		assert.Equal(t, want, snapNames(t, s.body), "snap %d, %v", i, s.body["state"])
	}
	assert.Greater(t, len(snaps), 4, "the snaps of a whole match")
	m := store.match.Load()
	require.NotNil(t, m)
	assert.Equal(t, map[string]string{"a": "SwiftFox4821", "b": "alice"}, m.Names)
}

// TestSessionBotHasAFixedName: in a bot match the bot is shown as BotName
// and the human by their own name.
func TestSessionBotHasAFixedName(t *testing.T) {
	tr := newFakeTransport("h")
	tr.setName("h", "QuietOwl9034")
	rig := newRig(t, fastConfig(), Spec{RoomID: "room-bot", Players: []string{"h"}, Bot: true}, tr, nil)
	rig.run(context.Background())
	rig.waitDone(t)

	snaps := tr.ofType("snap")
	require.NotEmpty(t, snaps)
	for _, s := range snaps {
		assert.Equal(t, map[string]any{"h": "QuietOwl9034", "bot": BotName}, snapNames(t, s.body))
	}
}

// TestRoomKeepsALearnedName: a name stays when the player's connection
// goes (the transport then knows none); a player not yet heard of has an
// empty one; the bot's cannot be replaced; a store sees only the names it
// knows.
func TestRoomKeepsALearnedName(t *testing.T) {
	h := newHarness(t, DefaultConfig(), "room-1", []string{"a", "b"}, "")
	h.r.setName("a", "SwiftFox4821")
	h.r.setPresence(at(ms(100)), "a", true)
	assert.Equal(t, map[string]any{"a": "SwiftFox4821", "b": ""}, snapNames(t, h.out.lastSnap(t)))

	h.r.setName("a", "")
	h.r.setPresence(at(ms(200)), "a", false)
	assert.Equal(t, map[string]any{"a": "SwiftFox4821", "b": ""}, snapNames(t, h.out.lastSnap(t)))
	assert.Equal(t, map[string]string{"a": "SwiftFox4821"}, h.r.names())

	bot := newHarness(t, DefaultConfig(), "room-2", []string{"h"}, "bot")
	bot.r.setName("bot", "mallory")
	bot.r.setPresence(at(ms(100)), "h", true)
	assert.Equal(t, BotName, snapNames(t, bot.out.lastSnap(t))["bot"])
	assert.Empty(t, bot.r.names(), "the bot's name is not the store's")
}
