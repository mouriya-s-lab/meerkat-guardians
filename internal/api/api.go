// Package api serves the guardians HTTP API (contract §3 and §4).
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/asks"
	"github.com/oh-my-omp/pm-guardians/internal/bases"
	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/proxy"
	"github.com/oh-my-omp/pm-guardians/internal/sessions"
)

type Server struct {
	cfg       *config.Config
	reg       *sessions.Registry
	bases     *bases.Manager
	proxy     *proxy.Runner
	asks      *asks.Runner
	startedAt time.Time
}

func New(cfg *config.Config, reg *sessions.Registry, b *bases.Manager, p *proxy.Runner, a *asks.Runner, startedAt time.Time) *Server {
	return &Server{cfg: cfg, reg: reg, bases: b, proxy: p, asks: a, startedAt: startedAt}
}

// Handler returns the API routes wrapped with CORS headers.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/bases", s.listBases)
	mux.HandleFunc("POST /v1/bases/{id}/rewarm", s.rewarm)
	mux.HandleFunc("POST /v1/proxy-reads", s.proxyRead)
	mux.HandleFunc("POST /v1/asks", s.submitAsk)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}/log", s.sessionLog)
	mux.HandleFunc("OPTIONS /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	version := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	writeJSON(w, http.StatusOK, map[string]any{"component": "guardians", "version": version, "startedAt": s.startedAt})
}

func (s *Server) listBases(w http.ResponseWriter, r *http.Request) {
	views, err := s.bases.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) rewarm(w http.ResponseWriter, r *http.Request) {
	switch err := s.bases.Rewarm(r.PathValue("id")); {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"base": r.PathValue("id")})
	case errors.Is(err, bases.ErrUnknown):
		writeError(w, http.StatusNotFound, "unknown_base", "no base "+r.PathValue("id"))
	case errors.Is(err, bases.ErrWarming):
		writeError(w, http.StatusConflict, "warming", "base "+r.PathValue("id")+" is warming")
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) proxyRead(w http.ResponseWriter, r *http.Request) {
	var req proxy.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body: "+err.Error())
		return
	}
	res, err := s.proxy.Read(r.Context(), req)
	var (
		rejected *proxy.RejectedError
		failed   *proxy.SessionFailed
	)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.As(err, &rejected):
		status, code := rejection(rejected.Reason)
		writeError(w, status, code, rejected.Message)
	case errors.As(err, &failed):
		writeError(w, http.StatusBadGateway, "session_failed", failed.Reason)
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func rejection(r proxy.Rejection) (int, string) {
	switch r {
	case proxy.Invalid:
		return http.StatusBadRequest, "invalid_request"
	case proxy.NoGuardian:
		return http.StatusNotFound, "no_guardian"
	case proxy.BaseNotReady:
		return http.StatusConflict, "base_not_ready"
	}
	panic(fmt.Sprintf("unhandled proxy rejection %d", r))
}

func (s *Server) submitAsk(w http.ResponseWriter, r *http.Request) {
	var req asks.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body: "+err.Error())
		return
	}
	out, err := s.asks.Submit(r.Context(), req)
	var rejected *asks.RejectedError
	switch {
	case errors.As(err, &rejected):
		status, code := askRejection(rejected.Reason)
		writeError(w, status, code, rejected.Message)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	body := map[string]string{"ask": strconv.FormatInt(req.Comment, 10)}
	switch out {
	case asks.Started:
		writeJSON(w, http.StatusAccepted, body)
	case asks.Exists:
		writeJSON(w, http.StatusOK, body)
	default:
		panic(fmt.Sprintf("unhandled ask outcome %d", out))
	}
}

func askRejection(r asks.Rejection) (int, string) {
	switch r {
	case asks.Invalid:
		return http.StatusBadRequest, "invalid_request"
	case asks.UnknownRepo:
		return http.StatusNotFound, "unknown_repo"
	case asks.BaseNotReady:
		return http.StatusConflict, "base_not_ready"
	}
	panic(fmt.Sprintf("unhandled ask rejection %d", r))
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	goals := map[sessions.Kind][]string{
		sessions.KindBase:  s.cfg.Goals.Base.Tools,
		sessions.KindProxy: s.cfg.Goals.Proxy.Tools,
		sessions.KindQuery: s.cfg.Goals.Query.Tools,
	}
	list, err := s.reg.List(s.cfg.Runtime.SessionsDir, goals, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) sessionLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, ok, err := sessions.FindFile(s.cfg.Runtime.SessionsDir, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_session", "no session "+id)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	if _, err := io.Copy(w, f); err != nil {
		slog.Warn("session log copy failed", "session", id, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
