package main

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// pair is one sender → receiver couple matched into the same room.
type pair struct {
	idx      int
	room     string
	sender   *player
	receiver *player
	recv     *receiver

	sent    int // messages written by the sender
	sendErr error
}

// receiver collects one pair's arrivals. record runs on the receiver's read
// goroutine; everything else reads under mu after the run.
type receiver struct {
	room   string
	from   string // the sender's player ID
	warmup int    // sequence numbers below this are not counted

	mu      sync.Mutex
	stopped bool
	got     []bool
	lat     []time.Duration // indexed by sequence number
	dup     int             // counted range only
	ooo     int             // counted range only
	maxSeq  int
	late    int // arrived after the grace period ended
	foreign int // relay frames we could not attribute to this run
}

func newReceiver(room, from string, samples, warmup int) *receiver {
	return &receiver{
		room:   room,
		from:   from,
		warmup: warmup,
		got:    make([]bool, samples),
		lat:    make([]time.Duration, samples),
		maxSeq: -1,
	}
}

// relayData is the payload a sender puts in "data". Short keys keep the
// fixed part small; p is padding up to -payload bytes.
type relayData struct {
	S int   `json:"s"` // sequence number
	T int64 `json:"t"` // send time, ns since process start (monotonic)
}

// record stores one arrival. now was taken right after ReadMessage returned.
func (r *receiver) record(m serverMsg, now time.Duration) {
	var d relayData
	if m.Room != r.room || (m.From != "" && m.From != r.from) || json.Unmarshal(m.Data, &d) != nil {
		r.mu.Lock()
		r.foreign++
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if d.S < 0 || d.S >= len(r.got) {
		r.foreign++
		return
	}
	if r.stopped {
		r.late++
		return
	}
	// Duplicates and reordering are counted, like loss, over the counted
	// range only; a warmup message still advances maxSeq.
	counted := d.S >= r.warmup
	if r.got[d.S] {
		if counted {
			r.dup++
		}
		return
	}
	if d.S < r.maxSeq {
		if counted {
			r.ooo++
		}
	} else {
		r.maxSeq = d.S
	}
	r.got[d.S] = true
	r.lat[d.S] = now - time.Duration(d.T)
}

func (r *receiver) stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
}

// pairResult is what one pair measured over the counted (post-warmup) range.
type pairResult struct {
	samples    []time.Duration
	expected   int // counted messages the sender actually sent
	lost       int
	warmupLost int
	dup, ooo   int
	late       int
	foreign    int
}

func (r *receiver) result(warmup, sent int) pairResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := pairResult{dup: r.dup, ooo: r.ooo, late: r.late, foreign: r.foreign}
	for s := 0; s < sent && s < len(r.got); s++ {
		if s < warmup {
			if !r.got[s] {
				res.warmupLost++
			}
			continue
		}
		res.expected++
		if r.got[s] {
			res.samples = append(res.samples, r.lat[s])
		} else {
			res.lost++
		}
	}
	return res
}

// sleepUntil sleeps until the monotonic offset t; false if ctx ended first.
func sleepUntil(ctx context.Context, t time.Duration) bool {
	d := t - mono()
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// runSender sends message seq at start + seq*interval (an absolute schedule,
// so a late wake-up does not shift every later message). The send stamp is
// taken after the sleep and only the few appends that encode it separate it
// from the write.
func runSender(ctx context.Context, p *pair, cfg *config, start time.Duration) {
	ws := p.sender.conn.ws
	prefix := []byte(`{"action":"relay","room":` + strconv.Quote(p.room) + `,"data":`)
	buf := make([]byte, 0, len(prefix)+cfg.payload+64)
	for seq := 0; seq < cfg.samples; seq++ {
		if !sleepUntil(ctx, start+time.Duration(seq)*cfg.interval) {
			return
		}
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		t := mono()
		buf = append(buf[:0], prefix...)
		dataStart := len(buf)
		buf = append(buf, `{"s":`...)
		buf = strconv.AppendInt(buf, int64(seq), 10)
		buf = append(buf, `,"t":`...)
		buf = strconv.AppendInt(buf, int64(t), 10)
		buf = append(buf, `,"p":"`...)
		for pad := cfg.payload - (len(buf) - dataStart) - len(`"}`); pad > 0; pad-- {
			buf = append(buf, 'x')
		}
		buf = append(buf, `"}}`...)
		if err := ws.WriteMessage(websocket.TextMessage, buf); err != nil {
			p.sendErr = err
			return
		}
		p.sent = seq + 1
	}
}

// runPinger sends n pings at evenly spaced offsets in [from, to).
func runPinger(ctx context.Context, c *wsConn, n int, from, to time.Duration, phase float64) {
	if n <= 0 || to <= from {
		return
	}
	step := (to - from) / time.Duration(n)
	for k := 0; k < n; k++ {
		at := from + time.Duration((float64(k)+phase)*float64(step))
		if !sleepUntil(ctx, at) {
			return
		}
		if !c.alive() {
			return
		}
		_ = c.ping()
	}
}
