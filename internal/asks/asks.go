// Package asks accepts decision asks (contract §3 POST /v1/asks) and runs one query session per
// ask: a fork of the ready base for (route guardian, repo, tier model) that settles the ask on
// GitHub through one of the plugin's tools. It also rebuilds unsettled asks from GitHub at
// startup (contract §1).
package asks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/oh-my-omp/pm-guardians/internal/bases"
	"github.com/oh-my-omp/pm-guardians/internal/checkout"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/github"
	"github.com/oh-my-omp/pm-guardians/internal/markers"
	"github.com/oh-my-omp/pm-guardians/internal/sessions"
)

// Request is the body of POST /v1/asks.
type Request struct {
	Issue    string `json:"issue"`
	Comment  int64  `json:"comment"`
	Question string `json:"question"`
	Tier     string `json:"tier"`
}

// Rejection is why an ask was refused before any session started.
type Rejection int

const (
	Invalid Rejection = iota
	UnknownRepo
	BaseNotReady
)

// RejectedError refuses a request; the API maps Reason to its status and code.
type RejectedError struct {
	Reason  Rejection
	Message string
}

func (e *RejectedError) Error() string { return e.Message }

func reject(reason Rejection, format string, args ...any) error {
	return &RejectedError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Outcome is what Submit did with an accepted ask.
type Outcome int

const (
	Started Outcome = iota // a query session was started
	Exists                 // a query session for this ask is running or done; nothing started
)

// ask is a validated Request.
type ask struct {
	Ref      string // owner/name#n
	Repo     string
	Number   int
	Comment  int64
	Question string
	Tier     config.Tier
}

var refPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)

func parse(req Request) (ask, error) {
	m := refPattern.FindStringSubmatch(req.Issue)
	if m == nil {
		return ask{}, reject(Invalid, "issue must be owner/name#n, got %q", req.Issue)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return ask{}, reject(Invalid, "issue number %q: %v", m[2], err)
	}
	if req.Comment <= 0 {
		return ask{}, reject(Invalid, "comment must be a positive ask comment id")
	}
	if strings.TrimSpace(req.Question) == "" {
		return ask{}, reject(Invalid, "question is required")
	}
	tier, ok := config.ParseTier(req.Tier)
	if !ok {
		return ask{}, reject(Invalid, "tier must be simple or complex, got %q", req.Tier)
	}
	return ask{Ref: req.Issue, Repo: m[1], Number: n, Comment: req.Comment, Question: req.Question, Tier: tier}, nil
}

// Runner starts query sessions. active holds, for this process lifetime, the asks whose session
// this process is starting or running; everything else about a session comes from its file.
type Runner struct {
	cfg       *config.Config
	reg       *sessions.Registry
	bases     *bases.Manager
	co        *checkout.Manager
	gh        *github.Client
	githubEnv string // PM_GITHUB=<json>

	mu     sync.Mutex
	active map[int64]bool
}

// githubEnv is the PM_GITHUB value the plugin's GitHub tools read.
type githubEnv struct {
	PMRepo  string               `json:"pmRepo"`
	Project config.Project       `json:"project"`
	Fields  config.ProjectFields `json:"fields"`
	Code    []config.CodeRepo    `json:"code"`
}

func New(cfg *config.Config, reg *sessions.Registry, b *bases.Manager, co *checkout.Manager, gh *github.Client) (*Runner, error) {
	env, err := json.Marshal(githubEnv{
		PMRepo: cfg.Repos.PM.Repo, Project: cfg.Repos.PM.Project, Fields: cfg.Project.Fields, Code: cfg.Repos.Code,
	})
	if err != nil {
		return nil, err
	}
	return &Runner{
		cfg: cfg, reg: reg, bases: b, co: co, gh: gh,
		githubEnv: "PM_GITHUB=" + string(env),
		active:    map[int64]bool{},
	}, nil
}

// Dir is the root of the query session directories, <sessionsDir>/query.
func (r *Runner) Dir() string { return filepath.Join(r.cfg.Runtime.SessionsDir, "query") }

// Submit validates req and starts a query session for it unless one is running or done.
func (r *Runner) Submit(ctx context.Context, req Request) (Outcome, error) {
	a, err := parse(req)
	if err != nil {
		return 0, err
	}
	if _, ok := r.cfg.Repos.CodeRepo(a.Repo); !ok {
		return 0, reject(UnknownRepo, "%s is not a code repository in shared/repos.yaml", a.Repo)
	}
	route, ok := r.cfg.Route(a.Repo)
	if !ok {
		return 0, reject(UnknownRepo, "routing.yaml has no guardian for %s", a.Repo)
	}
	model := route.Tiers.Model(a.Tier)
	baseID := config.BaseID(route.Guardian, a.Repo, model)
	base, ok := r.cfg.Base(baseID)
	if !ok {
		return 0, reject(BaseNotReady, "guardian %s has no base for %s with model %s", route.Guardian, a.Repo, model)
	}
	source, err := r.bases.ForkSource(baseID)
	if err != nil {
		return 0, err
	}
	if source == nil {
		return 0, reject(BaseNotReady, "base %s is not ready", baseID)
	}

	if claimed, err := r.claim(a.Comment); err != nil || !claimed {
		return Exists, err
	}
	spec, err := r.spec(ctx, a, base, source)
	if err != nil {
		r.release(a.Comment)
		return 0, err
	}
	go func() {
		defer r.release(a.Comment)
		res, err := r.reg.Run(context.Background(), spec)
		if err != nil {
			slog.Error("query session failed", "ask", a.Comment, "issue", a.Ref, "err", err)
			return
		}
		slog.Info("query session done", "ask", a.Comment, "issue", a.Ref, "session", res.SessionID)
	}()
	return Started, nil
}

// claim marks ask comment active unless a query session for it is active in this process, or a
// query session file for it is running or done.
func (r *Runner) claim(comment int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[comment] {
		return false, nil
	}
	files, err := sessions.Matching(r.Dir(), func(m *sessions.Meta) bool {
		return m.Kind == sessions.KindQuery && m.Ask == comment
	})
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if _, done := f.GoalAt(r.cfg.Goals.Query.Tools); done || r.reg.Live(f.ID) {
			return false, nil
		}
	}
	r.active[comment] = true
	return true, nil
}

func (r *Runner) release(comment int64) {
	r.mu.Lock()
	delete(r.active, comment)
	r.mu.Unlock()
}

func (r *Runner) spec(ctx context.Context, a ask, base config.Base, source *sessions.File) (sessions.Spec, error) {
	issue, err := r.gh.Issue(ctx, a.Repo, a.Number)
	if err != nil {
		return sessions.Spec{}, fmt.Errorf("read %s: %w", a.Ref, err)
	}
	opening, cont, err := r.cfg.Goals.Query.Render(config.QueryVars{
		Issue: a.Ref, IssueTitle: issue.Title, IssueBody: issue.Body, Question: a.Question, Tier: a.Tier,
	})
	if err != nil {
		return sessions.Spec{}, err
	}
	var fp *sessions.Fingerprint
	if source.Meta != nil {
		fp = source.Meta.Fingerprint
	}
	g, _ := r.cfg.Guardian(base.Guardian)
	rt := r.cfg.Runtime
	return sessions.Spec{
		OMP:   rt.OMP,
		Model: base.Model, SystemPrompt: g.Prompt, Tools: g.ToolsArg(),
		SessionDir: filepath.Join(r.Dir(), base.ID), Cwd: r.co.Dir(base.Repo),
		ForkFrom:  source.Path,
		GoalTools: r.cfg.Goals.Query.Tools, Opening: opening, Continue: cont,
		Meta: sessions.Meta{
			Kind: sessions.KindQuery, Ref: a.Ref, Ask: a.Comment, Base: base.ID, Model: base.Model,
			Tier: a.Tier, Fingerprint: fp,
		},
		Env:                []string{r.githubEnv},
		MaxContinuePrompts: rt.MaxContinuePrompts,
		MaxDuration:        rt.SessionTimeout(),
	}, nil
}

// Recover starts, as POST /v1/asks would, every ask on an open issue of a code repository that
// no reply, replaced or escalated marker settles and that has no running or done query session.
// Asks it cannot start now (no ready base, a GitHub error) are logged and left for the next start
// or the next POST.
func (r *Runner) Recover(ctx context.Context) {
	for _, cr := range r.cfg.Repos.Code {
		issues, err := r.gh.OpenIssues(ctx, cr.Repo)
		if err != nil {
			slog.Error("recover asks: list issues", "repo", cr.Repo, "err", err)
			continue
		}
		for _, is := range issues {
			ref := fmt.Sprintf("%s#%d", cr.Repo, is.Number)
			comments, err := r.gh.Comments(ctx, cr.Repo, is.Number)
			if err != nil {
				slog.Error("recover asks: list comments", "issue", ref, "err", err)
				continue
			}
			list := make([]markers.Comment, len(comments))
			for i, c := range comments {
				list[i] = markers.Comment{ID: c.ID, Body: c.Body}
			}
			pending, invalid := markers.Pending(list)
			for _, inv := range invalid {
				slog.Error("invalid marker ignored", "issue", ref, "comment", inv.Comment, "err", inv.Err)
			}
			for _, p := range pending {
				out, err := r.Submit(ctx, Request{Issue: ref, Comment: p.Comment, Question: p.Question, Tier: string(p.Tier)})
				var rejected *RejectedError
				switch {
				case errors.As(err, &rejected):
					slog.Warn("recover asks: skipped", "issue", ref, "ask", p.Comment, "reason", rejected.Message)
				case err != nil:
					slog.Error("recover asks: start failed", "issue", ref, "ask", p.Comment, "err", err)
				case out == Started:
					slog.Info("recover asks: started", "issue", ref, "ask", p.Comment)
				case out == Exists:
					slog.Info("recover asks: session running or done", "issue", ref, "ask", p.Comment)
				}
			}
		}
	}
}
