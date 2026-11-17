package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGhostIndex(t *testing.T) {
	const n = 550
	cases := []struct {
		name   string
		x, y   float64
		x0, y0 float64
		want   int
		ok     bool
	}{
		{"spawn is message 0", 225, 0, 225, 0, 0, true},
		{"grid point", 225, 137.5, 225, 0, 275, true},
		{"last message", -225, 274.5, -225, 0, 549, true},
		{"rounded on the wire, within 0.01", 225, 137.504, 225, 0, 275, true},
		{"exactly 0.01 off is still the grid", 225, 137.49, 225, 0, 275, true},
		{"spawn y not zero", 10, 3.25, 10, 0.75, 5, true},
		{"y off the grid", 225, 137.52, 225, 0, 0, false},
		{"y half way between", 225, 137.75, 225, 0, 0, false},
		{"x moved", 224.98, 137.5, 225, 0, 0, false},
		{"beyond the last index", 225, 275, 225, 0, 0, false},
		{"behind the spawn", 225, -0.5, 225, 0, 0, false},
		{"NaN y", 225, math.NaN(), 225, 0, 0, false},
		{"NaN x", math.NaN(), 0, 225, 0, 0, false},
	}
	for _, c := range cases {
		k, ok := ghostIndex(c.x, c.y, c.x0, c.y0, n)
		if ok != c.ok || (ok && k != c.want) {
			t.Errorf("%s: ghostIndex(%g, %g) = %d, %v; want %d, %v", c.name, c.x, c.y, k, ok, c.want, c.ok)
		}
	}
	// Every message the sender can send maps back to itself.
	for k := 0; k < n; k++ {
		if got, ok := ghostIndex(-225, ghostY(0, k), -225, 0, n); !ok || got != k {
			t.Fatalf("message %d maps to %d, %v", k, got, ok)
		}
	}
}

func TestGhostReceiverClipped(t *testing.T) {
	const room, from = "room-g", "sender-g"
	g := newGhostReceiver(room, from, 6, 1, 225, 0)
	for k := 0; k < 4; k++ { // 0..3 sent, 4 and 5 not
		g.sentAt[k].Store(int64(100 * (k + 1)))
	}
	pos := func(from string, x, y float64) gameData {
		return gameData{T: "pos", From: from, X: x, Y: y}
	}
	g.record(room, pos(from, 225, 0), 130)     // 0: warm-up
	g.record(room, pos(from, 225, 0.5), 250)   // 1: 50
	g.record(room, pos(from, 225, 1.0), 310)   // 2: 10
	g.record(room, pos(from, 225, 1.3), 999)   // off the grid: clipped
	g.record(room, pos(from, 224.5, 1.5), 999) // x corrected: clipped
	g.record(room, pos(from, 225, 2.0), 999)   // 4 was never sent: clipped
	g.record(room, pos(from, 225, 3.0), 999)   // out of range: clipped
	g.record(room, pos("someone", 225, 1.5), 999)
	g.record("other-room", pos(from, 225, 1.5), 999)
	missing, err := decodeGame([]byte(`{"t":"pos","from":"sender-g","x":225}`))
	if err != nil {
		t.Fatal(err)
	}
	g.record(room, missing, 999)             // no y: clipped, never index 0
	g.record(room, pos(from, 225, 1.5), 420) // 3: 20

	if got := g.clippedCount(); got != 5 {
		t.Errorf("clipped = %d, want 5", got)
	}
	res := g.result(1, 4)
	if !slices.Equal(res.samples, []time.Duration{50, 10, 20}) || res.lost != 0 || res.foreign != 2 {
		t.Errorf("result = %+v", res)
	}
}

func TestCheckGhostSchedule(t *testing.T) {
	// The brief's numbers: 550 messages at 40 ms from the lead-in.
	if err := checkGhostSchedule(leadIn+20*time.Millisecond, 550, 40*time.Millisecond); err != nil {
		t.Errorf("550 x 40ms: %v", err)
	}
	// The last message exactly at the limit is allowed, a nanosecond later is not.
	limit := ghostShrinkStart - ghostShrinkMargin
	if err := checkGhostSchedule(limit-99*time.Second/10, 100, 100*time.Millisecond); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := checkGhostSchedule(limit-99*time.Second/10+1, 100, 100*time.Millisecond); err == nil {
		t.Error("past the limit by 1ns: no error")
	}
	// What parseFlags gets: the defaults fit, 719 x 40ms does not, and an
	// interval that fits time but not the arena is refused too.
	cfg := &config{mode: modeGhost, api: "http://localhost:8080", ws: "ws://127.0.0.1:8084", samples: 550, interval: 40 * time.Millisecond}
	if err := checkGhostFlags(cfg); err != nil {
		t.Errorf("defaults: %v", err)
	}
	cfg.samples = 718
	if err := checkGhostFlags(cfg); err != nil {
		t.Errorf("718 x 40ms: %v", err)
	}
	cfg.samples = 719
	if err := checkGhostFlags(cfg); err == nil || !strings.Contains(err.Error(), "-samples 719") {
		t.Errorf("719 x 40ms: %v", err)
	}
	cfg.samples, cfg.interval = 1000, 20*time.Millisecond
	if err := checkGhostFlags(cfg); err == nil || !strings.Contains(err.Error(), "arena radius") {
		t.Errorf("1000 x 20ms (out of the arena): %v", err)
	}
}

func TestCheckGhostArena(t *testing.T) {
	// 891 messages end at y = 445: hypot(225, 445) = 498.65, inside 500 less
	// the 1-unit margin; 892 end at 445.5, 499.09 from the centre.
	if err := checkGhostArena(225, 0, -225, 0, 500, 891); err != nil {
		t.Errorf("891: %v", err)
	}
	if err := checkGhostArena(225, 0, -225, 0, 500, 892); err == nil {
		t.Error("892: no error")
	}
	// A path that passes the other player within capture distance.
	if err := checkGhostArena(225, 0, 200, 100, 500, 550); err == nil || !strings.Contains(err.Error(), "capture") {
		t.Errorf("near the other player: %v", err)
	}
	// The same player off the end of the path is far enough.
	if err := checkGhostArena(225, 0, 225, 400, 500, 550); err != nil {
		t.Errorf("beyond the end of the path: %v", err)
	}
}

func TestIsBotRoom(t *testing.T) {
	ours := map[string]*player{"a": {}, "b": {}}
	cases := []struct {
		players []string
		want    bool
	}{
		{[]string{"a"}, true},
		{[]string{"a", "b"}, false},
		{[]string{"a", "stranger"}, false},
		{[]string{"stranger"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isBotRoom(c.players, ours); got != c.want {
			t.Errorf("isBotRoom(%v) = %v, want %v", c.players, got, c.want)
		}
	}
}

func TestGhostModeIsLocalOnly(t *testing.T) {
	for _, u := range []string{"http://localhost:8080", "ws://127.0.0.1:8084", "http://[::1]:8084/metrics"} {
		if !isLocalURL(u) {
			t.Errorf("%s should be local", u)
		}
	}
	cfg := &config{mode: modeGhost, api: "https://api.gamemesh.app", ws: "wss://api.gamemesh.app", samples: 550, interval: 40 * time.Millisecond}
	if err := checkGhostFlags(cfg); err == nil || !strings.Contains(err.Error(), "local stack only") {
		t.Errorf("production target: %v", err)
	}
	cfg.api, cfg.ws, cfg.metrics = "http://localhost:8080", "ws://localhost:8084", "http://10.0.0.5:8084/metrics"
	if err := checkGhostFlags(cfg); err == nil {
		t.Error("non-local -metrics: no error")
	}
}

// TestJoinGhostQueue checks the body sent and that a reply without the
// unranked confirmation is refused.
func TestJoinGhostQueue(t *testing.T) {
	var body string
	reply := `{"status":"queued","rank":7,"mode":"ghosttag","unranked":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	a := newAPIClient(srv.URL, 1000)
	p := &player{token: "tok"}
	if err := a.joinGhostQueue(context.Background(), p, 7); err != nil {
		t.Fatal(err)
	}
	if want := `{"rank":7,"mode":"ghosttag","unranked":true}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	for _, r := range []string{`{"status":"queued","rank":7}`, `{"status":"queued","rank":7,"mode":"ghosttag","unranked":false}`} {
		reply = r
		if err := a.joinGhostQueue(context.Background(), p, 7); !errors.Is(err, errNotUnranked) {
			t.Errorf("reply %s: err = %v, want errNotUnranked", r, err)
		}
	}
}

// TestGhostSenderMessages checks what the sender writes: the inbound pos of
// section 6, seq increasing, on the grid, with the send time stored first.
func TestGhostSenderMessages(t *testing.T) {
	got := make(chan []byte, 8)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			got <- b
		}
	}))
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()

	p := &pair{room: "r1", sender: &player{conn: &wsConn{ws: ws}}}
	p.ghost = newGhostReceiver("r1", "s", 3, 0, -225, 0)
	runGhostSender(context.Background(), p, &config{samples: 3, interval: time.Millisecond}, mono())
	if p.sent != 3 || p.sendErr != nil {
		t.Fatalf("sent %d, err %v", p.sent, p.sendErr)
	}
	for k := 0; k < 3; k++ {
		var m struct {
			Action string `json:"action"`
			Room   string `json:"room"`
			Data   struct {
				T   string `json:"t"`
				Seq *int   `json:"seq"`
				Cts *int64 `json:"cts"`
			} `json:"data"`
		}
		b := <-got
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("message %d: %v: %s", k, err, b)
		}
		var raw struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(b, &raw)
		if m.Action != "relay" || m.Room != "r1" || m.Data.T != "pos" || m.Data.Seq == nil || *m.Data.Seq != k || m.Data.Cts == nil {
			t.Errorf("message %d: %s", k, b)
		}
		if raw.Data["x"] != -225.0 || raw.Data["y"] != ghostY(0, k) || raw.Data["vx"] != 0.0 || raw.Data["vy"] != 0.0 {
			t.Errorf("message %d position: %s", k, b)
		}
		if p.ghost.sentAt[k].Load() == 0 {
			t.Errorf("message %d: send time not stored", k)
		}
	}
}

// TestGhostScrapeAndReport: in ghosttag mode the stage table comes from the
// validated rooms' histogram, not the relay's, and the report says so.
func TestGhostScrapeAndReport(t *testing.T) {
	text := func(game, relay int) string {
		return strings.NewReplacer("G", strconv.Itoa(game), "R", strconv.Itoa(relay)).Replace(`# TYPE gamemesh_ws_game_duration_seconds histogram
gamemesh_ws_game_duration_seconds_bucket{stage="total",le="0.001"} G
gamemesh_ws_game_duration_seconds_bucket{stage="total",le="+Inf"} G
gamemesh_ws_game_duration_seconds_sum{stage="total"} 0
gamemesh_ws_game_duration_seconds_count{stage="total"} G
# TYPE gamemesh_ws_relay_duration_seconds histogram
gamemesh_ws_relay_duration_seconds_bucket{stage="total",le="0.001"} R
gamemesh_ws_relay_duration_seconds_bucket{stage="total",le="+Inf"} R
gamemesh_ws_relay_duration_seconds_sum{stage="total"} 0
gamemesh_ws_relay_duration_seconds_count{stage="total"} R
`)
	}
	before, after := parseText(t, text(10, 1000)), parseText(t, text(510, 1003))
	r := compareScrapesFor(scrapeFor(modeGhost), "x", before, after)
	if r.Histogram != gameHistogram || len(r.Stages) != 1 || r.Stages[0].Count != 500 {
		t.Errorf("ghosttag scrape = %+v", r)
	}
	if rr := compareScrapesFor(scrapeFor(modeRelay), "x", before, after); rr.Histogram != relayHistogram || rr.Stages[0].Count != 3 {
		t.Errorf("relay scrape = %+v", rr)
	}

	rep := &report{Mode: modeGhost, Server: r, Ghost: &ghostReport{StepUnits: ghostStep}}
	var out strings.Builder
	if err := rep.print(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mode=ghosttag", "player -> validated room -> player", "clipped=0", "Server stages: " + gameHistogram} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}
