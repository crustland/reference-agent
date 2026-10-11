// Tests of the runner against a fake game: nothing illegal is sent, pacing, rate limits, the planner schedule, the gate, the fleet and the roster.

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crustland/reference-agent/api"
	"github.com/crustland/reference-agent/jev"
	"github.com/crustland/reference-agent/planner"
)

// A fake game: it serves a fixed state and records every act it is sent.
type fakeGame struct {
	mu      sync.Mutex
	state   map[string]any
	acts    []api.Request
	actAuth []string
	states  int
	actErr  func(n int) (status int, body string, retryAfter string)
	regs    int
	regErr  func(n int) (int, string)
}

func newFakeGame(legal ...string) *fakeGame {
	la := make([]map[string]any, len(legal))
	for i, a := range legal {
		la[i] = map[string]any{"action": a}
	}
	return &fakeGame{state: map[string]any{
		"agent":         map[string]any{"id": "a1", "name": "Tester", "status": "active", "level": 6, "health": 100, "max_health": 100, "stamina": 90, "max_stamina": 100, "gold": 40},
		"legal_actions": la,
		"chunk":         map[string]any{"x": 20, "y": 20, "terrain": "plains", "region": "verdant_reach", "resources": []any{}, "mobs": []any{}},
		"tick":          1, "next_tick_in": 15,
	}}
}

func (g *fakeGame) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.URL.Path {
	case "/api/agent/state":
		g.states++
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid key","code":"UNAUTHORIZED"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(g.state)
	case "/api/agent/act":
		var req api.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.acts = append(g.acts, req)
		g.actAuth = append(g.actAuth, r.Header.Get("Authorization"))
		if g.actErr != nil {
			if status, body, ra := g.actErr(len(g.acts)); status != 0 {
				if ra != "" {
					w.Header().Set("Retry-After", ra)
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "action": req.Action})
	case "/api/agent/register":
		g.regs++
		if g.regErr != nil {
			if status, body := g.regErr(g.regs); status != 0 {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
		}
		var req api.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": fmt.Sprintf("id-%d", g.regs), "api_key": fmt.Sprintf("cl_key_%d", g.regs), "name": req.Name, "class": req.Class})
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGame) actCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.acts)
}

// fastAgent is an agent for the fake game with no waiting.
func fastAgent(t *testing.T, srv *httptest.Server, exec jev.Executor) *Agent {
	t.Helper()
	c := api.New(srv.URL)
	c.HTTP = srv.Client()
	gate := NewGate(Limits{ReadsPerMinute: 100000, ActsPerMinute: 100000, FightPerMinute: 100000}, nil, nil)
	a := New(api.Credentials{AgentID: "a1", Name: "Tester", APIKey: "cl_secret_key"}, c, gate, 1)
	a.Sleep = func(context.Context, time.Duration) error { return nil }
	a.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if exec != nil {
		a.Exec = exec
	}
	return a
}

type rogue struct{ choice jev.Choice }

func (r rogue) Choose(*api.State, jev.Plan, *jev.Memory, *rand.Rand) (jev.Choice, error) {
	return r.choice, nil
}

func TestAnExecutorThatChoosesAnIllegalActionSendsNothing(t *testing.T) {
	g := newFakeGame("MOVE", "REST")
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, rogue{jev.Choice{Action: "ENGAGE", Args: map[string]any{"mob_id": "m1"}}})
	_, err := a.Step(context.Background())
	if !errors.Is(err, ErrIllegalChoice) {
		t.Fatalf("got %v", err)
	}
	if g.actCount() != 0 {
		t.Fatalf("the illegal action reached the game: %v", g.acts)
	}
}

func TestAnExecutorThatChoosesADeniedActionSendsNothingEvenWhenTheStateListsIt(t *testing.T) {
	for _, action := range []string{"SUBMIT_OFFERING", "BIND_SEAT", "TRIAL_ENTER", "BUY_ITEM", "LIST_FRAGMENT"} {
		g := newFakeGame("MOVE", action)
		srv := httptest.NewServer(g)
		a := fastAgent(t, srv, rogue{jev.Choice{Action: action}})
		_, err := a.Step(context.Background())
		srv.Close()
		if !errors.Is(err, ErrIllegalChoice) || g.actCount() != 0 {
			t.Fatalf("%s: err %v, %d acts sent", action, err, g.actCount())
		}
	}
}

func TestAStepSendsOneLegalActionWithTheAgentsKey(t *testing.T) {
	g := newFakeGame("MOVE", "REST")
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	out, err := a.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.actCount() != 1 || (g.acts[0].Action != "MOVE" && g.acts[0].Action != "REST") {
		t.Fatalf("acts: %v", g.acts)
	}
	if g.actAuth[0] != "Bearer cl_secret_key" {
		t.Fatalf("auth header: %q", g.actAuth[0])
	}
	if out.Wait <= 0 {
		t.Fatalf("a step should say how long to wait: %v", out.Wait)
	}
}

func TestAReflectionIsAttachedWhenTheGameOffersOne(t *testing.T) {
	g := newFakeGame("MOVE", "REST")
	g.state["can_reflect"] = true
	g.state["event_type"] = "level_up"
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(g.acts[0].Reflection); n < jev.MinReflectionLen || n > jev.MaxReflectionLen {
		t.Fatalf("reflection length %d: %q", n, g.acts[0].Reflection)
	}
}

func TestNothingToDoWaitsForTheNextTickAndSendsNothing(t *testing.T) {
	g := newFakeGame()
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	out, err := a.Step(context.Background())
	if err != nil || g.actCount() != 0 || out.Wait < 15*time.Second {
		t.Fatalf("err %v, acts %d, wait %v", err, g.actCount(), out.Wait)
	}
}

func TestARejectedKeyStopsTheAgent(t *testing.T) {
	g := newFakeGame("MOVE")
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	a.Creds.APIKey = "bad"
	err := a.Run(context.Background())
	if !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("got %v", err)
	}
}

func TestARateLimitIsWaitedOutAndNotRetriedAtOnce(t *testing.T) {
	g := newFakeGame("MOVE")
	g.actErr = func(n int) (int, string, string) {
		return http.StatusTooManyRequests, `{"error":"slow down","code":"RATE_LIMITED"}`, "30"
	}
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	out, err := a.Step(context.Background())
	if err == nil || out.Wait < 30*time.Second {
		t.Fatalf("err %v, wait %v: want the 30 seconds the game asked for", err, out.Wait)
	}
	if g.actCount() != 1 {
		t.Fatalf("retried within the step: %d acts", g.actCount())
	}
}

func TestARefusedActionIsPartOfPlayingNotAnError(t *testing.T) {
	g := newFakeGame("MOVE")
	g.actErr = func(n int) (int, string, string) {
		return http.StatusBadRequest, `{"error":"BORDER_CLOSED: turned back","code":"BORDER_CLOSED"}`, ""
	}
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	out, err := a.Step(context.Background())
	if err != nil {
		t.Fatalf("a 400 on an action should not stop the loop: %v", err)
	}
	if out.Choice.Dest == nil || !a.Mem.Closed[*out.Choice.Dest] {
		t.Fatal("the closed border was not remembered")
	}
}

func TestTheKeyIsNeverPrintedOrLogged(t *testing.T) {
	c := api.Credentials{AgentID: "a1", Name: "Tester", APIKey: "cl_secret_key"}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c)} {
		if strings.Contains(s, "cl_secret_key") {
			t.Fatalf("printed the key: %s", s)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("hello", "creds", c)
	if strings.Contains(buf.String(), "cl_secret_key") {
		t.Fatalf("logged the key: %s", buf.String())
	}
}

type countingPlanner struct {
	calls atomic.Int32
	plan  jev.Plan
	err   error
}

func (p *countingPlanner) Plan(context.Context, planner.Checkpoint) (jev.Plan, error) {
	p.calls.Add(1)
	return p.plan, p.err
}

func TestThePlannerIsAskedOncePerCheckpointWhateverItAnswers(t *testing.T) {
	g := newFakeGame("MOVE", "REST")
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	p := &countingPlanner{err: errors.New("the planner is down")}
	a.Planner, a.Every = p, 10
	for i := 0; i < 35; i++ {
		if _, err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls.Load() != 3 {
		t.Fatalf("asked %d times in 35 turns at every 10, want 3", p.calls.Load())
	}
	if g.actCount() != 35 {
		t.Fatalf("a failing planner must not stop play: %d acts", g.actCount())
	}
}

func TestANewPlanSteersTheNextTurns(t *testing.T) {
	g := newFakeGame("MOVE", "REST", "SAY")
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	a.Planner, a.Every = &countingPlanner{plan: jev.Plan{Goal: jev.GoalExplore, Say: "The marsh smells of rain."}}, 3
	a.Mem.Turn = 1000
	for i := 0; i < 6; i++ {
		if _, err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	said := false
	for _, act := range g.acts {
		if act.Action == "SAY" && act.Args["text"] == "The marsh smells of rain." {
			said = true
		}
	}
	if !said {
		t.Fatalf("the plan's line was never said: %v", g.acts)
	}
}

// With the budget spent the live planner makes no request, and the agent plays on with its last plan.
type neverCalled struct{ calls atomic.Int32 }

func (n *neverCalled) Complete(context.Context, planner.Request) (planner.Response, error) {
	n.calls.Add(1)
	return planner.Response{Text: `{"goal":"hunt"}`, InputTokens: 1, OutputTokens: 1}, nil
}

func TestAnAgentWithAnExhaustedPlannerBudgetKeepsPlayingAndCostsNothing(t *testing.T) {
	g := newFakeGame("MOVE", "REST")
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	b, _ := planner.NewBudget(100, 100, nil) // smaller than one call's worst case
	c := &neverCalled{}
	a.Planner, a.Every = &planner.LLM{Completer: c, Budget: b, Model: "m", MaxOutput: 700}, 5
	for i := 0; i < 20; i++ {
		if _, err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c.calls.Load() != 0 {
		t.Fatalf("the model was called %d times with no budget", c.calls.Load())
	}
	if g.actCount() != 20 {
		t.Fatalf("play stopped: %d acts", g.actCount())
	}
}

func TestAPlannerThatIsOffNeverCallsAnything(t *testing.T) {
	g := newFakeGame("MOVE", "REST")
	srv := httptest.NewServer(g)
	defer srv.Close()
	a := fastAgent(t, srv, nil)
	a.Every = 2
	for i := 0; i < 10; i++ {
		if _, err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if g.actCount() != 10 {
		t.Fatalf("acts: %d", g.actCount())
	}
}

func TestTheGateHoldsAFleetToTheLimit(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	var slept time.Duration
	gate := NewGate(Limits{ReadsPerMinute: 3, ActsPerMinute: 2, FightPerMinute: 5}, func() time.Time { return now },
		func(_ context.Context, d time.Duration) error { slept += d; now = now.Add(d); return nil })
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = gate.Wait(ctx, KindRead)
	}
	if slept != 0 {
		t.Fatalf("slept %v within the limit", slept)
	}
	_ = gate.Wait(ctx, KindRead) // the fourth read in a minute has to wait for the first to leave the window
	if slept < time.Minute || slept > time.Minute+time.Second {
		t.Fatalf("slept %v, want about a minute", slept)
	}
	// The kinds are separate buckets.
	slept = 0
	_ = gate.Wait(ctx, KindFight)
	_ = gate.Wait(ctx, KindAct)
	if slept != 0 {
		t.Fatalf("a fight action should not wait for the act bucket: %v", slept)
	}
	if KindOf("PLAY") != KindFight || KindOf("FLEE") != KindFight || KindOf("REDRAW") != KindFight || KindOf("MOVE") != KindAct {
		t.Fatal("KindOf")
	}
}

func TestFleetSizeIsTwentyToFifty(t *testing.T) {
	for n, ok := range map[int]bool{0: false, 1: false, 19: false, 20: true, 35: true, 50: true, 51: false, 500: false} {
		err := ValidateFleetSize(n)
		if (err == nil) != ok {
			t.Errorf("size %d: err %v", n, err)
		}
		if !ok && !errors.Is(err, ErrFleetSize) {
			t.Errorf("size %d: the error should be ErrFleetSize, got %v", n, err)
		}
	}
	r := &Roster{}
	for i := 0; i < 19; i++ {
		r.Agents = append(r.Agents, RosterEntry{Name: fmt.Sprint(i), AgentID: fmt.Sprint(i), APIKey: "k"})
	}
	if _, err := NewFleet(r, api.New("http://x"), NewGate(DefaultLimits(), nil, nil), planner.Off{}, 100, 1, nil); !errors.Is(err, ErrFleetSize) {
		t.Fatalf("a fleet of 19 was built: %v", err)
	}
	r.Agents = append(r.Agents, RosterEntry{Name: "x", AgentID: "x", APIKey: "k"})
	f, err := NewFleet(r, api.New("http://x"), NewGate(DefaultLimits(), nil, nil), planner.Off{}, 100, 1, nil)
	if err != nil || len(f.Agents) != 20 {
		t.Fatalf("a fleet of 20: %v", err)
	}
}

func TestAFleetPlaysEveryAgentThroughOneGate(t *testing.T) {
	g := newFakeGame("MOVE", "REST")
	srv := httptest.NewServer(g)
	defer srv.Close()
	r := &Roster{}
	for i := 0; i < 20; i++ {
		r.Agents = append(r.Agents, RosterEntry{Name: fmt.Sprintf("Agent %d", i), AgentID: fmt.Sprint(i), APIKey: fmt.Sprintf("k%d", i)})
	}
	c := api.New(srv.URL)
	c.HTTP = srv.Client()
	f, err := NewFleet(r, c, NewGate(Limits{ReadsPerMinute: 100000, ActsPerMinute: 100000, FightPerMinute: 100000}, nil, nil), planner.Off{Reason: "test"}, 100, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.Stagger = 0
	var steps atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, a := range f.Agents {
		a.Sleep = func(ctx context.Context, d time.Duration) error {
			if steps.Add(1) > 100 {
				cancel()
			}
			return ctx.Err()
		}
	}
	_ = f.Run(ctx)
	seen := map[string]bool{}
	g.mu.Lock()
	for _, auth := range g.actAuth {
		seen[auth] = true
	}
	g.mu.Unlock()
	if len(seen) != 20 {
		t.Fatalf("%d of 20 agents acted", len(seen))
	}
}

func TestRosterFilesAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.json")
	r := &Roster{Agents: []RosterEntry{{Name: "A", AgentID: "id-a", APIKey: "key-a"}, {Name: "B", AgentID: "id-b", APIKey: "key-b"}}}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("saved with mode %v", info.Mode().Perm())
	}
	back, err := LoadRoster(path)
	if err != nil || len(back.Agents) != 2 || strings.Join(back.IDs(), ",") != "id-a,id-b" {
		t.Fatalf("load: %v %+v", err, back)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRoster(path); !errors.Is(err, ErrRosterPermissions) {
		t.Fatalf("a world-readable roster was accepted: %v", err)
	}
}

func TestRegisterMakesAgentsRetriesATakenNameAndStopsAtTheDailyLimit(t *testing.T) {
	g := newFakeGame()
	g.regErr = func(n int) (int, string) {
		switch {
		case n == 2:
			return http.StatusConflict, `{"error":"name taken","code":"CONFLICT"}`
		case n > 6:
			return http.StatusTooManyRequests, `{"error":"10 registrations per address per day","code":"RATE_LIMITED"}`
		}
		return 0, ""
	}
	srv := httptest.NewServer(g)
	defer srv.Close()
	c := api.New(srv.URL)
	c.HTTP = srv.Client()
	path := filepath.Join(t.TempDir(), "roster.json")
	r := &Roster{}
	rep, err := Register(context.Background(), c, r, path, 30, 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Registered != 5 || len(r.Agents) != 5 || !strings.Contains(rep.Stopped, "per address per day") {
		t.Fatalf("report %+v with %d agents", rep, len(r.Agents))
	}
	saved, err := LoadRoster(path)
	if err != nil || len(saved.Agents) != 5 {
		t.Fatalf("the partial roster was not kept: %v", err)
	}
	// Run again the next day: it resumes and does not repeat names.
	g.regErr = nil
	rep, err = Register(context.Background(), c, saved, path, 8, 42, "")
	if err != nil || rep.Registered != 3 || len(saved.Agents) != 8 {
		t.Fatalf("resume: %+v %v", rep, err)
	}
	names := map[string]bool{}
	for _, e := range saved.Agents {
		if names[e.Name] {
			t.Fatalf("duplicate name %s", e.Name)
		}
		names[e.Name] = true
	}
}

func TestNamesAreDistinctAndCarryNoMark(t *testing.T) {
	names := Names(7, 50)
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("duplicate %s", n)
		}
		seen[n] = true
		low := strings.ToLower(n)
		for _, mark := range []string{"house", "jev", "bot", "operator", "crust", "agent", "npc", "ai"} {
			if strings.Contains(low, mark) {
				t.Errorf("the name %q carries the mark %q", n, mark)
			}
		}
	}
	if len(names) != 50 {
		t.Fatalf("got %d names", len(names))
	}
}

func TestWithoutASeedTheNamesAreDrawnAtRandomAndKeptForTheNextDay(t *testing.T) {
	g := newFakeGame()
	srv := httptest.NewServer(g)
	defer srv.Close()
	c := api.New(srv.URL)
	c.HTTP = srv.Client()
	register := func(n int, r *Roster, path string) {
		t.Helper()
		if _, err := Register(context.Background(), c, r, path, n, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	a, b := &Roster{}, &Roster{}
	register(5, a, filepath.Join(dir, "a.json"))
	register(5, b, filepath.Join(dir, "b.json"))
	if a.Seed == 0 || a.Seed == b.Seed {
		t.Fatalf("seeds %d and %d: want two different seeds that are not the published default", a.Seed, b.Seed)
	}
	if reflect.DeepEqual(rosterNames(a), rosterNames(b)) {
		t.Fatal("two rosters made without a seed got the same names, so the names can be rebuilt")
	}
	for _, guess := range []uint64{0, 1, 42} {
		if reflect.DeepEqual(rosterNames(a), Names(guess, 5)) {
			t.Fatalf("the names are those of the guessable seed %d", guess)
		}
	}

	// The seed is saved with the roster, and the next day's run goes on with it.
	saved, err := LoadRoster(filepath.Join(dir, "a.json"))
	if err != nil || saved.Seed != a.Seed {
		t.Fatalf("saved seed %d (%v), want %d", saved.Seed, err, a.Seed)
	}
	register(8, saved, filepath.Join(dir, "a.json"))
	if saved.Seed != a.Seed || len(saved.Agents) != 8 {
		t.Fatalf("resumed with seed %d and %d agents", saved.Seed, len(saved.Agents))
	}
	seen := map[string]bool{}
	for _, name := range rosterNames(saved) {
		if seen[name] {
			t.Fatalf("duplicate name %s after resuming", name)
		}
		seen[name] = true
	}
}

func rosterNames(r *Roster) []string {
	var out []string
	for _, e := range r.Agents {
		out = append(out, e.Name)
	}
	return out
}
