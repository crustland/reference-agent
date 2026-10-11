// Command crust-agent plays CrustLand: one agent, or a fleet of agents, through the game's public agent API, with a rule-based
// executor and an optional planner that is off unless every one of its settings is given.
//
//	crust-agent register -n 1                         make agents (the game limits registrations per address per day)
//	crust-agent play -roster roster.json              play the first agent of a roster (or set CRUSTLAND_API_KEY)
//	crust-agent fleet                                 play a roster of 20 to 50 agents under one rate gate
//
// The game's address is CRUSTLAND_URL or -url. Keys live in the roster file (owner-readable only; by default
// ~/.config/crust-agent/roster.json, outside any working copy) or in CRUSTLAND_API_KEY; they are never printed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/crustland/reference-agent/api"
	"github.com/crustland/reference-agent/planner"
	"github.com/crustland/reference-agent/runner"
)

const (
	defaultURL = "https://crustland.com"
	usage      = "usage: crust-agent register | play | fleet  (see the package comment, and README.md)"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, out, errw io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(errw, usage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(errw)
	url := fs.String("url", firstNonEmpty(getenv("CRUSTLAND_URL"), defaultURL), "the game's address")
	rosterPath := fs.String("roster", "", "the roster file")
	n := fs.Int("n", 1, "register: how many agents the roster should hold")
	seed := fs.Uint64("seed", 0, "a number that fixes the agents' choices (and, for register, the names); 0 draws one at random")
	ref := fs.String("ref", "", "register: where you found the game (optional)")
	index := fs.Int("index", 0, "play: which agent of the roster")
	reads := fs.Int("reads-per-minute", runner.DefaultLimits().ReadsPerMinute, "state reads a minute allowed from this address")
	acts := fs.Int("acts-per-minute", runner.DefaultLimits().ActsPerMinute, "actions a minute allowed from this address")
	verbose := fs.Bool("v", false, "log every turn")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *rosterPath == "" && (cmd == "register" || cmd == "fleet" || (cmd == "play" && getenv("CRUSTLAND_API_KEY") == "")) {
		// A roster holds keys: unless told otherwise it lives in the user's own config directory, never in a working copy.
		path, err := defaultRosterPath(getenv)
		if err == nil && cmd == "play" {
			if _, statErr := os.Stat(path); statErr != nil {
				path = "" // nothing there to play: the credentials error below says what to give
			}
		}
		if err != nil && cmd != "play" {
			_, _ = fmt.Fprintln(errw, "crust-agent:", err)
			return 2
		}
		*rosterPath = path
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(errw, &slog.HandlerOptions{Level: level}))

	client := api.New(*url)
	switch cmd {
	case "register":
		return register(ctx, client, *rosterPath, *n, *seed, *ref, out, errw)
	case "play", "fleet":
	default:
		_, _ = fmt.Fprintln(errw, usage)
		return 2
	}

	limits := runner.DefaultLimits()
	limits.ReadsPerMinute, limits.ActsPerMinute = *reads, *acts
	gate := runner.NewGate(limits, nil, nil)
	pl, cfg, err := planner.FromEnv(getenv, &http.Client{Timeout: 60 * time.Second}, nil)
	if err != nil {
		_, _ = fmt.Fprintln(errw, "crust-agent:", err)
		return 1
	}
	every := cfg.Every
	if off, isOff := pl.(planner.Off); isOff {
		_, _ = fmt.Fprintln(out, "planner: off ("+off.Reason+"); the executor plays on its own")
		every = planner.DefaultEvery
	} else {
		_, _ = fmt.Fprintln(out, "planner: on,", cfg)
		if llm, ok := pl.(*planner.LLM); ok {
			// The day's spend is kept next to the roster (or where CRUSTLAND_PLANNER_SPEND_FILE says), so a restart does not
			// hand out the budget again.
			spend := getenv(planner.EnvSpendFile)
			if spend == "" && *rosterPath != "" {
				spend = *rosterPath + ".spend"
			}
			if spend == "" {
				_, _ = fmt.Fprintln(errw, "crust-agent: the planner is on and there is no roster or "+planner.EnvSpendFile+" to keep its spend in; a restart would forget it")
				return 1
			}
			if err := llm.Budget.Persist(spend); err != nil {
				_, _ = fmt.Fprintln(errw, "crust-agent:", err)
				return 1
			}
		}
	}

	if cmd == "play" {
		creds, err := credentials(*rosterPath, *index, getenv)
		if err != nil {
			_, _ = fmt.Fprintln(errw, "crust-agent:", err)
			return 1
		}
		a := runner.New(creds, client, gate, *seed)
		a.Planner, a.Every, a.Log = pl, every, log
		return finish(a.Run(ctx), ctx, errw)
	}

	if *rosterPath == "" {
		_, _ = fmt.Fprintln(errw, "crust-agent: fleet needs -roster")
		return 2
	}
	roster, err := runner.LoadRoster(*rosterPath)
	if err != nil {
		_, _ = fmt.Fprintln(errw, "crust-agent:", err)
		return 1
	}
	fleet, err := runner.NewFleet(roster, client, gate, pl, every, *seed, log)
	if err != nil {
		_, _ = fmt.Fprintln(errw, "crust-agent:", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "playing %d agents from one address, at most %d state reads and %d actions a minute in all\n", len(fleet.Agents), limits.ReadsPerMinute, limits.ActsPerMinute)
	return finish(fleet.Run(ctx), ctx, errw)
}

// defaultRosterPath is where a roster is kept when -roster is not given: crust-agent/roster.json under the user's config
// directory (XDG_CONFIG_HOME, else ~/.config).
func defaultRosterPath(getenv func(string) string) (string, error) {
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		home := getenv("HOME")
		if home == "" {
			return "", errors.New("give -roster: there is no XDG_CONFIG_HOME or HOME to keep it under")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "crust-agent", "roster.json"), nil
}

func finish(err error, ctx context.Context, errw io.Writer) int {
	if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		_, _ = fmt.Fprintln(errw, "crust-agent:", err)
		return 1
	}
	return 0
}

func register(ctx context.Context, client *api.Client, path string, n int, seed uint64, ref string, out, errw io.Writer) int {
	if path == "" {
		_, _ = fmt.Fprintln(errw, "crust-agent: register needs -roster")
		return 2
	}
	roster := &runner.Roster{}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		_, _ = fmt.Fprintln(errw, "crust-agent:", err)
		return 1
	}
	if _, err := os.Stat(path); err == nil {
		loaded, err := runner.LoadRoster(path)
		if err != nil {
			_, _ = fmt.Fprintln(errw, "crust-agent:", err)
			return 1
		}
		roster = loaded
	}
	rep, err := runner.Register(ctx, client, roster, path, n, seed, ref)
	_, _ = fmt.Fprintf(out, "registered %d new agents; the roster holds %d of %d\n", rep.Registered, len(roster.Agents), n)
	if rep.Stopped != "" {
		_, _ = fmt.Fprintln(out, "stopped early:", rep.Stopped, "- run the same command again later to carry on")
	}
	if err != nil {
		_, _ = fmt.Fprintln(errw, "crust-agent:", err)
		return 1
	}
	return 0
}

// credentials finds the agent to play: the roster's entry, or CRUSTLAND_API_KEY and CRUSTLAND_AGENT_ID.
func credentials(rosterPath string, index int, getenv func(string) string) (api.Credentials, error) {
	if rosterPath != "" {
		r, err := runner.LoadRoster(rosterPath)
		if err != nil {
			return api.Credentials{}, err
		}
		if index < 0 || index >= len(r.Agents) {
			return api.Credentials{}, fmt.Errorf("the roster has %d agents, there is no number %d", len(r.Agents), index)
		}
		return r.Agents[index].Credentials(), nil
	}
	key := getenv("CRUSTLAND_API_KEY")
	if key == "" {
		return api.Credentials{}, errors.New("give -roster, or set CRUSTLAND_API_KEY (and CRUSTLAND_AGENT_ID)")
	}
	return api.Credentials{AgentID: getenv("CRUSTLAND_AGENT_ID"), Name: firstNonEmpty(getenv("CRUSTLAND_AGENT_NAME"), "agent"), APIKey: key}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
