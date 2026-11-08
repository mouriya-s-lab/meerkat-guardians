package asks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mouriya-s-lab/meerkat-guardians/internal/bases"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/checkout"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/config"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/sessions"
)

const header = `{"type":"session","version":3,"id":"%s","timestamp":"2026-11-07T01:00:00.000Z","cwd":"/c"}`

func writeSession(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for _, l := range lines {
		b = append(b, l+"\n"...)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func toolResult(name string) string {
	return `{"type":"message","id":"m","timestamp":"2026-11-07T01:00:05.000Z","message":{"role":"toolResult","toolName":"` + name + `","isError":false}}`
}

// fixture: o/code routes simple -> m1 (ready base) and complex -> m2 (no ready base);
// o/unrouted is a code repository without a route. Query session files: ask 77 done, ask 79 failed.
func fixture(t *testing.T) *Runner {
	t.Helper()
	dir := t.TempDir()
	ready := config.BaseID("g", "o/code", "m1")
	cfg := &config.Config{
		Runtime:   config.Runtime{SessionsDir: dir},
		Guardians: []config.Guardian{{ID: "g", Prompt: "/p", Tools: []string{"read"}}},
		Bases: []config.Base{
			{ID: ready, Guardian: "g", Repo: "o/code", Model: "m1"},
			{ID: config.BaseID("g", "o/code", "m2"), Guardian: "g", Repo: "o/code", Model: "m2"},
		},
		Routes: []config.Route{{Repo: "o/code", Guardian: "g", Tiers: config.Tiers{Simple: "m1", Complex: "m2"}}},
		Goals: config.Goals{
			Base:  config.Goal{Tools: []string{"meerkat_base_ready"}},
			Query: config.Goal{Tools: []string{"meerkat_reply", "meerkat_replace", "meerkat_escalate"}},
		},
		Repos: config.Repos{Code: []config.CodeRepo{{Repo: "o/code", Cwd: "/w/code"}, {Repo: "o/unrouted", Cwd: "/w/u"}}},
	}
	writeSession(t, filepath.Join(dir, "bases", ready, "b.jsonl"),
		fmt.Sprintf(header, "base-1"), toolResult("meerkat_base_ready"))
	meta := func(ask string) string {
		return `{"type":"custom","customType":"meerkat-meta","data":{"kind":"query","ref":"o/code#1","ask":` + ask + `,"base":"` + ready + `","model":"m1","tier":"simple"},"id":"x","timestamp":"2026-11-07T01:00:01.000Z"}`
	}
	writeSession(t, filepath.Join(dir, "query", ready, "q77.jsonl"), fmt.Sprintf(header, "q-77"), meta("77"), toolResult("meerkat_replace"))
	writeSession(t, filepath.Join(dir, "query", ready, "q79.jsonl"), fmt.Sprintf(header, "q-79"), meta("79"), toolResult("read"))
	reg := sessions.NewRegistry(cfg.LaunchLink)
	co := checkout.New(filepath.Join(dir, "checkouts"))
	r, err := New(cfg, reg, bases.New(cfg, reg, co), co, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSubmitValidation(t *testing.T) {
	r := fixture(t)
	valid := Request{Issue: "o/code#1", Comment: 77, Question: "Q?", Tier: "simple"}
	with := func(f func(*Request)) Request {
		req := valid
		f(&req)
		return req
	}
	cases := []struct {
		name   string
		req    Request
		reason Rejection // checked when rejected
		reject bool
		out    Outcome
	}{
		{"missing issue", with(func(q *Request) { q.Issue = "" }), Invalid, true, 0},
		{"issue without number", with(func(q *Request) { q.Issue = "o/code" }), Invalid, true, 0},
		{"issue number zero", with(func(q *Request) { q.Issue = "o/code#0" }), Invalid, true, 0},
		{"missing comment", with(func(q *Request) { q.Comment = 0 }), Invalid, true, 0},
		{"missing question", with(func(q *Request) { q.Question = " " }), Invalid, true, 0},
		{"missing tier", with(func(q *Request) { q.Tier = "" }), Invalid, true, 0},
		{"unknown tier", with(func(q *Request) { q.Tier = "urgent" }), Invalid, true, 0},
		{"repo not in repos.yaml", with(func(q *Request) { q.Issue = "mouriya-s-lab/nope#1" }), UnknownRepo, true, 0},
		{"code repo without route", with(func(q *Request) { q.Issue = "o/unrouted#1" }), UnknownRepo, true, 0},
		{"tier model without ready base", with(func(q *Request) { q.Tier = "complex" }), BaseNotReady, true, 0},
		{"done session for comment", valid, 0, false, Exists},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := r.Submit(context.Background(), c.req)
			var rejected *RejectedError
			switch {
			case c.reject:
				if !errors.As(err, &rejected) || rejected.Reason != c.reason {
					t.Fatalf("want rejection %d, got %v", c.reason, err)
				}
			case err != nil:
				t.Fatal(err)
			case out != c.out:
				t.Fatalf("outcome = %d, want %d", out, c.out)
			}
		})
	}
}

func TestClaim(t *testing.T) {
	r := fixture(t)
	// A failed session file (no goal tool, not live) does not hold the ask.
	if ok, err := r.claim(79); err != nil || !ok {
		t.Fatalf("claim(79) = %v, %v", ok, err)
	}
	// Claimed in memory: a second submission finds it active.
	if ok, err := r.claim(79); err != nil || ok {
		t.Fatalf("second claim(79) = %v, %v", ok, err)
	}
	r.release(79)
	if ok, err := r.claim(79); err != nil || !ok {
		t.Fatalf("claim(79) after release = %v, %v", ok, err)
	}
	if ok, err := r.claim(77); err != nil || ok {
		t.Fatalf("claim(77) on a done session = %v, %v", ok, err)
	}
}
