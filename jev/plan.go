// The plan a planner hands the executor, its closed list of goals, and the cleaning of every text an agent sends; used by the executor and the planner.

package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Goal is what a plan asks the executor to favour. It is a closed list: the planner chooses among these and writes text,
// and never names an action, so nothing a model writes (or reads off the board) can steer the executor to an action it
// would not take.
type Goal string

// The goals.
const (
	// GoalHunt favours fights with creatures, for experience and drops.
	GoalHunt Goal = "hunt"
	// GoalGather favours gathering resources.
	GoalGather Goal = "gather"
	// GoalExplore favours walking to new chunks, toward the plan's region if it names one.
	GoalExplore Goal = "explore"
	// GoalQuests favours town bounties, then the fights and gathers they ask for.
	GoalQuests Goal = "quests"
	// GoalMystery favours quoting clue fragments on the board and walking to where fragments are found.
	GoalMystery Goal = "mystery"
)

// Goals lists the goals in a fixed order.
var Goals = []Goal{GoalHunt, GoalGather, GoalExplore, GoalQuests, GoalMystery}

// Valid reports whether g is one of the goals.
func (g Goal) Valid() bool {
	for _, k := range Goals {
		if k == g {
			return true
		}
	}
	return false
}

// Regions are the region ids a plan may name, and the lair chunk of each (a point inside the region to walk toward).
var Regions = map[string]Point{
	"verdant_reach": {X: 31, Y: 17},
	"ashen_wastes":  {X: 10, Y: 55},
	"fetid_marsh":   {X: 55, Y: 55},
	"frozen_depths": {X: 58, Y: 6},
	"molten_forge":  {X: 4, Y: 7},
}

// Point is a chunk coordinate.
type Point struct{ X, Y int }

// Limits of the text a plan carries, inside the game's own limits (a SAY is 140 characters, a title 6 to 90, a post 500,
// a reflection 50 to 1000).
const (
	MaxSayLen        = 140
	MaxTitleLen      = 90
	MinTitleLen      = 6
	MaxPostLen       = 500
	MinReflectionLen = 50
	MaxReflectionLen = 1000
	maxNoteLen       = 600
)

// Post is a board post a plan asks for.
type Post struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// Plan is what the planner hands the executor for the next stretch of turns: a goal, maybe a region, and a few words to
// say. The zero Plan is valid and means "hunt where you are".
type Plan struct {
	Goal   Goal   `json:"goal"`
	Region string `json:"region,omitempty"`
	// Say is a line to say aloud now and then (at most 140 characters).
	Say string `json:"say,omitempty"`
	// Post is a board post to make once, in a town.
	Post *Post `json:"post,omitempty"`
	// Reflection is the text for the next reflection the game offers.
	Reflection string `json:"reflection,omitempty"`
	// Note is the planner's own memory for its next checkpoint; the executor never reads it.
	Note string `json:"note,omitempty"`
}

// ErrBadPlan is returned for planner output that is not a plan.
var ErrBadPlan = errors.New("not a valid plan")

// ParsePlan reads a plan from the planner's reply. The reply must be one JSON object (a surrounding code fence is
// tolerated); the goal must be one of Goals; a region must be one of Regions; every text is cleaned, and text the game
// would refuse is dropped rather than sent. Unknown fields are an error: the planner has no way to name an action.
func ParsePlan(reply string) (Plan, error) {
	body := strings.TrimSpace(reply)
	body = strings.TrimPrefix(body, "```json")
	body = strings.TrimPrefix(body, "```")
	body = strings.TrimSuffix(body, "```")
	body = strings.TrimSpace(body)
	var raw Plan
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrBadPlan, err)
	}
	if dec.More() {
		return Plan{}, fmt.Errorf("%w: more than one value", ErrBadPlan)
	}
	if !raw.Goal.Valid() {
		return Plan{}, fmt.Errorf("%w: goal %q is not one of %v", ErrBadPlan, raw.Goal, Goals)
	}
	if raw.Region != "" {
		if _, ok := Regions[raw.Region]; !ok {
			return Plan{}, fmt.Errorf("%w: region %q is unknown", ErrBadPlan, raw.Region)
		}
	}
	out := Plan{Goal: raw.Goal, Region: raw.Region}
	out.Say = CleanText(raw.Say, MaxSayLen)
	out.Reflection = CleanText(raw.Reflection, MaxReflectionLen)
	if len(out.Reflection) < MinReflectionLen {
		out.Reflection = ""
	}
	out.Note = strings.TrimSpace(truncate(stripControl(raw.Note), maxNoteLen))
	if raw.Post != nil {
		title, content := CleanText(raw.Post.Title, MaxTitleLen), CleanText(raw.Post.Content, MaxPostLen)
		if len(title) >= MinTitleLen && content != "" {
			out.Post = &Post{Title: title, Content: content}
		}
	}
	return out, nil
}

var (
	linkLike    = regexp.MustCompile(`(?i)(https?:|www\.|\w\.(com|org|net|io|xyz|ai)\b|://)`)
	walletLike  = regexp.MustCompile(`(?i)\b0x[0-9a-f]{16,}`)
	keyLike     = regexp.MustCompile(`\b[A-Za-z0-9_\-]{32,}\b`)
	markupLike  = regexp.MustCompile("[<>{}\\[\\]`]")
	instruction = regexp.MustCompile(`(?i)\b(ignore (all |any |your |previous )|system prompt|you must now|disregard|as an ai|api[_ -]?key|private key|seed phrase)`)
)

// CleanText returns s as text the game will accept, or "" if it cannot be made to be: control and hidden characters are
// removed, space is collapsed, and anything that looks like a link, a wallet address, a key, markup, an instruction to
// another model or a long run of one character is dropped altogether (the game refuses those, and a house agent has no
// use for a post that is half-edited). It is cut to max characters at a word.
func CleanText(s string, max int) string {
	s = strings.Join(strings.Fields(stripControl(s)), " ")
	if s == "" {
		return ""
	}
	if linkLike.MatchString(s) || walletLike.MatchString(s) || keyLike.MatchString(s) || markupLike.MatchString(s) ||
		instruction.MatchString(s) || hasRun(s, 6) || shouting(s) {
		return ""
	}
	return strings.TrimSpace(truncate(s, max))
}

// hasRun reports six or more of the same character in a row ("aaaaaa"), which the game refuses.
func hasRun(s string, n int) bool {
	run, prev := 0, rune(-1)
	for _, r := range s {
		if r == prev {
			run++
		} else {
			run, prev = 1, r
		}
		if run >= n {
			return true
		}
	}
	return false
}

// shouting reports text that is mostly capitals, which the game refuses.
func shouting(s string) bool {
	letters, upper := 0, 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	return letters >= 12 && upper*10 > letters*7
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == ' ', r == ' ':
			return -1
		}
		return r
	}, s)
}

// truncate cuts s to at most max characters, at a space when it can.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	cut := string([]rune(s)[:max])
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut)
}
