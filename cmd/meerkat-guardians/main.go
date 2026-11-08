// Command meerkat-guardians serves proxy reads and decision asks from forks of pre-warmed headless omp
// base sessions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/mouriya-s-lab/meerkat-guardians/internal/api"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/asks"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/bases"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/checkout"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/config"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/github"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/proxy"
	"github.com/mouriya-s-lab/meerkat-guardians/internal/sessions"
)

func main() {
	root := flag.String("config-root", "", "directory holding guardians/ and shared/ (required)")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "meerkat-guardians: --config-root is required")
		os.Exit(2)
	}
	if err := run(*root); err != nil {
		fmt.Fprintln(os.Stderr, "meerkat-guardians:", err)
		os.Exit(1)
	}
}

func run(root string) error {
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		return errors.New("GH_TOKEN is empty; export it (for a local run: export GH_TOKEN=$(gh auth token))")
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	gh, err := github.New(token)
	if err != nil {
		return err
	}
	startedAt := time.Now().UTC()
	reg := sessions.NewRegistry(cfg.LaunchLink)
	co := checkout.New(cfg.Runtime.CheckoutsDir)
	bm := bases.New(cfg, reg, co)
	pr := proxy.New(cfg, reg, bm, co)
	ar, err := asks.New(cfg, reg, bm, co, gh)
	if err != nil {
		return err
	}
	srv := api.New(cfg, reg, bm, pr, ar, startedAt)

	if err := bm.WarmMissing(); err != nil {
		return err
	}
	go ar.Recover(context.Background())
	slog.Info("listening", "addr", cfg.Runtime.Listen)
	return http.ListenAndServe(cfg.Runtime.Listen, srv.Handler())
}
