// Tests of the API client against a local fake of the game, run by the reference agent's `go test`.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL + "/")
	c.HTTP = srv.Client()
	return c
}

func TestRegisterSendsTheNameAndClassAndReadsTheKeyOnce(t *testing.T) {
	var got RegisterRequest
	var auth string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/register" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"agent_id":"a1","api_key":"cl_abc","name":"Wren Ford","class":"mage","message":"welcome","next_step":"look around"}`)
	})
	reg, err := c.Register(context.Background(), RegisterRequest{Name: "Wren Ford", Class: "mage"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Wren Ford" || got.Class != "mage" || auth != "" {
		t.Fatalf("request %+v auth %q", got, auth)
	}
	creds := reg.Credentials()
	if creds.APIKey != "cl_abc" || creds.AgentID != "a1" || strings.Contains(creds.String(), "cl_abc") {
		t.Fatalf("credentials %+v", creds)
	}
}

func TestStateAsksForTheFullBagAndDecodesWhatThePlayerReads(t *testing.T) {
	var query, auth string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		query, auth = r.URL.RawQuery, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{
		  "agent": {"id":"a1","name":"Wren","level":7,"health":80,"max_health":100,"stamina":50,"max_stamina":100,"gold":12,"faction":"fighters"},
		  "legal_actions": [{"action":"MOVE","stamina":5,"example_args":{"chunk":[1,2]}},{"action":"REST"}],
		  "chunk": {"x":3,"y":4,"terrain":"forest","region":"verdant_reach","corruption":10,"resources":[{"id":"r1","type":"wood","amount":3}],"mobs":[{"id":"m1","type":"wolf","role":"gleaner","health":50}],"feature_type":"town"},
		  "combat": {"fight_id":"f1","round":1,"mob":{"type":"wolf","hp":40,"max_hp":50,"weakness":"ember","intent":{"name":"Bite","damage":9},"rules":[{"id":"t","rules":[{"trigger":"card_scored","effect":"add_rank","amount":1}]}]},
		             "you":{"hand":[{"id":"c1","element":"root","rank":4}],"plays_left":3,"redraws_left":2,"charms":[{"id":"ch","name":"Ring","rules":[{"trigger":"form_scored","effect":"add_power","amount":5}]}]}},
		  "clues":[{"fragment_id":"f9","mystery_id":"A1","text":"The first offering is a stone."}],
		  "something_new_the_game_added": {"x": 1},
		  "tick": 10, "next_tick_in": 7, "can_reflect": true, "event_type": "level_up"}`)
	})
	s, err := c.State(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if query != "include=all" || auth != "Bearer key" {
		t.Fatalf("query %q auth %q", query, auth)
	}
	if !s.Legal("MOVE") || s.Legal("ENGAGE") || s.Agent.Level != 7 || *s.Agent.Faction != "fighters" {
		t.Fatalf("state %+v", s)
	}
	if *s.Chunk.FeatureType != "town" || s.Chunk.Mobs[0].Role != "gleaner" || s.Combat.You.Hand[0].Rank != 4 || len(s.Combat.You.Charms[0].Rules) != 1 ||
		s.Combat.Mob.Intent.Damage != 9 || len(s.Combat.Mob.Rules[0].Rules) != 1 || s.Clues[0].MysteryID != "A1" || !s.CanReflect || s.NextTickIn != 7 {
		t.Fatalf("state %+v", s)
	}
}

func TestActSendsEmptyArgsForActionsWithNone(t *testing.T) {
	var raw map[string]json.RawMessage
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = io.WriteString(w, `{"success":true,"action":"REST","message":"rested","data":{"posts":[]}}`)
	})
	resp, err := c.Act(context.Background(), "key", Request{Action: "REST"})
	if err != nil || !resp.Success || string(resp.Data) != `{"posts":[]}` {
		t.Fatalf("%+v %v", resp, err)
	}
	if string(raw["args"]) != "{}" {
		t.Fatalf("args: %s (the game wants an object, even an empty one)", raw["args"])
	}
	if _, has := raw["reflection"]; has {
		t.Fatal("an empty reflection must be left out")
	}
}

func TestErrorsCarryTheStatusCodeAndRetryAfter(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/state":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"bad key","code":"UNAUTHORIZED"}`)
		default:
			w.Header().Set("Retry-After", "12")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"slow down","code":"RATE_LIMITED"}`)
		}
	})
	_, err := c.State(context.Background(), "key")
	if !IsUnauthorized(err) {
		t.Fatalf("got %v", err)
	}
	_, err = c.Act(context.Background(), "key", Request{Action: "REST"})
	d, limited := IsRateLimited(err)
	if !limited || d != 12*time.Second {
		t.Fatalf("got %v %v", d, limited)
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != "RATE_LIMITED" || e.Message != "slow down" {
		t.Fatalf("got %+v", e)
	}
}

func TestAnAnswerThatIsNotJSONIsAnError(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "<html>oops</html>") })
	if _, err := c.State(context.Background(), "k"); err == nil {
		t.Fatal("want an error")
	}
}

func TestTheClientNeverTouchesAnAdminOrInternalRoute(t *testing.T) {
	var paths []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `{"agent_id":"a","api_key":"k","name":"n","agent":{},"legal_actions":[],"success":true}`)
	})
	ctx := context.Background()
	_, _ = c.Register(ctx, RegisterRequest{Name: "n"})
	_, _ = c.State(ctx, "k")
	_, _ = c.Act(ctx, "k", Request{Action: "REST"})
	for _, p := range paths {
		if !strings.HasPrefix(p, "/api/agent/") {
			t.Errorf("reached %s", p)
		}
	}
	if len(paths) != 3 {
		t.Fatalf("paths: %v", paths)
	}
}
