package omprpc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFakeOmpBinary drives the built cmd/fake-omp the way pm-council will: through
// RunSession with the FAKE_OMP_* environment deciding the plan (acceptance rows 1–3).
func TestFakeOmpBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-omp")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/oh-my-omp/pm-guardians/cmd/fake-omp").CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/fake-omp: %v: %s", err, out)
	}

	drive := func(t *testing.T, sessDir string, env []string, maxDuration time.Duration) (SessionResult, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return RunSession(ctx, SessionSpec{
			Options:            Options{Bin: bin, Args: []string{"--session-dir", sessDir}, Env: env},
			Opening:            "open",
			Continue:           "keep going",
			GoalTools:          []string{"pm_base_ready", "pm_return_facts"},
			MaxContinuePrompts: 3,
			ReadyTimeout:       10 * time.Second,
			ExitGrace:          5 * time.Second,
			MaxDuration:        maxDuration,
		})
	}

	// sessionEntries decodes the session file: one line per entry, type+customType+role.
	sessionEntries := func(t *testing.T, dir string) []string {
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

	t.Run("goal", func(t *testing.T) {
		sessDir := t.TempDir()
		argsFile := filepath.Join(t.TempDir(), "facts.json")
		if err := os.WriteFile(argsFile, []byte(`{"facts":["one","two"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := drive(t, sessDir, []string{
			"FAKE_OMP_PLAN=goal", "FAKE_OMP_GOAL=pm_return_facts", "FAKE_OMP_ARGS=" + argsFile,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.ToolName != "pm_return_facts" || string(res.Args) != `{"facts":["one","two"]}` {
			t.Fatalf("RunSession = {tool %s args %s}, want the goal tool and the file's arguments", res.ToolName, res.Args)
		}
		// RunSession's session file is a real one: the header plus the goal call's omp
		// entries, closing with the tool result.
		entries := sessionEntries(t, sessDir)
		want := "session message:assistant custom:tool_execution_start custom:tool_execution_end message:toolResult"
		if got := strings.Join(entries, " "); got != want {
			t.Fatalf("session entries = %q, want %q", got, want)
		}
	})

	t.Run("loop", func(t *testing.T) {
		sessDir := t.TempDir()
		_, err := drive(t, sessDir, []string{"FAKE_OMP_PLAN=loop"}, 2*time.Second)
		var f *SessionFailure
		if !errors.As(err, &f) || f.Reason != "session timeout after 2s" {
			t.Fatalf("err = %v, want session timeout after 2s", err)
		}
		// The timed-out session still has its file; the council lists it as failed there.
		sessionEntries(t, sessDir)
	})

	t.Run("die", func(t *testing.T) {
		sessDir := t.TempDir()
		_, err := drive(t, sessDir, []string{"FAKE_OMP_PLAN=die"}, 0)
		if err == nil || !strings.Contains(err.Error(),
			"fatal: model not found: nope/nope") {
			t.Fatalf("err = %v, want the startup failure with the stderr tail", err)
		}
	})
}
