// Package playersim is the player-model client: the only honest instrument
package playersim

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PlayerConfig models one playback client.
type PlayerConfig struct {
	Bitrate       float64
	InitialBuffer time.Duration
	PlayoutCap    int64
	// StallThreshold is how long the buffer must stay empty before it counts
	// as a stall rather than jitter.
	StallThreshold time.Duration
	Timeout        time.Duration
}

const (
	defaultBitrate       = 25_000_000 / 8
	defaultInitialBuffer = 2 * time.Second
	defaultPlayoutCap    = 64 << 20
	defaultStallThresh   = 100 * time.Millisecond
	defaultTimeout       = 30 * time.Second
)

func (c PlayerConfig) withDefaults() PlayerConfig {
	if c.Bitrate <= 0 {
		c.Bitrate = defaultBitrate
	}
	if c.InitialBuffer <= 0 {
		c.InitialBuffer = defaultInitialBuffer
	}
	if c.PlayoutCap <= 0 {
		c.PlayoutCap = defaultPlayoutCap
	}
	if c.StallThreshold <= 0 {
		c.StallThreshold = defaultStallThresh
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	return c
}

// PlayerResult reports client-side experience. Server-side metrics are
// reported separately and are never merged with these.
type PlayerResult struct {
	StartupLatency time.Duration
	FirstByte      time.Duration
	Stalls         int
	UnderrunTime   time.Duration
	Bytes          int64
	Duration       time.Duration
	Completed      bool
}

// Player runs the model against a target URL.
type Player struct {
	cfg    PlayerConfig
	client *http.Client
}

// NewPlayer builds a player from config.
func NewPlayer(cfg PlayerConfig) *Player {
	cfg = cfg.withDefaults()
	return &Player{
		cfg: cfg,
		client: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{MaxIdleConns: 8},
		},
	}
}

// Play streams the asset at url and records startup/stall/underrun.
//
// The model: a playout buffer consumes media at Bitrate and plays whenever
// it holds media; reading the network fills the buffer. If the buffer empties
// before the stream ends, playback underruns and the duration is recorded —
// that is the rebuffer event a server log cannot see.
func (p *Player) Play(ctx context.Context, url string) (*PlayerResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "")

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("playersim: unexpected status %d", resp.StatusCode)
	}

	res := &PlayerResult{}
	total := resp.ContentLength

	var buffered int64
	var playingSince time.Time
	var consumedAtLastTick int64
	startupDone := false

	tick := 20 * time.Millisecond
	initialBytesNeeded := int64(p.cfg.Bitrate * p.cfg.InitialBuffer.Seconds())
	if initialBytesNeeded < 1 {
		initialBytesNeeded = 1
	}

	readBuf := make([]byte, 64<<10)

	for {
		n, rerr := resp.Body.Read(readBuf)
		if n > 0 {
			if res.FirstByte == 0 {
				res.FirstByte = time.Since(start)
			}
			buffered += int64(n)
			res.Bytes += int64(n)
			if buffered > p.cfg.PlayoutCap {
				// Capping the playout buffer keeps the instrument honest: a
				// bigger buffer would only hide underruns.
				buffered = p.cfg.PlayoutCap
			}
		}

		now := time.Now()

		if !startupDone && buffered >= initialBytesNeeded {
			startupDone = true
			playingSince = now
			res.StartupLatency = now.Sub(start)
		}

		if startupDone {
			elapsed := now.Sub(playingSince)
			wantConsumed := int64(p.cfg.Bitrate * elapsed.Seconds())
			if delta := wantConsumed - consumedAtLastTick; delta > 0 {
				buffered -= delta
				consumedAtLastTick = wantConsumed
			}
			if buffered <= 0 {
				buffered = 0
				if rerr != nil {
					res.Duration = now.Sub(start)
					res.Completed = rerr == io.EOF || rerr == nil
					return res, nil
				}
				// Buffer empty while the stream continues: stall.
				res.Stalls++
				res.UnderrunTime += p.cfg.StallThreshold
				playingSince = time.Now()
				consumedAtLastTick = 0
			}
		}

		if rerr != nil {
			res.Duration = time.Since(start)
			res.Completed = rerr == io.EOF
			if total >= 0 && res.Bytes < total {
				res.Completed = false
			}
			return res, nil
		}

		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(tick):
		}
	}
}
