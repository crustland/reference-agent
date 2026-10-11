// Package scorer scores a CrustLand play and a strike exactly as the game server does, so an agent knows what a play is
// worth before it sends PLAY (there is no preview endpoint). It is a Go port of the reference scorer the game publishes
// (crust-scorer.js, printed in skill.md); testdata/score_vectors.json holds the vectors the server's own engine is held
// to, and scorer_test.go runs every one of them.
//
// A play is the cards you would play, the charms you carry (state.combat.you.charms), the context of the fight and,
// against a creature with rules, those rules (state.combat.mob.rules). Everything is integers and nothing here is random.
package scorer

import (
	"sort"
)

// Limits of the rules.
const (
	// MaxCardsPerPlay is the most cards one PLAY may spend.
	MaxCardsPerPlay = 5
	edgeMulCap      = 4
	rankMulCap      = 4
	blockMulCap     = 4
)

// Card is a card in your hand, as state.combat.you.hand gives it.
type Card struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Element string `json:"element"`
	Rank    int    `json:"rank"`
	Guard   bool   `json:"guard,omitempty"`
}

// Condition is one condition of a rule (state.combat.you.charms[].rules[].when[]).
type Condition struct {
	Kind    string `json:"kind"`
	Element string `json:"element,omitempty"`
	Value   int    `json:"value,omitempty"`
	Text    string `json:"text,omitempty"`
}

// Rule is one rule of a charm or a creature.
type Rule struct {
	Trigger string      `json:"trigger"`
	When    []Condition `json:"when,omitempty"`
	Effect  string      `json:"effect"`
	Amount  int         `json:"amount,omitempty"`
	Per     string      `json:"per,omitempty"`
	Element string      `json:"element,omitempty"`
}

// RuleSet is a charm, a creature's trait, or a Keeper's rule of the week: a named list of rules.
type RuleSet struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Text  string `json:"text,omitempty"`
	Rules []Rule `json:"rules"`
}

// Context is what a play is scored against. Terrain, RedrawsLeft, PlayNumber, HPPercent and Weakness are always needed;
// the rest only against a creature that has rules.
type Context struct {
	Terrain string `json:"terrain,omitempty"`
	// RedrawsLeft is state.combat.you.redraws_left.
	RedrawsLeft int `json:"redraws_left"`
	// PlayNumber is 1 + the plays already made this fight.
	PlayNumber int `json:"play_number"`
	// HPPercent is your health as a percent of your maximum.
	HPPercent int `json:"hp_percent"`
	// Weakness is state.combat.mob.weakness.
	Weakness string `json:"weakness,omitempty"`

	// Round is the round you are playing: state.combat.round + 1.
	Round int `json:"round,omitempty"`
	// FormsPlayed are the forms your earlier plays against this creature scored, oldest first.
	FormsPlayed []string `json:"forms_played,omitempty"`
	// LastDamage is the damage your previous play dealt, 0 before the first.
	LastDamage int `json:"last_damage,omitempty"`
	// BestForm is the form of BestPlay, for rules that name your best play.
	BestForm string `json:"best_form,omitempty"`
}

// Score is what a play is worth. SelfDamage is the HP a creature's rule bills you for the play.
type Score struct {
	Form       string
	Power      int
	Edge       int
	Damage     int
	SelfDamage int
}

// Strike is the creature's telegraphed move landing on your hand.
type Strike struct {
	// Damage is the strike after your charms reduce it, Block what the Guard cards still in hand stop, Taken what is left.
	Damage, Block, Taken int
}

type form struct {
	id          string
	power, edge int
}

var forms = []form{
	{"high_card", 5, 1}, {"pair", 10, 2}, {"two_pair", 20, 2}, {"triple", 30, 3}, {"run", 30, 4},
	{"bond", 35, 4}, {"full_kit", 40, 4}, {"quad", 60, 7}, {"true_run", 100, 8},
}

func formSpec(id string) (form, bool) {
	for _, f := range forms {
		if f.id == id {
			return f, true
		}
	}
	return form{}, false
}

type detected struct {
	id          string
	power, edge int
	scoring     []int
}

// detectForm reads the form from printed ranks and elements, before any rank change. Ripe counts as any element in a
// Bond.
func detectForm(cards []Card) detected {
	var best *detected
	consider := func(id string, scoring []int) {
		f, _ := formSpec(id)
		if best == nil || f.power > best.power || (f.power == best.power && f.edge > best.edge) {
			best = &detected{id: id, power: f.power, edge: f.edge, scoring: scoring}
		}
	}
	high := 0
	for i, c := range cards {
		if c.Rank > cards[high].Rank {
			high = i
		}
	}
	consider("high_card", []int{high})

	byRank := map[int][]int{}
	for i, c := range cards {
		byRank[c.Rank] = append(byRank[c.Rank], i)
	}
	ranks := make([]int, 0, len(byRank))
	for r := range byRank {
		ranks = append(ranks, r)
	}
	sort.Ints(ranks)
	var groups [][]int
	for _, r := range ranks {
		groups = append(groups, byRank[r])
	}
	var pairs, triples, quads [][]int
	for _, g := range groups {
		if len(g) >= 2 {
			pairs = append(pairs, g[:2])
		}
		if len(g) >= 3 {
			triples = append(triples, g[:3])
		}
		if len(g) >= 4 {
			quads = append(quads, g[:4])
		}
	}
	if len(pairs) > 0 {
		consider("pair", pairs[len(pairs)-1])
	}
	if len(pairs) >= 2 {
		two := append(append([]int{}, pairs[len(pairs)-2]...), pairs[len(pairs)-1]...)
		sort.Ints(two)
		consider("two_pair", two)
	}
	if len(triples) > 0 {
		consider("triple", triples[len(triples)-1])
	}
	if len(quads) > 0 {
		consider("quad", quads[len(quads)-1])
	}
	if len(cards) == 5 {
		all := []int{0, 1, 2, 3, 4}
		sorted := make([]int, 0, 5)
		for _, c := range cards {
			sorted = append(sorted, c.Rank)
		}
		sort.Ints(sorted)
		isRun := true
		for i := 1; i < len(sorted); i++ {
			if sorted[i] != sorted[i-1]+1 {
				isRun = false
			}
		}
		suits := map[string]bool{}
		for _, c := range cards {
			if c.Element != "ripe" {
				suits[c.Element] = true
			}
		}
		isBond := len(suits) <= 1
		if isRun {
			consider("run", all)
		}
		if isBond {
			consider("bond", all)
		}
		if isRun && isBond {
			consider("true_run", all)
		}
		if len(groups) == 2 && len(groups[0]) >= 2 && len(groups[1]) >= 2 && (len(groups[0]) == 3 || len(groups[1]) == 3) {
			consider("full_kit", all)
		}
	}
	return *best
}

// env is what a condition is checked against.
type env struct {
	ctx       Context
	scoring   []Card
	form      string
	cardCount int
	heldGuard int
	group     int
	damage    int
	// card is set while a card is being scored or held.
	card    *Card
	element string
	rank    int
}

func (e *env) holds(conds []Condition) bool {
	for _, c := range conds {
		if !e.holdsOne(c) {
			return false
		}
	}
	return true
}

func (e *env) holdsOne(c Condition) bool {
	switch c.Kind {
	case "card_element":
		return e.card != nil && e.element == c.Element
	case "card_ripe":
		return e.card != nil && e.element == "ripe"
	case "card_rank_min":
		return e.card != nil && e.rank >= c.Value
	case "card_rank_max":
		return e.card != nil && e.rank <= c.Value
	case "card_guard":
		return e.card != nil && e.card.Guard
	case "form_is":
		return e.form == c.Text
	case "group_min":
		return e.group >= c.Value
	case "cards_min":
		return e.cardCount >= c.Value
	case "terrain":
		return e.ctx.Terrain == c.Text
	case "redraws_min":
		return e.ctx.RedrawsLeft >= c.Value
	case "first_play":
		return e.ctx.PlayNumber == 1
	case "play_number":
		return e.ctx.PlayNumber == c.Value
	case "hp_below_percent":
		return e.ctx.HPPercent < c.Value
	case "held_guard_min":
		return e.heldGuard >= c.Value
	case "play_element_min":
		n := 0
		for _, s := range e.scoring {
			if s.Element == c.Element {
				n++
			}
		}
		return n >= c.Value
	// Creature rules only.
	case "form_below":
		have, ok := formSpec(e.form)
		other, known := formSpec(c.Text)
		return !ok || (known && have.power < other.power)
	case "form_repeat":
		if e.form == "" {
			return false
		}
		for _, f := range e.ctx.FormsPlayed {
			if f == e.form {
				return true
			}
		}
		return false
	case "form_repeat_first":
		return e.form != "" && len(e.ctx.FormsPlayed) > 0 && e.ctx.FormsPlayed[0] == e.form
	case "form_is_best":
		return e.form != "" && e.form == e.ctx.BestForm
	case "not_beating_last":
		return e.ctx.PlayNumber > 1 && e.damage <= e.ctx.LastDamage
	case "cards_max":
		return e.cardCount >= 1 && e.cardCount <= c.Value
	case "round_min":
		return e.ctx.Round >= c.Value
	case "round_every":
		return e.ctx.Round > 0 && c.Value > 0 && e.ctx.Round%c.Value == 0
	}
	return false
}

func (e *env) count(per string) int {
	switch per {
	case "redraw_left":
		return e.ctx.RedrawsLeft
	case "scored_card":
		return len(e.scoring)
	case "held_guard":
		return e.heldGuard
	}
	return 1
}

// eachRule calls fn for every rule of a trigger whose conditions hold, in the fixed order: rule sets left to right, rules
// in the order written. Conditions are checked as the walk reaches each rule, so a rule sees what the rules before it
// changed in e (a set_element, a raised rank), exactly as the published scorer does.
func eachRule(sets []RuleSet, trigger string, e *env, fn func(r Rule, n int)) {
	for _, set := range sets {
		for _, r := range set.Rules {
			if r.Trigger != trigger || !e.holds(r.When) {
				continue
			}
			n := r.Amount
			if r.Per != "" {
				n *= e.count(r.Per)
			}
			fn(r, n)
		}
	}
}

func guards(cards []Card) int {
	n := 0
	for _, c := range cards {
		if c.Guard {
			n++
		}
	}
	return n
}

// ScorePlay scores the cards you would play. held are the cards that stay in your hand; mob is the creature's rule sets
// (nil for a creature without rules).
//
// Damage = (form Power + each scoring card's rank + added Power) x (form Edge + added Edge) x (x-Edge effects, capped at
// x4). Only the cards that make the form score. A scoring card's rank starts as its printed rank, doubled if it is the
// creature's weak element (a Ripe card never is), then each charm's rank rules apply in order. Then the creature's rules.
func ScorePlay(cards []Card, charms []RuleSet, ctx Context, held []Card, mob []RuleSet) Score {
	f := detectForm(cards)
	scoring := make([]Card, len(f.scoring))
	for i, idx := range f.scoring {
		scoring[i] = cards[idx]
	}
	byRank := map[int]int{}
	group := 0
	for _, c := range scoring {
		byRank[c.Rank]++
		group = max(group, byRank[c.Rank])
	}
	e := &env{ctx: ctx, scoring: scoring, form: f.id, cardCount: len(cards), heldGuard: guards(held), group: group}
	power := f.power
	edgeAdd := f.edge
	edgeMul := 1
	selfDamage := 0
	for i := range scoring {
		card := scoring[i]
		rankAdd := card.Rank
		if ctx.Weakness != "" && card.Element == ctx.Weakness && card.Element != "ripe" {
			rankAdd *= 2
		}
		rankMul := 1
		ce := *e
		ce.card, ce.element, ce.rank = &card, card.Element, rankAdd
		eachRule(charms, "card_scored", &ce, func(r Rule, n int) {
			switch r.Effect {
			case "add_rank":
				rankAdd += n
			case "mul_rank":
				rankMul = min(rankMul*r.Amount, rankMulCap)
			case "set_element":
				ce.element = r.Element
			case "add_power":
				power += n
			}
			ce.rank = rankAdd * rankMul
		})
		eachRule(mob, "card_scored", &ce, func(r Rule, n int) {
			switch r.Effect {
			case "add_rank":
				rankAdd += n
			case "self_damage":
				selfDamage += n
			}
			ce.rank = rankAdd * rankMul
		})
		power += ce.rank
	}
	eachRule(charms, "form_scored", e, func(r Rule, n int) {
		switch r.Effect {
		case "add_power":
			power += n
		case "add_edge":
			edgeAdd += n
		case "mul_edge":
			edgeMul = min(edgeMul*r.Amount, edgeMulCap)
		}
	})
	edge := edgeAdd * edgeMul
	reduce, block := 0, 0
	e.damage = power * edge
	eachRule(mob, "form_scored", e, func(r Rule, n int) {
		switch r.Effect {
		case "cap_edge":
			edge = min(edge, r.Amount)
		case "reduce_damage":
			reduce += n
		case "mob_block":
			block += n
		}
	})
	damage := max(power*edge*(100-min(reduce, 100))/100-block, 0)
	return Score{Form: f.id, Power: power, Edge: edge, Damage: damage, SelfDamage: selfDamage}
}

// Option is one play from a hand, with what it scores and what it leaves in hand.
type Option struct {
	// IDs are the card ids to play, in hand order.
	IDs   []string
	Score Score
	// Held are the cards the play leaves in hand.
	Held []Card
}

// Plays lists every play of 1 to MaxCardsPerPlay cards from the hand (at most 218 from a hand of 8) with its score.
func Plays(hand []Card, charms []RuleSet, ctx Context, mob []RuleSet) []Option {
	var out []Option
	chosen := make([]int, 0, MaxCardsPerPlay)
	var walk func(start int)
	walk = func(start int) {
		if len(chosen) > 0 {
			cards := make([]Card, len(chosen))
			ids := make([]string, len(chosen))
			in := make(map[int]bool, len(chosen))
			for i, idx := range chosen {
				cards[i], ids[i] = hand[idx], hand[idx].ID
				in[idx] = true
			}
			held := make([]Card, 0, len(hand)-len(chosen))
			for i, c := range hand {
				if !in[i] {
					held = append(held, c)
				}
			}
			out = append(out, Option{IDs: ids, Score: ScorePlay(cards, charms, ctx, held, mob), Held: held})
		}
		if len(chosen) == MaxCardsPerPlay {
			return
		}
		for i := start; i < len(hand); i++ {
			chosen = append(chosen, i)
			walk(i + 1)
			chosen = chosen[:len(chosen)-1]
		}
	}
	walk(0)
	return out
}

// BestPlay is the play of at most five cards that does the most damage right now. Ties go to the play with fewer cards,
// then to the one found first walking the hand in order. It ignores what a play would spend or save for later. It
// returns false for an empty hand.
func BestPlay(hand []Card, charms []RuleSet, ctx Context, mob []RuleSet) (Option, bool) {
	var best Option
	found := false
	for _, opt := range Plays(hand, charms, ctx, mob) {
		if !found || opt.Score.Damage > best.Score.Damage || (opt.Score.Damage == best.Score.Damage && len(opt.IDs) < len(best.IDs)) {
			best, found = opt, true
		}
	}
	return best, found
}

// StrikeOn is the creature's telegraphed move landing: strike reduction first, then every Guard card still in hand blocks
// its rank (Guard cards are not spent by blocking), then block rules; what is left hits you. A creature that ignores low
// Guard leaves those cards out of the block altogether.
func StrikeOn(held []Card, charms []RuleSet, ctx Context, damage int, mob []RuleSet) Strike {
	e := &env{ctx: ctx, heldGuard: guards(held)}
	reduce := 0
	eachRule(charms, "strike", e, func(_ Rule, n int) { reduce += n })
	hit := damage * (100 - min(reduce, 100)) / 100
	ignored := 0
	eachRule(mob, "strike", e, func(r Rule, _ int) {
		if r.Effect == "ignore_guard" {
			ignored = max(ignored, r.Amount)
		}
	})
	var held2 []Card
	for _, c := range held {
		if c.Guard && c.Rank > ignored {
			held2 = append(held2, c)
		}
	}
	block := 0
	for i := range held2 {
		g := held2[i]
		block += g.Rank
		ge := *e
		ge.card, ge.element, ge.rank = &g, g.Element, g.Rank
		eachRule(charms, "card_held", &ge, func(_ Rule, n int) { block += n })
	}
	if len(held2) > 0 {
		mul := 1
		eachRule(charms, "block", e, func(r Rule, n int) {
			switch r.Effect {
			case "add_block":
				block += n
			case "mul_block":
				mul = min(mul*r.Amount, blockMulCap)
			}
		})
		block *= mul
	}
	return Strike{Damage: hit, Block: block, Taken: max(hit-block, 0)}
}
