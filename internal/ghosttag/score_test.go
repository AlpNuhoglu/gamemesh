package ghosttag

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// hunted is a player's tally: capture time (ms, or -1 for no capture) and
// mean pressure over ten samples.
func hunted(id string, captureMs int, meanPressure float64) tally {
	t := tally{id: id, pressureSum: meanPressure * 10, pressureN: 10}
	if captureMs >= 0 {
		t.captured, t.captureMs = true, captureMs
	}
	return t
}

func completed(a, b tally) matchResult {
	return matchResult{outcome: outcomeCompleted, players: []tally{a, b}}
}

func score(res matchResult, prior map[string]Standing) map[string]Award {
	c := DefaultConfig()
	return scorer(&c, res)(prior)
}

// TestScoreCorrectedExampleOfSection6: a capture at 28 400 ms and a streak
// that becomes 1 give 40 + floor(16 600 / 1000) + 5 = 61; the loser gets
// the +5 participation and loses any streak. (Version 3.0 said 58, which no
// streak bonus in steps of 5 can produce.)
func TestScoreCorrectedExampleOfSection6(t *testing.T) {
	res := completed(hunted("p_9f2a", 28400, 100), hunted("p_1b3c", -1, 50))

	got := score(res, map[string]Standing{"p_1b3c": {WinStreak: 3}})

	assert.Equal(t, Award{Delta: 61, WinStreak: 1, Win: true, CaptureMs: 28400}, got["p_9f2a"])
	assert.Equal(t, Award{Delta: 5, WinStreak: 0}, got["p_1b3c"])
}

// TestScoreStreakBonusIncludesThisWinAndCaps: the streak bonus counts the
// streak after this win, 5 per win, at most 25.
func TestScoreStreakBonusIncludesThisWinAndCaps(t *testing.T) {
	res := completed(hunted("a", 44000, 100), hunted("b", -1, 100)) // speed bonus 1
	for prior, want := range map[int]int{0: 46, 1: 51, 3: 61, 4: 66, 9: 66} {
		got := score(res, map[string]Standing{"a": {WinStreak: prior}})
		assert.Equal(t, want, got["a"].Delta, "prior streak %d", prior)
		assert.Equal(t, prior+1, got["a"].WinStreak)
	}
}

// TestScoreSpeedBonusIsTheWinnersOwnCapture: both captured; the winner's
// 20 s gives 25, not the loser's 30 s (15). Each player's capture is
// reported for the fastest-capture record.
func TestScoreSpeedBonusIsTheWinnersOwnCapture(t *testing.T) {
	res := completed(hunted("a", 30000, 1), hunted("b", 20000, 300))

	got := score(res, nil)

	assert.Equal(t, 40+25+5, got["b"].Delta)
	assert.True(t, got["b"].Win)
	assert.Equal(t, 5, got["a"].Delta)
	assert.Equal(t, 30000, got["a"].CaptureMs)
	assert.Equal(t, 20000, got["b"].CaptureMs)
}

// TestScoreTieBreakWinHasNoSpeedBonus: no capture; the lower mean pressure
// wins 40 + streak, nothing for speed.
func TestScoreTieBreakWinHasNoSpeedBonus(t *testing.T) {
	res := completed(hunted("a", -1, 120), hunted("b", -1, 80))

	got := score(res, nil)

	assert.Equal(t, 45, got["b"].Delta)
	assert.Equal(t, 5, got["a"].Delta)
}

// TestScoreDrawIsTwentyEachStreaksUnchanged: a draw neither breaks nor
// extends a streak.
func TestScoreDrawIsTwentyEachStreaksUnchanged(t *testing.T) {
	res := completed(hunted("a", -1, 100), hunted("b", -1, 100.0005))

	got := score(res, map[string]Standing{"a": {WinStreak: 2}, "b": {WinStreak: 0}})

	assert.Equal(t, Award{Delta: 20, WinStreak: 2}, got["a"])
	assert.Equal(t, Award{Delta: 20, WinStreak: 0}, got["b"])
}

// TestScoreForfeit: the leaver loses 25 and their streak; the player who
// stayed gets 40 with no bonus, keeps their streak and does not extend it.
func TestScoreForfeit(t *testing.T) {
	res := matchResult{
		outcome: outcomeForfeit,
		leaver:  "a",
		players: []tally{hunted("a", 10000, 50), hunted("b", -1, 50)},
	}

	got := score(res, map[string]Standing{"a": {WinStreak: 4}, "b": {WinStreak: 3}})

	assert.Equal(t, Award{Delta: -25, WinStreak: 0, CaptureMs: 10000}, got["a"])
	assert.Equal(t, Award{Delta: 40, WinStreak: 3, Win: true}, got["b"])
}

// TestScoreNothingForAMatchThatDoesNotSettle: void and cancelled matches
// award nothing even if a store called the rule.
func TestScoreNothingForAMatchThatDoesNotSettle(t *testing.T) {
	for _, o := range []outcome{outcomeVoid, outcomeCancelled} {
		res := matchResult{outcome: o, players: []tally{hunted("a", 1000, 1), hunted("b", -1, 1)}}
		assert.Empty(t, score(res, nil))
	}
}

// TestDecideWinnerOrder: a capture beats no capture whatever the pressure;
// the faster of two captures wins; equal captures and no captures go to the
// lower mean pressure; within 1e-3 it is a draw; a forfeit goes to the one
// who stayed.
func TestDecideWinnerOrder(t *testing.T) {
	c := DefaultConfig()
	cases := []struct {
		name string
		res  matchResult
		want verdict
	}{
		{"capture beats none", completed(hunted("a", -1, 1), hunted("b", 44000, 400)),
			verdict{winner: "b", byCapture: true}},
		{"faster capture", completed(hunted("a", 12000, 400), hunted("b", 12001, 1)),
			verdict{winner: "a", byCapture: true}},
		{"equal captures, pressure", completed(hunted("a", 12000, 90), hunted("b", 12000, 80)),
			verdict{winner: "b"}},
		{"no capture, pressure", completed(hunted("a", -1, 79.9), hunted("b", -1, 80)),
			verdict{winner: "a"}},
		{"within tolerance", completed(hunted("a", -1, 80), hunted("b", -1, 80.0009)),
			verdict{draw: true}},
		{"just outside tolerance", completed(hunted("a", -1, 80), hunted("b", -1, 80.0011)),
			verdict{winner: "a"}},
		{"no samples at all", completed(tally{id: "a"}, tally{id: "b"}),
			verdict{draw: true}},
		{"no samples loses", completed(tally{id: "a"}, hunted("b", -1, 300)),
			verdict{winner: "b"}},
		{"forfeit", matchResult{outcome: outcomeForfeit, leaver: "b", players: []tally{{id: "a"}, {id: "b"}}},
			verdict{winner: "a"}},
		{"void", matchResult{outcome: outcomeVoid, players: []tally{{id: "a"}, {id: "b"}}},
			verdict{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, decide(&c, tc.res))
		})
	}
}
