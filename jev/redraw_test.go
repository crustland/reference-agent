// Tests of the executor's redraw: when it redraws, when it must not, and how it knows what is left to draw.

package jev

import (
	"reflect"
	"testing"

	"github.com/crustland/reference-agent/api"
	"github.com/crustland/reference-agent/scorer"
)

func deckOf(hand []scorer.Card, pool ...scorer.Card) *api.Loadout {
	l := &api.Loadout{}
	for _, c := range append(append([]scorer.Card{}, hand...), pool...) {
		l.Deck = append(l.Deck, api.DeckCard{Key: "k", Element: c.Element, Rank: c.Rank, Guard: c.Guard})
	}
	return l
}

func richPool(n int) []scorer.Card {
	var pool []scorer.Card
	for i := 0; i < n; i++ {
		pool = append(pool, scorer.Card{Element: "root", Rank: 10})
	}
	return pool
}

func weakHand() []scorer.Card {
	// No pair, no run of five, no Bond: the best play is a lone high card.
	return []scorer.Card{
		{ID: "a", Element: "root", Rank: 1}, {ID: "b", Element: "ash", Rank: 3}, {ID: "c", Element: "mire", Rank: 5}, {ID: "d", Element: "rime", Rank: 7},
		{ID: "e", Element: "ember", Rank: 9}, {ID: "f", Element: "root", Rank: 2}, {ID: "g", Element: "ash", Rank: 8, Guard: true}, {ID: "h", Element: "mire", Rank: 10},
	}
}

func TestAWeakHandWithAStrongDeckBehindItIsRedrawn(t *testing.T) {
	hand := weakHand()
	s := fightState(hand, 600, 4)
	s.Combat.You.DrawPile = 14
	s.Loadout = deckOf(hand, richPool(14)...)
	c, err := New().Choose(s, Plan{}, NewMemory(), rng(20))
	if err != nil {
		t.Fatal(err)
	}
	if c.Action != "REDRAW" {
		t.Fatalf("got %+v", c)
	}
	ids, _ := c.Args["cards"].([]string)
	if len(ids) == 0 || len(ids) > 5 {
		t.Fatalf("redrew %v", ids)
	}
	for _, id := range ids {
		if id == "g" {
			t.Fatal("redrew away the Guard card")
		}
	}
}

func TestItDoesNotRedrawWithoutTheDeckOrAnythingToDraw(t *testing.T) {
	hand := weakHand()
	s := fightState(hand, 600, 4)
	s.Combat.You.DrawPile = 14
	if c, _ := New().Choose(s, Plan{}, NewMemory(), rng(21)); c.Action == "REDRAW" {
		t.Fatal("redrew without knowing what is left to draw")
	}
	s.Loadout = deckOf(hand, richPool(14)...)
	s.Combat.You.DrawPile = 0
	if c, _ := New().Choose(s, Plan{}, NewMemory(), rng(21)); c.Action == "REDRAW" {
		t.Fatal("redrew with an empty draw pile")
	}
	s.Combat.You.DrawPile = 14
	s.Combat.You.RedrawsLeft = 0
	if c, _ := New().Choose(s, Plan{}, NewMemory(), rng(21)); c.Action == "REDRAW" {
		t.Fatal("redrew with none left")
	}
}

func TestItDoesNotRedrawWhenThePlayKills(t *testing.T) {
	hand := []scorer.Card{{ID: "a", Element: "root", Rank: 9}, {ID: "b", Element: "ash", Rank: 9}, {ID: "c", Element: "mire", Rank: 2}}
	s := fightState(hand, 40, 4)
	s.Combat.You.DrawPile = 10
	s.Loadout = deckOf(hand, richPool(10)...)
	if c, _ := New().Choose(s, Plan{}, NewMemory(), rng(22)); c.Action != "PLAY" {
		t.Fatalf("got %+v", c)
	}
}

func TestItDoesNotRedrawIntoAStrikeItCannotTake(t *testing.T) {
	hand := weakHand()
	s := fightState(hand, 600, 400)
	s.Agent.Health = 60
	s.Combat.You.DrawPile = 14
	s.Loadout = deckOf(hand, richPool(14)...)
	if c, _ := New().Choose(s, Plan{}, NewMemory(), rng(23)); c.Action == "REDRAW" {
		t.Fatalf("redrew into a strike it could not take: %+v", c)
	}
}

func TestTheRedrawIsTheSameEveryTimeForTheSameFight(t *testing.T) {
	hand := weakHand()
	s := fightState(hand, 600, 4)
	s.Combat.You.DrawPile = 14
	s.Loadout = deckOf(hand, richPool(14)...)
	first, _ := New().Choose(s, Plan{}, NewMemory(), rng(24))
	again, _ := New().Choose(s, Plan{}, NewMemory(), rng(25))
	if !reflect.DeepEqual(first.Args, again.Args) {
		t.Fatalf("%v then %v", first.Args, again.Args)
	}
}

func TestThePoolIsTheDeckLessWhatIsInHandDiscardedOrStored(t *testing.T) {
	hand := []scorer.Card{{ID: "a", Element: "root", Rank: 3}}
	s := fightState(hand, 100, 1)
	s.Combat.You.Discard = []scorer.Card{{ID: "x", Element: "ash", Rank: 4}}
	s.Combat.You.Stored = []api.Stored{{Card: scorer.Card{ID: "y", Element: "mire", Rank: 5}, ReturnRound: 3}}
	s.Loadout = &api.Loadout{Deck: []api.DeckCard{
		{Element: "root", Rank: 3}, {Element: "ash", Rank: 4}, {Element: "mire", Rank: 5}, {Element: "rime", Rank: 6}, {Element: "rime", Rank: 6},
	}}
	pool := remainingPool(s, hand)
	if len(pool) != 2 || pool[0].Rank != 6 || pool[1].Rank != 6 {
		t.Fatalf("pool %+v", pool)
	}
}
