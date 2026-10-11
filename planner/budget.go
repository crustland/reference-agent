// The planner's per-agent and fleet token budgets, per UTC day, used by planner.LLM before every request.

package planner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Errors of the budget.
var (
	// ErrAgentBudget means the agent has spent its tokens for the day.
	ErrAgentBudget = errors.New("the agent's planner budget for today is spent")
	// ErrFleetBudget means the fleet has spent its tokens for the day.
	ErrFleetBudget = errors.New("the fleet's planner budget for today is spent")
	// ErrBudgetStore means the day's spend could not be written down, so no more can be promised: a restart would forget it.
	ErrBudgetStore = errors.New("the planner's spend could not be saved")
)

// Budget caps what the planner may spend, in tokens (input plus output) per UTC day: one cap for each agent and one for
// the fleet. A call reserves its worst case before it is made and settles to what the answer reports, so the caps hold
// even when calls overlap; nothing is spent without a reservation.
type Budget struct {
	mu       sync.Mutex
	perAgent int64
	fleet    int64
	now      func() time.Time
	day      string
	agent    map[string]int64
	used     int64
	path     string // where the day's spend is kept, empty if it is kept in memory only
}

// spendFile is what is kept between runs.
type spendFile struct {
	Day    string           `json:"day"`
	Used   int64            `json:"used"`
	Agents map[string]int64 `json:"agents"`
}

// Persist keeps the day's spend in a file (owner-readable only), so that a restart does not hand out a fresh budget: it loads
// what was spent today if the file holds today's spend, and writes it again after every change. A file that cannot be read
// as a spend file is an error, not a fresh start. Reserve fails if the spend cannot be written.
func (b *Budget) Persist(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.path = path
	b.roll()
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return b.save()
	case err != nil:
		return fmt.Errorf("%w: %v", ErrBudgetStore, err)
	}
	var f spendFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("%w: %s is not a spend file (delete it to start the day afresh): %v", ErrBudgetStore, path, err)
	}
	if f.Day == b.now().UTC().Format("2006-01-02") {
		b.day, b.used, b.agent = f.Day, f.Used, f.Agents
		if b.agent == nil {
			b.agent = map[string]int64{}
		}
	}
	return nil
}

// save writes the books; the caller holds the lock.
func (b *Budget) save() error {
	if b.path == "" {
		return nil
	}
	raw, err := json.Marshal(spendFile{Day: b.day, Used: b.used, Agents: b.agent})
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrBudgetStore, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrBudgetStore, err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return fmt.Errorf("%w: %v", ErrBudgetStore, err)
	}
	return nil
}

// NewBudget returns a budget with the two daily caps, which must be positive. now is the clock (time.Now in production).
func NewBudget(perAgent, fleet int64, now func() time.Time) (*Budget, error) {
	if perAgent <= 0 || fleet <= 0 {
		return nil, fmt.Errorf("both budgets must be positive, got %d and %d", perAgent, fleet)
	}
	if now == nil {
		now = time.Now
	}
	return &Budget{perAgent: perAgent, fleet: fleet, now: now, agent: map[string]int64{}}, nil
}

// roll starts a new day's books at UTC midnight.
func (b *Budget) roll() {
	day := b.now().UTC().Format("2006-01-02")
	if day != b.day {
		b.day, b.agent, b.used = day, map[string]int64{}, 0
	}
}

// Reservation is tokens set aside for one call.
type Reservation struct {
	b      *Budget
	agent  string
	day    string
	tokens int64
	done   bool
}

// Reserve sets aside tokens for a call by an agent. It fails, reserving nothing, if either the agent or the fleet would
// pass its cap.
func (b *Budget) Reserve(agent string, tokens int64) (*Reservation, error) {
	if tokens <= 0 {
		return nil, fmt.Errorf("a reservation needs a positive number of tokens, got %d", tokens)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	if b.agent[agent]+tokens > b.perAgent {
		return nil, ErrAgentBudget
	}
	if b.used+tokens > b.fleet {
		return nil, ErrFleetBudget
	}
	b.agent[agent] += tokens
	b.used += tokens
	if err := b.save(); err != nil {
		b.agent[agent] -= tokens
		b.used -= tokens
		return nil, err
	}
	return &Reservation{b: b, agent: agent, day: b.day, tokens: tokens}, nil
}

// Settle replaces the reservation with what the call really used. A call that spent more than it reserved is charged in
// full (the books may then pass the cap by that much, and the next reservation will fail). It is a no-op after the first
// call, and after the day has rolled over.
func (r *Reservation) Settle(actual int64) {
	r.finish(max(actual, 0))
}

// Release gives the whole reservation back. Use it only when no request was sent.
func (r *Reservation) Release() { r.finish(0) }

func (r *Reservation) finish(spent int64) {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	r.b.roll()
	if r.b.day != r.day {
		return
	}
	delta := spent - r.tokens
	r.b.agent[r.agent] += delta
	r.b.used += delta
	// A write that fails here leaves the file holding the reservation, which is never less than the spend it replaced for a
	// call that ran over; the next Reserve fails if the file cannot be written at all.
	_ = r.b.save()
}

// Used returns what an agent and the whole fleet have spent today.
func (b *Budget) Used(agent string) (agentTokens, fleetTokens int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	return b.agent[agent], b.used
}
