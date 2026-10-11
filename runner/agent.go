// One agent's loop (poll, plan every so often, choose, check, act), used by the fleet and by the crust-agent command.

package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/crustland/reference-agent/api"
	"github.com/crustland/reference-agent/jev"
	"github.com/crustland/reference-agent/planner"
)

// Errors of a step.
var (
	// ErrKeyRejected means the game does not know the key: the world was renewed and the character is gone.
	ErrKeyRejected = errors.New("the game rejected the agent's key")
	// ErrIllegalChoice means the executor returned an action the state does not allow; nothing was sent.
	ErrIllegalChoice = errors.New("the executor chose an action that is not allowed; nothing was sent")
)

// Pacing holds the waits of the loop. The game allows about ten non-fight actions a minute to one agent and thirty fight
// actions, so the defaults sit at those rates; waiting for the world uses the tick length the state reports.
type Pacing struct {
	// Act is the least time between two non-fight actions of one agent, Fight between two fight actions.
	Act, Fight time.Duration
	// Idle is how long to wait when there is nothing to do and the state gives no tick to wait for.
	Idle time.Duration
	// Backoff is the wait after an error that is not a rate limit.
	Backoff time.Duration
}

// DefaultPacing is the pacing an agent runs with.
func DefaultPacing() Pacing {
	return Pacing{Act: 6500 * time.Millisecond, Fight: 2200 * time.Millisecond, Idle: 15 * time.Second, Backoff: 20 * time.Second}
}

// Agent is one character and everything its loop needs. One goroutine owns an Agent.
type Agent struct {
	Creds    api.Credentials
	Client   *api.Client
	Exec     jev.Executor
	Planner  planner.Planner
	Every    int
	Gate     *Gate
	Pacing   Pacing
	Rng      *rand.Rand
	Log      *slog.Logger
	Sleep    func(ctx context.Context, d time.Duration) error
	Mem      *jev.Memory
	Plan     jev.Plan
	turns    int
	planTurn int
	recent   []string
}

// New returns an agent with the reference executor, the default pacing and no planner (the executor plays on its own).
func New(creds api.Credentials, client *api.Client, gate *Gate, seed uint64) *Agent {
	return &Agent{
		Creds: creds, Client: client, Exec: jev.New(), Planner: planner.Off{Reason: "no planner configured"}, Every: planner.DefaultEvery,
		Gate: gate, Pacing: DefaultPacing(), Rng: rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15)), Log: slog.Default(), Sleep: Sleep,
		Mem: jev.NewMemory(), Plan: jev.Plan{Goal: jev.GoalHunt},
	}
}

// Outcome is what one step did.
type Outcome struct {
	// Choice is what was sent; zero if nothing was.
	Choice jev.Choice
	// Waited is how long the loop should wait before the next step.
	Wait time.Duration
}

// Step polls the state, asks the planner if a checkpoint is due, lets the executor choose, checks the choice against the
// state once more and sends it. It returns how long to wait before the next step. It never sends an action the state does
// not list as legal.
func (a *Agent) Step(ctx context.Context) (Outcome, error) {
	if err := a.Gate.Wait(ctx, KindRead); err != nil {
		return Outcome{}, err
	}
	s, err := a.Client.State(ctx, a.Creds.APIKey)
	if err != nil {
		return Outcome{Wait: a.failureWait(err)}, a.classify("state", err)
	}
	a.turns++
	a.checkpoint(ctx, s)

	choice, err := a.Exec.Choose(s, a.Plan, a.Mem, a.Rng)
	switch {
	case errors.Is(err, jev.ErrWait):
		return Outcome{Wait: a.idleWait(s)}, nil
	case err != nil:
		return Outcome{Wait: a.Pacing.Backoff}, fmt.Errorf("choosing: %w", err)
	}
	// The executor is trusted to choose well and not to be right: the state decides what may be sent.
	if err := jev.Validate(s, choice); err != nil {
		return Outcome{Wait: a.Pacing.Backoff}, fmt.Errorf("%w: %v", ErrIllegalChoice, err)
	}

	req := api.Request{Action: choice.Action, Args: choice.Args}
	if s.CanReflect {
		req.Reflection = jev.Reflect(s, a.Plan)
		a.Plan.Reflection = ""
	}
	if err := a.Gate.Wait(ctx, KindOf(choice.Action)); err != nil {
		return Outcome{}, err
	}
	resp, err := a.Client.Act(ctx, a.Creds.APIKey, req)
	var data []byte
	if resp != nil {
		data = resp.Data
	}
	a.Mem.Observe(choice, err, data)
	a.note(choice, err)
	wait := a.Pacing.Act
	if KindOf(choice.Action) == KindFight {
		wait = a.Pacing.Fight
	}
	if err != nil {
		if w := a.failureWait(err); w > wait {
			wait = w
		}
		// A refusal of the action is part of playing (a mob moved, a stamina cost changed); only the other errors stop a step.
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != 401 && ae.Status != 429 && ae.Status != 425 {
			return Outcome{Choice: choice, Wait: wait}, nil
		}
		return Outcome{Choice: choice, Wait: wait}, a.classify("act", err)
	}
	return Outcome{Choice: choice, Wait: wait}, nil
}

// Run plays until the context ends or the game rejects the key.
func (a *Agent) Run(ctx context.Context) error {
	for {
		out, err := a.Step(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrKeyRejected) {
				return err
			}
			a.Log.Warn("step failed", "agent", a.Creds.Name, "error", err)
		}
		if err := a.Sleep(ctx, out.Wait); err != nil {
			return err
		}
	}
}

// idleWait is how long to wait when nothing can be done: until the next world tick if the state says when, else Idle.
func (a *Agent) idleWait(s *api.State) time.Duration {
	if s.NextTickIn > 0 {
		return time.Duration(s.NextTickIn)*time.Second + 500*time.Millisecond
	}
	return a.Pacing.Idle
}

func (a *Agent) failureWait(err error) time.Duration {
	if d, limited := api.IsRateLimited(err); limited {
		return max(d, 5*time.Second)
	}
	return a.Pacing.Backoff
}

func (a *Agent) classify(what string, err error) error {
	if api.IsUnauthorized(err) {
		return fmt.Errorf("%w (%s)", ErrKeyRejected, what)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// checkpoint asks the planner for a new plan every a.Every turns. It asks once per checkpoint, win or lose: a failed or
// refused plan is not retried until the next one, so a planner that is off, out of budget or down costs nothing.
func (a *Agent) checkpoint(ctx context.Context, s *api.State) {
	if a.Every <= 0 || a.turns-a.planTurn < a.Every {
		return
	}
	a.planTurn = a.turns
	cp := planner.Checkpoint{
		Agent: a.Creds.AgentID, Turn: a.turns, Level: s.Agent.Level, Health: s.Agent.Health, MaxHealth: s.Agent.MaxHealth, Gold: s.Agent.Gold,
		Plan: a.Plan, Recent: a.recent,
	}
	if s.Chunk != nil {
		cp.Region = s.Chunk.Region
	}
	for _, f := range s.Clues {
		cp.Clues = append(cp.Clues, f.MysteryID+": "+f.Text)
	}
	for _, say := range s.NearbySay {
		cp.Heard = append(cp.Heard, say.AgentName+": "+say.Text)
	}
	plan, err := a.Planner.Plan(ctx, cp)
	a.recent = nil
	if err != nil {
		if !errors.Is(err, planner.ErrOff) {
			a.Log.Info("no new plan", "agent", a.Creds.Name, "reason", err.Error())
		}
		return
	}
	a.Plan = plan
	a.Mem.Reset()
}

// note keeps a short line about what the executor did, for the next checkpoint.
func (a *Agent) note(c jev.Choice, err error) {
	line := c.Action
	if c.Why != "" {
		line += ": " + c.Why
	}
	if err != nil {
		line += " (refused)"
	}
	a.recent = append(a.recent, line)
	if len(a.recent) > 40 {
		a.recent = a.recent[len(a.recent)-40:]
	}
	a.Log.Debug("turn", "agent", a.Creds.Name, "turn", a.turns, "action", strings.ToLower(c.Action))
}
