// The executor's fight turn (play, redraw or flee) and the pricing of a play, used by Jev.Choose and by the sims that seat it in an engine fight.

package jev

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sort"

	"github.com/crustland/reference-agent/api"
	"github.com/crustland/reference-agent/scorer"
)

// startPlays is how many plays a fight starts with; the play number is startPlays - plays_left + 1.
const startPlays = 4

// frozen is the status that keeps a card out of a play.
const frozen = "frozen"

// FightRules turns a fight's view into the scorer's inputs: the hand without frozen cards, the charms and the creature's
// rules, and the context of the play about to be made.
func FightRules(s *api.State, mem *Memory) (hand []scorer.Card, charms, mob []scorer.RuleSet, ctx scorer.Context) {
	cb := s.Combat
	skip := map[string]bool{}
	for _, st := range cb.You.Statuses {
		if st.Status == frozen && st.CardID != "" {
			skip[st.CardID] = true
		}
	}
	for _, c := range cb.You.Hand {
		if !skip[c.ID] {
			hand = append(hand, c)
		}
	}
	for _, ch := range cb.You.Charms {
		charms = append(charms, scorer.RuleSet{ID: ch.ID, Name: ch.Name, Rules: ch.Rules})
	}
	for _, m := range cb.Mob.Rules {
		mob = append(mob, scorer.RuleSet{ID: m.ID, Rules: m.Rules})
	}
	terrain := ""
	if s.Chunk != nil {
		terrain = s.Chunk.Terrain
	}
	hp := 100
	if s.Agent.MaxHealth > 0 {
		hp = s.Agent.Health * 100 / s.Agent.MaxHealth
	}
	ctx = scorer.Context{
		Terrain: terrain, RedrawsLeft: cb.You.RedrawsLeft, PlayNumber: startPlays - cb.You.PlaysLeft + 1, HPPercent: hp,
		Weakness: cb.Mob.Weakness, Round: cb.Round + 1,
	}
	if mem != nil && mem.Fight.ID == cb.FightID {
		ctx.FormsPlayed = append([]string(nil), mem.Fight.Forms...)
		ctx.LastDamage = mem.Fight.LastDamage
	}
	return hand, charms, mob, ctx
}

// valuer prices a play: the damage it deals, less the damage it leaves the creature's telegraphed strike to do, at the
// exchange rate between the two sides' health. A play that kills ends the fight before the strike.
type valuer struct {
	charms, mob []scorer.RuleSet
	ctx         scorer.Context
	strike      int
	rate        float64
	mobHP       int
}

// price is what one option is worth, what it costs in health, and whether it kills.
func (v valuer) price(opt scorer.Option) (value float64, taken int, kills bool) {
	dealt := min(opt.Score.Damage, v.mobHP)
	kills = opt.Score.Damage >= v.mobHP
	taken = opt.Score.SelfDamage
	if !kills {
		taken += scorer.StrikeOn(opt.Held, v.charms, v.ctx, v.strike, v.mob).Taken
	}
	value = float64(dealt) - v.rate*float64(taken)
	if kills {
		value += 1e9
	}
	return value, taken, kills
}

// best is the most valuable play from a hand.
func (v valuer) best(hand []scorer.Card) (opt scorer.Option, value float64, taken int, kills bool, ok bool) {
	value = -1e18
	for _, o := range scorer.Plays(hand, v.charms, v.ctx, v.mob) {
		val, t, k := v.price(o)
		if val > value {
			opt, value, taken, kills, ok = o, val, t, k, true
		}
	}
	return opt, value, taken, kills, ok
}

// fight chooses the action of a round. Of every play from the hand it takes the one that is worth most once the creature's
// telegraphed strike lands, counting the Guard cards it keeps; a play that kills ends the fight before the strike. It
// redraws instead when the cards it expects to draw are worth more than the round it gives the creature, and flees when
// it cannot win this round and the strike would kill it or leave it under the flee threshold.
func (j *Jev) fight(s *api.State, mem *Memory) (Choice, bool) {
	cb := s.Combat
	if mem.Fight.ID != cb.FightID {
		mem.Fight = FightMemory{ID: cb.FightID}
	}
	hand, charms, mobRules, ctx := FightRules(s, mem)
	canPlay, canFlee := s.Legal("PLAY"), s.Legal("FLEE")
	if !canPlay {
		return Choice{}, false
	}
	if len(mobRules) > 0 {
		if b, ok := scorer.BestPlay(hand, charms, ctx, mobRules); ok {
			ctx.BestForm = b.Score.Form
		}
	}
	v := valuer{charms: charms, mob: mobRules, ctx: ctx, rate: 1, mobHP: cb.Mob.HP}
	if cb.Mob.Intent != nil {
		v.strike = cb.Mob.Intent.Damage
	}
	if s.Agent.MaxHealth > 0 && cb.Mob.MaxHP > 0 {
		v.rate = float64(cb.Mob.MaxHP) / float64(s.Agent.MaxHealth)
	}
	best, bestValue, bestTaken, bestKills, ok := v.best(hand)
	if !ok {
		if canFlee {
			return Choice{Action: "FLEE", Args: map[string]any{}, Why: "no card to play"}, true
		}
		return Choice{}, false
	}
	if canFlee && !bestKills && (bestTaken >= s.Agent.Health || ctx.HPPercent < j.Config.FleeBelowPercent) && j.Config.FleeBelowPercent > 0 {
		return Choice{Action: "FLEE", Args: map[string]any{}, Why: fmt.Sprintf("the best play leaves %d HP and the strike takes %d", s.Agent.Health, bestTaken)}, true
	}
	if !bestKills && !j.Config.NoRedraw && s.Legal("REDRAW") {
		if ids, why, redraw := j.redraw(s, hand, v, best, bestValue); redraw {
			return Choice{Action: "REDRAW", Args: map[string]any{"cards": ids}, Why: why}, true
		}
	}
	reason := fmt.Sprintf("%s for %d against %d HP", best.Score.Form, best.Score.Damage, cb.Mob.HP)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	return Choice{
		Action: "PLAY", Args: map[string]any{"cards": best.IDs, "reason": reason}, Why: reason,
		Form: best.Score.Form, Damage: best.Score.Damage,
	}, true
}

// Redraw tuning: how many hands are drawn to judge a discard, and the least gain, as a share of the creature's health,
// that a redraw must promise before the creature is given a free round for it.
const (
	redrawSamples    = 14
	redrawMinGain    = 0.03
	redrawMaxDiscard = 5
)

// remainingPool is the cards left to draw: the whole deck less the cards in hand, discarded or in storage, matched by
// element, rank and Guard. It is empty when the state does not carry the deck.
func remainingPool(s *api.State, inHand []scorer.Card) []scorer.Card {
	if s.Loadout == nil || len(s.Loadout.Deck) == 0 {
		return nil
	}
	type sig struct {
		element string
		rank    int
		guard   bool
	}
	gone := map[sig]int{}
	for _, c := range s.Combat.You.Hand {
		gone[sig{c.Element, c.Rank, c.Guard}]++
	}
	for _, c := range s.Combat.You.Discard {
		gone[sig{c.Element, c.Rank, c.Guard}]++
	}
	for _, st := range s.Combat.You.Stored {
		gone[sig{st.Card.Element, st.Card.Rank, st.Card.Guard}]++
	}
	var pool []scorer.Card
	for _, d := range s.Loadout.Deck {
		k := sig{d.Element, d.Rank, d.Guard}
		if gone[k] > 0 {
			gone[k]--
			continue
		}
		pool = append(pool, scorer.Card{Element: d.Element, Rank: d.Rank, Guard: d.Guard})
	}
	return pool
}

// redraw decides whether to discard some cards and draw others instead of playing now. It judges each of a few discards
// by drawing hands from the cards left in the deck and pricing the best play of each, less the strike the creature lands
// in the round the redraw takes. It never draws more than is left, never redraws a Guard card away, and does so only when the
// agent can stand the strike.
func (j *Jev) redraw(s *api.State, hand []scorer.Card, v valuer, now scorer.Option, nowValue float64) ([]string, string, bool) {
	cb := s.Combat
	if cb.You.RedrawsLeft <= 0 || cb.You.DrawPile <= 0 || v.ctx.HPPercent < j.Config.FleeBelowPercent {
		return nil, "", false
	}
	pool := remainingPool(s, hand)
	if len(pool) == 0 {
		return nil, "", false
	}
	keep := map[string]bool{}
	for _, id := range now.IDs {
		keep[id] = true
	}
	var spare []scorer.Card
	for _, c := range hand {
		if !keep[c.ID] && !c.Guard {
			spare = append(spare, c)
		}
	}
	sort.SliceStable(spare, func(a, b int) bool { return spare[a].Rank < spare[b].Rank })
	var candidates [][]scorer.Card
	seen := map[int]bool{}
	for _, n := range []int{len(spare), 3, 1} {
		n = min(n, redrawMaxDiscard, len(spare), len(pool), cb.You.DrawPile)
		if n > 0 && !seen[n] {
			seen[n] = true
			candidates = append(candidates, spare[:n])
		}
	}
	if len(candidates) == 0 {
		return nil, "", false
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(cb.FightID))
	rng := rand.New(rand.NewPCG(h.Sum64(), uint64(cb.Round)+1))

	bestGain, bestIdx := 0.0, -1
	for ci, discard := range candidates {
		drop := map[string]bool{}
		for _, c := range discard {
			drop[c.ID] = true
		}
		var kept []scorer.Card
		for _, c := range hand {
			if !drop[c.ID] {
				kept = append(kept, c)
			}
		}
		var total, meanTaken float64
		for i := 0; i < redrawSamples; i++ {
			next := append([]scorer.Card(nil), kept...)
			for _, k := range rng.Perm(len(pool))[:len(discard)] {
				drawn := pool[k]
				drawn.ID = fmt.Sprintf("draw-%d", len(next))
				next = append(next, drawn)
			}
			_, value, _, _, ok := v.best(next)
			if !ok {
				continue
			}
			// The creature strikes at the end of the round the redraw takes, with the whole new hand held.
			strike := scorer.StrikeOn(next, v.charms, v.ctx, v.strike, v.mob).Taken
			total += value - v.rate*float64(strike)
			meanTaken += float64(strike) / redrawSamples
		}
		if meanTaken >= float64(s.Agent.Health) {
			continue
		}
		gain := total/redrawSamples - nowValue
		if gain > bestGain {
			bestGain, bestIdx = gain, ci
		}
	}
	if bestIdx < 0 || bestGain < redrawMinGain*float64(max(cb.Mob.MaxHP, 1)) {
		return nil, "", false
	}
	ids := make([]string, len(candidates[bestIdx]))
	for i, c := range candidates[bestIdx] {
		ids[i] = c.ID
	}
	return ids, fmt.Sprintf("redraw %d cards for a better hand than %s for %d", len(ids), now.Score.Form, now.Score.Damage), true
}
