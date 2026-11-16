package ghosttag

import (
	"context"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionStress hammers sessions from many goroutines at once, under
// -race: four posters (two per player, sharing each player's seq counter
// so packets arrive interleaved and out of order, with jittered arrival
// times, garbage and pings mixed in), a goroutine flapping both players'
// presence through the transport, and a 1 ms tick, until the match ends.
//
// What must hold: nothing panics, Done closes, exactly one match_end is
// broadcast, the store is called at most once. The fake transport's send
// counter is unsynchronised, so a send from any goroutine but the
// session's would be reported by the race detector; so would any access to
// the room from outside the loop.
//
// It waits only on Done (observable), never on elapsed time.
func TestSessionStress(t *testing.T) {
	for i := 0; i < 4; i++ {
		t.Run(fmt.Sprintf("room-%d", i), func(t *testing.T) {
			t.Parallel()
			stressOnce(t, fmt.Sprintf("stress-%d", i), uint64(i))
		})
	}
}

func stressOnce(t *testing.T, roomID string, seed uint64) {
	store := &fakeStore{}
	tr := newFakeTransport("a", "b")
	rig := newRig(t, fastConfig(), Spec{RoomID: roomID, Players: []string{"a", "b"}}, tr, store)
	rig.run(context.Background())
	done := rig.s.Done()

	var seqs [2]atomic.Int64
	ids := [2]string{"a", "b"}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(g)))
			who := g % 2
			for {
				select {
				case <-done:
					return
				default:
				}
				var payload string
				switch rng.IntN(20) {
				case 0:
					payload = `{"t":"ping","cts":1}`
				case 1:
					payload = `{"t":"pos","seq":"x"}`
				default:
					payload = fmt.Sprintf(`{"t":"pos","seq":%d,"x":%f,"y":%f,"vx":%f,"vy":0}`,
						seqs[who].Add(1)-int64(rng.IntN(3)),
						rng.Float64()*1200-600, rng.Float64()*1200-600, rng.Float64()*1e6)
				}
				arrived := time.Now().Add(-time.Duration(rng.IntN(5)) * time.Millisecond)
				rig.s.Post(ids[who], []byte(payload), arrived)
				runtime.Gosched()
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewPCG(seed, 99))
		for flap := 0; flap < 300; flap++ {
			select {
			case <-done:
				return
			default:
			}
			id := ids[rng.IntN(2)]
			tr.setConnected(id, rng.IntN(3) != 0)
			rig.s.Notify(id)
			runtime.Gosched()
		}
		// Everyone back, so the match can run out.
		for _, id := range ids {
			tr.setConnected(id, true)
			rig.s.Notify(id)
		}
	}()

	rig.waitDone(t)
	wg.Wait()

	require.Len(t, tr.ofType("match_end"), 1, "exactly one match_end")
	assert.LessOrEqual(t, store.calls.Load(), int32(1))
	assert.Zero(t, rig.obs.get(EventRoomPanic))
	assert.Positive(t, tr.sendCalls) // read after Done: ordered by the close
}
