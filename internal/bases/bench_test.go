package bases

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/checkout"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/sessions"
	"github.com/oh-my-omp/pm-guardians/internal/sessions/synth"
)

// BenchmarkForkSource finds the ready base among base directories of two sizes and with lines
// longer than a 64 KiB read buffer.
func BenchmarkForkSource(b *testing.B) {
	for _, c := range []struct{ n, lineBytes int }{{10, 16 << 10}, {40, 16 << 10}, {40, 128 << 10}} {
		n := c.n
		b.Run(fmt.Sprintf("N=%d/line=%dKiB", n, c.lineBytes>>10), func(b *testing.B) {
			dir := b.TempDir()
			id := config.BaseID("g", "o/code", "m")
			w, err := synth.WriteDir(dir, synth.Layout{
				Base: id, Model: "m", N: n, FileBytes: 256 << 10, LineBytes: c.lineBytes, AskFrom: 1000,
				Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			})
			if err != nil {
				b.Fatal(err)
			}
			cfg := &config.Config{
				Runtime: config.Runtime{SessionsDir: dir, MaxConcurrentWarmups: 1},
				Goals:   config.Goals{Base: config.Goal{Tools: []string{"pm_base_ready"}}},
			}
			m := New(cfg, sessions.NewRegistry(config.LaunchLink{}), checkout.New(filepath.Join(dir, "checkouts")))
			b.ReportAllocs()
			for b.Loop() {
				f, err := m.ForkSource(id)
				if err != nil || f == nil || f.ID != w.Ready {
					b.Fatal(f, err)
				}
			}
		})
	}
}
