// Package sessions runs guardian omp sessions and derives session records from omp session files.
package sessions

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/config"
)

// Kind is the kind of a guardian session.
type Kind string

const (
	KindBase  Kind = "base"
	KindProxy Kind = "proxy"
	KindQuery Kind = "query"
)

// Meta is the pm-meta custom entry the plugin appends from PM_META at session start.
type Meta struct {
	Kind        Kind         `json:"kind"`
	Ref         string       `json:"ref,omitempty"`
	Ask         int64        `json:"ask,omitempty"` // query: the ask comment id
	Base        string       `json:"base"`
	Model       string       `json:"model"`
	Seat        string       `json:"seat,omitempty"`
	Tier        config.Tier  `json:"tier,omitempty"`
	Fingerprint *Fingerprint `json:"fingerprint,omitempty"`
}

// Fingerprint records what a base was warmed with; forks share its prompt cache only while
// model, system prompt, tools and the read commit are unchanged.
type Fingerprint struct {
	Model        string `json:"model"`
	PromptSha256 string `json:"promptSha256"`
	Tools        string `json:"tools"`
	Commit       string `json:"commit"`
}

// File is what the program derives from one omp session file and the failure record the
// program keeps beside it. Entries timestamped before the session header (history copied by a
// fork) count only for Meta.
type File struct {
	Path          string
	ID            string
	ParentSession string
	StartedAt     time.Time
	LastAt        time.Time
	Meta          *Meta                // latest pm-meta entry
	ToolOK        map[string]time.Time // tool name -> timestamp of its latest successful tool result
	LastError     string               // errorMessage of the latest assistant message that failed
	Failure       string               // why the program's run of this session failed; see FailurePath
	FailedAt      time.Time            // when the program decided it failed; zero for records without it
}

// failureRecord is the file at FailurePath. Records written before At existed lack it.
type failureRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at,omitzero"` // RFC 3339
}

// FailurePath is where the program records why and when its run of the session in the omp
// session file path failed (omp exited, a non-retryable error, the continue limit, the session
// timeout). omp writes the session file and stops writing it when killed; the program owns this
// record, so the reason and the end time outlive the process.
func FailurePath(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".pm-failure.json"
}

// writeFailure records that the session in sessionFile failed at at for reason, atomically.
func writeFailure(sessionFile, reason string, at time.Time) error {
	b, err := json.Marshal(failureRecord{Reason: reason, At: at})
	if err != nil {
		return err
	}
	path := FailurePath(sessionFile)
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// GoalAt returns when the latest of tools succeeded, and whether any did.
func (f *File) GoalAt(tools []string) (time.Time, bool) {
	var at time.Time
	var ok bool
	for _, t := range tools {
		if ts, done := f.ToolOK[t]; done && (!ok || ts.After(at)) {
			at, ok = ts, true
		}
	}
	return at, ok
}

// ErrNotSession marks a .jsonl file that is not an omp session file (no session header).
var ErrNotSession = errors.New("not an omp session file")

const maxLineBytes = 64 << 20

type entry struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	Timestamp     string          `json:"timestamp"`
	ParentSession string          `json:"parentSession"`
	CustomType    string          `json:"customType"`
	Data          json.RawMessage `json:"data"`
	Message       *struct {
		Role         string `json:"role"`
		ToolName     string `json:"toolName"`
		IsError      bool   `json:"isError"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
}

// ParseFile reads an omp session file. Lines before the header may only be title lines.
func ParseFile(path string) (*File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	f := &File{Path: path, ToolOK: map[string]time.Time{}}
	for sc.Scan() {
		var e entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // a line being written concurrently
		}
		if f.ID == "" {
			switch e.Type {
			case "title":
				continue
			case "session":
				f.ID = e.ID
				f.ParentSession = e.ParentSession
				f.StartedAt = parseTime(e.Timestamp)
				f.LastAt = f.StartedAt
				continue
			default:
				return nil, ErrNotSession
			}
		}
		ts := parseTime(e.Timestamp)
		if ts.After(f.LastAt) {
			f.LastAt = ts
		}
		if e.Type == "custom" && e.CustomType == "pm-meta" {
			var m Meta
			if json.Unmarshal(e.Data, &m) == nil {
				f.Meta = &m
			}
			continue
		}
		if e.Type != "message" || e.Message == nil || ts.Before(f.StartedAt) {
			continue
		}
		switch e.Message.Role {
		case "toolResult":
			if !e.Message.IsError {
				f.ToolOK[e.Message.ToolName] = ts
			}
		case "assistant":
			if e.Message.ErrorMessage != "" {
				f.LastError = e.Message.ErrorMessage
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if f.ID == "" {
		return nil, ErrNotSession
	}
	switch b, err := os.ReadFile(FailurePath(path)); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var rec failureRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", FailurePath(path), err)
		}
		f.Failure, f.FailedAt = rec.Reason, rec.At
	}
	return f, nil
}

// ScanDir parses every session file under dir (recursively); other .jsonl files are skipped.
// A missing dir yields no files.
func ScanDir(dir string) ([]*File, error) {
	var files []*File
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		f, err := ParseFile(path)
		if errors.Is(err, ErrNotSession) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	return files, err
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
