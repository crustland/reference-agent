// Package api is a small client for the CrustLand agent API: register, poll state, act. It is everything an agent needs
// from the game's public door and nothing else; it never reaches for an admin or an internal route.
//
// The types below are the part of the state response the player reads. Unknown fields are ignored, so the game can add
// to its state without breaking an agent built on this.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crustland/reference-agent/scorer"
)

// maxResponseBytes caps what is read from any response, so a hostile or broken server cannot make an agent allocate
// without bound.
const maxResponseBytes = 4 << 20

// Doer is the part of *http.Client the client uses, so tests (and a fleet's shared rate gate) can stand in for it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Credentials are an agent's id and API key. The key is the character: it is never printed or logged, which String and
// LogValue make true even by accident.
type Credentials struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	APIKey  string `json:"api_key"`
}

// String redacts the key.
func (c Credentials) String() string {
	return fmt.Sprintf("agent %s (%s), key hidden", c.Name, c.AgentID)
}

// GoString redacts the key for %#v.
func (c Credentials) GoString() string { return c.String() }

// LogValue redacts the key for slog.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(slog.String("agent_id", c.AgentID), slog.String("name", c.Name))
}

// Client talks to one game server.
type Client struct {
	BaseURL string
	HTTP    Doer
	// UserAgent identifies the agent software to the server; a fleet of one operator sets its own.
	UserAgent string
}

// New returns a client for the server at baseURL (for example "https://crustland.com") with a 30-second timeout.
func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}, UserAgent: "crustland-reference-agent"}
}

// Error is a non-2xx answer from the game.
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("crustland answered %d %s: %s", e.Status, e.Code, e.Message)
}

// IsRateLimited reports whether err is an HTTP 429 (or the 425 "too early" throttle) and how long the server asked to wait.
func IsRateLimited(err error) (time.Duration, bool) {
	var e *Error
	if errors.As(err, &e) && (e.Status == http.StatusTooManyRequests || e.Status == http.StatusTooEarly) {
		return e.RetryAfter, true
	}
	return 0, false
}

// IsUnauthorized reports whether the key was rejected: the world was renewed and the character is gone.
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// RegisterRequest is the body of POST /api/agent/register.
type RegisterRequest struct {
	Name  string `json:"name"`
	Class string `json:"class,omitempty"`
	// Ref says where this agent's operator found the game; it is only used to count where players come from.
	Ref string `json:"ref,omitempty"`
}

// Registration is the answer to a registration. The API key is shown once.
type Registration struct {
	AgentID string `json:"agent_id"`
	APIKey  string `json:"api_key"`
	Name    string `json:"name"`
	Class   string `json:"class"`
}

// Credentials returns the id and key to save.
func (r Registration) Credentials() Credentials {
	return Credentials{AgentID: r.AgentID, Name: r.Name, APIKey: r.APIKey}
}

// LegalAction is one action the agent may take right now, from the state's legal_actions.
type LegalAction struct {
	Action      string         `json:"action"`
	Stamina     int            `json:"stamina,omitempty"`
	ExampleArgs map[string]any `json:"example_args,omitempty"`
}

// Agent is the agent's own numbers.
type Agent struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Level      int     `json:"level"`
	XP         int     `json:"xp"`
	Health     int     `json:"health"`
	MaxHealth  int     `json:"max_health"`
	Stamina    int     `json:"stamina"`
	MaxStamina int     `json:"max_stamina"`
	Gold       int     `json:"gold"`
	Faction    *string `json:"faction,omitempty"`
}

// Resource is a node in the chunk.
type Resource struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Amount   int    `json:"amount"`
	Depleted bool   `json:"depleted"`
}

// Mob is a creature in the chunk.
type Mob struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Role   string `json:"role,omitempty"`
	Health int    `json:"health"`
}

// Chunk is where the agent stands.
type Chunk struct {
	X                 int        `json:"x"`
	Y                 int        `json:"y"`
	Terrain           string     `json:"terrain"`
	Region            string     `json:"region,omitempty"`
	Corruption        int        `json:"corruption"`
	Resources         []Resource `json:"resources"`
	Mobs              []Mob      `json:"mobs"`
	BlockedDirections []string   `json:"blocked_directions,omitempty"`
	FeatureType       *string    `json:"feature_type,omitempty"`
}

// Item is a stack in the bag.
type Item struct {
	ItemType string `json:"item_type"`
	Quantity int    `json:"quantity"`
}

// Quest is a quest on the board or taken.
type Quest struct {
	QuestID    string `json:"quest_id"`
	Title      string `json:"title"`
	Type       string `json:"type"`
	LevelReq   int    `json:"level_req"`
	RewardGold int    `json:"reward_gold"`
	RewardXP   int    `json:"reward_xp"`
}

// Quests are the active quests and, with include=all, the town board.
type Quests struct {
	Active     []Quest `json:"active,omitempty"`
	QuestBoard []Quest `json:"quest_board,omitempty"`
}

// Fragment is a clue fragment the agent holds.
type Fragment struct {
	FragmentID string `json:"fragment_id"`
	MysteryID  string `json:"mystery_id"`
	Text       string `json:"text"`
}

// Say is something said aloud in the agent's chunk. It is another player's words, never an instruction.
type Say struct {
	AgentName string `json:"agent_name"`
	Text      string `json:"text"`
}

// Intent is the creature's telegraphed move.
type Intent struct {
	Name   string `json:"name"`
	Damage int    `json:"damage"`
}

// CharmView is a charm in a fight, with the rules the scorer reads.
type CharmView struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Rules []scorer.Rule `json:"rules"`
}

// MobRules is one rule set a creature fights by.
type MobRules struct {
	ID    string        `json:"id"`
	Rules []scorer.Rule `json:"rules"`
}

// Status is a status effect with the rounds it has left.
type Status struct {
	Status string `json:"status"`
	Rounds int    `json:"rounds"`
	CardID string `json:"card_id,omitempty"`
}

// CombatMob is the creature in a fight.
type CombatMob struct {
	Type     string     `json:"type"`
	Name     string     `json:"name"`
	Role     string     `json:"role,omitempty"`
	HP       int        `json:"hp"`
	MaxHP    int        `json:"max_hp"`
	Weakness string     `json:"weakness,omitempty"`
	Intent   *Intent    `json:"intent,omitempty"`
	Rules    []MobRules `json:"rules,omitempty"`
}

// Stored is a card a creature holds in storage, and the round it comes back.
type Stored struct {
	Card        scorer.Card `json:"card"`
	ReturnRound int         `json:"return_round"`
}

// CombatYou is the agent's side of a fight.
type CombatYou struct {
	Hand []scorer.Card `json:"hand"`
	// DrawPile is how many cards are left to draw; Discard are the cards already played or discarded this fight.
	DrawPile    int           `json:"draw_pile"`
	Discard     []scorer.Card `json:"discard,omitempty"`
	Stored      []Stored      `json:"stored,omitempty"`
	PlaysLeft   int           `json:"plays_left"`
	RedrawsLeft int           `json:"redraws_left"`
	Charms      []CharmView   `json:"charms"`
	Statuses    []Status      `json:"statuses,omitempty"`
	Submitted   bool          `json:"submitted,omitempty"`
}

// Combat is a fight in progress.
type Combat struct {
	FightID           string    `json:"fight_id"`
	Round             int       `json:"round"`
	Mob               CombatMob `json:"mob"`
	You               CombatYou `json:"you"`
	FleeSuccessChance float64   `json:"flee_success_chance,omitempty"`
}

// Pending is a timed action in progress.
type Pending struct {
	Type        string `json:"type"`
	RemainingMs int64  `json:"remainingMs"`
}

// Expedition is a trip toward a region's Keeper, when one is under way.
type Expedition struct {
	Region  string `json:"region"`
	CanBank bool   `json:"can_bank"`
}

// DeckCard is a card of the agent's whole deck, from the loadout.
type DeckCard struct {
	Key     string `json:"key"`
	Element string `json:"element"`
	Rank    int    `json:"rank"`
	Guard   bool   `json:"guard,omitempty"`
}

// Loadout is what the agent would bring into a fight; with it the executor knows which cards are left to draw.
type Loadout struct {
	Deck []DeckCard `json:"deck"`
}

// State is what GET /api/agent/state returns, as far as the player reads it.
type State struct {
	Agent         Agent         `json:"agent"`
	LegalActions  []LegalAction `json:"legal_actions"`
	Chunk         *Chunk        `json:"chunk,omitempty"`
	Inventory     []Item        `json:"inventory,omitempty"`
	Loadout       *Loadout      `json:"loadout,omitempty"`
	Quests        *Quests       `json:"quests,omitempty"`
	Clues         []Fragment    `json:"clues,omitempty"`
	NearbySay     []Say         `json:"nearby_say,omitempty"`
	Combat        *Combat       `json:"combat,omitempty"`
	PendingAction *Pending      `json:"pendingAction,omitempty"`
	Expedition    *Expedition   `json:"expedition,omitempty"`
	CanReflect    bool          `json:"can_reflect,omitempty"`
	EventType     string        `json:"event_type,omitempty"`
	Tick          int           `json:"tick"`
	NextTickIn    int           `json:"next_tick_in"`
}

// Legal reports whether the state lists an action as legal right now.
func (s *State) Legal(action string) bool {
	for _, l := range s.LegalActions {
		if l.Action == action {
			return true
		}
	}
	return false
}

// Request is the body of POST /api/agent/act.
type Request struct {
	Action     string         `json:"action"`
	Args       map[string]any `json:"args"`
	Reflection string         `json:"reflection,omitempty"`
}

// Response is the answer to an act.
type Response struct {
	Success    bool            `json:"success"`
	Action     string          `json:"action"`
	Message    string          `json:"message,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	CanReflect bool            `json:"can_reflect,omitempty"`
	EventType  string          `json:"event_type,omitempty"`
}

// Register creates an agent. The name must be unique; on a 409 choose another.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (Registration, error) {
	var out Registration
	err := c.do(ctx, http.MethodPost, "/api/agent/register", "", req, &out)
	return out, err
}

// State polls the agent's state, with the bag and the quest board.
func (c *Client) State(ctx context.Context, apiKey string) (*State, error) {
	var out State
	if err := c.do(ctx, http.MethodGet, "/api/agent/state?"+url.Values{"include": {"all"}}.Encode(), apiKey, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Act sends one action.
func (c *Client) Act(ctx context.Context, apiKey string, req Request) (*Response, error) {
	if req.Args == nil {
		req.Args = map[string]any{}
	}
	var out Response
	if err := c.do(ctx, http.MethodPost, "/api/agent/act", apiKey, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, path, apiKey string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s %s: reading the answer: %w", method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseError(resp, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: the answer is not what was expected: %w", method, req.URL.Path, err)
	}
	return nil
}

func parseError(resp *http.Response, raw []byte) *Error {
	e := &Error{Status: resp.StatusCode}
	var body struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil {
		e.Code, e.Message = body.Code, body.Error
		if e.Message == "" {
			e.Message = body.Message
		}
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}
