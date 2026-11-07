package bases

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
