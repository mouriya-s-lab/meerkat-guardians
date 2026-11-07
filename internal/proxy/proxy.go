// Package proxy runs proxy reads: a fork of the ready base for (guardian, repo, model) that
// answers one question with facts.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/oh-my-omp/pm-guardians/internal/bases"
	"github.com/oh-my-omp/pm-guardians/internal/checkout"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/sessions"
)

// Request is the body of POST /v1/proxy-reads.
type Request struct {
	Seat     string `json:"seat"`
	Model    string `json:"model"`
	Repo     string `json:"repo"`
	Question string `json:"question"`
	Agenda   string `json:"agenda"`
}

// Result is the success body of POST /v1/proxy-reads.
type Result struct {
	Session string `json:"session"`
	Facts   string `json:"facts"`
}

// Rejection is why a proxy read was refused before any session started.
type Rejection int

const (
	Invalid Rejection = iota
	NoGuardian
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

// SessionFailed is a proxy session that ended without returning facts.
type SessionFailed struct{ Reason string }

func (e *SessionFailed) Error() string { return e.Reason }

type Runner struct {
	cfg   *config.Config
	reg   *sessions.Registry
	bases *bases.Manager
	co    *checkout.Manager
}

func New(cfg *config.Config, reg *sessions.Registry, b *bases.Manager, co *checkout.Manager) *Runner {
	return &Runner{cfg: cfg, reg: reg, bases: b, co: co}
}

// Read validates req, forks the ready base and waits for the facts.
func (r *Runner) Read(ctx context.Context, req Request) (Result, error) {
	for _, f := range []struct{ name, value string }{
		{"seat", req.Seat}, {"model", req.Model}, {"repo", req.Repo}, {"question", req.Question}, {"agenda", req.Agenda},
	} {
		if f.value == "" {
			return Result{}, reject(Invalid, "%s is required", f.name)
		}
	}
	route, ok := r.cfg.Route(req.Repo)
	if !ok {
		return Result{}, reject(NoGuardian, "routing.yaml has no guardian for %s", req.Repo)
	}
	baseID := config.BaseID(route.Guardian, req.Repo, req.Model)
	base, ok := r.cfg.Base(baseID)
	if !ok {
		return Result{}, reject(BaseNotReady, "guardian %s has no base for %s with model %s", route.Guardian, req.Repo, req.Model)
	}
	source, err := r.bases.ForkSource(baseID)
	if err != nil {
		return Result{}, err
	}
	if source == nil {
		return Result{}, reject(BaseNotReady, "base %s is not ready", baseID)
	}
	g, _ := r.cfg.Guardian(base.Guardian)
	opening, cont, err := r.cfg.Goals.Proxy.Render(config.ProxyVars{Seat: req.Seat, Agenda: req.Agenda, Repo: req.Repo, Question: req.Question})
	if err != nil {
		return Result{}, err
	}
	rt := r.cfg.Runtime
	res, err := r.reg.Run(ctx, sessions.Spec{
		OMP:   rt.OMP,
		Model: base.Model, SystemPrompt: g.Prompt, Tools: g.ToolsArg(),
		SessionDir: filepath.Join(rt.SessionsDir, "proxy", baseID), Cwd: r.co.Dir(base.Repo),
		ForkFrom:  source.Path,
		GoalTools: r.cfg.Goals.Proxy.Tools, Opening: opening, Continue: cont,
		Meta:               sessions.Meta{Kind: sessions.KindProxy, Ref: req.Agenda, Base: baseID, Model: base.Model, Seat: req.Seat},
		MaxContinuePrompts: rt.MaxContinuePrompts,
		MaxDuration:        rt.SessionTimeout(),
	})
	if err != nil {
		var f *sessions.Failure
		if errors.As(err, &f) {
			return Result{}, &SessionFailed{Reason: f.Reason}
		}
		return Result{}, err
	}
	var args struct {
		Facts string `json:"facts"`
	}
	if err := json.Unmarshal(res.Args, &args); err != nil {
		return Result{}, &SessionFailed{Reason: fmt.Sprintf("%s arguments: %v", strings.Join(r.cfg.Goals.Proxy.Tools, "/"), err)}
	}
	return Result{Session: res.SessionID, Facts: args.Facts}, nil
}
