// The roster file that holds a fleet's credentials, the registration that fills it, and ordinary-looking names, used by the crust-agent command.

package runner

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"strings"

	"github.com/crustland/reference-agent/api"
)

// RosterEntry is one agent of a roster: the credentials the game gave it and the class it chose.
type RosterEntry struct {
	Name    string `json:"name"`
	AgentID string `json:"agent_id"`
	APIKey  string `json:"api_key"`
	Class   string `json:"class,omitempty"`
}

// Credentials returns the entry's credentials.
func (e RosterEntry) Credentials() api.Credentials {
	return api.Credentials{AgentID: e.AgentID, Name: e.Name, APIKey: e.APIKey}
}

// Roster is a file of agents and their keys. The keys are the characters: the file is written readable by its owner only,
// and is refused if it is readable by anyone else.
type Roster struct {
	// Seed fixes the names the roster's agents get. It is drawn at random the first time agents are registered and kept in the
	// roster, so a run that resumes on another day goes on with the same names; nobody who reads the code can rebuild the
	// names without it.
	Seed   uint64        `json:"seed,omitempty"`
	Agents []RosterEntry `json:"agents"`
}

// ErrRosterPermissions is returned for a roster file that other users can read.
var ErrRosterPermissions = errors.New("the roster file holds API keys and must be readable by its owner only (chmod 600)")

// LoadRoster reads a roster file.
func LoadRoster(path string) (*Roster, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: %w", path, ErrRosterPermissions)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Roster
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s is not a roster: %w", path, err)
	}
	return &r, nil
}

// Save writes the roster, replacing the file, readable by its owner only.
func (r *Roster) Save(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IDs lists the agent ids of the roster, which hold nothing secret.
func (r *Roster) IDs() []string {
	ids := make([]string, len(r.Agents))
	for i, e := range r.Agents {
		ids[i] = e.AgentID
	}
	return ids
}

// Classes the registration offers.
var classes = []string{"warrior", "mage", "rogue"}

var (
	firstNames = []string{"Alder", "Briar", "Corin", "Dunmore", "Elsa", "Fenwick", "Gale", "Hesper", "Isolde", "Jorin", "Kestrel", "Lorne", "Maren", "Nettle", "Orrin",
		"Perrin", "Quill", "Rowan", "Sable", "Tamsin", "Ulric", "Vesper", "Wren", "Yarrow", "Zephyr", "Bram", "Calla", "Dorian", "Ember", "Finch"}
	lastNames = []string{"Ashdown", "Brook", "Carrow", "Dell", "Eastmere", "Fallow", "Greaves", "Hollis", "Ironwood", "Juniper", "Kerr", "Lyle", "Marsh", "Northcott", "Oakes",
		"Penhallow", "Quarry", "Reed", "Stroud", "Thorne", "Underhill", "Vance", "Wick", "Yates", "Ford"}
)

// Names returns n distinct, ordinary-looking character names, in an order fixed by the seed. An operator's agents carry no
// mark in their names.
func Names(seed uint64, n int) []string {
	rng := mrand.New(mrand.NewPCG(seed, 0xa5a5))
	seen := map[string]bool{}
	var out []string
	for len(out) < n {
		name := firstNames[rng.IntN(len(firstNames))] + " " + lastNames[rng.IntN(len(lastNames))]
		if len(seen) >= len(firstNames)*len(lastNames)-1 {
			name += fmt.Sprintf(" %d", len(out))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// randomSeed draws a seed that is not guessable.
func randomSeed() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("failed to draw a seed: %w", err)
	}
	if seed := binary.BigEndian.Uint64(b[:]); seed != 0 {
		return seed, nil
	}
	return 1, nil
}

// RegisterReport says how far registration got.
type RegisterReport struct {
	Registered int
	// Stopped is why it stopped before the target, empty if it did not.
	Stopped string
}

// Register registers agents through the public door until the roster holds target of them, saving the roster after each
// one. The game limits registrations per address per day; when it answers 429 the agents made so far are kept and
// Register stops, so the same command can be run again later and picks up where it left off.
func Register(ctx context.Context, c *api.Client, r *Roster, path string, target int, seed uint64, ref string) (RegisterReport, error) {
	var rep RegisterReport
	if r.Seed == 0 {
		r.Seed = seed // an explicit seed is for tests and for reproducing a roster; 0 draws one at random
		if r.Seed == 0 {
			var err error
			if r.Seed, err = randomSeed(); err != nil {
				return rep, err
			}
		}
	}
	have := map[string]bool{}
	for _, e := range r.Agents {
		have[e.Name] = true
	}
	names := Names(r.Seed, target+len(r.Agents)+10)
	for i := 0; len(r.Agents) < target; i++ {
		if i >= len(names) {
			return rep, errors.New("ran out of names")
		}
		name := names[i]
		if have[name] {
			continue
		}
		reg, err := c.Register(ctx, api.RegisterRequest{Name: name, Class: classes[len(r.Agents)%len(classes)], Ref: ref})
		if err != nil {
			var ae *api.Error
			switch {
			case errors.As(err, &ae) && ae.Status == http.StatusConflict:
				continue // the name is taken: take the next
			case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
				rep.Stopped = "the game limits registrations per address per day: " + strings.TrimSpace(ae.Message)
				return rep, nil
			}
			return rep, err
		}
		r.Agents = append(r.Agents, RosterEntry{Name: reg.Name, AgentID: reg.AgentID, APIKey: reg.APIKey, Class: reg.Class})
		have[name] = true
		rep.Registered++
		if err := r.Save(path); err != nil {
			return rep, fmt.Errorf("saving the roster (an agent was registered and its key is not saved): %w", err)
		}
	}
	return rep, nil
}
