package omprpc

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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

	spec := func(sessDir string, env []string, maxDuration time.Duration) SessionSpec {
		return SessionSpec{
			Options:            Options{Bin: bin, Args: []string{"--session-dir", sessDir}, Env: env},
			Opening:            "open",
			Continue:           "keep going",
			GoalTools:          []string{"pm_base_ready", "pm_return_facts"},
			MaxContinuePrompts: 3,
			ReadyTimeout:       10 * time.Second,
			ExitGrace:          5 * time.Second,
			MaxDuration:        maxDuration,
		}
	}
	run := func(t *testing.T, s SessionSpec) (SessionResult, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return RunSession(ctx, s)
	}
	drive := func(t *testing.T, sessDir string, env []string, maxDuration time.Duration) (SessionResult, error) {
		t.Helper()
		return run(t, spec(sessDir, env, maxDuration))
	}

	// header decodes the session file's first line.
	type header struct {
		Type, ID, ParentSession string
	}
	readHeader := func(t *testing.T, path string) header {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		line, _, _ := strings.Cut(string(b), "\n")
		var h header
		if err := json.Unmarshal([]byte(line), &h); err != nil {
			t.Fatalf("header line %s: %v", line, err)
		}
		return h
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
		s := spec(sessDir, []string{
			"FAKE_OMP_PLAN=goal", "FAKE_OMP_GOAL=pm_return_facts", "FAKE_OMP_ARGS=" + argsFile,
		}, 0)
		// Like omp's, a fresh session has no file at ready; get_state already names it.
		var readyErr error
		s.OnReady = func(_, file string) { _, readyErr = os.Stat(file) }
		res, err := run(t, s)
		if err != nil {
			t.Fatal(err)
		}
		if !errors.Is(readyErr, fs.ErrNotExist) {
			t.Fatalf("session file at ready: %v, want it absent", readyErr)
		}
		if res.ToolName != "pm_return_facts" || string(res.Args) != `{"facts":["one","two"]}` {
			t.Fatalf("RunSession = {tool %s args %s}, want the goal tool and the file's arguments", res.ToolName, res.Args)
		}
		// The first assistant message created the file at get_state's path: the header plus
		// the goal call's omp entries, closing with the tool result.
		if h := readHeader(t, res.SessionFile); h.Type != "session" || h.ID != res.SessionID || h.ParentSession != "" {
			t.Fatalf("header = %+v, want a fresh session header with id %s", h, res.SessionID)
		}
		entries := sessionEntries(t, sessDir)
		want := "session message:assistant custom:tool_execution_start custom:tool_execution_end message:toolResult"
		if got := strings.Join(entries, " "); got != want {
			t.Fatalf("session entries = %q, want %q", got, want)
		}
	})

	// A text turn ends with an assistant text message, which creates a fresh session's file
	// as omp's does. No continue prompt is allowed, so the session fails after that one turn.
	t.Run("text", func(t *testing.T) {
		sessDir := t.TempDir()
		s := spec(sessDir, []string{"FAKE_OMP_PLAN=text"}, 0)
		s.MaxContinuePrompts = 0
		var (
			readyID, readyFile string
			readyErr           error
		)
		s.OnReady = func(id, file string) { readyID, readyFile = id, file; _, readyErr = os.Stat(file) }
		_, err := run(t, s)
		var f *SessionFailure
		if !errors.As(err, &f) || f.Reason != "continue limit reached" {
			t.Fatalf("err = %v, want continue limit reached", err)
		}
		if !errors.Is(readyErr, fs.ErrNotExist) {
			t.Fatalf("session file at ready: %v, want it absent", readyErr)
		}
		if h := readHeader(t, readyFile); h.Type != "session" || h.ID != readyID {
			t.Fatalf("header = %+v, want a session header with id %s", h, readyID)
		}
		if got := strings.Join(sessionEntries(t, sessDir), " "); got != "session message:assistant" {
			t.Fatalf("session entries = %q, want the header and one assistant message", got)
		}
	})

	// stall stops the turn before its first assistant message, so the session never gets a
	// file: the failed session without one that the session lists must show (pm-guardians#26).
	t.Run("stall", func(t *testing.T) {
		sessDir := t.TempDir()
		s := spec(sessDir, []string{"FAKE_OMP_PLAN=stall"}, 2*time.Second)
		var readyFile string
		s.OnReady = func(_, file string) { readyFile = file }
		_, err := run(t, s)
		var f *SessionFailure
		if !errors.As(err, &f) || f.Reason != "session timeout after 2s" {
			t.Fatalf("err = %v, want session timeout after 2s", err)
		}
		if !strings.HasPrefix(readyFile, sessDir+"/") || !strings.HasSuffix(readyFile, ".jsonl") {
			t.Fatalf("get_state sessionFile = %q, want a .jsonl under %s", readyFile, sessDir)
		}
		// RunSession killed the process on timeout; nothing was ever written.
		if names, err := os.ReadDir(sessDir); err != nil || len(names) != 0 {
			t.Fatalf("session dir = %v (%v), want it empty", names, err)
		}
	})

	// --fork creates the new file before the ready frame, as omp's forkFrom does, its header
	// naming the source session; the source's entries are not copied.
	t.Run("fork", func(t *testing.T) {
		src, err := drive(t, t.TempDir(), []string{"FAKE_OMP_PLAN=goal"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		sessDir := t.TempDir()
		s := spec(sessDir, []string{"FAKE_OMP_PLAN=stall"}, 2*time.Second)
		s.Options.Args = append(s.Options.Args, "--fork", src.SessionFile)
		var (
			readyID   string
			readyHead header
		)
		s.OnReady = func(id, file string) { readyID, readyHead = id, readHeader(t, file) }
		_, err = run(t, s)
		var f *SessionFailure
		if !errors.As(err, &f) || f.Reason != "session timeout after 2s" {
			t.Fatalf("err = %v, want session timeout after 2s", err)
		}
		if readyHead.Type != "session" || readyHead.ID != readyID || readyHead.ParentSession != src.SessionID {
			t.Fatalf("header at ready = %+v, want id %s and parentSession %s", readyHead, readyID, src.SessionID)
		}
		if got := strings.Join(sessionEntries(t, sessDir), " "); got != "session" {
			t.Fatalf("fork session entries = %q, want the header alone", got)
		}
	})

	// The council plugin appends PM_META as the pm-meta entry at session start; the council's
	// session list reads the session kind from it, so the fake writes it right after the header.
	t.Run("meta", func(t *testing.T) {
		sessDir := t.TempDir()
		meta := `{"kind":"draft","seat":"Coordination","ref":"o/pm#2","round":1,"version":1}`
		if _, err := drive(t, sessDir, []string{"FAKE_OMP_PLAN=goal", "PM_META=" + meta}, 0); err != nil {
			t.Fatal(err)
		}
		want := "session custom:pm-meta message:assistant custom:tool_execution_start custom:tool_execution_end message:toolResult"
		if got := strings.Join(sessionEntries(t, sessDir), " "); got != want {
			t.Fatalf("session entries = %q, want %q", got, want)
		}
		names, _ := filepath.Glob(filepath.Join(sessDir, "*.jsonl"))
		b, err := os.ReadFile(names[0])
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(strings.Split(string(b), "\n")[1]), &e); err != nil || string(e.Data) != meta {
			t.Fatalf("pm-meta data = %s (%v), want %s", e.Data, err, meta)
		}
	})

	t.Run("badmeta", func(t *testing.T) {
		_, err := drive(t, t.TempDir(), []string{"FAKE_OMP_PLAN=goal", "PM_META={not json"}, 0)
		if err == nil || !strings.Contains(err.Error(), "PM_META is not JSON") {
			t.Fatalf("err = %v, want the startup failure naming PM_META", err)
		}
	})

	t.Run("loop", func(t *testing.T) {
		sessDir := t.TempDir()
		_, err := drive(t, sessDir, []string{"FAKE_OMP_PLAN=loop"}, 2*time.Second)
		var f *SessionFailure
		if !errors.As(err, &f) || f.Reason != "session timeout after 2s" {
			t.Fatalf("err = %v, want session timeout after 2s", err)
		}
		// loop's tool calls wrote assistant messages, so the timed-out session has its file;
		// the council lists it as failed there.
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
