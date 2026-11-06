// Command pm-guardians serves proxy reads and decision asks from forks of pre-warmed headless omp
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

	"github.com/oh-my-omp/pm-guardians/internal/api"
	"github.com/oh-my-omp/pm-guardians/internal/asks"
	"github.com/oh-my-omp/pm-guardians/internal/bases"
	"github.com/oh-my-omp/pm-guardians/internal/checkout"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/github"
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
	reg := sessions.NewRegistry()
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
