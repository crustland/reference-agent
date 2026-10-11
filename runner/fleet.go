// A fleet of 20 to 50 agents run together under one rate gate and one planner budget, used by the crust-agent command.

package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crustland/reference-agent/api"
	"github.com/crustland/reference-agent/planner"
)

// A fleet is a set of agents run together from one place under one rate gate and one planner budget. The reference agent
// runs a single agent with Play-style use; a fleet is for an operator running a house of agents, and is held to a size
// that stays inside the game's per-address limits.
const (
	// MinFleet and MaxFleet bound a fleet: fewer than twenty is not worth a fleet's care, and more than fifty cannot be
	// served from one address inside the game's limits.
	MinFleet = 20
	MaxFleet = 50
)

// ErrFleetSize is returned for a fleet outside MinFleet..MaxFleet.
var ErrFleetSize = fmt.Errorf("a fleet has %d to %d agents", MinFleet, MaxFleet)

// ValidateFleetSize checks a fleet size.
func ValidateFleetSize(n int) error {
	if n < MinFleet || n > MaxFleet {
		return fmt.Errorf("%w, got %d", ErrFleetSize, n)
	}
	return nil
}

// Fleet runs a roster's agents together.
type Fleet struct {
	Agents []*Agent
	// Stagger spreads the agents' first steps over this long, so a fleet started at once does not all poll in the same second.
	Stagger time.Duration
}

// NewFleet builds the agents of a roster. They share the client, the gate, the planner and its budget, and each gets its own
// memory and random stream. The size is checked.
func NewFleet(r *Roster, client *api.Client, gate *Gate, pl planner.Planner, every int, seed uint64, log *slog.Logger) (*Fleet, error) {
	if err := ValidateFleetSize(len(r.Agents)); err != nil {
		return nil, err
	}
	f := &Fleet{Stagger: 2 * time.Minute}
	for i, e := range r.Agents {
		a := New(e.Credentials(), client, gate, seed+uint64(i)*7919)
		a.Planner, a.Every = pl, every
		if log != nil {
			a.Log = log
		}
		f.Agents = append(f.Agents, a)
	}
	return f, nil
}

// Run plays every agent until the context ends. An agent whose key the game rejects stops; the others go on.
func (f *Fleet) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(f.Agents))
	for i, a := range f.Agents {
		wg.Add(1)
		delay := time.Duration(0)
		if len(f.Agents) > 1 {
			delay = f.Stagger * time.Duration(i) / time.Duration(len(f.Agents))
		}
		go func() {
			defer wg.Done()
			if err := a.Sleep(ctx, delay); err != nil {
				return
			}
			// Run only returns when the context ends or the key is rejected.
			if err := a.Run(ctx); ctx.Err() == nil {
				errs <- fmt.Errorf("%s: %w", a.Creds.Name, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	var all []error
	for err := range errs {
		all = append(all, err)
	}
	return errors.Join(all...)
}
