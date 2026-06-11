package matchmaking

// Pair walks tickets (which arrive sorted by rank, courtesy of the ZSET) and
// greedily pairs adjacent players whose rank gap is within window.
//
// Greedy adjacent pairing on a sorted list is optimal for maximising the
// number of pairs under a max-gap constraint, runs in O(n), and is a pure
// function — trivially unit-testable with no Redis involved.
func Pair(tickets []Ticket, window int) [][2]Ticket {
	var pairs [][2]Ticket
	i := 0
	for i+1 < len(tickets) {
		if tickets[i+1].Rank-tickets[i].Rank <= window {
			pairs = append(pairs, [2]Ticket{tickets[i], tickets[i+1]})
			i += 2
			continue
		}
		// tickets[i] has no close-enough neighbour; it waits for the next
		// tick (a production system would widen its window over time).
		i++
	}
	return pairs
}
