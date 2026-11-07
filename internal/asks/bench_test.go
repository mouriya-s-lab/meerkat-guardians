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

// BenchmarkClaim claims an ask no query session file holds, over query directories of two sizes
// and with lines longer than a 64 KiB read buffer.
func BenchmarkClaim(b *testing.B) {
	for _, c := range []struct{ n, lineBytes int }{{10, 16 << 10}, {40, 16 << 10}, {40, 128 << 10}} {
		n := c.n
		b.Run(fmt.Sprintf("N=%d/line=%dKiB", n, c.lineBytes>>10), func(b *testing.B) {
			dir := b.TempDir()
			base := config.BaseID("g", "o/code", "m")
			if _, err := synth.WriteDir(dir, synth.Layout{
				Base: base, Model: "m", N: n, FileBytes: 256 << 10, LineBytes: c.lineBytes, AskFrom: 1000,
				Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			}); err != nil {
				b.Fatal(err)
			}
			cfg := &config.Config{
				Runtime: config.Runtime{SessionsDir: dir},
				Goals:   config.Goals{Query: config.Goal{Tools: []string{"pm_reply", "pm_replace", "pm_escalate"}}},
			}
			reg := sessions.NewRegistry(config.LaunchLink{})
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
