package ghosttag

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultConfigIsSection8: the defaults are the table of section 8, and
// they pass validation.
func TestDefaultConfigIsSection8(t *testing.T) {
	c := DefaultConfig()
	require.NoError(t, c.Validate())

	assert.Equal(t, 260.0, c.VRunner)
	assert.Equal(t, 225.0, c.VHunter)
	assert.Equal(t, 4.2, c.OmegaMax)
	assert.Equal(t, 0.45, c.Alpha)
	assert.Equal(t, 500.0, c.R0)
	assert.Equal(t, 280.0, c.RMin)
	assert.Equal(t, 36.0, c.DCapture)
	assert.Equal(t, 25.0, c.SlackMax)
	assert.Equal(t, 1e-3, c.MoveEpsilon)
	assert.Equal(t, 250*time.Millisecond, c.DtMax)
	assert.Equal(t, 40*time.Millisecond, c.DtInitial)
	assert.Equal(t, 60*time.Millisecond, c.ClientRenderDelay)
	assert.Equal(t, 80*time.Millisecond, c.ClientDeadReckoning)
	assert.Equal(t, 45*time.Second, c.THalf)
	assert.Equal(t, 15*time.Second, c.TShrink)
	assert.Equal(t, 2*time.Second, c.TCountdown)
	assert.Equal(t, 3*time.Second, c.TRoleSwap)
	assert.Equal(t, 5*time.Second, c.TGrace)
	assert.Equal(t, 15*time.Second, c.TGraceTotal)
	assert.Equal(t, 10*time.Second, c.TJoin)
	assert.Equal(t, 150*time.Millisecond, c.CaptureWindow)
	assert.Equal(t, 2000*time.Millisecond, c.BotSightDelay)
	assert.Equal(t, 200*time.Millisecond, c.PressureInterval)
	assert.Equal(t, 400*time.Millisecond, c.ClampInterval)
	assert.Equal(t, 40*time.Millisecond, c.Tick)
}

// TestConfigValidateRejects: α outside (0, 1), RMin outside (0, R0] and the
// other values that would make the game meaningless are refused, NaN
// included.
func TestConfigValidateRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"alpha zero":            func(c *Config) { c.Alpha = 0 },
		"alpha one":             func(c *Config) { c.Alpha = 1 },
		"alpha above one":       func(c *Config) { c.Alpha = 1.2 },
		"alpha negative":        func(c *Config) { c.Alpha = -0.1 },
		"alpha NaN":             func(c *Config) { c.Alpha = math.NaN() },
		"rmin zero":             func(c *Config) { c.RMin = 0 },
		"rmin above r0":         func(c *Config) { c.RMin = c.R0 + 1 },
		"rmin NaN":              func(c *Config) { c.RMin = math.NaN() },
		"runner speed zero":     func(c *Config) { c.VRunner = 0 },
		"hunter speed NaN":      func(c *Config) { c.VHunter = math.NaN() },
		"omega zero":            func(c *Config) { c.OmegaMax = 0 },
		"capture zero":          func(c *Config) { c.DCapture = 0 },
		"slack negative":        func(c *Config) { c.SlackMax = -1 },
		"dtinitial zero":        func(c *Config) { c.DtInitial = 0 },
		"move epsilon negative": func(c *Config) { c.MoveEpsilon = -1e-3 },
		"dtinitial above dtmax": func(c *Config) { c.DtInitial = c.DtMax + time.Millisecond },
		"shrink longer than half": func(c *Config) {
			c.TShrink = c.THalf + time.Second
		},
		"tick zero":               func(c *Config) { c.Tick = 0 },
		"spawn outside arena":     func(c *Config) { c.SpawnDistance = c.R0 },
		"spawns within capture":   func(c *Config) { c.SpawnDistance = c.DCapture / 4 },
		"ring too small":          func(c *Config) { c.RingSize = 1 },
		"ring shorter than sight": func(c *Config) { c.RingSize = 50 },
		"grace total under grace": func(c *Config) { c.TGraceTotal = c.TGrace - time.Millisecond },
		"inbox zero":              func(c *Config) { c.InboxSize = 0 },
		"reaction min above max":  func(c *Config) { c.BotReactionMin = c.BotReactionMax + time.Millisecond },
		"settle timeout zero":     func(c *Config) { c.SettleTimeout = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := DefaultConfig()
			mutate(&c)
			assert.Error(t, c.Validate())
		})
	}
}

// TestConfigValidateAcceptsEdges: RMin equal to R0 (no shrink) is allowed.
func TestConfigValidateAcceptsEdges(t *testing.T) {
	c := DefaultConfig()
	c.RMin = c.R0
	assert.NoError(t, c.Validate())
}
