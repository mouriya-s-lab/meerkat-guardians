package sessions

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/sessions/synth"
)

const benchBase = "g.o__code.m"

func benchLayout(n int) synth.Layout {
	return synth.Layout{
		Base: benchBase, Model: "m", N: n, FileBytes: 256 << 10, LineBytes: 16 << 10, AskFrom: 1000,
		Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

func benchDir(b *testing.B, n int) (string, synth.Written) {
	b.Helper()
	dir := b.TempDir()
	w, err := synth.WriteDir(dir, benchLayout(n))
	if err != nil {
		b.Fatal(err)
	}
	return dir, w
}

var benchGoals = map[Kind][]string{
	KindBase: {"pm_base_ready"}, KindProxy: {"pm_return_facts"}, KindQuery: {"pm_reply", "pm_replace", "pm_escalate"},
}

// BenchmarkParseFile parses one ~1 MiB session file whose turns carry 64 KiB payloads.
func BenchmarkParseFile(b *testing.B) {
	dir := b.TempDir()
	l := benchLayout(0)
	l.FileBytes, l.LineBytes = 1<<20, 64<<10
	if _, err := synth.WriteDir(dir, l); err != nil {
		b.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "bases", benchBase, "*.jsonl"))
	if err != nil || len(files) != 1 {
		b.Fatalf("files = %v %v", files, err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseFile(files[0]); err != nil {
			b.Fatal(err)
		}
	}
}

// The directory benchmarks run at two sizes so the cost's growth with the file count shows.
var benchSizes = []int{10, 40}

func BenchmarkFindFile(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			dir, w := benchDir(b, n)
			id := w.Proxies[n/2]
			b.ReportAllocs()
			for b.Loop() {
				if _, ok, err := FindFile(dir, id); err != nil || !ok {
					b.Fatal(ok, err)
				}
			}
		})
	}
}

func BenchmarkList(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			dir, _ := benchDir(b, n)
			r := NewRegistry()
			b.ReportAllocs()
			for b.Loop() {
				if list, err := r.List(dir, benchGoals, 5); err != nil || len(list) != 5 {
					b.Fatal(len(list), err)
				}
			}
		})
	}
}
