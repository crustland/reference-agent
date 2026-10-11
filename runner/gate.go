// Package runner plays agents: one agent's loop (poll, plan every so often, choose, act), a shared rate gate that keeps a
// fleet inside the game's per-address limits, the fleet itself, and the roster file that holds its credentials.
package runner

import (
	"context"
	"sync"
	"time"
)

// Kind is a family of requests the game limits separately.
type Kind int

// The kinds.
const (
	// KindRead is GET /api/agent/state.
	KindRead Kind = iota
	// KindAct is POST /api/agent/act for everything but a fight.
	KindAct
	// KindFight is PLAY, REDRAW and FLEE, which have their own, larger bucket.
	KindFight
)

// Limits are requests allowed per minute for one address. The game's limits per address (skill.md, "Action Timing") are 30
// reads, 30 acts and 90 fight actions a minute; the defaults sit a little under them so a stray extra request does not
// trip the limit.
type Limits struct {
	ReadsPerMinute, ActsPerMinute, FightPerMinute int
}

// DefaultLimits are the limits a fleet on one address runs under.
func DefaultLimits() Limits { return Limits{ReadsPerMinute: 26, ActsPerMinute: 26, FightPerMinute: 80} }

// Gate is a sliding-window rate limiter shared by every agent that goes out from one address. Wait blocks until a request
// of a kind may be sent.
type Gate struct {
	mu     sync.Mutex
	limits map[Kind]int
	sent   map[Kind][]time.Time
	window time.Duration
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
}

// NewGate returns a gate. now and sleep default to the real clock; tests pass their own.
func NewGate(l Limits, now func() time.Time, sleep func(context.Context, time.Duration) error) *Gate {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = Sleep
	}
	return &Gate{
		limits: map[Kind]int{KindRead: max(l.ReadsPerMinute, 1), KindAct: max(l.ActsPerMinute, 1), KindFight: max(l.FightPerMinute, 1)},
		sent:   map[Kind][]time.Time{}, window: time.Minute, now: now, sleep: sleep,
	}
}

// Sleep waits for d or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wait blocks until a request of kind k fits in the window, then counts it.
func (g *Gate) Wait(ctx context.Context, k Kind) error {
	for {
		g.mu.Lock()
		now := g.now()
		recent := g.sent[k][:0]
		for _, t := range g.sent[k] {
			if now.Sub(t) < g.window {
				recent = append(recent, t)
			}
		}
		g.sent[k] = recent
		if len(recent) < g.limits[k] {
			g.sent[k] = append(recent, now)
			g.mu.Unlock()
			return ctx.Err()
		}
		wait := g.window - now.Sub(recent[0])
		g.mu.Unlock()
		if err := g.sleep(ctx, wait+time.Millisecond); err != nil {
			return err
		}
	}
}

// KindOf is the kind of an action.
func KindOf(action string) Kind {
	switch action {
	case "PLAY", "REDRAW", "FLEE":
		return KindFight
	}
	return KindAct
}
