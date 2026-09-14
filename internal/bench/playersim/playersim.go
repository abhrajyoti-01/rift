// Package playersim is the player-model client (TECHNICAL_SPEC §8.3) —
// the only honest instrument for rebuffer: a server log cannot see a
// client stall (P3). Playout buffer consumes at Bitrate from a fake
// playback clock; startup completes when InitialBuffer is filled; a stall
// is a playback-clock tick with an empty buffer while the asset is not
// exhausted.
//
// Phase 4 (ROADMAP.md). This file pins the public contract.
package playersim

import (
	"context"
	"time"
)

// PlayerConfig models one playback client.
type PlayerConfig struct {
	Bitrate       float64       // bytes/s of media consumption
	InitialBuffer time.Duration // startup threshold of buffered media
	PlayoutCap    int64         // playout buffer bytes cap
}

// PlayerResult reports client-side experience separately from server-side
// metrics — never merged (P3, FR-46).
type PlayerResult struct {
	StartupLatency time.Duration
	FirstByte      time.Duration
	Stalls         int
	UnderrunTime   time.Duration
	Bytes          int64
	Duration       time.Duration
}

// Player runs the model against a target URL.
type Player struct {
	// Phase 4: playout buffer, playback clock, range-request client.
}

// NewPlayer builds a player from config.
func NewPlayer(cfg PlayerConfig) *Player {
	// Phase 4 (ROADMAP.md).
	_ = cfg
	return &Player{}
}

// Play streams the asset at url and records startup/stall/underrun.
func (p *Player) Play(ctx context.Context, url string) (*PlayerResult, error) {
	// Phase 4 (ROADMAP.md).
	_, _ = p, url
	return nil, nil
}
