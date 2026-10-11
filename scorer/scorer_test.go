// Runs every vector of the game engine's score file through the scorer, so the port is held to the engine.

package scorer

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The vectors are plays, strikes and best plays with the numbers the game server's engine gives them. They are the
// contract this port must meet; the game's own tests check its engine against the same file.

type playVector struct {
	Name       string    `json:"name"`
	Cards      []Card    `json:"cards"`
	Held       []Card    `json:"held"`
	Charms     []RuleSet `json:"charms"`
	Mob        []RuleSet `json:"mob"`
	Ctx        Context   `json:"ctx"`
	Form       string    `json:"form"`
	Power      int       `json:"power"`
	Edge       int       `json:"edge"`
	Damage     int       `json:"damage"`
	SelfDamage int       `json:"self_damage"`
}

type bestVector struct {
	Name   string    `json:"name"`
	Hand   []Card    `json:"hand"`
	Charms []RuleSet `json:"charms"`
	Mob    []RuleSet `json:"mob"`
	Ctx    Context   `json:"ctx"`
	IDs    []string  `json:"ids"`
	Form   string    `json:"form"`
	Damage int       `json:"damage"`
}

type strikeVector struct {
	Name      string    `json:"name"`
	Held      []Card    `json:"held"`
	Charms    []RuleSet `json:"charms"`
	Mob       []RuleSet `json:"mob"`
	Ctx       Context   `json:"ctx"`
	Strike    int       `json:"strike"`
	Block     int       `json:"block"`
	Taken     int       `json:"taken"`
	DamageOut int       `json:"damage_after_reduction"`
}

type vectorFile struct {
	Plays   []playVector   `json:"plays"`
	Strikes []strikeVector `json:"strikes"`
	Bests   []bestVector   `json:"bests"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/score_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheVectorsAreThere(t *testing.T) {
	v := loadVectors(t)
	if len(v.Plays) < 25 || len(v.Strikes) < 5 || len(v.Bests) < 3 {
		t.Fatalf("the vector file is thin: %d plays, %d strikes, %d bests", len(v.Plays), len(v.Strikes), len(v.Bests))
	}
	withMob := 0
	for _, p := range v.Plays {
		if len(p.Mob) > 0 {
			withMob++
		}
	}
	if withMob < 10 {
		t.Fatalf("only %d vectors score against creature rules", withMob)
	}
}

func TestScorePlayMatchesTheEngineVectors(t *testing.T) {
	for _, v := range loadVectors(t).Plays {
		t.Run(v.Name, func(t *testing.T) {
			got := ScorePlay(v.Cards, v.Charms, v.Ctx, v.Held, v.Mob)
			want := Score{Form: v.Form, Power: v.Power, Edge: v.Edge, Damage: v.Damage}
			if len(v.Mob) > 0 {
				want.SelfDamage = v.SelfDamage
			}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestStrikeMatchesTheEngineVectors(t *testing.T) {
	for _, v := range loadVectors(t).Strikes {
		t.Run(v.Name, func(t *testing.T) {
			got := StrikeOn(v.Held, v.Charms, v.Ctx, v.Strike, v.Mob)
			want := Strike{Damage: v.DamageOut, Block: v.Block, Taken: v.Taken}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestBestPlayMatchesTheEngineVectors(t *testing.T) {
	for _, v := range loadVectors(t).Bests {
		t.Run(v.Name, func(t *testing.T) {
			got, ok := BestPlay(v.Hand, v.Charms, v.Ctx, v.Mob)
			if !ok {
				t.Fatal("no play found")
			}
			if !reflect.DeepEqual(got.IDs, v.IDs) || got.Score.Form != v.Form || got.Score.Damage != v.Damage {
				t.Fatalf("got %v %s %d, want %v %s %d", got.IDs, got.Score.Form, got.Score.Damage, v.IDs, v.Form, v.Damage)
			}
		})
	}
}

func TestAnEmptyHandHasNoPlay(t *testing.T) {
	if _, ok := BestPlay(nil, nil, Context{}, nil); ok {
		t.Fatal("an empty hand cannot play")
	}
}

func TestAHandOfEightHasTwoHundredAndEighteenPlays(t *testing.T) {
	hand := make([]Card, 8)
	for i := range hand {
		hand[i] = Card{ID: string(rune('a' + i)), Element: "root", Rank: i + 1}
	}
	if n := len(Plays(hand, nil, Context{PlayNumber: 1, HPPercent: 100}, nil)); n != 218 {
		t.Fatalf("got %d plays, want 218", n)
	}
}
