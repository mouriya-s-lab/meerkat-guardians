package bases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/checkout"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/sessions"
)

func TestCompare(t *testing.T) {
	rec := &sessions.Fingerprint{Model: "m1", PromptSha256: "p1", Tools: "read,pm_base_ready,pm_return_facts", OMPSettingsSha256: "s1", Commit: "1a2b3c4d5e"}
	same := sessions.Fingerprint{Model: "m1", PromptSha256: "p1", Tools: "read,pm_base_ready,pm_return_facts", OMPSettingsSha256: "s1"}
	cases := []struct {
		name       string
		rec        *sessions.Fingerprint
		current    sessions.Fingerprint
		head       string
		headErr    error
		wantStale  bool
		wantReason string
	}{
		{"unchanged", rec, same, "1a2b3c4d5e", nil, false, ""},
		{"branch moved", rec, same, "5d6e7f8a9b", nil, true, "branch main moved 1a2b3c4 -> 5d6e7f8"},
		{"tools changed and branch moved", rec,
			sessions.Fingerprint{Model: "m1", PromptSha256: "p1", Tools: "read,grep,pm_base_ready,pm_return_facts", OMPSettingsSha256: "s1"},
			"5d6e7f8a9b", nil, true,
			"tools changed read,pm_base_ready,pm_return_facts -> read,grep,pm_base_ready,pm_return_facts; branch main moved 1a2b3c4 -> 5d6e7f8"},
		{"prompt changed", rec, sessions.Fingerprint{Model: "m1", PromptSha256: "p2", Tools: same.Tools, OMPSettingsSha256: "s1"}, "1a2b3c4d5e", nil, true, "system prompt changed"},
		{"model changed", rec, sessions.Fingerprint{Model: "m2", PromptSha256: "p1", Tools: same.Tools, OMPSettingsSha256: "s1"}, "1a2b3c4d5e", nil, true, "model changed m1 -> m2"},
		{"omp settings changed", rec, sessions.Fingerprint{Model: "m1", PromptSha256: "p1", Tools: same.Tools, OMPSettingsSha256: "s2"}, "1a2b3c4d5e", nil, true, "omp settings changed"},
		{"prompt and omp settings changed, branch moved", rec, sessions.Fingerprint{Model: "m1", PromptSha256: "p2", Tools: same.Tools, OMPSettingsSha256: "s2"}, "5d6e7f8a9b", nil, true,
			"system prompt changed; omp settings changed; branch main moved 1a2b3c4 -> 5d6e7f8"},
		{"record without omp settings", &sessions.Fingerprint{Model: "m1", PromptSha256: "p1", Tools: same.Tools, Commit: "1a2b3c4d5e"}, same, "1a2b3c4d5e", nil, true, "omp settings changed"},
		{"remote check failed alone", rec, same, "", errors.New("exit status 128"), false, "remote check failed: exit status 128"},
		{"remote failed with prompt change", rec, sessions.Fingerprint{Model: "m1", PromptSha256: "p2", Tools: same.Tools, OMPSettingsSha256: "s1"}, "", errors.New("offline"), true,
			"system prompt changed; remote check failed: offline"},
		{"no fingerprint", nil, same, "1a2b3c4d5e", nil, true, "no fingerprint recorded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stale, reason := Compare(c.rec, c.current, "main", c.head, c.headErr)
			if stale != c.wantStale || reason != c.wantReason {
				t.Fatalf("got (%v, %q), want (%v, %q)", stale, reason, c.wantStale, c.wantReason)
			}
		})
	}
}

// TestOMPSettingsFingerprint follows a base through warm, restart, a settings change and a
// rewarm: the fingerprint a warm run records (currentFingerprint plus the commit, stored as JSON
// in the pm-meta entry) is compared with currentFingerprint over a temporary agent dir.
func TestOMPSettingsFingerprint(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "omp")
	prompt := filepath.Join(dir, "prompt.md")
	settings := filepath.Join(agentDir, "config.yml")
	writeFile(t, prompt, "you guard the repository\n")
	writeFile(t, settings, "defaultThinkingLevel: high\n")
	m := &Manager{cfg: &config.Config{Runtime: config.Runtime{OMP: config.OMP{AgentDir: agentDir}}}}
	b := config.Base{ID: "g.o__r.m", Guardian: "g", Repo: "o/r", Branch: "main", Model: "p/m"}
	g := config.Guardian{ID: "g", Prompt: prompt, Tools: []string{"read"}}
	const commit = "1a2b3c4d5e"

	// warm records what run() records: the current fingerprint plus the checked-out commit,
	// round-tripped through the session file's JSON.
	warm := func() *sessions.Fingerprint {
		t.Helper()
		fp, err := m.currentFingerprint(b, g)
		if err != nil {
			t.Fatal(err)
		}
		fp.Commit = commit
		return roundTrip(t, fp)
	}
	compare := func(rec *sessions.Fingerprint) (bool, string) {
		t.Helper()
		current, err := m.currentFingerprint(b, g)
		if err != nil {
			t.Fatal(err)
		}
		return Compare(rec, current, b.Branch, commit, nil)
	}

	rec := warm()
	if len(rec.OMPSettingsSha256) != 64 {
		t.Fatalf("recorded ompSettingsSha256 = %q", rec.OMPSettingsSha256)
	}
	if stale, reason := compare(rec); stale || reason != "" {
		t.Fatalf("same settings: got (%v, %q), want (false, \"\")", stale, reason)
	}

	writeFile(t, settings, "defaultThinkingLevel: medium\n")
	if stale, reason := compare(rec); !stale || !strings.Contains(reason, "omp settings changed") {
		t.Fatalf("changed settings: got (%v, %q), want stale with %q", stale, reason, "omp settings changed")
	}

	var old sessions.Fingerprint
	legacy := `{"model":"p/m","promptSha256":"` + rec.PromptSha256 + `","tools":"` + rec.Tools + `","commit":"` + commit + `"}`
	if err := json.Unmarshal([]byte(legacy), &old); err != nil {
		t.Fatal(err)
	}
	if stale, reason := compare(&old); !stale || !strings.Contains(reason, "omp settings changed") {
		t.Fatalf("record without the settings hash: got (%v, %q), want stale with %q", stale, reason, "omp settings changed")
	}

	rewarmed := warm()
	if rewarmed.OMPSettingsSha256 == rec.OMPSettingsSha256 {
		t.Fatalf("rewarm recorded the old settings hash %q", rec.OMPSettingsSha256)
	}
	if stale, reason := compare(rewarmed); stale || reason != "" {
		t.Fatalf("after rewarm: got (%v, %q), want (false, \"\")", stale, reason)
	}
}

func TestCurrentFingerprintWithoutOMPSettings(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt.md")
	writeFile(t, prompt, "you guard the repository\n")
	m := &Manager{cfg: &config.Config{Runtime: config.Runtime{OMP: config.OMP{AgentDir: filepath.Join(dir, "omp")}}}}
	_, err := m.currentFingerprint(config.Base{Model: "p/m"}, config.Guardian{Prompt: prompt})
	if !errors.Is(err, os.ErrNotExist) || !strings.HasPrefix(err.Error(), "omp settings: ") {
		t.Fatalf("err = %v, want omp settings: <not exist>", err)
	}
}

func roundTrip(t *testing.T, fp sessions.Fingerprint) *sessions.Fingerprint {
	t.Helper()
	data, err := json.Marshal(sessions.Meta{Kind: sessions.KindBase, Fingerprint: &fp})
	if err != nil {
		t.Fatal(err)
	}
	var meta sessions.Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	return meta.Fingerprint
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const testBase = "g.o__r.m"

// newTestManager is a Manager over one base whose session directory is under a temp root.
// The checkout directory does not exist, so the remote head check fails without touching the
// network, which alone never makes a base stale.
// The omp agent dir holds the config.yml that the fingerprint hashes, so List can read it.
func newTestManager(t *testing.T, root string) *Manager {
	t.Helper()
	prompt := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(root, "omp")
	writeFile(t, filepath.Join(agentDir, "config.yml"), "defaultThinkingLevel: high\n")
	cfg := &config.Config{
		Runtime:   config.Runtime{SessionsDir: filepath.Join(root, "sessions"), MaxConcurrentWarmups: 1, OMP: config.OMP{AgentDir: agentDir}},
		Guardians: []config.Guardian{{ID: "g", Prompt: prompt}},
		Bases:     []config.Base{{ID: testBase, Guardian: "g", Repo: "o/r", Branch: "main", Model: "m"}},
		Goals:     config.Goals{Base: config.Goal{Tools: []string{"pm_base_ready"}}},
	}
	return New(cfg, sessions.NewRegistry(config.LaunchLink{}), checkout.New(filepath.Join(root, "checkouts")))
}

// writeSession writes a base session file started at started; readyAt, when non-zero, is the
// time of its successful pm_base_ready result.
func writeSession(t *testing.T, m *Manager, id string, started, readyAt time.Time) string {
	t.Helper()
	dir := m.Dir(testBase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := func(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
	body := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":%q,"cwd":"/c"}`+"\n", id, ts(started)) +
		fmt.Sprintf(`{"type":"custom","customType":"pm-meta","data":{"kind":"base","base":%q,"model":"m"},"id":"x","timestamp":%q}`+"\n", testBase, ts(started.Add(time.Second)))
	if !readyAt.IsZero() {
		body += fmt.Sprintf(`{"type":"message","id":"y","timestamp":%q,"message":{"role":"toolResult","toolName":"pm_base_ready","isError":false}}`+"\n", ts(readyAt))
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeFailure writes the failure record Registry.Run leaves beside a failed session file.
func writeFailure(t *testing.T, sessionFile, reason string, at time.Time) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"reason": reason, "at": at})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessions.FailurePath(sessionFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func listOne(t *testing.T, m *Manager) View {
	t.Helper()
	views, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %+v", views)
	}
	return views[0]
}

func lastWarmJSON(t *testing.T, v View) string {
	t.Helper()
	b, err := json.Marshal(v.LastWarm)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	readyStarted = time.Date(2026, 11, 7, 15, 10, 0, 0, time.UTC)
	readyAt      = time.Date(2026, 11, 7, 15, 14, 20, 137e6, time.UTC)
)

// (a) Only a ready session: the last warm is done at warmedAt.
func TestLastWarmReadyOnly(t *testing.T) {
	m := newTestManager(t, t.TempDir())
	writeSession(t, m, "ready-1", readyStarted, readyAt)
	v := listOne(t, m)
	if v.State != "ready" || v.Session == nil || *v.Session != "ready-1" || v.WarmedAt == nil || v.Error != nil {
		t.Fatalf("view = %+v", v)
	}
	if v.LastWarm == nil || v.LastWarm.Outcome != WarmDone || !v.LastWarm.At.Equal(*v.WarmedAt) || v.LastWarm.Error != nil {
		t.Fatalf("lastWarm = %+v", v.LastWarm)
	}
	if got, want := lastWarmJSON(t, v), `{"at":"2026-11-07T15:14:20.137Z","outcome":"done","error":null}`; got != want {
		t.Fatalf("lastWarm json = %s, want %s", got, want)
	}
}

// (b) A rewarm that failed after the ready session: the base still serves the old session and
// the last warm is the failure, with the record's reason and time.
func TestLastWarmFailedRewarm(t *testing.T) {
	root := t.TempDir()
	m := newTestManager(t, root)
	writeSession(t, m, "ready-1", readyStarted, readyAt)
	failed := writeSession(t, m, "rewarm-1", time.Date(2026, 11, 7, 16, 46, 58, 0, time.UTC), time.Time{})
	writeFailure(t, failed, "session timeout after 300s", time.Date(2026, 11, 7, 16, 51, 58, 0, time.UTC))
	assertFailedRewarm(t, listOne(t, m))
}

func assertFailedRewarm(t *testing.T, v View) {
	t.Helper()
	if v.State != "ready" || v.Session == nil || *v.Session != "ready-1" ||
		v.WarmedAt == nil || !v.WarmedAt.Equal(readyAt) || v.Error != nil {
		t.Fatalf("view = %+v", v)
	}
	failedAt := time.Date(2026, 11, 7, 16, 51, 58, 0, time.UTC)
	if v.LastWarm == nil || v.LastWarm.Outcome != WarmFailed || !v.LastWarm.At.Equal(failedAt) ||
		v.LastWarm.Error == nil || *v.LastWarm.Error != "session timeout after 300s" {
		t.Fatalf("lastWarm = %+v", v.LastWarm)
	}
	if got, want := lastWarmJSON(t, v), `{"at":"2026-11-07T16:51:58Z","outcome":"failed","error":"session timeout after 300s"}`; got != want {
		t.Fatalf("lastWarm json = %s, want %s", got, want)
	}
}

// (c) A failure recorded before the ready session: the last warm is the ready one.
func TestLastWarmFailureBeforeReady(t *testing.T) {
	m := newTestManager(t, t.TempDir())
	failed := writeSession(t, m, "warm-0", time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC), time.Time{})
	writeFailure(t, failed, "omp exited", time.Date(2026, 11, 7, 14, 5, 0, 0, time.UTC))
	writeSession(t, m, "ready-1", readyStarted, readyAt)
	v := listOne(t, m)
	if v.State != "ready" || v.Session == nil || *v.Session != "ready-1" || v.WarmedAt == nil || v.Error != nil {
		t.Fatalf("view = %+v", v)
	}
	if v.LastWarm == nil || v.LastWarm.Outcome != WarmDone || !v.LastWarm.At.Equal(*v.WarmedAt) || v.LastWarm.Error != nil {
		t.Fatalf("lastWarm = %+v", v.LastWarm)
	}
}

// (d) A new Manager over the same directory (a restarted program) reports the failed rewarm
// from the record on disk.
func TestLastWarmFailedRewarmAfterRestart(t *testing.T) {
	root := t.TempDir()
	m := newTestManager(t, root)
	writeSession(t, m, "ready-1", readyStarted, readyAt)
	failed := writeSession(t, m, "rewarm-1", time.Date(2026, 11, 7, 16, 46, 58, 0, time.UTC), time.Time{})
	writeFailure(t, failed, "session timeout after 300s", time.Date(2026, 11, 7, 16, 51, 58, 0, time.UTC))
	assertFailedRewarm(t, listOne(t, m))
	assertFailedRewarm(t, listOne(t, newTestManager(t, root)))
}
