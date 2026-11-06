package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/oh-my-omp/pm-guardians/omprpc"
)

const (
	readyTimeout = 2 * time.Minute
	exitGrace    = 30 * time.Second
)

// Spec is one omp session to run until its goal tool succeeds.
type Spec struct {
	Bin, AgentDir, Extension string
	Model, SystemPrompt, Tools string
	SessionDir, Cwd          string
	ForkFrom                 string // base session file for forks; empty for bases
	GoalTool                 string
	Opening, Continue        string
	Meta                     Meta
	MaxContinuePrompts       int
}

// Result is a session that reached its goal: Args are the goal tool call's arguments.
type Result struct {
	SessionID   string
	SessionFile string
	Args        json.RawMessage
}

// Failure is a session that ended without its goal. SessionID is empty when omp failed before
// creating the session.
type Failure struct {
	SessionID string
	Reason    string
}

func (f *Failure) Error() string { return f.Reason }

// Registry runs sessions and remembers, for this process lifetime, which are live and why
// failed ones failed. Everything else about a session comes from its file.
type Registry struct {
	mu       sync.Mutex
	live     map[string]bool
	failures map[string]string
}

func NewRegistry() *Registry {
	return &Registry{live: map[string]bool{}, failures: map[string]string{}}
}

// Run starts omp per s, sends the opening prompt and holds the session until the goal tool
// succeeds (then closes stdin and waits for exit), omp exits, a non-retryable error is reported,
// or MaxContinuePrompts continue prompts did not reach the goal.
func (r *Registry) Run(ctx context.Context, s Spec) (Result, error) {
	if err := os.MkdirAll(s.SessionDir, 0o755); err != nil {
		return Result{}, &Failure{Reason: err.Error()}
	}
	meta, err := json.Marshal(s.Meta)
	if err != nil {
		return Result{}, &Failure{Reason: err.Error()}
	}
	args := []string{
		"--mode", "rpc",
		"--session-dir", s.SessionDir,
		"--model", s.Model,
		"--system-prompt", s.SystemPrompt,
		"--tools", s.Tools,
		// An exact extension allowlist and no ambient skills, rules or LSP: a fork's system prompt
		// and tool set stay identical to its base's, so the fork reuses the base's prompt cache.
		"--trusted-extension", s.Extension,
		"--no-skills", "--no-rules", "--no-lsp",
	}
	if s.ForkFrom != "" {
		args = append(args, "--fork", s.ForkFrom)
	}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	proc, err := omprpc.Start(readyCtx, omprpc.Options{
		Bin:  s.Bin,
		Args: args,
		Dir:  s.Cwd,
		Env: []string{
			"PI_CODING_AGENT_DIR=" + s.AgentDir,
			"PM_KIND=" + string(s.Meta.Kind),
			"PM_GOAL_TOOL=" + s.GoalTool,
			"PM_CONTINUE=" + s.Continue,
			"PM_META=" + string(meta),
		},
	})
	cancel()
	if err != nil {
		return Result{}, &Failure{Reason: err.Error()}
	}

	res, err := r.hold(ctx, proc, s)
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		var f *Failure
		if errors.As(err, &f) && f.SessionID != "" {
			r.mu.Lock()
			r.failures[f.SessionID] = f.Reason
			r.mu.Unlock()
		}
	}
	return res, err
}

func (r *Registry) hold(ctx context.Context, proc *omprpc.Process, s Spec) (Result, error) {
	data, err := proc.Call(ctx, map[string]any{"type": "get_state"})
	if err != nil {
		return Result{}, &Failure{Reason: err.Error()}
	}
	var state struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
	}
	if err := json.Unmarshal(data, &state); err != nil || state.SessionID == "" {
		return Result{}, &Failure{Reason: fmt.Sprintf("get_state without sessionId: %s", data)}
	}
	r.mu.Lock()
	r.live[state.SessionID] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.live, state.SessionID)
		r.mu.Unlock()
	}()
	fail := func(reason string) (Result, error) {
		return Result{}, &Failure{SessionID: state.SessionID, Reason: reason}
	}

	if err := proc.Prompt(ctx, s.Opening); err != nil {
		return fail(err.Error())
	}
	starts := map[string]json.RawMessage{}
	continues := 0
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err().Error())
		case e, ok := <-proc.Events():
			if !ok {
				return fail(fmt.Sprintf("omp exited before %s succeeded: %s", s.GoalTool, proc.StderrTail()))
			}
			if v, ok := omprpc.DecodeToolStart(e); ok && v.ToolName == s.GoalTool {
				starts[v.ToolCallID] = v.Args
				continue
			}
			if v, ok := omprpc.DecodeToolEnd(e); ok && v.ToolName == s.GoalTool && !v.IsError {
				args, ok := starts[v.ToolCallID]
				if !ok {
					return fail(fmt.Sprintf("%s ended without a start event for %s", s.GoalTool, v.ToolCallID))
				}
				if err := proc.CloseStdin(); err != nil {
					return fail(err.Error())
				}
				waitExit(proc)
				return Result{SessionID: state.SessionID, SessionFile: state.SessionFile, Args: args}, nil
			}
			if v, ok := omprpc.DecodePromptResult(e); ok {
				if v.Status == "error" && v.Error != nil && !v.Error.Retryable {
					return fail(v.Error.Message)
				}
				if continues >= s.MaxContinuePrompts {
					return fail("continue limit reached")
				}
				continues++
				if err := proc.Prompt(ctx, s.Continue); err != nil {
					return fail(err.Error())
				}
			}
		}
	}
}

// waitExit waits for a process whose stdin is closed, killing it after exitGrace.
func waitExit(proc *omprpc.Process) {
	done := make(chan struct{})
	go func() {
		_ = proc.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(exitGrace):
		_ = proc.Kill()
		<-done
	}
}

// Summary is one row of GET /v1/sessions.
type Summary struct {
	ID        string     `json:"id"`
	Kind      Kind       `json:"kind"`
	Label     string     `json:"label"`
	Model     string     `json:"model"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt"`
	State     string     `json:"state"`
	Error     *string    `json:"error"`
	Ref       *string    `json:"ref"`
}

// List derives session rows from every session file under dir, newest first, at most limit rows.
// goalTools maps each kind to the tool whose success makes a session of that kind done.
func (r *Registry) List(dir string, goalTools map[Kind]string, limit int) ([]Summary, error) {
	files, err := ScanDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].StartedAt.After(files[j].StartedAt) })
	if len(files) > limit {
		files = files[:limit]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Summary, 0, len(files))
	for _, f := range files {
		s := Summary{ID: f.ID, StartedAt: f.StartedAt}
		var label string
		if m := f.Meta; m != nil {
			s.Kind, s.Model = m.Kind, m.Model
			label = m.Base
			if m.Ref != "" {
				ref := m.Ref
				s.Ref = &ref
				label = ref
			}
		}
		s.Label = string(s.Kind) + " " + label
		errText := f.LastError
		if reason, ok := r.failures[f.ID]; ok {
			errText = reason
		}
		switch _, done := f.ToolOK[goalTools[s.Kind]]; {
		case r.live[f.ID]:
			s.State = "running"
		case done:
			s.State = "done"
		default:
			s.State = "failed"
			if errText == "" {
				errText = "session ended without calling " + goalTools[s.Kind]
			}
		}
		if s.State != "running" {
			ended := f.LastAt
			s.EndedAt = &ended
		}
		if errText != "" {
			s.Error = &errText
		}
		out = append(out, s)
	}
	return out, nil
}

// Live reports whether the session id is held by a running process.
func (r *Registry) Live(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[id]
}

// FindFile returns the session file under dir whose header id is id.
func FindFile(dir, id string) (string, bool, error) {
	files, err := ScanDir(dir)
	if err != nil {
		return "", false, err
	}
	for _, f := range files {
		if f.ID == id {
			return f.Path, true, nil
		}
	}
	return "", false, nil
}
