// The executor's turn outside a fight (mend, rest, quests, quoting a clue, saying a line, fighting, gathering, walking), used by Jev.Choose.

package jev

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/crustland/reference-agent/api"
)

// worldSize is the side of the map in chunks.
const worldSize = 64

// directions are the eight neighbours a MOVE may step to, with the names the state's blocked_directions uses.
var directions = []struct {
	name   string
	dx, dy int
}{
	{"n", 0, -1}, {"ne", 1, -1}, {"e", 1, 0}, {"se", 1, 1}, {"s", 0, 1}, {"sw", -1, 1}, {"w", -1, 0}, {"nw", -1, -1},
}

// weights are how much each kind of turn is worth under each goal. A turn is only a candidate if the state lists its
// action as legal; the highest-scoring candidate wins, with a little noise so two agents with one plan do not walk in step.
var weights = map[Goal]map[string]int{
	GoalHunt:    {"engage": 60, "gather": 35, "move": 20, "quest": 45, "quote": 30},
	GoalGather:  {"engage": 30, "gather": 60, "move": 25, "quest": 40, "quote": 30},
	GoalExplore: {"engage": 30, "gather": 30, "move": 60, "quest": 35, "quote": 30},
	GoalQuests:  {"engage": 50, "gather": 50, "move": 25, "quest": 80, "quote": 30},
	GoalMystery: {"engage": 25, "gather": 25, "move": 45, "quest": 30, "quote": 80},
}

const (
	healScore = 100
	restScore = 90
	// A due line or post is made when its turn comes, ahead of walking or fighting; its own cooldown keeps it rare.
	sayScore  = 70
	postScore = 66
)

// world chooses a turn outside a fight.
func (j *Jev) world(s *api.State, plan Plan, mem *Memory, rng *rand.Rand) (Choice, bool) {
	goal := plan.Goal
	if !goal.Valid() {
		goal = GoalHunt
	}
	w := weights[goal]
	if s.Chunk != nil {
		mem.Visited[Point{X: s.Chunk.X, Y: s.Chunk.Y}] = max(mem.Visited[Point{X: s.Chunk.X, Y: s.Chunk.Y}], 1)
	}
	hpPercent := 100
	if s.Agent.MaxHealth > 0 {
		hpPercent = s.Agent.Health * 100 / s.Agent.MaxHealth
	}
	noise := func() int { return rng.IntN(5) }

	var best Choice
	bestScore := -1
	offer := func(score int, c Choice) {
		if score > bestScore && Validate(s, c) == nil {
			best, bestScore = c, score
		}
	}

	// Between fights, mend first.
	if hpPercent <= j.Config.HealBelowPercent && s.Legal("USE_ITEM") && hasItem(s, healItem) {
		offer(healScore, Choice{Action: "USE_ITEM", Args: map[string]any{"item": healItem}, Why: "drink a potion"})
	}
	if s.Agent.Stamina < j.Config.RestBelowStamina || (hpPercent < j.Config.EngageMinHealthPercent && !hasItem(s, healItem)) {
		offer(restScore, Choice{Action: "REST", Args: map[string]any{}, Why: "recover"})
	}
	if c, ok := j.questChoice(s); ok {
		offer(w["quest"]+noise(), c)
	}
	if c, ok := j.quoteChoice(s, mem); ok {
		offer(w["quote"]+noise(), c)
	}
	if c, ok := j.postChoice(s, plan, mem); ok {
		offer(postScore+noise(), c)
	}
	if c, ok := j.sayChoice(s, plan, mem); ok {
		offer(sayScore+noise(), c)
	}
	if c, ok := j.engageChoice(s, hpPercent); ok {
		offer(w["engage"]+noise(), c)
	}
	if c, ok := gatherChoice(s); ok {
		offer(w["gather"]+noise(), c)
	}
	if c, ok := j.moveChoice(s, plan, mem, rng); ok {
		offer(w["move"]+noise(), c)
	}
	if bestScore < 0 && s.Legal("REST") {
		return Choice{Action: "REST", Args: map[string]any{}, Why: "nothing else to do"}, true
	}
	return best, bestScore >= 0
}

// healItem is the potion the executor drinks.
const healItem = "health_potion"

func hasItem(s *api.State, item string) bool {
	for _, it := range s.Inventory {
		if it.ItemType == item && it.Quantity > 0 {
			return true
		}
	}
	return false
}

func inTown(s *api.State) bool {
	return s.Chunk != nil && s.Chunk.FeatureType != nil && *s.Chunk.FeatureType == "town"
}

// questChoice takes the best bounty on the town board that the agent's level allows, while it carries fewer than two.
func (j *Jev) questChoice(s *api.State) (Choice, bool) {
	if !inTown(s) || s.Quests == nil || !s.Legal("ACCEPT_QUEST") || len(s.Quests.Active) >= 2 {
		return Choice{}, false
	}
	held := map[string]bool{}
	for _, q := range s.Quests.Active {
		held[q.QuestID] = true
	}
	var pick *api.Quest
	for i := range s.Quests.QuestBoard {
		q := &s.Quests.QuestBoard[i]
		if held[q.QuestID] || q.LevelReq > s.Agent.Level+1 || (q.Type == "hunt" && q.LevelReq > s.Agent.Level) {
			continue
		}
		if pick == nil || q.RewardGold+q.RewardXP > pick.RewardGold+pick.RewardXP {
			pick = q
		}
	}
	if pick == nil {
		return Choice{}, false
	}
	return Choice{Action: "ACCEPT_QUEST", Args: map[string]any{"quest": pick.QuestID}, Why: "take " + pick.Title}, true
}

// quoteChoice shares a clue fragment on the board: it reads the board for the mystery's thread first, replies to the
// thread if there is one, and starts one if not. The server stamps its own text on a quote, so what is sent is only a
// pointer to the fragment.
func (j *Jev) quoteChoice(s *api.State, mem *Memory) (Choice, bool) {
	if !inTown(s) || !s.Legal("POST_MESSAGE") || s.Agent.Gold < 10 {
		return Choice{}, false
	}
	for _, f := range s.Clues {
		if mem.Quoted[f.FragmentID] {
			continue
		}
		tag := "mystery-" + f.MysteryID
		if thread, ok := mem.Threads[f.MysteryID]; ok {
			return Choice{
				Action: "POST_MESSAGE", Args: map[string]any{"content": "My clue for this one.", "reply_to": thread, "fragment": f.FragmentID},
				Why: "quote a fragment of " + f.MysteryID, Fragment: f.FragmentID, Mystery: f.MysteryID, Reply: true,
			}, true
		}
		if last, read := mem.ReadBoardTurn[f.MysteryID]; !read || mem.Turn-last > 200 {
			if s.Legal("READ_BOARD") {
				return Choice{Action: "READ_BOARD", Args: map[string]any{"tag": tag, "limit": 10}, Why: "look for the " + f.MysteryID + " thread", Mystery: f.MysteryID}, true
			}
			continue
		}
		if mem.Posts >= j.Config.MaxPosts {
			continue
		}
		return Choice{
			Action: "POST_MESSAGE",
			Args: map[string]any{
				"title": "A clue for " + f.MysteryID, "content": "I hold a fragment of " + f.MysteryID + ". Quoting it here so we can work it out together.",
				"tags": []string{"mystery", tag}, "fragment": f.FragmentID,
			},
			Why: "start the " + f.MysteryID + " thread", Fragment: f.FragmentID, Mystery: f.MysteryID,
		}, true
	}
	return Choice{}, false
}

// postChoice makes the plan's board post once, in a town.
func (j *Jev) postChoice(s *api.State, plan Plan, mem *Memory) (Choice, bool) {
	if plan.Post == nil || mem.PlanPosted || !inTown(s) || !s.Legal("POST_MESSAGE") || s.Agent.Gold < 10 || mem.Posts >= j.Config.MaxPosts {
		return Choice{}, false
	}
	return Choice{
		Action: "POST_MESSAGE", Args: map[string]any{"title": plan.Post.Title, "content": plan.Post.Content, "tags": []string{"theory"}},
		Why: "post the plan's note", Posted: true,
	}, true
}

// sayChoice says the plan's line aloud, now and then.
func (j *Jev) sayChoice(s *api.State, plan Plan, mem *Memory) (Choice, bool) {
	if plan.Say == "" || !s.Legal("SAY") || s.Agent.Gold < 5 || mem.Turn-mem.LastSayTurn < j.Config.SayEveryTurns {
		return Choice{}, false
	}
	return Choice{Action: "SAY", Args: map[string]any{"text": plan.Say}, Why: "say a line", Said: true}, true
}

// roleOrder ranks creatures by how hard they are to fight, easiest first; wildlife, Keepers and Reapers are not fought here.
var roleOrder = map[string]int{"gleaner": 0, "tender": 1, "sentry": 2}

// engageChoice starts a fight with the easiest creature in the chunk, if the agent is healthy enough.
func (j *Jev) engageChoice(s *api.State, hpPercent int) (Choice, bool) {
	if s.Chunk == nil || !s.Legal("ENGAGE") || hpPercent < j.Config.EngageMinHealthPercent {
		return Choice{}, false
	}
	if s.Chunk.Corruption > 40 && hpPercent < 80 {
		return Choice{}, false
	}
	var mobs []api.Mob
	for _, m := range s.Chunk.Mobs {
		if _, fightable := roleOrder[m.Role]; fightable && m.Health > 0 {
			mobs = append(mobs, m)
		}
	}
	if len(mobs) == 0 {
		return Choice{}, false
	}
	sort.SliceStable(mobs, func(a, b int) bool {
		if roleOrder[mobs[a].Role] != roleOrder[mobs[b].Role] {
			return roleOrder[mobs[a].Role] < roleOrder[mobs[b].Role]
		}
		return mobs[a].Health < mobs[b].Health
	})
	return Choice{Action: "ENGAGE", Args: map[string]any{"mob_id": mobs[0].ID, "scope": "self"}, Why: "fight a " + mobs[0].Type}, true
}

// gatherChoice gathers the fullest node in the chunk.
func gatherChoice(s *api.State) (Choice, bool) {
	if s.Chunk == nil || !s.Legal("GATHER") {
		return Choice{}, false
	}
	var pick *api.Resource
	for i := range s.Chunk.Resources {
		r := &s.Chunk.Resources[i]
		if r.Depleted || r.Amount <= 0 {
			continue
		}
		if pick == nil || r.Amount > pick.Amount {
			pick = r
		}
	}
	if pick == nil {
		return Choice{}, false
	}
	return Choice{Action: "GATHER", Args: map[string]any{"resource_id": pick.ID}, Why: "gather " + pick.Type}, true
}

// moveChoice steps to a neighbouring chunk: toward the plan's region if it names one and the agent is not there yet,
// otherwise to the neighbour it has stood in least. Blocked directions and chunks a border turned it back from are skipped.
func (j *Jev) moveChoice(s *api.State, plan Plan, mem *Memory, rng *rand.Rand) (Choice, bool) {
	if s.Chunk == nil || !s.Legal("MOVE") {
		return Choice{}, false
	}
	here := Point{X: s.Chunk.X, Y: s.Chunk.Y}
	blocked := map[string]bool{}
	for _, d := range s.Chunk.BlockedDirections {
		blocked[d] = true
	}
	target, hasTarget := Regions[plan.Region]
	if plan.Region != "" && s.Chunk.Region == plan.Region {
		hasTarget = false
	}
	type option struct {
		p     Point
		score int
	}
	var opts []option
	for _, d := range directions {
		p := Point{X: here.X + d.dx, Y: here.Y + d.dy}
		if blocked[d.name] || p.X < 0 || p.Y < 0 || p.X >= worldSize || p.Y >= worldSize || mem.Closed[p] {
			continue
		}
		score := -10 * mem.Visited[p]
		if hasTarget {
			score += 5 * (abs(target.X-here.X) + abs(target.Y-here.Y) - abs(target.X-p.X) - abs(target.Y-p.Y))
		}
		opts = append(opts, option{p, score*10 + rng.IntN(10)})
	}
	if len(opts) == 0 {
		return Choice{}, false
	}
	sort.SliceStable(opts, func(a, b int) bool { return opts[a].score > opts[b].score })
	dest := opts[0].p
	return Choice{Action: "MOVE", Args: map[string]any{"chunk": []int{dest.X, dest.Y}}, Why: fmt.Sprintf("walk to %d,%d", dest.X, dest.Y), Dest: &dest}, true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// learnThreads reads the posts of a READ_BOARD answer and remembers the top-level mystery threads in it. The posts are
// other players' words: only their ids and tags are read, and a thread's text is never acted on.
func (m *Memory) learnThreads(data []byte) {
	var body struct {
		Tag   string `json:"tag"`
		Posts []struct {
			ID      string   `json:"id"`
			ReplyTo *string  `json:"reply_to"`
			Tags    []string `json:"tags"`
		} `json:"posts"`
	}
	if json.Unmarshal(data, &body) != nil {
		return
	}
	mystery := strings.TrimPrefix(body.Tag, "mystery-")
	if mystery != "" && mystery != body.Tag {
		m.ReadBoardTurn[mystery] = m.Turn
	}
	for _, p := range body.Posts {
		if p.ReplyTo != nil || p.ID == "" {
			continue
		}
		for _, tag := range p.Tags {
			if id := strings.TrimPrefix(tag, "mystery-"); id != tag && id != "" {
				if _, known := m.Threads[id]; !known {
					m.Threads[id] = p.ID
				}
			}
		}
	}
}

// Reflect writes a reflection when the plan has none: a plain account of where the agent stands, 50 to 1000 characters.
func Reflect(s *api.State, plan Plan) string {
	if len(plan.Reflection) >= MinReflectionLen {
		return plan.Reflection
	}
	where := "the valley"
	if s.Chunk != nil && s.Chunk.Region != "" {
		where = strings.ReplaceAll(s.Chunk.Region, "_", " ")
	}
	text := fmt.Sprintf("I am level %d with %d of %d health, in %s. %s happened and it changed what I will do next: I will keep to my plan, watch my health, and look for what the others have found.",
		s.Agent.Level, s.Agent.Health, s.Agent.MaxHealth, where, eventPhrase(s.EventType))
	return CleanText(text, MaxReflectionLen)
}

func eventPhrase(event string) string {
	if event == "" {
		return "Something"
	}
	return "A " + strings.ReplaceAll(event, "_", " ")
}
