package ghosttag

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecodeInbound: the two client messages of section 6, with unknown
// fields ignored.
func TestDecodeInbound(t *testing.T) {
	in, err := decodeInbound([]byte(`{"t":"pos","seq":142,"cts":173168,"x":120.4,"y":-45.2,"vx":180,"vy":-90,"from":"mallory"}`))
	require.NoError(t, err)
	assert.Equal(t, inbound{t: msgPos, pos: posPacket{seq: 142, raw: vec{120.4, -45.2}, vx: 180, vy: -90}}, in)

	in, err = decodeInbound([]byte(`{"t":"pos","seq":0,"x":0,"y":0}`))
	require.NoError(t, err, "vx, vy and cts are optional; seq 0 is a seq")
	assert.Equal(t, int64(0), in.pos.seq)

	in, err = decodeInbound([]byte(`{"t":"ping","cts":173168}`))
	require.NoError(t, err)
	assert.Equal(t, inbound{t: msgPing, cts: 173168}, in)
}

// TestDecodeInboundRejects: an unknown type is refused, not relayed; a
// position without seq, x or y, a negative or fractional seq, a ping
// without cts, and anything that is not such an object are malformed.
func TestDecodeInboundRejects(t *testing.T) {
	_, err := decodeInbound([]byte(`{"t":"snap","state":"FINISHED"}`))
	assert.ErrorIs(t, err, errUnknownType)
	_, err = decodeInbound([]byte(`{"x":1,"y":2,"seq":1}`))
	assert.ErrorIs(t, err, errUnknownType)
	_, err = decodeInbound([]byte(`null`))
	assert.ErrorIs(t, err, errUnknownType, "null decodes to no type")

	for _, raw := range []string{
		`{"t":"pos","x":1,"y":2}`,
		`{"t":"pos","seq":1,"y":2}`,
		`{"t":"pos","seq":1,"x":1}`,
		`{"t":"pos","seq":-1,"x":1,"y":2}`,
		`{"t":"pos","seq":1.5,"x":1,"y":2}`,
		`{"t":"pos","seq":1,"x":"1","y":2}`,
		`{"t":"ping"}`,
		`[1,2]`,
		`{"t":"pos"`,
	} {
		_, err := decodeInbound([]byte(raw))
		assert.ErrorIs(t, err, errMalformed, raw)
	}
}

// TestEncodePos: the hand-built pos is valid JSON of exactly the section 6
// fields, with the sender's ID escaped as JSON.
func TestEncodePos(t *testing.T) {
	b := encodePos(quoteID(`p_"9f2a"`), 173168040, vec{120.404, -45.2}, vec{180, -90})
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got), string(b))
	assert.Equal(t, map[string]any{
		"t": "pos", "from": `p_"9f2a"`, "sts": 173168040.0,
		"x": 120.4, "y": -45.2, "vx": 180.0, "vy": -90.0,
	}, got)
}

// TestMatchEndAndPongShape: the field names of section 6.
func TestMatchEndAndPongShape(t *testing.T) {
	assert.JSONEq(t,
		`{"t":"match_end","winner":"p_9f2a","is_draw":false,"ranked":true,"deltas":{"p_9f2a":61,"p_1b3c":5}}`,
		string(marshal(matchEndMsg{T: "match_end", Winner: "p_9f2a", Ranked: true, Deltas: map[string]int{"p_9f2a": 61, "p_1b3c": 5}})))
	assert.JSONEq(t, `{"t":"pong","cts":173168,"sts":40}`, string(marshal(pongMsg{T: "pong", Cts: 173168, Sts: 40})))
}
