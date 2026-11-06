package sessions

import (
	"path/filepath"
	"testing"
	"time"
)

// testdata/base.jsonl and testdata/proxy.jsonl are real omp 18.17 session files written by a
// base warm and by a proxy read forked from a base, with repository and model names replaced.

func TestParseBaseFile(t *testing.T) {
	f, err := ParseFile("testdata/base.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "01a1b2c3-ebac-7278-9d2c-cc847f67771e" || f.ParentSession != "" {
		t.Fatalf("header = %q parent %q", f.ID, f.ParentSession)
	}
	if want := time.Date(2026, 11, 6, 21, 29, 6, 732e6, time.UTC); !f.StartedAt.Equal(want) {
		t.Fatalf("startedAt = %v", f.StartedAt)
	}
	ready, ok := f.ToolOK["pm_base_ready"]
	if !ok || ready.Before(f.StartedAt) || f.LastAt.Before(ready) {
		t.Fatalf("pm_base_ready = %v %v (last %v)", ready, ok, f.LastAt)
	}
	m := f.Meta
	if m == nil || m.Kind != KindBase || m.Fingerprint == nil || m.Fingerprint.Commit == "" ||
		m.Fingerprint.Tools != "read,grep,glob,find,pm_base_ready,pm_return_facts" || len(m.Fingerprint.PromptSha256) != 64 {
		t.Fatalf("meta = %+v", m)
	}
}

func TestParseForkFile(t *testing.T) {
	f, err := ParseFile("testdata/proxy.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if f.ParentSession != "01a1b2c3-ebdd-7074-8ab9-ed037cac3607" {
		t.Fatalf("parent = %q", f.ParentSession)
	}
	// The latest pm-meta is the fork's own, after the copied base entry.
	if f.Meta == nil || f.Meta.Kind != KindProxy || f.Meta.Seat != "ClaudeHigh" || f.Meta.Ref == "" {
		t.Fatalf("meta = %+v", f.Meta)
	}
	if _, ok := f.ToolOK["pm_return_facts"]; !ok {
		t.Fatal("pm_return_facts result missing")
	}
	// The copied base history predates the fork header and does not make the fork a ready base.
	if _, ok := f.ToolOK["pm_base_ready"]; ok {
		t.Fatal("copied pm_base_ready result counted")
	}
}

func TestListDerivesState(t *testing.T) {
	dir, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.failures["01a1b2c3-ebac-7278-9d2c-cc847f67771e"] = "omp exited"
	list, err := reg.List(dir, map[Kind][]string{KindBase: {"pm_base_ready"}, KindProxy: {"pm_return_facts"}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list) // other.jsonl is not a session file
	}
	proxy, base := list[0], list[1] // newest first
	if proxy.Kind != KindProxy || proxy.State != "done" || proxy.Ref == nil || proxy.Label != "proxy "+*proxy.Ref || proxy.EndedAt == nil {
		t.Fatalf("proxy = %+v", proxy)
	}
	if base.Kind != KindBase || base.State != "done" || base.Ref != nil || base.Error == nil || *base.Error != "omp exited" {
		t.Fatalf("base = %+v", base)
	}

	limited, err := reg.List(dir, map[Kind][]string{KindBase: {"pm_base_ready"}, KindProxy: {"pm_return_facts"}}, 1)
	if err != nil || len(limited) != 1 || limited[0].ID != proxy.ID {
		t.Fatalf("limited = %+v %v", limited, err)
	}

	path, ok, err := FindFile(dir, proxy.ID)
	if err != nil || !ok || filepath.Base(path) != "proxy.jsonl" {
		t.Fatalf("FindFile = %q %v %v", path, ok, err)
	}
}

// fixtures/parent holds a real base session file and a proxy read forked from it, unchanged.
func TestListParent(t *testing.T) {
	dir, err := filepath.Abs("fixtures/parent")
	if err != nil {
		t.Fatal(err)
	}
	list, err := NewRegistry().List(dir, map[Kind][]string{KindBase: {"pm_base_ready"}, KindProxy: {"pm_return_facts"}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	fork, base := list[0], list[1] // newest first
	if base.Kind != KindBase || base.Parent != nil {
		t.Fatalf("base = %+v", base)
	}
	if fork.Kind != KindProxy || fork.Parent == nil || *fork.Parent != base.ID {
		t.Fatalf("fork = %+v, want parent %q", fork, base.ID)
	}
}
