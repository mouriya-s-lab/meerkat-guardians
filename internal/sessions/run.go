package sessions

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/omprpc"
)

// Spec is one omp session to run until one of its goal tools succeeds.
type Spec struct {
	OMP                        config.OMP
	Model, SystemPrompt, Tools string
	SessionDir, Cwd            string
	ForkFrom                   string   // base session file for forks; empty for bases
	GoalTools                  []string // any one succeeding completes the session
	Opening, Continue          string
	Meta                       Meta
	Env                        []string // extra environment of the omp child, KEY=VALUE
	MaxContinuePrompts         int
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

// Run starts omp per s through omprpc.RunSession and records, while it runs, that the session is
// live and, if it fails, why.
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
		"--trusted-extension", s.OMP.Extension,
		"--no-skills", "--no-rules", "--no-lsp",
	}
	if s.ForkFrom != "" {
		args = append(args, "--fork", s.ForkFrom)
	}
	var sessionID string
	res, err := omprpc.RunSession(ctx, omprpc.SessionSpec{
		Options: omprpc.Options{
			Bin:  s.OMP.Bin,
			Args: args,
			Dir:  s.Cwd,
			Env: append([]string{
				"PI_CODING_AGENT_DIR=" + s.OMP.AgentDir,
				"PM_KIND=" + string(s.Meta.Kind),
				"PM_GOAL_TOOL=" + strings.Join(s.GoalTools, ","),
				"PM_CONTINUE=" + s.Continue,
				"PM_META=" + string(meta),
			}, s.Env...),
		},
		Opening:            s.Opening,
		Continue:           s.Continue,
		GoalTools:          s.GoalTools,
		MaxContinuePrompts: s.MaxContinuePrompts,
		ReadyTimeout:       s.OMP.ReadyTimeout(),
		ExitGrace:          s.OMP.ExitGrace(),
		OnReady: func(id, _ string) {
			sessionID = id
			r.mu.Lock()
			r.live[id] = true
			r.mu.Unlock()
		},
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID != "" {
		delete(r.live, sessionID)
	}
	if err != nil {
		if sessionID != "" {
			r.failures[sessionID] = err.Error()
		}
		return Result{}, &Failure{SessionID: sessionID, Reason: err.Error()}
	}
	return Result{SessionID: res.SessionID, SessionFile: res.SessionFile, Args: res.Args}, nil
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
	Parent    *string    `json:"parent"`
}

// List derives session rows from every session file under dir, newest first, at most limit rows.
// goalTools maps each kind to the tools any one of whose success makes a session of that kind done.
func (r *Registry) List(dir string, goalTools map[Kind][]string, limit int) ([]Summary, error) {
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
		if f.ParentSession != "" {
			parent := f.ParentSession
			s.Parent = &parent
		}
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
		switch _, done := f.GoalAt(goalTools[s.Kind]); {
		case r.live[f.ID]:
			s.State = "running"
		case done:
			s.State = "done"
		default:
			s.State = "failed"
			if errText == "" {
				errText = "session ended without calling " + strings.Join(goalTools[s.Kind], " or ")
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
