// Package checkout keeps git checkouts of base repositories under <checkoutsDir>/<owner>/<name>.
// Git authenticates with the host's credential helper; this package handles no tokens.
package checkout

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// remoteHeadTTL bounds how often RemoteHead asks the remote for one (repo, branch).
const remoteHeadTTL = 60 * time.Second

// Manager serializes git operations per repository.
type Manager struct {
	root string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	heads map[string]remoteHead // key repo + "@" + branch
}

type remoteHead struct {
	at     time.Time
	commit string
	err    error
}

func New(root string) *Manager {
	return &Manager{root: root, locks: map[string]*sync.Mutex{}, heads: map[string]remoteHead{}}
}

// Dir is the checkout directory of repo ("owner/name").
func (m *Manager) Dir(repo string) string { return filepath.Join(m.root, filepath.FromSlash(repo)) }

func (m *Manager) lock(repo string) func() {
	m.mu.Lock()
	l, ok := m.locks[repo]
	if !ok {
		l = &sync.Mutex{}
		m.locks[repo] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Sync clones repo if absent, fetches branch and checks out the fetched commit detached.
// It returns the checkout directory and the checked-out commit.
func (m *Manager) Sync(ctx context.Context, repo, branch string) (dir, commit string, err error) {
	defer m.lock(repo)()
	dir = m.Dir(repo)
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", "", err
		}
		if _, err := git(ctx, "", "clone", "https://github.com/"+repo+".git", dir); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	if _, err := git(ctx, dir, "fetch", "origin", branch); err != nil {
		return "", "", err
	}
	if _, err := git(ctx, dir, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return "", "", err
	}
	commit, err = git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	return dir, commit, nil
}

// RemoteHead returns the commit at refs/heads/<branch> on the checkout's origin, asking the
// remote at most once per minute per (repo, branch).
func (m *Manager) RemoteHead(ctx context.Context, repo, branch string) (string, error) {
	key := repo + "@" + branch
	m.mu.Lock()
	h, ok := m.heads[key]
	m.mu.Unlock()
	if ok && time.Since(h.at) < remoteHeadTTL {
		return h.commit, h.err
	}
	out, err := git(ctx, m.Dir(repo), "ls-remote", "origin", "refs/heads/"+branch)
	commit := ""
	if err == nil {
		fields := strings.Fields(out)
		if len(fields) == 0 {
			err = fmt.Errorf("branch %s not found on origin", branch)
		} else {
			commit = fields[0]
		}
	}
	m.mu.Lock()
	m.heads[key] = remoteHead{at: time.Now(), commit: commit, err: err}
	m.mu.Unlock()
	return commit, err
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
