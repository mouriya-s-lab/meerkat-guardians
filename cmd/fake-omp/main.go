// Command fake-omp is a deterministic fake omp for the single-repo acceptance runs of the
// PM council and guardians (meerkat-guardians#20): it speaks the omp RPC protocol on stdio, and
// the council and guardians point runtime.yaml's omp.executable at it to exercise
// timeouts, failures and successful goal tools without a real multi-seat LLM session.
//
// Behaviour comes entirely from the environment (the variables are documented on
// internal/fakeomp); the arguments it consumes are --session-dir, which is required, and
// --fork; every other argument is ignored.
package main

import (
	"os"

	"github.com/mouriya-s-lab/meerkat-guardians/internal/fakeomp"
)

func main() {
	os.Exit(fakeomp.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
