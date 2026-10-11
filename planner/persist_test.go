// Tests that the planner's daily spend survives a restart, so a restart cannot hand out the budget again.

package planner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestASpendSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json.spend")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	first, _ := NewBudget(1000, 5000, clock)
	if err := first.Persist(path); err != nil {
		t.Fatal(err)
	}
	r, err := first.Reserve("a1", 900)
	if err != nil {
		t.Fatal(err)
	}
	r.Settle(800)

	// A new process: the same file, the same day.
	second, _ := NewBudget(1000, 5000, clock)
	if err := second.Persist(path); err != nil {
		t.Fatal(err)
	}
	if a, f := second.Used("a1"); a != 800 || f != 800 {
		t.Fatalf("after a restart the books show %d and %d, want the 800 that was spent", a, f)
	}
	if _, err := second.Reserve("a1", 300); !errors.Is(err, ErrAgentBudget) {
		t.Fatalf("a restart handed the agent a fresh budget: %v", err)
	}
	if _, err := second.Reserve("a2", 900); err != nil {
		t.Fatalf("another agent still has its own budget: %v", err)
	}

	// The next UTC day starts clean.
	now = now.Add(24 * time.Hour)
	third, _ := NewBudget(1000, 5000, clock)
	if err := third.Persist(path); err != nil {
		t.Fatal(err)
	}
	if a, f := third.Used("a1"); a != 0 || f != 0 {
		t.Fatalf("a new day shows %d and %d", a, f)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the spend file: %v %v", info, err)
	}
}

func TestACorruptSpendFileIsAnErrorAndNotAFreshBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := NewBudget(1000, 5000, nil)
	if err := b.Persist(path); !errors.Is(err, ErrBudgetStore) {
		t.Fatalf("got %v", err)
	}
}

func TestWhenTheSpendCannotBeSavedNoRequestIsMade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spend")
	b, _ := NewBudget(100_000, 1_000_000, nil)
	if err := b.Persist(path); err != nil {
		t.Fatal(err)
	}
	b.path = filepath.Join(dir, "no-such-directory", "spend") // the file can no longer be written
	c := &recorded{reply: Response{Text: goodReply, InputTokens: 1, OutputTokens: 1}}
	llm := &LLM{Completer: c, Budget: b, Model: "m", MaxOutput: 700}
	if _, err := llm.Plan(context.Background(), Checkpoint{Agent: "a"}); !errors.Is(err, ErrBudgetStore) {
		t.Fatalf("got %v", err)
	}
	if c.calls.Load() != 0 {
		t.Fatalf("a request was made while the spend could not be saved: %d", c.calls.Load())
	}
	if a, _ := b.Used("a"); a != 0 {
		t.Fatalf("the failed reservation stayed on the books: %d", a)
	}
}
