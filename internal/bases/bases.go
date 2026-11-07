// Package bases warms base sessions and derives their state from the base session files.
package bases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/checkout"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/sessions"
)

var (
	ErrUnknown = errors.New("unknown base")
	ErrWarming = errors.New("base is warming")
)

// Manager owns warm runs. A base's state is derived from its session directory plus the
// in-memory set of live warm runs and their last failure reasons.
type Manager struct {
	cfg *config.Config
	reg *sessions.Registry
	co  *checkout.Manager
	sem chan struct{}

	mu      sync.Mutex
	warming map[string]bool
	failure map[string]string
}

func New(cfg *config.Config, reg *sessions.Registry, co *checkout.Manager) *Manager {
	return &Manager{
		cfg: cfg, reg: reg, co: co,
		sem:     make(chan struct{}, cfg.Runtime.MaxConcurrentWarmups),
		warming: map[string]bool{},
		failure: map[string]string{},
	}
}

// Dir is the session directory of base id.
func (m *Manager) Dir(id string) string {
	return filepath.Join(m.cfg.Runtime.SessionsDir, "bases", id)
}

// WarmMissing starts a warm run for every configured base without a ready session file.
func (m *Manager) WarmMissing() error {
	for _, b := range m.cfg.Bases {
		ready, err := m.ForkSource(b.ID)
		if err != nil {
			return err
		}
		if ready != nil {
			slog.Info("base ready", "base", b.ID, "session", ready.ID)
			continue
		}
		if err := m.warm(b); err != nil {
			return err
		}
	}
	return nil
}

// Rewarm starts a warm run for base id. The previous ready file stays the fork source until the
// new one is ready.
func (m *Manager) Rewarm(id string) error {
	b, ok := m.cfg.Base(id)
	if !ok {
		return ErrUnknown
	}
	return m.warm(b)
}

func (m *Manager) warm(b config.Base) error {
	m.mu.Lock()
	if m.warming[b.ID] {
		m.mu.Unlock()
		return ErrWarming
	}
	m.warming[b.ID] = true
	m.mu.Unlock()
	go func() {
		m.sem <- struct{}{}
		err := m.run(b)
		<-m.sem
		m.mu.Lock()
		delete(m.warming, b.ID)
		if err != nil {
			m.failure[b.ID] = err.Error()
		} else {
			delete(m.failure, b.ID)
		}
		m.mu.Unlock()
		if err != nil {
			slog.Error("warm failed", "base", b.ID, "err", err)
		} else {
			slog.Info("warm done", "base", b.ID)
		}
	}()
	return nil
}

func (m *Manager) run(b config.Base) error {
	ctx := context.Background()
	g, _ := m.cfg.Guardian(b.Guardian)
	dir, commit, err := m.co.Sync(ctx, b.Repo, b.Branch)
	if err != nil {
		return err
	}
	fp, err := m.currentFingerprint(b, g)
	if err != nil {
		return err
	}
	fp.Commit = commit
	opening, cont, err := m.cfg.Goals.Base.Render(config.BaseVars{Repo: b.Repo, Branch: b.Branch, Focus: b.Focus})
	if err != nil {
		return err
	}
	rt := m.cfg.Runtime
	_, err = m.reg.Run(ctx, sessions.Spec{
		OMP:   rt.OMP,
		Model: b.Model, SystemPrompt: g.Prompt, Tools: g.ToolsArg(),
		SessionDir: m.Dir(b.ID), Cwd: dir,
		GoalTools: m.cfg.Goals.Base.Tools, Opening: opening, Continue: cont,
		Meta:               sessions.Meta{Kind: sessions.KindBase, Base: b.ID, Model: b.Model, Fingerprint: &fp},
		MaxContinuePrompts: rt.MaxContinuePrompts,
		MaxDuration:        rt.SessionTimeout(),
	})
	return err
}

// currentFingerprint is the fingerprint the current configuration would warm base b with,
// without the commit.
func (m *Manager) currentFingerprint(b config.Base, g config.Guardian) (sessions.Fingerprint, error) {
	prompt, err := os.ReadFile(g.Prompt)
	if err != nil {
		return sessions.Fingerprint{}, err
	}
	sum := sha256.Sum256(prompt)
	return sessions.Fingerprint{Model: b.Model, PromptSha256: hex.EncodeToString(sum[:]), Tools: g.ToolsArg()}, nil
}

// scan returns the newest session of base id and the newest one in which a base goal tool
// succeeded (nil if none). It parses session files newest first and stops at the ready one.
func (m *Manager) scan(id string) (newest, ready *sessions.File, err error) {
	cands, err := sessions.Newest(m.Dir(id))
	if err != nil {
		return nil, nil, err
	}
	for _, c := range cands {
		f, ok, err := c.Parse()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		if newest == nil {
			newest = f
		}
		if _, ok := f.GoalAt(m.cfg.Goals.Base.Tools); ok {
			return newest, f, nil
		}
	}
	return newest, nil, nil
}

// ForkSource returns the newest ready session file of base id, nil when there is none.
func (m *Manager) ForkSource(id string) (*sessions.File, error) {
	_, ready, err := m.scan(id)
	return ready, err
}

// View is one row of GET /v1/bases.
type View struct {
	ID          string     `json:"id"`
	Guardian    string     `json:"guardian"`
	Repo        string     `json:"repo"`
	Model       string     `json:"model"`
	State       string     `json:"state"`
	Session     *string    `json:"session"`
	WarmedAt    *time.Time `json:"warmedAt"`
	Error       *string    `json:"error"`
	Stale       bool       `json:"stale"`
	StaleReason string     `json:"staleReason"`
}

// List derives the state of every configured base.
func (m *Manager) List(ctx context.Context) ([]View, error) {
	out := make([]View, 0, len(m.cfg.Bases))
	for _, b := range m.cfg.Bases {
		v := View{ID: b.ID, Guardian: b.Guardian, Repo: b.Repo, Model: b.Model}
		newest, ready, err := m.scan(b.ID)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		warming, failure := m.warming[b.ID], m.failure[b.ID]
		m.mu.Unlock()

		if ready != nil {
			id := ready.ID
			at, _ := ready.GoalAt(m.cfg.Goals.Base.Tools)
			v.Session, v.WarmedAt = &id, &at
			g, _ := m.cfg.Guardian(b.Guardian)
			current, err := m.currentFingerprint(b, g)
			if err != nil {
				return nil, err
			}
			head, headErr := m.co.RemoteHead(ctx, b.Repo, b.Branch)
			v.Stale, v.StaleReason = Compare(recorded(ready), current, b.Branch, head, headErr)
		}
		switch {
		case warming:
			v.State = "warming"
		case ready != nil:
			v.State = "ready"
		default:
			v.State = "failed"
			reason := failure
			if reason == "" && newest != nil {
				reason = newest.Failure
			}
			if reason == "" && newest != nil {
				reason = newest.LastError
			}
			if reason == "" {
				reason = "no ready session"
			}
			v.Error = &reason
		}
		out = append(out, v)
	}
	return out, nil
}

func recorded(f *sessions.File) *sessions.Fingerprint {
	if f.Meta == nil {
		return nil
	}
	return f.Meta.Fingerprint
}

// Compare decides whether a ready base is stale: forks under the current configuration would
// not share its prompt cache (model, system prompt or tools differ from those recorded at warm
// time) or the branch it read has moved. current carries no commit; remoteHead is the branch
// head on the remote, or remoteErr when it could not be read (which alone never makes it stale).
func Compare(rec *sessions.Fingerprint, current sessions.Fingerprint, branch, remoteHead string, remoteErr error) (bool, string) {
	if rec == nil {
		return true, "no fingerprint recorded"
	}
	var reasons []string
	if rec.Model != current.Model {
		reasons = append(reasons, fmt.Sprintf("model changed %s -> %s", rec.Model, current.Model))
	}
	if rec.PromptSha256 != current.PromptSha256 {
		reasons = append(reasons, "system prompt changed")
	}
	if rec.Tools != current.Tools {
		reasons = append(reasons, fmt.Sprintf("tools changed %s -> %s", rec.Tools, current.Tools))
	}
	stale := len(reasons) > 0
	switch {
	case remoteErr != nil:
		reasons = append(reasons, "remote check failed: "+remoteErr.Error())
	case remoteHead != rec.Commit:
		stale = true
		reasons = append(reasons, fmt.Sprintf("branch %s moved %s -> %s", branch, short(rec.Commit), short(remoteHead)))
	}
	return stale, strings.Join(reasons, "; ")
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
