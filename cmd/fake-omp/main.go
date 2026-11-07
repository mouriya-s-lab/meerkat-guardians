// Command fake-omp is a deterministic fake omp for the single-repo acceptance runs of the
// PM council and guardians (pm-guardians#20): it speaks the omp RPC protocol on stdio, and
// the council and guardians point runtime.yaml's omp.executable at it to exercise
// timeouts, failures and successful goal tools without a real multi-seat LLM session.
//
// Behaviour comes entirely from the environment (the variables are documented on
// internal/fakeomp); the only argument it consumes is --session-dir, every other argument
// is ignored. Without --session-dir it answers get_state without a file and writes nothing
// to disk.
package main

import (
	"os"

	"github.com/oh-my-omp/pm-guardians/internal/fakeomp"
)

func main() {
	os.Exit(fakeomp.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
