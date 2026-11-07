package sessions

import (
	"os"
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

// The failure reason and end time come from the record beside the session file, so a fresh
// Registry (a restarted program) still reports them.
func TestListDerivesState(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata")); err != nil {
		t.Fatal(err)
	}
	// A record written before records carried "at": the end time falls back to the last entry.
	if err := os.WriteFile(FailurePath(filepath.Join(dir, "base.jsonl")), []byte(`{"reason":"omp exited"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A proxy read that timed out: header and pm-meta, no goal tool result, nothing after.
	timedOut := filepath.Join(dir, "timedout.jsonl")
	if err := os.WriteFile(timedOut, []byte(
		`{"type":"session","version":3,"id":"t-1","timestamp":"2026-11-06T20:00:00.000Z","cwd":"/c"}`+"\n"+
			`{"type":"custom","customType":"pm-meta","data":{"kind":"proxy","ref":"o/a#1","base":"b","model":"m"},"id":"x","timestamp":"2026-11-06T20:00:01.000Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	killedAt := time.Date(2026, 11, 6, 20, 0, 20, 5e6, time.UTC)
	if err := writeFailure(timedOut, failureRecord{Reason: "session timeout after 20s", At: killedAt}); err != nil {
		t.Fatal(err)
	}
	goals := map[Kind][]string{KindBase: {"pm_base_ready"}, KindProxy: {"pm_return_facts"}}
	list, err := NewRegistry().List(dir, goals, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %+v", list) // other.jsonl is not a session file
	}
	lastAt := func(name string) time.Time {
		f, err := ParseFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return f.LastAt
	}
	proxy, base, failed := list[0], list[1], list[2] // newest first
	if proxy.Kind != KindProxy || proxy.State != "done" || proxy.Ref == nil || proxy.Label != "proxy "+*proxy.Ref ||
		proxy.EndedAt == nil || !proxy.EndedAt.Equal(lastAt("proxy.jsonl")) {
		t.Fatalf("proxy = %+v", proxy)
	}
	if base.Kind != KindBase || base.State != "done" || base.Ref != nil || base.Error == nil || *base.Error != "omp exited" ||
		base.EndedAt == nil || !base.EndedAt.Equal(lastAt("base.jsonl")) {
		t.Fatalf("base = %+v", base)
	}
	if failed.Kind != KindProxy || failed.State != "failed" || failed.Error == nil || *failed.Error != "session timeout after 20s" ||
		failed.EndedAt == nil || !failed.EndedAt.Equal(killedAt) {
		t.Fatalf("timed-out proxy = %+v", failed)
	}
	if b, err := os.ReadFile(FailurePath(timedOut)); err != nil || string(b) != `{"reason":"session timeout after 20s","at":"2026-11-06T20:00:20.005Z"}` {
		t.Fatalf("failure record = %s, %v", b, err)
	}

	limited, err := NewRegistry().List(dir, goals, 1)
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

// A failure record whose session file omp never wrote lists as its own row (§I): kind,
// model, base label and startedAt come from what Run knew at launch, state is failed,
// error is the reason, endedAt is the decision time.
func TestListOrphanFailure(t *testing.T) {
	dir := t.TempDir()
	killedAt := time.Date(2026, 11, 7, 14, 0, 20, 0, time.UTC)
	launchedAt := time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)
	sessionFile := filepath.Join(dir, "bases", "g", "r", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFailure(sessionFile, failureRecord{
		Reason: "session timeout after 20s", At: killedAt,
		ID: "orphan-1", Kind: KindBase, Base: "g/r", Model: "m", StartedAt: launchedAt,
	}); err != nil {
		t.Fatal(err)
	}
	goals := map[Kind][]string{KindBase: {"pm_base_ready"}}
	list, err := NewRegistry().List(dir, goals, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	row := list[0]
	if row.ID != "orphan-1" || row.Kind != KindBase || row.Model != "m" || row.Label != "base g/r" ||
		row.State != "failed" || row.Parent != nil || row.Ref != nil {
		t.Fatalf("orphan = %+v", row)
	}
	if row.Error == nil || *row.Error != "session timeout after 20s" {
		t.Fatalf("orphan error = %+v", row.Error)
	}
	if row.StartedAt.IsZero() || !row.StartedAt.Equal(launchedAt) ||
		row.EndedAt == nil || !row.EndedAt.Equal(killedAt) || !row.EndedAt.After(row.StartedAt) {
		t.Fatalf("orphan times = %+v", row)
	}
	if _, ok, err := FindFile(dir, "orphan-1"); err != nil || ok {
		t.Fatalf("FindFile orphan = %v %v", ok, err)
	}
}

// When the session file appears after all, the file is authoritative: the same id lists
// once, with the file's last entry as the end time and no orphan row beside it (§I).
func TestListOrphanSupersededByFile(t *testing.T) {
	dir := t.TempDir()
	killedAt := time.Date(2026, 11, 7, 14, 0, 20, 0, time.UTC)
	launchedAt := time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)
	sessionFile := filepath.Join(dir, "timedout.jsonl")
	if err := os.WriteFile(sessionFile, []byte(
		`{"type":"session","version":3,"id":"t-1","timestamp":"2026-11-06T20:00:00.000Z","cwd":"/c"}`+"\n"+
			`{"type":"custom","customType":"pm-meta","data":{"kind":"proxy","ref":"o/a#1","base":"b","model":"m"},"id":"x","timestamp":"2026-11-06T20:00:01.000Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFailure(sessionFile, failureRecord{
		Reason: "session timeout after 20s", At: killedAt,
		ID: "t-1", Kind: KindProxy, Base: "b", Ref: "o/a#1", Model: "m", StartedAt: launchedAt,
	}); err != nil {
		t.Fatal(err)
	}
	goals := map[Kind][]string{KindProxy: {"pm_return_facts"}}
	files, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	list, err := NewRegistry().List(dir, goals, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "t-1" || list[0].State != "failed" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].StartedAt.IsZero() || !list[0].StartedAt.Equal(time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("file start wins = %+v", list[0])
	}
	if list[0].EndedAt == nil || !list[0].EndedAt.Equal(killedAt) {
		t.Fatalf("failure end wins = %+v", list[0])
	}
	path, ok, err := FindFile(dir, "t-1")
	if err != nil || !ok || path != sessionFile {
		t.Fatalf("FindFile = %q %v %v", path, ok, err)
	}
}

// Records predating the identity fields (reason and at only, e.g. TestListDerivesState's
// base record) cannot name a session on their own, so they yield no extra row; the reason
// still annotates the matching session file (§4 failure text survives).
func TestListOldFormatRecord(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "missing.jsonl")
	parent := filepath.Dir(sessionFile)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(FailurePath(sessionFile), []byte(`{"reason":"omp exited","at":"2026-11-06T21:30:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("old-format record without a file must not list, got %+v", files)
	}
	list, err := NewRegistry().List(dir, map[Kind][]string{KindBase: {"pm_base_ready"}}, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("list = %+v %v", list, err)
	}
	if _, ok, err := FindFile(dir, "missing"); err != nil || ok {
		t.Fatalf("FindFile old-format = %v %v", ok, err)
	}
}
