package player

import (
	"context"
	"time"

	"songloft/internal/player/sink"
)

// ControlSyncLoop keeps seekable pull renderers near the PCM presentation clock.
// Integer-second UPnP reports cannot support the push loop's 10ms deadband.
type ControlSyncLoop struct {
	Members   func() []sink.Sink
	Reference func() (int64, time.Duration, bool)
	Clock     func() time.Time
	OnResult  func(string, time.Duration, error)
	Delay     func(string) time.Duration
	states    map[string]controlSyncState
}

type controlSyncState struct {
	songID                   int64
	direction, confirmations int
	lastAttempt              time.Time
}

const controlSyncThreshold = 1200 * time.Millisecond
const controlSyncCooldown = 15 * time.Second

func (l *ControlSyncLoop) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.CheckOnce(ctx)
		}
	}
}

func (l *ControlSyncLoop) CheckOnce(ctx context.Context) {
	if ctx.Err() != nil || l.Members == nil || l.Reference == nil {
		return
	}
	songID, expected, valid := l.Reference()
	if !valid || songID == 0 || expected < 0 {
		return
	}
	now := time.Now()
	if l.Clock != nil {
		now = l.Clock()
	}
	if l.states == nil {
		l.states = make(map[string]controlSyncState)
	}
	for _, member := range l.Members() {
		if quality, ok := member.(interface{ PlaybackAlignmentAvailable() bool }); ok && !quality.PlaybackAlignmentAvailable() {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		currentSong, currentExpected, currentValid := l.Reference()
		if !currentValid || currentSong != songID {
			return
		}
		expected = currentExpected
		if l.Delay != nil {
			expected = max(time.Duration(0), expected-l.Delay(member.ID()))
		}
		if l.Clock != nil {
			now = l.Clock()
		} else {
			now = time.Now()
		}
		aligner, ok := member.(sink.PositionAligner)
		if !ok {
			continue
		}
		reporter, ok := member.(sink.PositionReporter)
		if !ok {
			continue
		}
		actual, ok := reporter.PlaybackPosition()
		if !ok {
			continue
		}
		state := l.states[member.ID()]
		if state.songID != songID {
			state = controlSyncState{songID: songID}
		}
		delta := actual - expected
		direction := 0
		if delta >= controlSyncThreshold {
			direction = 1
		} else if delta <= -controlSyncThreshold {
			direction = -1
		}
		if direction == 0 {
			state.confirmations = 0
		} else if direction == state.direction {
			state.confirmations++
		} else {
			state.confirmations = 1
		}
		state.direction = direction
		if state.confirmations >= 2 && (state.lastAttempt.IsZero() || now.Sub(state.lastAttempt) >= controlSyncCooldown) {
			state.lastAttempt, state.confirmations = now, 0
			commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := aligner.AlignPlayback(commandCtx, songID, expected)
			cancel()
			if ctx.Err() == nil && l.OnResult != nil {
				l.OnResult(member.ID(), delta, err)
			}
		}
		l.states[member.ID()] = state
	}
}

func (e *Engine) alignmentReference(ctx context.Context) (int64, time.Duration, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx.Err() != nil || e.runCtx != ctx || !e.running || e.delegated || e.timeline == nil || e.curPlan.Duration <= 0 || e.transport.State() != StatePlaying {
		return 0, 0, false
	}
	// The transport's UI clock may run ahead of scheduled audio while buffering.
	// Map the current track's first sample to its actual presentation timestamp.
	start := e.timeline.At(uint64(e.planBase))
	elapsed := e.deps.Clock().Sub(start)
	if elapsed < 3*time.Second {
		return 0, 0, false
	}
	position := e.curPlan.Position + elapsed
	if position >= e.curPlan.Duration {
		return 0, 0, false
	}
	return e.curPlan.SongID, position, true
}

func (e *Engine) alignControls(ctx context.Context) {
	loop := ControlSyncLoop{Members: e.controlSinks, Reference: func() (int64, time.Duration, bool) { return e.alignmentReference(ctx) }, Clock: e.deps.Clock, Delay: e.activeControlDelay,
		OnResult: func(id string, delta time.Duration, err error) {
			if err != nil {
				e.deps.Logger.Warn("pull playback alignment failed", "player_id", id, "delta_ms", delta.Milliseconds(), "err", err)
			} else {
				e.deps.Logger.Info("pull playback alignment completed", "player_id", id, "delta_ms", delta.Milliseconds())
			}
		},
	}
	loop.Run(ctx)
}
