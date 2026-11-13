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

func (p roomParticipants) IsParticipant(ctx context.Context, room, playerID string) (bool, error) {
	r, err := p.store.Get(ctx, room)
	if errors.Is(err, matchmaking.ErrRoomNotFound) {
		return false, wsgateway.ErrRoomNotFound
	}
	if err != nil {
		return false, err
	}
	return r.HasPlayer(playerID), nil
}
