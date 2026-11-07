package omprpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeSession answers each prompt per the next step of FAKE_PLAN (comma separated; the last step
// repeats): "goal" calls FAKE_GOAL with the prompt text as its argument, "text" ends the turn
// without it, "retry" ends it with a retryable error, "fatal" with a non-retryable one, "loop"
// keeps calling a non-goal tool and never ends the turn.
func fakeSession() {
	out := json.NewEncoder(os.Stdout)
	plan := strings.Split(os.Getenv("FAKE_PLAN"), ",")
	goal := os.Getenv("FAKE_GOAL")
	_ = out.Encode(map[string]any{"type": "ready"})
	sc := bufio.NewScanner(os.Stdin)
	step := 0
	for sc.Scan() {
		var cmd struct {
			ID, Type, Message string
		}
		if json.Unmarshal(sc.Bytes(), &cmd) != nil {
			continue
		}
		switch cmd.Type {
		case "get_state":
			_ = out.Encode(map[string]any{"type": "response", "id": cmd.ID, "command": "get_state", "success": true,
				"data": map[string]any{"sessionId": "s-9", "sessionFile": "/tmp/s-9.jsonl"}})
		case "prompt":
			_ = out.Encode(map[string]any{"type": "response", "id": cmd.ID, "command": "prompt", "success": true})
			action := plan[min(step, len(plan)-1)]
			step++
			result := map[string]any{"type": "prompt_result", "id": cmd.ID, "agentInvoked": true, "status": "completed"}
			switch action {
			case "goal":
				id := "call-" + cmd.ID
				_ = out.Encode(map[string]any{"type": "tool_execution_start", "toolCallId": id, "toolName": goal,
					"args": map[string]any{"facts": cmd.Message}})
				_ = out.Encode(map[string]any{"type": "tool_execution_end", "toolCallId": id, "toolName": goal, "isError": false})
			case "retry":
				result["status"] = "error"
				result["error"] = map[string]any{"message": "429 slow down", "retryable": true}
			case "fatal":
				result["status"] = "error"
				result["error"] = map[string]any{"message": "400 model rejected the request", "retryable": false}
			case "loop":
				for i := 0; ; i++ {
					id := fmt.Sprintf("loop-%s-%d", cmd.ID, i)
					_ = out.Encode(map[string]any{"type": "tool_execution_start", "toolCallId": id, "toolName": "read",
						"args": map[string]any{"path": "store.go"}})
					_ = out.Encode(map[string]any{"type": "tool_execution_end", "toolCallId": id, "toolName": "read", "isError": false})
					time.Sleep(20 * time.Millisecond)
				}
			}
			_ = out.Encode(result)
		}
	}
}

func runFake(t *testing.T, plan string, maxContinue int, maxDuration time.Duration) (SessionResult, []string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var ready []string
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := RunSession(ctx, SessionSpec{
		Options:            Options{Bin: exe, Env: []string{"FAKE_OMP=session", "FAKE_PLAN=" + plan, "FAKE_GOAL=pm_return_facts"}},
		Opening:            "open",
		Continue:           "keep going",
		GoalTools:          []string{"pm_base_ready", "pm_return_facts"},
		MaxContinuePrompts: maxContinue,
		ReadyTimeout:       10 * time.Second,
		ExitGrace:          5 * time.Second,
		MaxDuration:        maxDuration,
		OnReady:            func(id, file string) { ready = append(ready, id, file) },
	})
	return res, ready, err
}

func TestRunSessionGoalReached(t *testing.T) {
	res, ready, err := runFake(t, "goal", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "s-9" || res.SessionFile != "/tmp/s-9.jsonl" || res.ToolName != "pm_return_facts" ||
		string(res.Args) != `{"facts":"open"}` {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(ready, " ") != "s-9 /tmp/s-9.jsonl" {
		t.Fatalf("OnReady = %v", ready)
	}
}

func TestRunSessionContinuesUntilGoal(t *testing.T) {
	res, _, err := runFake(t, "text,retry,goal", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Args) != `{"facts":"keep going"}` {
		t.Fatalf("goal reached with %s, want the continue prompt", res.Args)
	}
}

func TestRunSessionNonRetryableError(t *testing.T) {
	_, _, err := runFake(t, "text,fatal", 3, 0)
	var f *SessionFailure
	if !errors.As(err, &f) || f.Reason != "400 model rejected the request" {
		t.Fatalf("err = %v", err)
	}
}

func TestRunSessionContinueLimit(t *testing.T) {
	_, _, err := runFake(t, "text", 2, 0)
	var f *SessionFailure
	if !errors.As(err, &f) || f.Reason != "continue limit reached" {
		t.Fatalf("err = %v", err)
	}
}

// A turn that keeps calling a non-goal tool never yields a prompt_result, so only MaxDuration
// ends the session.
func TestRunSessionTimeout(t *testing.T) {
	start := time.Now()
	_, _, err := runFake(t, "loop", 3, 1500*time.Millisecond)
	elapsed := time.Since(start)
	var f *SessionFailure
	if !errors.As(err, &f) || f.Reason != "session timeout after 1.5s" {
		t.Fatalf("err = %v", err)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 10*time.Second {
		t.Fatalf("returned after %v", elapsed)
	}
}
