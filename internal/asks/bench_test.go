package asks

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/bases"
	"github.com/oh-my-omp/pm-guardians/internal/checkout"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/sessions"
	"github.com/oh-my-omp/pm-guardians/internal/sessions/synth"
)

// BenchmarkClaim claims an ask no query session file holds, over query directories of two sizes.
func BenchmarkClaim(b *testing.B) {
	for _, n := range []int{10, 40} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			base := config.BaseID("g", "o/code", "m")
			if _, err := synth.WriteDir(dir, synth.Layout{
				Base: base, Model: "m", N: n, FileBytes: 256 << 10, LineBytes: 16 << 10, AskFrom: 1000,
				Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			}); err != nil {
				b.Fatal(err)
			}
			cfg := &config.Config{
				Runtime: config.Runtime{SessionsDir: dir},
				Goals:   config.Goals{Query: config.Goal{Tools: []string{"pm_reply", "pm_replace", "pm_escalate"}}},
			}
			reg := sessions.NewRegistry()
			co := checkout.New(filepath.Join(dir, "checkouts"))
			r, err := New(cfg, reg, bases.New(cfg, reg, co), co, nil)
			if err != nil {
				b.Fatal(err)
			}
			if ok, err := r.claim(1000); err != nil || ok {
				b.Fatalf("claim(done ask) = %v %v", ok, err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if ok, err := r.claim(1); err != nil || !ok {
					b.Fatal(ok, err)
				}
				r.release(1)
			}
		})
	}
}
