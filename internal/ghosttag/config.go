package ghosttag

import (
	"errors"
	"fmt"
	"time"
)

// Config holds every tunable of the game. The first block is the table of
// section 8 of the design document; the rest are the server's own knobs. A
// value is never written into the code that uses it, so playtesting changes
// a Config and nothing else.
//
// Distances are arena units, speeds units per second, angles radians.
type Config struct {
	// VRunner is the runner's top speed (V_kaçak).
	VRunner float64
	// VHunter is the hunter's top speed (V_avcı).
	VHunter float64
	// OmegaMax is the runner's top turn rate (ω_max); the hunter's is
	// unbounded.
	OmegaMax float64
	// Alpha is the turn speed penalty (α): a runner turning by Δθ in one step
	// may move at VRunner·(1 − Alpha·|Δθ|/π). It must lie strictly inside
	// (0, 1): at 1 a half turn stops the runner, above 1 the speed goes
	// negative.
	Alpha float64
	// R0 is the arena radius until the shrink starts.
	R0 float64
	// RMin is the arena radius at the end of a half.
	RMin float64
	// DCapture is the capture radius.
	DCapture float64
	// SlackMax is the cap of the slack budget (S_max), the network jitter
	// tolerance. It is not added to every packet: a player's slack fills
	// with the distance they were allowed but did not cover, up to SlackMax,
	// and is spent when a packet asks for more (section 1.2, step 3). Over
	// any T seconds a player covers at most V·T + SlackMax, whatever the
	// packet rate.
	SlackMax float64
	// DtMax caps the validation's time step, so a backlog of packets cannot
	// teleport. The step is otherwise the real elapsed time, never raised,
	// for the distance budget and the runner's turn limit alike (section
	// 1.2, step 1): the real intervals of one second add up to one second,
	// so neither distance nor turn depends on the packet rate.
	DtMax time.Duration
	// MoveEpsilon is ε_hareket: a requested displacement shorter than this
	// counts as standing still, and no heading is derived from it. The
	// angle of a sub-millimetre displacement is noise.
	MoveEpsilon float64
	// DtInitial is the time step of the first packet after a spawn or a
	// return from DISCONNECT_GRACE, when there is no previous packet to
	// measure from.
	DtInitial time.Duration
	// ClientRenderDelay and ClientDeadReckoning are the client's
	// interpolation delay and dead-reckoning limit. The server does not use
	// them; they are here so that section 8 has one home.
	ClientRenderDelay, ClientDeadReckoning time.Duration
	// THalf is the length of a half (T_half). The speed bonus counts from it.
	THalf time.Duration
	// TShrink is the closing part of a half in which the arena shrinks
	// linearly from R0 to RMin.
	TShrink time.Duration
	// TCountdown is the countdown before the first half.
	TCountdown time.Duration
	// TRoleSwap is the pause between the halves.
	TRoleSwap time.Duration
	// TGrace is how long a disconnected player has to come back, per drop.
	TGrace time.Duration
	// TGraceTotal is how long the room waits for one player over a whole
	// match: a drop's deadline is the lesser of TGrace and what is left of
	// it, so dropping and returning again and again cannot hold a match
	// open (section 2).
	TGraceTotal time.Duration
	// TJoin is how long the room waits in WAITING for every human player to
	// connect before it cancels the match.
	TJoin time.Duration
	// CaptureWindow is how far back the capture check looks into the other
	// player's history.
	CaptureWindow time.Duration
	// BotSightDelay is how old the hunter position a bot runner sees is.
	BotSightDelay time.Duration
	// PressureInterval is the period of the hunter pressure sample.
	PressureInterval time.Duration
	// ClampInterval is the minimum time between two clamp messages to the
	// same player. Clipping itself is never throttled.
	ClampInterval time.Duration

	// Tick is the period of the room's one ticker. The countdown, the half
	// clock, the grace, T_join, the pressure sample and the bot all advance
	// on it; the bot moves once per tick, so Tick is also the bot's packet
	// interval and should equal the client's (40 ms) for the bot to be
	// neither faster nor slower than a human.
	Tick time.Duration
	// SpawnDistance is how far from the centre each player spawns: the
	// hunter at (−SpawnDistance, 0), the runner at (+SpawnDistance, 0).
	SpawnDistance float64
	// RingSize is the number of validated positions kept per player. It must
	// cover the capture window and the bot's sight delay at the fastest rate
	// a client can send (the gateway allows bursts above 25 Hz), not only at
	// 25 Hz.
	RingSize int
	// InboxSize is the number of inbound payloads a session buffers before
	// Post starts dropping.
	InboxSize int
	// PresenceBuffer is the number of presence notifications a session
	// buffers before Notify waits. Notify never drops one.
	PresenceBuffer int
	// SettleTimeout bounds the one ScoreStore.Settle call of a ranked match.
	SettleTimeout time.Duration
	// BotReactionMin and BotReactionMax bound the bot's reaction delay,
	// drawn afresh on every step, on top of BotSightDelay.
	BotReactionMin, BotReactionMax time.Duration
	// BotTurnBuffer is added to a runner's tightest turning radius
	// (VRunner/OmegaMax) to give how close to the arena edge the bot runner
	// starts turning along it, so the arena shrinking meanwhile does not
	// catch it still heading out.
	BotTurnBuffer float64
	// BotLeadMax caps how far ahead a bot hunter predicts the runner.
	BotLeadMax time.Duration
	// BotPinOffset is how far inside the runner (towards the centre) a bot
	// hunter aims while it is still far away, so that it approaches from the
	// centre and drives the runner towards the shrinking edge.
	BotPinOffset float64

	// DrawTolerance is how close the two mean hunter pressure distances must
	// be for a match without a decisive capture to be a draw (section 1.4).
	DrawTolerance float64
	// Points is the scoring table of section 4.
	Points Points
}

// Points is the scoring table of section 4.
type Points struct {
	// Win is the base award of a winner of a completed match.
	Win int
	// Loss is the participation award of the loser of a completed match.
	Loss int
	// Draw is what each player gets for a draw.
	Draw int
	// Forfeit is what a player who did not come back gets (negative).
	Forfeit int
	// ForfeitWin is what the remaining player gets for a forfeit, with no
	// speed or streak bonus.
	ForfeitWin int
	// StreakStep is the streak bonus per win in the streak, this win
	// included, up to StreakCap.
	StreakStep, StreakCap int
	// SpeedUnit is the speed bonus resolution: one point for every
	// SpeedUnit by which the winner's capture beat THalf.
	SpeedUnit time.Duration
}

// DefaultConfig returns the starting values of section 8 and the server's
// defaults for the rest.
func DefaultConfig() Config {
	return Config{
		VRunner:             260,
		VHunter:             225,
		OmegaMax:            4.2,
		Alpha:               0.45,
		R0:                  500,
		RMin:                280,
		DCapture:            36,
		SlackMax:            25,
		MoveEpsilon:         1e-3,
		DtMax:               250 * time.Millisecond,
		DtInitial:           40 * time.Millisecond,
		ClientRenderDelay:   60 * time.Millisecond,
		ClientDeadReckoning: 80 * time.Millisecond,
		THalf:               45 * time.Second,
		TShrink:             15 * time.Second,
		TCountdown:          2 * time.Second,
		TRoleSwap:           3 * time.Second,
		TGrace:              5 * time.Second,
		TGraceTotal:         15 * time.Second,
		TJoin:               10 * time.Second,
		CaptureWindow:       150 * time.Millisecond,
		BotSightDelay:       2000 * time.Millisecond,
		PressureInterval:    200 * time.Millisecond,
		ClampInterval:       400 * time.Millisecond,

		Tick:          40 * time.Millisecond,
		SpawnDistance: 225, // the snap example of section 6
		// 2.5 s at the gateway's sustained 60 msg/s is 150 positions; at
		// 25 Hz this is 6.4 s.
		RingSize:       160,
		InboxSize:      64,
		PresenceBuffer: 16,
		SettleTimeout:  3 * time.Second,
		BotReactionMin: 180 * time.Millisecond,
		BotReactionMax: 260 * time.Millisecond,
		BotTurnBuffer:  15,
		BotLeadMax:     time.Second,
		BotPinOffset:   40,

		DrawTolerance: 1e-3,
		Points: Points{
			Win:        40,
			Loss:       5,
			Draw:       20,
			Forfeit:    -25,
			ForfeitWin: 40,
			StreakStep: 5,
			StreakCap:  25,
			SpeedUnit:  time.Second,
		},
	}
}

// Validate reports every value that would break the game. The
// comparisons are written so that a NaN fails them too.
func (c Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(c.VRunner > 0, "VRunner must be > 0, got %v", c.VRunner)
	check(c.VHunter > 0, "VHunter must be > 0, got %v", c.VHunter)
	check(c.OmegaMax > 0, "OmegaMax must be > 0, got %v", c.OmegaMax)
	check(c.Alpha > 0 && c.Alpha < 1, "Alpha must be in (0, 1), got %v", c.Alpha)
	check(c.RMin > 0 && c.RMin <= c.R0, "need 0 < RMin <= R0, got RMin %v, R0 %v", c.RMin, c.R0)
	check(c.DCapture > 0, "DCapture must be > 0, got %v", c.DCapture)
	check(c.SlackMax >= 0, "SlackMax must be >= 0, got %v", c.SlackMax)
	check(c.DtMax > 0, "DtMax must be > 0, got %v", c.DtMax)
	check(c.DtInitial > 0 && c.DtInitial <= c.DtMax, "DtInitial must lie in (0, DtMax], got %v", c.DtInitial)
	check(c.MoveEpsilon >= 0, "MoveEpsilon must be >= 0, got %v", c.MoveEpsilon)
	check(c.ClientRenderDelay >= 0 && c.ClientDeadReckoning >= 0, "client delays must be >= 0")
	check(c.THalf > 0, "THalf must be > 0, got %v", c.THalf)
	check(c.TShrink >= 0 && c.TShrink <= c.THalf, "TShrink must lie in [0, THalf], got %v", c.TShrink)
	check(c.TCountdown >= 0, "TCountdown must be >= 0, got %v", c.TCountdown)
	check(c.TRoleSwap >= 0, "TRoleSwap must be >= 0, got %v", c.TRoleSwap)
	check(c.TGrace > 0, "TGrace must be > 0, got %v", c.TGrace)
	check(c.TGraceTotal >= c.TGrace, "TGraceTotal must be >= TGrace, got %v", c.TGraceTotal)
	check(c.TJoin > 0, "TJoin must be > 0, got %v", c.TJoin)
	check(c.CaptureWindow >= 0, "CaptureWindow must be >= 0, got %v", c.CaptureWindow)
	check(c.BotSightDelay >= 0, "BotSightDelay must be >= 0, got %v", c.BotSightDelay)
	check(c.PressureInterval > 0, "PressureInterval must be > 0, got %v", c.PressureInterval)
	check(c.ClampInterval >= 0, "ClampInterval must be >= 0, got %v", c.ClampInterval)
	check(c.Tick > 0, "Tick must be > 0, got %v", c.Tick)
	// Both spawn points inside the arena, and not already within capture
	// distance of each other.
	check(c.SpawnDistance > 0 && c.SpawnDistance < c.R0, "need 0 < SpawnDistance < R0, got %v", c.SpawnDistance)
	check(2*c.SpawnDistance > c.DCapture, "the spawn points are within DCapture of each other")
	check(c.RingSize >= 2, "RingSize must be >= 2, got %d", c.RingSize)
	// At one position per tick (the bot, a client at the nominal rate) the
	// ring must reach back past what the bot runner looks at.
	check(time.Duration(c.RingSize)*c.Tick > c.BotSightDelay+c.BotReactionMax,
		"RingSize·Tick (%v) does not reach back BotSightDelay+BotReactionMax (%v)",
		time.Duration(c.RingSize)*c.Tick, c.BotSightDelay+c.BotReactionMax)
	check(c.InboxSize > 0, "InboxSize must be > 0, got %d", c.InboxSize)
	check(c.PresenceBuffer >= 0, "PresenceBuffer must be >= 0, got %d", c.PresenceBuffer)
	check(c.SettleTimeout > 0, "SettleTimeout must be > 0, got %v", c.SettleTimeout)
	check(c.BotReactionMin >= 0 && c.BotReactionMin <= c.BotReactionMax,
		"need 0 <= BotReactionMin <= BotReactionMax, got %v, %v", c.BotReactionMin, c.BotReactionMax)
	check(c.BotTurnBuffer >= 0 && c.VRunner/c.OmegaMax+c.BotTurnBuffer < c.RMin,
		"need BotTurnBuffer >= 0 and VRunner/OmegaMax + BotTurnBuffer < RMin, got %v", c.BotTurnBuffer)
	check(c.BotLeadMax >= 0, "BotLeadMax must be >= 0, got %v", c.BotLeadMax)
	check(c.BotPinOffset >= 0, "BotPinOffset must be >= 0, got %v", c.BotPinOffset)
	check(c.DrawTolerance >= 0, "DrawTolerance must be >= 0, got %v", c.DrawTolerance)
	check(c.Points.StreakStep >= 0 && c.Points.StreakCap >= 0, "the streak bonus must not be negative")
	check(c.Points.SpeedUnit >= time.Millisecond, "Points.SpeedUnit must be >= 1ms, got %v", c.Points.SpeedUnit)
	if len(errs) > 0 {
		return fmt.Errorf("ghosttag: invalid config: %w", errors.Join(errs...))
	}
	return nil
}
