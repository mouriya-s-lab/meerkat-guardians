// Command mk-session-fixture writes a synthetic sessionsDir (see internal/sessions/synth) for
// load measurements of the guardians HTTP endpoints and prints the session ids it wrote as JSON.
//
//	go run ./scripts/mk-session-fixture -root /tmp/load/sessions -base <base id> -n 200
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mouriya-s-lab/meerkat-guardians/internal/sessions/synth"
)

func main() {
	root := flag.String("root", "", "sessionsDir to write (required)")
	base := flag.String("base", "", "base id, config.BaseID(guardian, repo, model) (required)")
	model := flag.String("model", "", "model recorded in pm-meta")
	n := flag.Int("n", 200, "historic base, query and proxy files, each")
	fileBytes := flag.Int("file-bytes", 1<<20, "approximate size of every session file")
	lineBytes := flag.Int("line-bytes", 64<<10, "approximate payload size of one large line")
	askFrom := flag.Int64("ask-from", 1000, "first query ask comment id")
	fingerprint := flag.String("fingerprint", "", "ready base pm-meta fingerprint as JSON")
	flag.Parse()
	if *root == "" || *base == "" {
		fmt.Fprintln(os.Stderr, "mk-session-fixture: -root and -base are required")
		os.Exit(2)
	}
	if *fingerprint != "" && !json.Valid([]byte(*fingerprint)) {
		fmt.Fprintln(os.Stderr, "mk-session-fixture: -fingerprint is not JSON")
		os.Exit(2)
	}
	w, err := synth.WriteDir(*root, synth.Layout{
		Base: *base, Model: *model, N: *n, FileBytes: *fileBytes, LineBytes: *lineBytes, AskFrom: *askFrom,
		Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Fingerprint: json.RawMessage(*fingerprint),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mk-session-fixture:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(w); err != nil {
		fmt.Fprintln(os.Stderr, "mk-session-fixture:", err)
		os.Exit(1)
	}
}
