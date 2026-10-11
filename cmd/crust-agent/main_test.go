// Tests of the crust-agent command's flags and its report of whether the planner is on, run by `go test` in the reference agent.

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func TestUsageAndUnknownCommands(t *testing.T) {
	var out, errw bytes.Buffer
	if code := run(context.Background(), nil, noEnv, &out, &errw); code != 2 {
		t.Fatalf("no command: %d", code)
	}
	if code := run(context.Background(), []string{"launch-missiles"}, noEnv, &out, &errw); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
}

func TestAFleetNeedsAReadableRoster(t *testing.T) {
	var out, errw bytes.Buffer
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")
	if code := run(context.Background(), []string{"fleet", "-roster", path, "-url", "http://crustland.invalid"}, noEnv, &out, &errw); code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errw.String(), "no such file") {
		t.Fatalf("stderr: %s", errw.String())
	}
}

func TestPlayNeedsCredentials(t *testing.T) {
	var out, errw bytes.Buffer
	if code := run(context.Background(), []string{"play"}, noEnv, &out, &errw); code != 1 || !strings.Contains(errw.String(), "CRUSTLAND_API_KEY") {
		t.Fatalf("code %d: %s", code, errw.String())
	}
}

func TestThePlannerIsReportedOffWithoutItsSettingsAndTheKeyIsNeverPrinted(t *testing.T) {
	var out, errw bytes.Buffer
	env := map[string]string{"CRUSTLAND_PLANNER_API_KEY": "sk-test-key-0000"} // the other three are missing
	getenv := func(k string) string { return env[k] }
	run(context.Background(), []string{"play"}, getenv, &out, &errw)
	if !strings.Contains(out.String(), "planner: off") || !strings.Contains(out.String(), "CRUSTLAND_PLANNER_MODEL") {
		t.Fatalf("stdout: %s", out.String())
	}
	if strings.Contains(out.String()+errw.String(), "sk-test-key-0000") {
		t.Fatal("the key was printed")
	}
}

func TestTheDefaultRosterLivesInTheConfigDirectoryNotInTheWorkingCopy(t *testing.T) {
	got, err := defaultRosterPath(func(k string) string {
		return map[string]string{"XDG_CONFIG_HOME": "/cfg", "HOME": "/h"}[k]
	})
	if err != nil || got != "/cfg/crust-agent/roster.json" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = defaultRosterPath(func(k string) string { return map[string]string{"HOME": "/h"}[k] })
	if err != nil || got != "/h/.config/crust-agent/roster.json" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := defaultRosterPath(func(string) string { return "" }); err == nil {
		t.Fatal("with nowhere to keep it, the command must ask for -roster")
	}
}
