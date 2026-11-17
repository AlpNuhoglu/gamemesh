package main

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/ghosttag"
	"github.com/alpnuhoglu/gamemesh/internal/matchmaking"
	"github.com/alpnuhoglu/gamemesh/internal/wsgateway"
)

// gameHost starts the sessions of validated rooms (wsgateway.GameHost). It
// is where the hub, which knows no game, meets the game, which knows no
// WebSocket: the hub's GameTransport is the game's Transport and the game's
// Session is the hub's GameSession, method for method, so neither package
// imports the other.
type gameHost struct {
	cfg ghosttag.Config
	// scores settles ranked matches. Nil means every match is unranked.
	scores ghosttag.ScoreStore
	events ghosttag.Observer
	log    *zap.Logger
}

// Start implements wsgateway.GameHost. It only builds the session; the hub
// runs it.
func (h gameHost) Start(room string, spec wsgateway.GameSpec, out wsgateway.GameTransport) (wsgateway.GameSession, error) {
	if spec.Mode != matchmaking.ModeGhostTag {
		return nil, fmt.Errorf("unknown game mode %q", spec.Mode)
	}
	s, err := ghosttag.NewSession(h.cfg, ghosttag.Spec{
		RoomID:   room,
		Players:  spec.Players,
		Bot:      spec.Bot,
		Unranked: spec.Unranked,
		Logf:     h.log.Sugar().Warnf, // rare events only: a failed settle, a cancellation, a panic
	}, out, h.scores, h.events)
	if err != nil {
		return nil, err // not a nil *Session inside a non-nil interface
	}
	return s, nil
}

// gameEvents counts what happens in validated rooms (ghosttag.Observer) in
// the service's metrics. The events are a small fixed set, so the label
// stays bounded.
type gameEvents struct {
	total *prometheus.CounterVec
}

func (e gameEvents) Count(event string) { e.total.WithLabelValues(event).Inc() }
