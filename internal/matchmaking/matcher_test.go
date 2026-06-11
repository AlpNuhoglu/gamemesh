package matchmaking

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPair(t *testing.T) {
	tests := []struct {
		name    string
		tickets []Ticket
		window  int
		want    [][2]string // expected player ID pairs
	}{
		{
			name:    "empty queue",
			tickets: nil,
			window:  100,
			want:    nil,
		},
		{
			name:    "single player waits",
			tickets: []Ticket{{"a", 1000}},
			window:  100,
			want:    nil,
		},
		{
			name:    "two players within window",
			tickets: []Ticket{{"a", 1000}, {"b", 1050}},
			window:  100,
			want:    [][2]string{{"a", "b"}},
		},
		{
			name:    "two players outside window",
			tickets: []Ticket{{"a", 1000}, {"b", 1200}},
			window:  100,
			want:    nil,
		},
		{
			name:    "exact window boundary matches",
			tickets: []Ticket{{"a", 1000}, {"b", 1100}},
			window:  100,
			want:    [][2]string{{"a", "b"}},
		},
		{
			name: "odd player out skipped, later players still match",
			// a-b pair; c is 500 away from d... c=1500, d=1550 pair.
			tickets: []Ticket{{"a", 1000}, {"b", 1050}, {"c", 1500}, {"d", 1550}},
			window:  100,
			want:    [][2]string{{"a", "b"}, {"c", "d"}},
		},
		{
			name: "isolated low-rank player does not block the rest",
			// "a" has no neighbour within 100; b-c should pair.
			tickets: []Ticket{{"a", 100}, {"b", 1000}, {"c", 1060}},
			window:  100,
			want:    [][2]string{{"b", "c"}},
		},
		{
			name:    "equal ranks pair",
			tickets: []Ticket{{"a", 1000}, {"b", 1000}, {"c", 1000}, {"d", 1000}},
			window:  0,
			want:    [][2]string{{"a", "b"}, {"c", "d"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pairs := Pair(tt.tickets, tt.window)
			var got [][2]string
			for _, p := range pairs {
				got = append(got, [2]string{p[0].PlayerID, p[1].PlayerID})
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
