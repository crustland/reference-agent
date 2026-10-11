// Package jev is the cheap turn executor of a CrustLand agent: given the state the game returns, it picks the next action
// from the state's legal_actions, with no model call. It is rules and arithmetic: the exact scorer for fights, a few
// priorities for the rest, nudged by a Plan.
//
// "Jev" names the role (the fast System One decision-maker that picks among enumerated options, as opposed to the slow
// planner that sets a direction every hundred turns). This package is a plain rule-based implementation of the role. It
// does not call any hosted decision model; a different executor can replace it behind the Executor interface.
//
// Every choice is checked against the state before it can be sent (Validate): an action the state does not list as legal
// is never returned, and a short list of actions that touch seats, prizes, wallets or trades is never returned either,
// whatever the state lists, because an operator's agents stay out of everything that pays.
package jev

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/crustland/reference-agent/api"
)

// Executor picks the next action of an agent from its state and plan. Jev is the reference implementation.
type Executor interface {
	Choose(s *api.State, plan Plan, mem *Memory, rng *rand.Rand) (Choice, error)
}

// Errors of Choose and Validate.
var (
	// ErrWait means there is nothing to send this turn: the agent is busy, dead, or the state lists nothing legal.
	ErrWait = errors.New("nothing to do this turn")
	// ErrNotLegal means a choice is not among the state's legal_actions.
	ErrNotLegal = errors.New("the action is not in the state's legal_actions")
	// ErrDenied means a choice is an action an operator's agent never takes.
	ErrDenied = errors.New("the action is one this agent never takes")
)

// denied are the actions an operator's agent never takes, with the reason. The game already refuses a house agent most of
// them; the executor does not rely on that.
var denied = map[string]string{
	"SUBMIT_OFFERING":  "house agents never make an offering",
	"LIST_FRAGMENT":    "house agents never trade clues",
	"BUY_FRAGMENT":     "house agents never trade clues",
	"UNLIST_FRAGMENT":  "house agents never trade clues",
	"BIND_SEAT":        "house agents never hold a seat",
	"LIST_ITEM":        "house agents never trade",
	"UNLIST_ITEM":      "house agents never trade",
	"BUY_ITEM":         "house agents never trade",
	"BUY_FROM_TRADER":  "house agents never trade",
	"SELL_TO_TRADER":   "house agents never trade",
	"PRESTIGE":         "house agents earn no badge",
	"GATHER_RARE":      "rare nodes are for seats",
	"PICK_KEEPER_RULE": "territory is for seats",
}

// Denied reports whether an action is one the executor never takes, and why. Every TRIAL_ action is.
func Denied(action string) (string, bool) {
	if strings.HasPrefix(action, "TRIAL_") {
		return "house agents never enter the Trial", true
	}
	why, ok := denied[action]
	return why, ok
}

// Choice is one action to send, with what the executor expected of it (so Observe can learn from the answer).
type Choice struct {
	Action string
	Args   map[string]any
	// Why is a short line for the log; for PLAY it is also the reason spectators see (at most 120 characters).
	Why string

	// Form and Damage are what the scorer expects of a PLAY.
	Form   string
	Damage int
	// Dest is the chunk a MOVE walks to.
	Dest *Point
	// Fragment and Mystery name the clue a POST_MESSAGE quotes.
	Fragment, Mystery string
	// Reply is true if the post is a reply to a thread.
	Reply bool
	// Said is true for a SAY, and Posted for a plan's board post.
	Said, Posted bool
}

// Validate is the gate every choice passes before it is sent: the action must be among the state's legal_actions and not
// among the actions an operator's agent never takes. It checks nothing about args; the game does.
func Validate(s *api.State, c Choice) error {
	if c.Action == "" {
		return fmt.Errorf("%w: no action", ErrNotLegal)
	}
	if why, bad := Denied(c.Action); bad {
		return fmt.Errorf("%w: %s (%s)", ErrDenied, c.Action, why)
	}
	if !s.Legal(c.Action) {
		return fmt.Errorf("%w: %s", ErrNotLegal, c.Action)
	}
	return nil
}

// Config holds the executor's few thresholds.
type Config struct {
	// FleeBelowPercent is the health percent under which a fight that cannot be won this round is fled.
	FleeBelowPercent int
	// HealBelowPercent is the health percent under which a potion is drunk between fights.
	HealBelowPercent int
	// EngageMinHealthPercent is the health percent a fight is started at or above.
	EngageMinHealthPercent int
	// RestBelowStamina is the stamina under which the agent rests.
	RestBelowStamina int
	// SayEveryTurns is how many turns pass between two lines said aloud.
	SayEveryTurns int
	// NoRedraw turns the redraw off: the executor then only ever plays or flees.
	NoRedraw bool
	// MaxPosts is how many top-level board posts one agent makes in its life, so a long-running agent does not fill a board.
	MaxPosts int
}

// DefaultConfig is the thresholds the reference agent ships with.
func DefaultConfig() Config {
	return Config{FleeBelowPercent: 25, HealBelowPercent: 45, EngageMinHealthPercent: 60, RestBelowStamina: 20, SayEveryTurns: 80, MaxPosts: 6}
}

// Jev is the rule-based executor. It keeps no state of its own: what an agent has done lives in its Memory.
type Jev struct {
	Config Config
}

// New returns a Jev with the default thresholds.
func New() *Jev { return &Jev{Config: DefaultConfig()} }

// Memory is what one agent remembers between turns. It is not safe for concurrent use: one goroutine owns an agent.
type Memory struct {
	// Turn counts the turns the executor has been asked for.
	Turn int
	// Visited counts the chunks the agent has stood in; Closed are chunks a border turned it back from.
	Visited map[Point]int
	Closed  map[Point]bool
	// Quoted are the fragments already quoted on the board; Threads are the mystery threads seen (mystery id -> post id).
	Quoted  map[string]bool
	Threads map[string]string
	// ReadBoardTurn is the turn the board was last read for a mystery, so it is not read every turn.
	ReadBoardTurn map[string]int
	// Posts counts the top-level posts made; PlanPosted is set once the current plan's post was made.
	Posts      int
	PlanPosted bool
	// LastSayTurn is the turn of the last line said.
	LastSayTurn int
	// Fight is what the executor keeps about the fight in progress.
	Fight FightMemory
}

// FightMemory is the creature-rule facts of one fight that the state does not carry.
type FightMemory struct {
	ID         string
	Forms      []string
	LastDamage int
}

// NewMemory returns an empty memory.
func NewMemory() *Memory {
	return &Memory{Visited: map[Point]int{}, Closed: map[Point]bool{}, Quoted: map[string]bool{}, Threads: map[string]string{}, ReadBoardTurn: map[string]int{}}
}

// Choose picks the next action. It returns ErrWait when there is nothing to send and never returns a choice that fails
// Validate.
func (j *Jev) Choose(s *api.State, plan Plan, mem *Memory, rng *rand.Rand) (Choice, error) {
	mem.Turn++
	if s.PendingAction != nil || len(s.LegalActions) == 0 {
		return Choice{}, ErrWait
	}
	var c Choice
	var ok bool
	if s.Combat != nil {
		c, ok = j.fight(s, mem)
	} else {
		c, ok = j.world(s, plan, mem, rng)
	}
	if !ok {
		return Choice{}, ErrWait
	}
	if err := Validate(s, c); err != nil {
		return Choice{}, err
	}
	return c, nil
}

// Observe records what the game answered to a choice, so the next turn knows. err is the error of the act call, nil on
// success; data is the answer's data (for READ_BOARD, the posts).
func (m *Memory) Observe(c Choice, err error, data []byte) {
	if c.Action == "READ_BOARD" && c.Mystery != "" {
		// Read once per stretch whatever the answer, so a refused or empty read is not repeated every turn.
		m.ReadBoardTurn[c.Mystery] = m.Turn
	}
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == "BORDER_CLOSED" && c.Dest != nil {
			m.Closed[*c.Dest] = true
		}
		return
	}
	switch c.Action {
	case "PLAY":
		m.Fight.Forms = append(m.Fight.Forms, c.Form)
		m.Fight.LastDamage = c.Damage
	case "MOVE":
		if c.Dest != nil {
			m.Visited[*c.Dest]++
		}
	case "POST_MESSAGE":
		if c.Fragment != "" {
			m.Quoted[c.Fragment] = true
		}
		if !c.Reply {
			m.Posts++
		}
		if c.Posted {
			m.PlanPosted = true
		}
	case "SAY":
		m.LastSayTurn = m.Turn
	case "READ_BOARD":
		m.learnThreads(data)
	}
}

// Reset clears what belongs to one plan; call it when a new plan arrives.
func (m *Memory) Reset() { m.PlanPosted = false }
