package sessions

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
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
	MaxDuration                time.Duration // bounds the session from start to its goal tool
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

// Registry runs sessions and remembers, for this process lifetime, which are live. Everything
// else about a session comes from its file and the failure record beside it. Every omp child it
// starts receives the configured launch link as PM_LAUNCH_LINK.
type Registry struct {
	launchLink config.LaunchLink
	mu         sync.Mutex
	live       map[string]bool
}

func NewRegistry(launchLink config.LaunchLink) *Registry {
	return &Registry{launchLink: launchLink, live: map[string]bool{}}
}

// Run starts omp per s through omprpc.RunSession and records, while it runs, that the session is
// live and, if it fails, why (at FailurePath of its session file).
func (r *Registry) Run(ctx context.Context, s Spec) (Result, error) {
	started := time.Now().UTC()
	if err := os.MkdirAll(s.SessionDir, 0o755); err != nil {
		return Result{}, &Failure{Reason: err.Error()}
	}
	meta, err := json.Marshal(s.Meta)
	if err != nil {
		return Result{}, &Failure{Reason: err.Error()}
	}
	launchLink, err := json.Marshal(r.launchLink)
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
	var sessionID, sessionFile string
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
				"PM_LAUNCH_LINK=" + string(launchLink),
			}, s.Env...),
		},
		Opening:            s.Opening,
		Continue:           s.Continue,
		GoalTools:          s.GoalTools,
		MaxContinuePrompts: s.MaxContinuePrompts,
		ReadyTimeout:       s.OMP.ReadyTimeout(),
		ExitGrace:          s.OMP.ExitGrace(),
		MaxDuration:        s.MaxDuration,
		OnReady: func(id, file string) {
			sessionID, sessionFile = id, file
			r.mu.Lock()
			r.live[id] = true
			r.mu.Unlock()
		},
	})
	// The failure record is written before the session stops being live, so a listing never sees
	// a finished failed session without its reason and end time. omp writes nothing once killed,
	// so this moment, not the session file's last entry, is when the session ended. The record
	// carries what Run knew from its Spec, so the session lists even when omp never wrote its
	// file (§I).
	if err != nil && sessionFile != "" {
		rec := failureRecord{
			Reason: err.Error(), At: time.Now().UTC(),
			ID: sessionID, Kind: s.Meta.Kind, Base: s.Meta.Base, Ref: s.Meta.Ref,
			Seat: s.Meta.Seat, Ask: s.Meta.Ask, Tier: s.Meta.Tier,
			Model: s.Meta.Model, StartedAt: started,
		}
		if rec.Model == "" {
			rec.Model = s.Model
		}
		if werr := writeFailure(sessionFile, rec); werr != nil {
			slog.Error("record session failure", "session", sessionID, "err", werr)
		}
	}
	if sessionID != "" {
		r.mu.Lock()
		delete(r.live, sessionID)
		r.mu.Unlock()
	}
	if err != nil {
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

// List derives session rows from the session files under dir, newest first, at most limit rows.
// Only the files that make the rows are parsed; the rest are known by their header. A session
// whose file omp never wrote lists from its failure record alone (§I).
func (r *Registry) List(dir string, goalTools map[Kind][]string, limit int) ([]Summary, error) {
	cands, err := Newest(dir)
	if err != nil {
		return nil, err
	}
	files := make([]*File, 0, min(limit, len(cands)))
	for _, c := range cands {
		if len(files) == limit {
			break
		}
		f, ok, err := c.Parse()
		if err != nil {
			return nil, err
		}
		if ok {
			files = append(files, f)
		}
	}
	return r.summaries(files, goalTools), nil
}

// summaries derives one row per file, in order.
func (r *Registry) summaries(files []*File, goalTools map[Kind][]string) []Summary {
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
		if f.Failure != "" {
			errText = f.Failure
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
			if !f.FailedAt.IsZero() {
				ended = f.FailedAt
			}
			s.EndedAt = &ended
		}
		if errText != "" {
			s.Error = &errText
		}
		out = append(out, s)
	}
	return out
}

// Live reports whether the session id is held by a running process.
func (r *Registry) Live(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[id]
}

// FindFile returns the session file under dir whose header id is id, reading headers only. A
// failure record without its session file names no openable log, so orphan entries never match.
func FindFile(dir, id string) (string, bool, error) {
	var found string
	lr := newLineReader(headerBuf)
	err := walk(dir, func(path string) error {
		h, err := readHeader(path, lr)
		if skipped(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.id != id {
			return nil
		}
		found = path
		return fs.SkipAll
	}, nil)
	if err != nil {
		return "", false, err
	}
	return found, found != "", nil
}
