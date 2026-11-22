package ghosttag

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// distinctConfig sets every Config value snap.cfg comes from to a value of
// its own, none of them the default, so a field read from the wrong
// source shows up as a wrong number.
func distinctConfig() Config {
	c := DefaultConfig()
	c.VRunner = 263
	c.VHunter = 227
	c.OmegaMax = 4.4
	c.Alpha = 0.47
	c.R0 = 503
	c.RMin = 283
	c.DCapture = 37
	c.SlackMax = 26
	c.MoveEpsilon = 0.002
	c.DtMax = 251 * time.Millisecond
	c.THalf = 45001 * time.Millisecond
	c.TShrink = 15002 * time.Millisecond
	c.Tick = 41 * time.Millisecond
	c.TrailDelay = 2003 * time.Millisecond
	c.ClientRenderDelay = 61 * time.Millisecond
	c.ClientDeadReckoning = 82 * time.Millisecond
	return c
}

// TestSnapCarriesClientConfig: every snap, whatever the state, carries
// cfg, and each of its fields is the Config value it comes from, by name
// and in milliseconds for a duration. Nothing else is in it.
func TestSnapCarriesClientConfig(t *testing.T) {
	h := newHarness(t, distinctConfig(), "room-1", []string{"a", "b"}, "")
	h.r.setPresence(at(ms(100)), "a", true) // WAITING
	h.started()                             // COUNTDOWN_1, HALF_1_ACTIVE

	want := map[string]any{
		"v_runner": 263.0, "v_hunter": 227.0, "omega_max": 4.4, "alpha": 0.47,
		"r0": 503.0, "r_min": 283.0, "d_capture": 37.0, "slack_max": 26.0,
		"move_eps": 0.002, "dt_max_ms": 251.0, "t_half_ms": 45001.0,
		"t_shrink_ms": 15002.0, "send_ms": 41.0, "trail_ms": 2003.0,
		"interp_ms": 61.0, "dr_ms": 82.0,
	}
	snaps := h.out.ofType("snap")
	states := map[any]bool{}
	for _, s := range snaps {
		states[s.body["state"]] = true
		assert.Equal(t, want, s.body["cfg"], "cfg of the %v snap", s.body["state"])
	}
	assert.Equal(t, map[any]bool{"WAITING": true, "COUNTDOWN_1": true, "HALF_1_ACTIVE": true}, states)
}

// TestSnapConfigDefaultsAreSection6: with the default Config, cfg is the
// example of section 6, byte for byte.
func TestSnapConfigDefaultsAreSection6(t *testing.T) {
	c := DefaultConfig()
	b, err := json.Marshal(clientConfigOf(&c))
	require.NoError(t, err)
	assert.Equal(t, `{"v_runner":260,"v_hunter":225,"omega_max":4.2,"alpha":0.45,`+
		`"r0":500,"r_min":280,"d_capture":36,"slack_max":25,"move_eps":0.001,`+
		`"dt_max_ms":250,"t_half_ms":45000,"t_shrink_ms":15000,"send_ms":40,`+
		`"trail_ms":2000,"interp_ms":60,"dr_ms":80}`, string(b))
}

// TestSnapFitsTheMessageLimit: a snap with real (36-character) player IDs,
// two names as long as the player service allows a username (32
// characters) and every number at its widest stays under the gateway's
// 1024 bytes (section 6). The size is logged.
func TestSnapFitsTheMessageLimit(t *testing.T) {
	a, b := "0b9f8e2c-5a41-4c7d-9e3f-1d2c3b4a5f60", "7e6d5c4b-3a29-4f18-8e07-f6e5d4c3b2a1"
	h := newHarness(t, DefaultConfig(), "room-1", []string{a, b}, "")
	h.r.setName(a, "SwiftFox4821SwiftFox4821SwiftFox")
	h.r.setName(b, "QuietOwl9034QuietOwl9034QuietOwl")
	h.r.setPresence(at(time.Second), a, true)
	h.r.setPresence(at(time.Second), b, true)
	h.r.tick(at(time.Second + h.cfg.TCountdown))
	require.Equal(t, stateHalf1, h.r.state)
	for _, p := range h.r.players {
		p.mot.pos, p.mot.theta = vec{-353.553391, -353.553391}, -math.Pi+1e-5
	}
	// Dropped while the arena shrinks: the longest state name, a radius
	// with decimals.
	drop := at(time.Second + h.cfg.TCountdown + 37777*time.Millisecond)
	h.r.setPresence(drop, b, false)
	require.Equal(t, stateGrace, h.r.state)

	snap := h.r.snap(drop.Add(ms(3)))
	require.Contains(t, string(snap), `"name":"QuietOwl9034QuietOwl9034QuietOwl"`)
	t.Logf("snap: %d bytes: %s", len(snap), snap)
	assert.Less(t, len(snap), 1024)
}
