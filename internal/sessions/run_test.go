package sessions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mouriya-s-lab/meerkat-guardians/internal/config"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/fakeomp"
)

// The test binary doubles as the omp child Run starts: with FAKE_OMP_PLAN set it writes the
// PM_LAUNCH_LINK it received to RUN_TEST_LAUNCH_LINK_OUT and then serves fakeomp on stdio.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_OMP_PLAN") != "" {
		if out := os.Getenv("RUN_TEST_LAUNCH_LINK_OUT"); out != "" {
			if err := os.WriteFile(out, []byte(os.Getenv("PM_LAUNCH_LINK")), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		os.Exit(fakeomp.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestRunHandsLaunchLinkToChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "launch-link.json")
	link := config.LaunchLink{Base: "http://127.0.0.1:47599", Frontend: config.Frontend{Kind: "terminal"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = NewRegistry(link).Run(ctx, Spec{
		OMP:                config.OMP{Bin: exe, ReadyTimeoutSeconds: 20, ExitGraceSeconds: 5},
		Model:              "m",
		SessionDir:         t.TempDir(),
		Cwd:                t.TempDir(),
		GoalTools:          []string{"pm_base_ready"},
		Opening:            "open",
		Continue:           "keep going",
		Meta:               Meta{Kind: KindBase, Base: "b"},
		Env:                []string{"FAKE_OMP_PLAN=goal", "RUN_TEST_LAUNCH_LINK_OUT=" + out},
		MaxContinuePrompts: 1,
		MaxDuration:        20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"base":"http://127.0.0.1:47599","frontend":{"kind":"terminal"}}`; string(got) != want {
		t.Fatalf("PM_LAUNCH_LINK = %s, want %s", got, want)
	}
}
