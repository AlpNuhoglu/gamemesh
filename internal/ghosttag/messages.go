package ghosttag

// The messages of section 6. Each is a payload: the WebSocket service wraps
// it in its envelope ({"type":"Game","room":...,"data":<payload>}), which
// this package does not see.
//
//	client → room   {"t":"pos","seq":142,"cts":173168,"x":120.4,"y":-45.2,"vx":180,"vy":-90}
//	                {"t":"ping","cts":173168}
//	room → others   {"t":"pos","from":"p_9f2a","sts":173168040,"x":120.4,"y":-45.2,"vx":180,"vy":-90}
//	room → sender   {"t":"pong","cts":173168,"sts":173168040}
//	                {"t":"clamp","x":118,"y":-44,"theta":1.52}
//	room → all      {"t":"snap",...} {"t":"half_end",...} {"t":"match_end",...}
//
// The outbound pos carries no client claim: from is the server's identity of
// the sender, sts the server's receive time, x and y the validated position;
// seq and cts are dropped. vx and vy are passed on as a dead-reckoning hint
// for the other client only, bounded to the sender's top speed; the server
// never uses them.
//
// Positions, velocities and angles are rounded on the wire (to 0.01 units
// and 0.0001 rad) to keep the payloads near the sizes of section 6; the
// room's decisions use the unrounded values.

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"
)

// Inbound message types.
const (
	msgPos  = "pos"
	msgPing = "ping"
)

var (
	// errUnknownType: a payload whose t the room does not take. Section 6:
	// a validated room rejects it rather than relay it.
	errUnknownType = errors.New("ghosttag: unknown message type")
	// errMalformed: a payload that is not the JSON object of its type.
	errMalformed = errors.New("ghosttag: malformed message")
)

// inbound is a decoded client payload.
type inbound struct {
	t   string
	pos posPacket
	cts float64 // ping only
}

// posPacket is a client's position report.
type posPacket struct {
	seq    int64
	raw    vec // the position the client claims
	vx, vy float64
}

// inboundJSON is the shape both client messages share. A field the
// message needs starts as NaN or -1, so a missing one is detected without
// pointer fields (and their allocations) on the per-packet path.
type inboundJSON struct {
	T   string  `json:"t"`
	Seq int64   `json:"seq"`
	Cts float64 `json:"cts"`
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	VX  float64 `json:"vx"`
	VY  float64 `json:"vy"`
}

// decodeInbound parses one client payload. Unknown fields are ignored; an
// unknown t, a missing or mistyped field, or a negative seq is an error.
func decodeInbound(data []byte) (inbound, error) {
	m := inboundJSON{Seq: -1, Cts: math.NaN(), X: math.NaN(), Y: math.NaN()}
	if err := json.Unmarshal(data, &m); err != nil {
		return inbound{}, errMalformed
	}
	switch m.T {
	case msgPos:
		if m.Seq < 0 || math.IsNaN(m.X) || math.IsNaN(m.Y) {
			return inbound{}, errMalformed
		}
		return inbound{t: msgPos, pos: posPacket{seq: m.Seq, raw: vec{m.X, m.Y}, vx: m.VX, vy: m.VY}}, nil
	case msgPing:
		if math.IsNaN(m.Cts) {
			return inbound{}, errMalformed
		}
		return inbound{t: msgPing, cts: m.Cts}, nil
	}
	return inbound{}, errUnknownType
}

// quoteID returns id as a JSON string, for building payloads by hand.
func quoteID(id string) []byte {
	b, _ := json.Marshal(id) // a string always marshals
	return b
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

func appendNum(b []byte, f float64) []byte {
	return strconv.AppendFloat(b, f, 'f', -1, 64)
}

// encodePos builds the outbound pos by hand: it is the one message sent at
// packet rate, and this costs a single allocation.
func encodePos(from []byte, sts int64, p, v vec) []byte {
	b := make([]byte, 0, 96+len(from))
	b = append(b, `{"t":"pos","from":`...)
	b = append(b, from...)
	b = append(b, `,"sts":`...)
	b = strconv.AppendInt(b, sts, 10)
	b = append(b, `,"x":`...)
	b = appendNum(b, round2(p.X))
	b = append(b, `,"y":`...)
	b = appendNum(b, round2(p.Y))
	b = append(b, `,"vx":`...)
	b = appendNum(b, round2(v.X))
	b = append(b, `,"vy":`...)
	b = appendNum(b, round2(v.Y))
	return append(b, '}')
}

type pongMsg struct {
	T   string  `json:"t"`
	Cts float64 `json:"cts"`
	Sts int64   `json:"sts"`
}

type clampMsg struct {
	T     string  `json:"t"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Theta float64 `json:"theta"`
}

type snapPlayer struct {
	ID string `json:"id"`
	// Name is the player's display name, for the top bar and the match
	// end screen; "" while the room has not learned it.
	Name      string  `json:"name"`
	Role      string  `json:"role"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Theta     float64 `json:"theta"`
	Connected bool    `json:"connected"`
}

type snapMsg struct {
	T     string `json:"t"`
	State string `json:"state"`
	Sts   int64  `json:"sts"`
	// RemMs is what is left of the current state's clock: the half in
	// HALF_x_ACTIVE, the half frozen in DISCONNECT_GRACE (section 2), the
	// countdown, the role swap, or T_join in WAITING.
	RemMs   int64        `json:"rem_ms"`
	R       float64      `json:"r"`
	Players []snapPlayer `json:"players"`
	Cfg     clientConfig `json:"cfg"`
}

// clientConfig is snap.cfg (section 6): every constant the client needs,
// so that it carries none of its own (section 3.5). The movement constants
// let it predict its own motion with the validation's formula, the arena
// ones compute R(t) between two snaps, the rest pace and draw. The golden
// vectors of the validation carry the same object (validate_vectors.json).
//
// Durations are milliseconds. They are floats so a sub-millisecond Config
// value is not truncated; a whole number of milliseconds encodes as an
// integer.
type clientConfig struct {
	VRunner   float64 `json:"v_runner"`
	VHunter   float64 `json:"v_hunter"`
	OmegaMax  float64 `json:"omega_max"`
	Alpha     float64 `json:"alpha"`
	R0        float64 `json:"r0"`
	RMin      float64 `json:"r_min"`
	DCapture  float64 `json:"d_capture"`
	SlackMax  float64 `json:"slack_max"`
	MoveEps   float64 `json:"move_eps"`
	DtMaxMs   float64 `json:"dt_max_ms"`
	THalfMs   float64 `json:"t_half_ms"`
	TShrinkMs float64 `json:"t_shrink_ms"`
	SendMs    float64 `json:"send_ms"`
	TrailMs   float64 `json:"trail_ms"`
	InterpMs  float64 `json:"interp_ms"`
	DrMs      float64 `json:"dr_ms"`
}

// clientConfigOf is the one place snap.cfg is made from a Config.
//
// send_ms is Config.Tick: the room's tick is the bot's packet interval,
// and it must equal the client's for the bot to be neither faster nor
// slower than a human (section 1.5), so the client is told to send at it.
func clientConfigOf(c *Config) clientConfig {
	return clientConfig{
		VRunner:   c.VRunner,
		VHunter:   c.VHunter,
		OmegaMax:  c.OmegaMax,
		Alpha:     c.Alpha,
		R0:        c.R0,
		RMin:      c.RMin,
		DCapture:  c.DCapture,
		SlackMax:  c.SlackMax,
		MoveEps:   c.MoveEpsilon,
		DtMaxMs:   millis(c.DtMax),
		THalfMs:   millis(c.THalf),
		TShrinkMs: millis(c.TShrink),
		SendMs:    millis(c.Tick),
		TrailMs:   millis(c.TrailDelay),
		InterpMs:  millis(c.ClientRenderDelay),
		DrMs:      millis(c.ClientDeadReckoning),
	}
}

// millis is d in milliseconds.
func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

type halfEndMsg struct {
	T      string `json:"t"`
	Half   int    `json:"half"`
	Reason string `json:"reason"`
	DurMs  int    `json:"dur_ms"`
	Hunter string `json:"hunter"`
	Runner string `json:"runner"`
}

// Half end reasons.
const (
	reasonCapture = "CAPTURE"
	reasonTimeout = "TIMEOUT"
)

type matchEndMsg struct {
	T      string `json:"t"`
	Winner string `json:"winner"`
	IsDraw bool   `json:"is_draw"`
	Ranked bool   `json:"ranked"`
	// Deltas has every player of the match, the bot included, and is all
	// zeros when the match was not ranked.
	Deltas map[string]int `json:"deltas"`
}

// marshal encodes one of the message structs above. None of them can fail
// to encode (no channels, no funcs, and every float is finite: a validated
// position, a radius, or a client's finite JSON number), but if one ever
// did, the message is dropped (nil) rather than the room's goroutine
// panicking and taking the server down.
func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
