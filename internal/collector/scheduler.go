package collector

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// startDelay lets the web server come up before the first cycle.
const startDelay = 3 * time.Second

// Scheduler runs collection cycles inside serve: every IntervalMinutes,
// on Trigger (the "Refresh now" button), and in between with only parse
// polling when a parse job falls due earlier. Cycles never overlap.
type Scheduler struct {
	c       *Collector
	trigger chan struct{}
	mu      sync.Mutex // held by a cycle or an Exclusive job
	running atomic.Bool
	// onCycle, when set, is called after each cycle (tests).
	onCycle func(pollOnly bool)
}

// NewScheduler returns a Scheduler for c.
func NewScheduler(c *Collector) *Scheduler {
	return &Scheduler{c: c, trigger: make(chan struct{}, 1)}
}

// Trigger asks for a full cycle as soon as possible. A trigger during a
// cycle runs one more right after it; more triggers are merged.
func (s *Scheduler) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Running reports whether a cycle is in progress.
func (s *Scheduler) Running() bool { return s.running.Load() }

// Run loops until ctx is cancelled. A cancelled cycle stops at its next
// step boundary; every step is idempotent, so the next start resumes.
func (s *Scheduler) Run(ctx context.Context) {
	nextFull := s.c.now().Add(startDelay)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		wake := nextFull
		if at, ok, err := nextPollAt(ctx, s.c.db.Read); err == nil && ok && at.Before(wake) {
			wake = at
		}
		timer.Reset(max(time.Until(wake), 0))

		full := true
		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
		case <-timer.C:
			full = !s.c.now().Before(nextFull)
		}
		s.cycle(ctx, full)
		if ctx.Err() != nil {
			return
		}
		if full {
			nextFull = s.c.now().Add(s.interval(ctx))
		}
	}
}

// Exclusive runs fn while no cycle runs, e.g. "Prune now" from the web UI.
func (s *Scheduler) Exclusive(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

func (s *Scheduler) cycle(ctx context.Context, full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running.Store(true)
	defer s.running.Store(false)
	start := time.Now()
	rep, err := s.c.Run(ctx, RunOptions{PollOnly: !full})
	attrs := []any{"full", full, "took", time.Since(start).Round(time.Millisecond),
		"new_matches", rep.NewMatches(), "details", rep.Details, "parse_requests", rep.ParseRequests,
		"parsed", rep.Parsed, "calls", rep.Calls()}
	switch {
	case ctx.Err() != nil:
		slog.Info("collect cycle interrupted; the next start resumes it", attrs...)
	case err != nil:
		slog.Error("collect cycle failed", append(attrs, "err", err)...)
	case rep.BudgetExhausted:
		slog.Warn("collect cycle stopped: daily API budget used up", attrs...)
	case full || rep.Parsed > 0:
		slog.Info("collect cycle done", attrs...)
	}
	if s.onCycle != nil {
		s.onCycle(!full)
	}
}

func (s *Scheduler) interval(ctx context.Context) time.Duration {
	settings, err := LoadSettings(ctx, s.c.db.Read)
	if err != nil || settings.IntervalMinutes < 1 {
		return time.Duration(DefaultSettings.IntervalMinutes) * time.Minute
	}
	return time.Duration(settings.IntervalMinutes) * time.Minute
}
