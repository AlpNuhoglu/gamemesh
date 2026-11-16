//go:build raceviolation

package ghosttag

// This file is the proof by violation of the package's concurrency rule. It
// is EXPECTED TO FAIL, and only runs on request:
//
//	go test -race -tags raceviolation -run TestRoomWithoutTheLoopIsADataRace ./internal/ghosttag/
//
// It drives the room directly from two goroutines, a packet reader and a
// ticker, the way a careless integration would, without the session loop.
// The race detector reports a DATA RACE on the room's fields. That failure
// is the point: the room has no lock, and the only thing that makes it safe
// is that Session.Run is the one goroutine touching it. The stress test
// (stress_test.go) drives the same room through the session under -race
// and stays clean, so it is the loop, not luck, that keeps the room safe.

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

type nopSender struct{}

func (nopSender) Broadcast([]byte)                          {}
func (nopSender) BroadcastExcept(string, []byte, time.Time) {}
func (nopSender) SendTo(string, []byte)                     {}

// lockedCounts is a thread-safe Observer, so the only shared state left
// unprotected is the room's own.
type lockedCounts struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *lockedCounts) Count(e string) { c.mu.Lock(); c.n[e]++; c.mu.Unlock() }

func TestRoomWithoutTheLoopIsADataRace(t *testing.T) {
	cfg := DefaultConfig()
	start := time.Now()
	r := newRoom(&cfg, "race", []string{"a", "b"}, "", nopSender{}, &lockedCounts{n: map[string]int{}},
		rand.New(rand.NewPCG(1, 1)), start)
	r.setPresence(start, "a", true)
	r.setPresence(start, "b", true)
	r.tick(start.Add(cfg.TCountdown)) // HALF_1_ACTIVE

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // a packet reader calling the room itself
		defer wg.Done()
		for i := 1; i <= 2000; i++ {
			now := start.Add(cfg.TCountdown + time.Duration(i)*time.Millisecond)
			r.handlePos(now, "a", posPacket{seq: int64(i), raw: vec{float64(i % 50), 0}}, now)
		}
	}()
	go func() { // a ticker calling the room itself
		defer wg.Done()
		for i := 1; i <= 2000; i++ {
			r.tick(start.Add(cfg.TCountdown + time.Duration(i)*time.Millisecond))
		}
	}()
	wg.Wait()
	t.Log("if this test passed, it ran without -race: the detector is what reports the violation")
}
