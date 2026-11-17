package main

import (
	"context"
	"errors"

	"github.com/alpnuhoglu/gamemesh/internal/matchmaking"
	"github.com/alpnuhoglu/gamemesh/internal/wsgateway"
)

// roomParticipants answers the hub's join check (wsgateway.RoomChecker) from
// matchmaking's room records. matchmaking's RoomStore stays the only code
// that knows the room:{id} key and its format; the WebSocket service only
// reads it.
type roomParticipants struct {
	store *matchmaking.RoomStore
}

func (p roomParticipants) Access(ctx context.Context, room, playerID string) (wsgateway.RoomAccess, error) {
	r, err := p.store.Get(ctx, room)
	if errors.Is(err, matchmaking.ErrRoomNotFound) {
		return wsgateway.RoomAccess{}, wsgateway.ErrRoomNotFound
	}
	if err != nil {
		return wsgateway.RoomAccess{}, err
	}
	access := wsgateway.RoomAccess{Participant: r.HasPlayer(playerID)}
	if r.Mode != "" {
		// A validated room: the record names its game, and the hub hands
		// its traffic to that game instead of relaying it.
		access.Game = &wsgateway.GameSpec{
			Mode:      r.Mode,
			Players:   r.Players,
			Bot:       r.Bot,
			Unranked:  r.Unranked,
			CreatedAt: r.CreatedAt,
		}
	}
	return access, nil
}
