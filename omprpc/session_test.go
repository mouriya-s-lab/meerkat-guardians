package omprpc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The session tests drive the fake omp the same way session.go runs real ones: the FAKE_OMP
// env decides the plan, --session-dir names where the fake writes its session file, and the
// tests assert the session-file facts RunSession's acceptance cares about.

func runFake(t *testing.T, plan string, maxContinue int, maxDuration time.Duration) (SessionResult, []string, string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sessDir := t.TempDir()
	var ready []string
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := RunSession(ctx, SessionSpec{
		Options:            Options{Bin: exe, Env: []string{"FAKE_OMP_PLAN=" + plan, "FAKE_OMP_GOAL=pm_return_facts"}, Args: []string{"--session-dir", sessDir}},
		Opening:            "open",
		Continue:           "keep going",
		GoalTools:          []string{"pm_base_ready", "pm_return_facts"},
		MaxContinuePrompts: maxContinue,
		ReadyTimeout:       10 * time.Second,
		ExitGrace:          5 * time.Second,
		MaxDuration:        maxDuration,
		OnReady:            func(id, file string) { ready = append(ready, id, file) },
	})
	return res, ready, sessDir, err
}

// sessionEntries decodes the fake's session file: type, customType and message role per line.
func sessionEntries(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(names) != 1 {
		t.Fatalf("want exactly one .jsonl under %s, got %v (%v)", dir, names, err)
	}
	b, err := os.ReadFile(names[0])
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Message    *struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			t.Fatalf("session file line is not JSON: %s", line)
		}
		if e.Type == "message" && e.Message != nil {
			out = append(out, "message:"+e.Message.Role)
		} else if e.Type == "custom" {
			out = append(out, "custom:"+e.CustomType)
		} else {
			out = append(out, e.Type)
		}
	}
	return out
}

func TestRunSessionGoalReached(t *testing.T) {
	res, ready, sessDir, err := runFake(t, "goal", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToolName != "pm_return_facts" || string(res.Args) != `{"facts":"open"}` {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(ready, " ") != res.SessionID+" "+res.SessionFile {
		t.Fatalf("OnReady = %v, result = %+v", ready, res)
	}
	if res.SessionID == "" || !strings.HasPrefix(res.SessionFile, sessDir+"/") {
		t.Fatalf("result = %+v, want a session file under %s", res, sessDir)
	}
	// The session file exists and carries the session header plus the goal call's omp
	// entries: the assistant toolCall message, both tool_execution custom events and the
	// toolResult message that closes the call.
	entries := sessionEntries(t, sessDir)
	want := "session message:assistant custom:tool_execution_start custom:tool_execution_end message:toolResult"
	if got := strings.Join(entries, " "); got != want {
		t.Fatalf("session entries = %q, want %q", got, want)
	}
}

func TestRunSessionContinuesUntilGoal(t *testing.T) {
	res, _, _, err := runFake(t, "text,retry,goal", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Args) != `{"facts":"keep going"}` {
		t.Fatalf("goal reached with %s, want the continue prompt", res.Args)
	}
}

func TestRunSessionNonRetryableError(t *testing.T) {
	_, _, _, err := runFake(t, "text,fatal", 3, 0)
	var f *SessionFailure
	if !errors.As(err, &f) || f.Reason != "400 model rejected the request" {
		t.Fatalf("err = %v", err)
	}
}

func TestRunSessionContinueLimit(t *testing.T) {
	_, _, _, err := runFake(t, "text", 2, 0)
	var f *SessionFailure
	if !errors.As(err, &f) || f.Reason != "continue limit reached" {
		t.Fatalf("err = %v", err)
	}
}

// A turn that keeps calling a non-goal tool never yields a prompt_result, so only MaxDuration
// ends the session.
func TestRunSessionTimeout(t *testing.T) {
	start := time.Now()
	_, _, sessDir, err := runFake(t, "loop", 3, 1500*time.Millisecond)
	elapsed := time.Since(start)
	var f *SessionFailure
	if !errors.As(err, &f) || f.Reason != "session timeout after 1.5s" {
		t.Fatalf("err = %v", err)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 10*time.Second {
		t.Fatalf("returned after %v", elapsed)
	}
	// loop's tool calls wrote assistant messages, so the timed-out session has its session
	// file (acceptance row 2).
	sessionEntries(t, sessDir)
}
