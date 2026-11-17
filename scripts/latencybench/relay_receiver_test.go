package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"
)

// TestRelayReceiverPinned pins what the relay receiver computes from a
// scripted stream of Relay frames: which frames are foreign, late,
// duplicate or out of order, and the latency and loss of every counted
// sequence number. The published numbers come from this path; the ghosttag
// mode must leave it as it was.
func TestRelayReceiverPinned(t *testing.T) {
	const room, from = "room-1", "sender-1"
	r := newReceiver(room, from, 8, 2) // 8 samples, the first 2 are warm-up

	frame := func(room, from string, data string) serverMsg {
		return serverMsg{Type: "Relay", Room: room, From: from, Data: json.RawMessage(data)}
	}
	rel := func(s int, sentNs int64) serverMsg {
		return frame(room, from, fmt.Sprintf(`{"s":%d,"t":%d,"p":"xx"}`, s, sentNs))
	}

	r.record(frame("other-room", from, `{"s":3,"t":1}`), 10) // foreign: room
	r.record(frame(room, "someone-else", `{"s":3,"t":1}`), 10)
	r.record(frame(room, from, `not json`), 10)
	r.record(rel(8, 1), 10)  // foreign: out of range
	r.record(rel(-1, 1), 10) // foreign: negative

	r.record(rel(1, 100), 150) // warm-up
	r.record(rel(0, 100), 170) // warm-up, out of order: not counted
	r.record(rel(1, 100), 180) // warm-up duplicate: not counted
	r.record(rel(3, 300), 340)
	r.record(rel(2, 200), 260) // out of order, counted
	r.record(rel(3, 300), 999) // duplicate, counted; latency stays 40
	r.record(rel(5, 500), 512)
	// 4 is lost, 6 arrives after stop, 7 was never sent.
	r.stop()
	r.record(rel(6, 600), 700)

	res := r.result(2, 7) // the sender wrote 0..6
	want := pairResult{
		samples:  []time.Duration{60, 40, 12},
		expected: 5, lost: 2, warmupLost: 0,
		dup: 1, ooo: 1, late: 1, foreign: 5,
	}
	if !slices.Equal(res.samples, want.samples) {
		t.Errorf("samples = %v, want %v", res.samples, want.samples)
	}
	res.samples, want.samples = nil, nil
	if res.expected != want.expected || res.lost != want.lost || res.warmupLost != want.warmupLost ||
		res.dup != want.dup || res.ooo != want.ooo || res.late != want.late || res.foreign != want.foreign {
		t.Errorf("result = %+v, want %+v", res, want)
	}

	// A warm-up message that never came is warm-up loss, not loss.
	r2 := newReceiver(room, from, 4, 2)
	r2.record(rel(1, 0), 5)
	r2.record(rel(2, 0), 5)
	if res := r2.result(2, 4); res.warmupLost != 1 || res.lost != 1 || res.expected != 2 {
		t.Errorf("warm-up loss: %+v", res)
	}
}
