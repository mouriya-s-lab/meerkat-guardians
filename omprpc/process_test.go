package omprpc

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mouriya-s-lab/meerkat-guardians/internal/fakeomp"
)

// The test binary doubles as a fake omp: every FAKE_OMP_PLAN set runs fakeomp on stdio, so
// the tests drive the same implementation cmd/fake-omp serves.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_OMP_PLAN") != "" {
		os.Exit(fakeomp.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// startFake starts the test binary as a fake omp. env carries the extra control variables;
// args carries the arguments (at least --session-dir).
func startFake(t *testing.T, plan string, args []string, env ...string) (*Process, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Start(ctx, Options{Bin: exe, Args: args,
		Env: append([]string{"FAKE_OMP_PLAN=" + plan}, env...)})
}

// goalEnv resolves the goal step's canned arguments file once per test.
func goalEnv(t *testing.T) []string {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "args.json")
	if err := os.WriteFile(argsFile, []byte(`{"facts":"main.go:15"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"FAKE_OMP_GOAL=pm_return_facts", "FAKE_OMP_ARGS=" + argsFile}
}

func TestStartFailsWithStderrTail(t *testing.T) {
	_, err := startFake(t, "die", nil)
	if err == nil || !strings.Contains(err.Error(), "model not found: nope/nope") {
		t.Fatalf("want error with stderr tail, got %v", err)
	}
}

func TestStartFailsOnUnknownPlanStep(t *testing.T) {
	_, err := startFake(t, "brew", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown plan step") {
		t.Fatalf("want unknown-plan-step error, got %v", err)
	}
}

func TestStartFailsWithoutSessionDir(t *testing.T) {
	_, err := startFake(t, "goal", nil)
	if err == nil || !strings.Contains(err.Error(), "--session-dir") {
		t.Fatalf("want missing --session-dir error, got %v", err)
	}
}

func TestCallAndEvents(t *testing.T) {
	sessDir := t.TempDir()
	p, err := startFake(t, "text,goal,retry", []string{"--session-dir", sessDir}, goalEnv(t)...)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	data, err := p.Call(ctx, map[string]any{"type": "get_state"})
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		SessionID   string
		SessionFile string
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.SessionID == "" || !strings.Contains(st.SessionFile, sessDir+"/") ||
		!strings.Contains(filepath.Base(st.SessionFile), st.SessionID) ||
		!strings.HasSuffix(st.SessionFile, ".jsonl") {
		t.Fatalf("get_state data %s, want a %s/<UTC>_<uuid>.jsonl named after its id", data, sessDir)
	}
	// Like omp's, a fresh session has no file before its first assistant message.
	if _, err := os.Stat(st.SessionFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("session file before the first assistant message: %v, want it absent", err)
	}

	if _, err := p.Call(ctx, map[string]any{"type": "bogus"}); err == nil ||
		!strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "Unknown command") {
		t.Fatalf("want failure naming command and message, got %v", err)
	}

	// The three prompts run the plan's three steps; drain each turn's events before the
	// next prompt, as RunSession does between prompts.
	if err := p.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	var updates int
	for e := range p.Events() {
		if e.Type == "message_update" {
			updates++
		}
		if _, ok := DecodePromptResult(e); ok {
			break
		}
	}
	if err := p.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	var (
		start  ToolStart
		end    ToolEnd
		result PromptResult
	)
	// The goal turn's own prompt_result is consumed too; the retry turn supplies the
	// error result the old protocol fixture asserted.
	for e := range p.Events() {
		if e.Type == "response" {
			t.Fatalf("response leaked into events: %s", e.Raw)
		}
		if v, ok := DecodeToolStart(e); ok {
			start = v
		}
		if v, ok := DecodeToolEnd(e); ok {
			end = v
		}
		if v, ok := DecodePromptResult(e); ok {
			result = v
			if v.Status == "error" {
				break
			}
			if err := p.Prompt(ctx, "hi"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if updates != 500 {
		t.Fatalf("updates = %d", updates)
	}
	if start.ToolName != "pm_return_facts" || string(start.Args) != `{"facts":"main.go:15"}` {
		t.Fatalf("start = %+v", start)
	}
	if start.ToolCallID != end.ToolCallID || end.IsError {
		t.Fatalf("end = %+v", end)
	}
	if len(end.Result.Content) != 1 || end.Result.Content[0].Text != "recorded" {
		t.Fatalf("end = %+v", end)
	}
	if result.Status != "error" || result.Error == nil || !result.Error.Retryable || result.Error.Message != "429 slow down" {
		t.Fatalf("result = %+v", result)
	}
	// The text turn's assistant message created the file, with get_state's id as its header id.
	b, err := os.ReadFile(st.SessionFile)
	if err != nil {
		t.Fatalf("session file after the goal turn: %v", err)
	}
	header, _, _ := strings.Cut(string(b), "\n")
	if !strings.HasPrefix(header, `{"type":"session",`) || !strings.Contains(header, `"id":"`+st.SessionID+`"`) {
		t.Fatalf("session header %s, want a session header with get_state's id", header)
	}

	if err := p.CloseStdin(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(p.StderrTail(), "booting") {
		t.Fatalf("stderr tail = %q", p.StderrTail())
	}
}

func TestDecodersRejectOtherTypes(t *testing.T) {
	e := Event{Type: "tool_execution_end", Raw: json.RawMessage(`{"type":"tool_execution_end","toolCallId":"x"}`)}
	if _, ok := DecodeToolStart(e); ok {
		t.Fatal("DecodeToolStart accepted tool_execution_end")
	}
	if _, ok := DecodePromptResult(e); ok {
		t.Fatal("DecodePromptResult accepted tool_execution_end")
	}
	if v, ok := DecodeToolEnd(e); !ok || v.ToolCallID != "x" {
		t.Fatalf("DecodeToolEnd = %+v %v", v, ok)
	}
}
