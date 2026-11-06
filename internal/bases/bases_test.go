package bases

import (
	"errors"
	"testing"

	"github.com/oh-my-omp/pm-guardians/internal/sessions"
)

func TestCompare(t *testing.T) {
	rec := &sessions.Fingerprint{Model: "m1", PromptSha256: "p1", Tools: "read,pm_base_ready,pm_return_facts", Commit: "1a2b3c4d5e"}
	same := sessions.Fingerprint{Model: "m1", PromptSha256: "p1", Tools: "read,pm_base_ready,pm_return_facts"}
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
			sessions.Fingerprint{Model: "m1", PromptSha256: "p1", Tools: "read,grep,pm_base_ready,pm_return_facts"},
			"5d6e7f8a9b", nil, true,
			"tools changed read,pm_base_ready,pm_return_facts -> read,grep,pm_base_ready,pm_return_facts; branch main moved 1a2b3c4 -> 5d6e7f8"},
		{"prompt changed", rec, sessions.Fingerprint{Model: "m1", PromptSha256: "p2", Tools: same.Tools}, "1a2b3c4d5e", nil, true, "system prompt changed"},
		{"model changed", rec, sessions.Fingerprint{Model: "m2", PromptSha256: "p1", Tools: same.Tools}, "1a2b3c4d5e", nil, true, "model changed m1 -> m2"},
		{"remote check failed alone", rec, same, "", errors.New("exit status 128"), false, "remote check failed: exit status 128"},
		{"remote failed with prompt change", rec, sessions.Fingerprint{Model: "m1", PromptSha256: "p2", Tools: same.Tools}, "", errors.New("offline"), true,
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
