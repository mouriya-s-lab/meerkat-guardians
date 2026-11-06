// Command pm-guardians serves proxy reads from forks of pre-warmed headless omp base sessions.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/api"
	"github.com/oh-my-omp/pm-guardians/internal/bases"
	"github.com/oh-my-omp/pm-guardians/internal/checkout"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/proxy"
	"github.com/oh-my-omp/pm-guardians/internal/sessions"
)

func main() {
	root := flag.String("config-root", "", "directory holding guardians/ and shared/ (required)")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "pm-guardians: --config-root is required")
		os.Exit(2)
	}
	if err := run(*root); err != nil {
		fmt.Fprintln(os.Stderr, "pm-guardians:", err)
		os.Exit(1)
	}
}

func run(root string) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	startedAt := time.Now().UTC()
	reg := sessions.NewRegistry()
	co := checkout.New(cfg.Runtime.CheckoutsDir)
	bm := bases.New(cfg, reg, co)
	pr := proxy.New(cfg, reg, bm, co)
	srv := api.New(cfg, reg, bm, pr, startedAt)

	if err := bm.WarmMissing(); err != nil {
		return err
	}
	slog.Info("listening", "addr", cfg.Runtime.Listen)
	return http.ListenAndServe(cfg.Runtime.Listen, srv.Handler())
}
