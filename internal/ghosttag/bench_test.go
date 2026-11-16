package ghosttag

import (
	"math/rand/v2"
	"testing"
	"time"
)

type discard struct{}

func (discard) Broadcast([]byte)                          {}
func (discard) BroadcastExcept(string, []byte, time.Time) {}
func (discard) SendTo(string, []byte)                     {}

type discardCounts struct{}

func (discardCounts) Count(string) {}

// benchRoom is a room in HALF_1_ACTIVE whose rings are full of 25 Hz
// history for both players, far apart, so no packet captures.
func benchRoom(b *testing.B) (*room, *player, time.Time) {
	b.Helper()
	cfg := DefaultConfig()
	cfg.THalf = 100000 * time.Hour // the half does not end during the run (40 ms per iteration)
	r := newRoom(&cfg, "bench", []string{"a", "b"}, "", discard{}, discardCounts{}, rand.New(rand.NewPCG(1, 1)), t0)
	r.setPresence(t0, "a", true)
	r.setPresence(t0, "b", true)
	start := t0.Add(cfg.TCountdown)
	r.tick(start)
	h, run := r.withRole(roleHunter), r.withRole(roleRunner)
	now := start
	for i := 0; i < cfg.RingSize; i++ {
		now = now.Add(cfg.Tick)
		run.hist.push(sample{ts: now, p: vec{300, float64(i % 10)}})
		h.hist.push(sample{ts: now, p: vec{-300, float64(i % 10)}})
	}
	h.mot = motion{pos: vec{-300, 0}, ts: now, hasPrev: true}
	return r, h, now
}

// BenchmarkPacketCore is the per-packet core of section 7: the movement
// validation, the ring write and the symmetric capture check, as room.step
// runs them for every accepted position, without decoding or encoding.
func BenchmarkPacketCore(b *testing.B) {
	r, h, now := benchRoom(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now = now.Add(40 * time.Millisecond)
		y := float64(i%2) * 20
		if _, captured, err := r.step(now, h, vec{-300, y}); err != nil || captured {
			b.Fatalf("captured %v, err %v", captured, err)
		}
	}
}

// BenchmarkHandlePos is the whole path of an inbound position after the
// session's decode: state checks, seq filter, the core above, the clamp
// rule and the encoding of the outbound pos (to a transport that drops
// it).
func BenchmarkHandlePos(b *testing.B) {
	r, h, now := benchRoom(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now = now.Add(40 * time.Millisecond)
		r.handlePos(now, h.id, posPacket{seq: int64(i + 1), raw: vec{-300, float64(i%2) * 20}, vx: 100}, now)
	}
	if !r.active() || h.seq != int64(b.N) {
		b.Fatalf("packets were not all processed: state %s, last seq %d of %d", r.state, h.seq, b.N)
	}
}

// BenchmarkDecodePos is the session's decode of one inbound pos payload.
func BenchmarkDecodePos(b *testing.B) {
	payload := []byte(`{"t":"pos","seq":142,"cts":173168,"x":120.4,"y":-45.2,"vx":180,"vy":-90}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := decodeInbound(payload); err != nil {
			b.Fatal(err)
		}
	}
}
