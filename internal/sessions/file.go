// Package sessions runs guardian omp sessions and derives session records from omp session files.
package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// failureRecord is the file at FailurePath. It carries what Registry.Run knew from its Spec
// when the session failed, so a session omp never wrote a file for still lists with its
// identity. Records written before the identity fields existed carry only reason and at;
// without an id they cannot name a session on their own.
type failureRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at,omitzero"` // RFC 3339
	ID     string    `json:"id,omitzero"`
	// Kind, Base, Ref, Seat, Ask, Tier and Model are what Registry.Run knew from its Spec
	// when the session failed; StartedAt is when Run started it. They let List name a
	// session whose .jsonl omp never wrote.
	Kind      Kind        `json:"kind,omitzero"`
	Base      string      `json:"base,omitzero"`
	Ref       string      `json:"ref,omitzero"`
	Seat      string      `json:"seat,omitzero"`
	Ask       int64       `json:"ask,omitzero"`
	Tier      config.Tier `json:"tier,omitzero"`
	Model     string      `json:"model,omitzero"`
	StartedAt time.Time   `json:"startedAt,omitzero"` // RFC 3339
}

// FailurePath is where the program records why and when its run of the session in the omp
// session file path failed (omp exited, a non-retryable error, the continue limit, the session
// timeout). omp writes the session file and stops writing it when killed; the program owns this
// record, so the reason and the end time outlive the process.
func FailurePath(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".pm-failure.json"
}

// writeFailure records rec for the session in sessionFile, atomically.
func writeFailure(sessionFile string, rec failureRecord) error {
	b, err := json.Marshal(rec)
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

// entry is what one line is judged on. A line that does not decode into it is skipped (a line
// being written concurrently). The data payload is not part of it: only pm-meta lines decode
// theirs, through metaLine.
type entry struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	ParentSession string `json:"parentSession"`
	CustomType    string `json:"customType"`
	Message       struct {
		Role         string `json:"role"`
		ToolName     string `json:"toolName"`
		IsError      bool   `json:"isError"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
}

func (e *entry) isMeta() bool { return e.Type == "custom" && e.CustomType == "pm-meta" }

// metaLine is a pm-meta line with its payload.
type metaLine struct {
	entry
	Data json.RawMessage `json:"data"`
}

// meta decodes the payload of pm-meta line l; ok is false when it is not a Meta.
func (l *metaLine) meta() (*Meta, bool) {
	var m Meta
	if json.Unmarshal(l.Data, &m) != nil {
		return nil, false
	}
	return &m, true
}

// header is the session entry that opens a session file.
type header struct {
	id, parentSession string
	startedAt         time.Time
}

// lineReader reads the lines of one file after another as bufio.Scanner with ScanLines does
// (the newline and a carriage return before it are dropped, a last line without a newline still
// counts, a line over maxLineBytes is bufio.ErrTooLong). A line that does not fit the reader's
// buffer is assembled in long, which the lineReader keeps for the next file, so a walk that
// reuses one lineReader grows it once for its longest line.
type lineReader struct {
	r    *bufio.Reader
	long []byte
	line []byte
	err  error
}

// Reader buffer sizes: a header and the title lines before it are short; entries are read in
// 64 KiB chunks, the size bufio.Scanner starts with.
const (
	headerBuf = 4 << 10
	entryBuf  = 64 << 10
)

func newLineReader(size int) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(nil, size)}
}

// open starts reading the file at path; the caller closes the returned file.
func (l *lineReader) open(path string) (*os.File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	l.r.Reset(fh)
	l.line, l.err = nil, nil
	return fh, nil
}

// scan advances to the next line, which bytes returns until the following scan.
func (l *lineReader) scan() bool {
	if l.err != nil {
		return false
	}
	line, err := l.r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		l.long = append(l.long[:0], line...)
		for err == bufio.ErrBufferFull {
			if len(l.long) > maxLineBytes {
				l.err = bufio.ErrTooLong
				return false
			}
			line, err = l.r.ReadSlice('\n')
			l.long = append(l.long, line...)
		}
		line = l.long
	}
	switch {
	case err == io.EOF && len(line) == 0:
		l.err = err
		return false
	case err != nil && err != io.EOF:
		l.err = err
		return false
	}
	l.err = err // io.EOF after a last line without a newline: this line, then stop
	if len(line) > maxLineBytes {
		l.err = bufio.ErrTooLong
		return false
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	l.line = bytes.TrimSuffix(line, []byte{'\r'})
	return true
}

func (l *lineReader) bytes() []byte { return l.line }

// failed is the read error that ended the lines, nil at the end of the file.
func (l *lineReader) failed() error {
	if l.err == io.EOF {
		return nil
	}
	return l.err
}

// scanHeader consumes the lines of l up to the session header. Lines before it may only be
// title lines.
func scanHeader(l *lineReader) (header, error) {
	for l.scan() {
		var e entry
		if json.Unmarshal(l.bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "title":
		case "session":
			if e.ID != "" {
				return header{id: e.ID, parentSession: e.ParentSession, startedAt: parseTime(e.Timestamp)}, nil
			}
		default:
			return header{}, ErrNotSession
		}
	}
	if err := l.failed(); err != nil {
		return header{}, err
	}
	return header{}, ErrNotSession
}

// readHeader reads the header of the session file at path and nothing after it.
func readHeader(path string, l *lineReader) (header, error) {
	fh, err := l.open(path)
	if err != nil {
		return header{}, err
	}
	defer fh.Close()
	return scanHeader(l)
}

// readMeta reads the latest pm-meta entry of the session file at path (nil if it has none),
// decoding only the header and the lines that carry a pm-meta token.
func readMeta(path string, l *lineReader) (*Meta, error) {
	fh, err := l.open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	if _, err := scanHeader(l); err != nil {
		return nil, err
	}
	var meta *Meta
	for l.scan() {
		line := l.bytes()
		if !bytes.Contains(line, metaToken) {
			continue
		}
		var ml metaLine
		if json.Unmarshal(line, &ml) != nil || !ml.isMeta() {
			continue
		}
		if m, ok := ml.meta(); ok {
			meta = m
		}
	}
	return meta, l.failed()
}

// metaToken is in every line that carries a pm-meta entry: omp writes the customType value as
// a plain JSON string.
var metaToken = []byte(`"pm-meta"`)

// ParseFile reads an omp session file. Lines before the header may only be title lines.
func ParseFile(path string) (*File, error) {
	l := newLineReader(entryBuf)
	fh, err := l.open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	h, err := scanHeader(l)
	if err != nil {
		return nil, err
	}
	f := &File{
		Path: path, ID: h.id, ParentSession: h.parentSession, StartedAt: h.startedAt, LastAt: h.startedAt,
		ToolOK: map[string]time.Time{},
	}
	for l.scan() {
		var e entry
		if json.Unmarshal(l.bytes(), &e) != nil {
			continue
		}
		ts := parseTime(e.Timestamp)
		if ts.After(f.LastAt) {
			f.LastAt = ts
		}
		if e.isMeta() {
			var ml metaLine
			if json.Unmarshal(l.bytes(), &ml) == nil {
				if m, ok := ml.meta(); ok {
					f.Meta = m
				}
			}
			continue
		}
		if e.Type != "message" || ts.Before(f.StartedAt) {
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
	if err := l.failed(); err != nil {
		return nil, err
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

// skipped reports whether err means the .jsonl file at hand is no session to list: not an omp
// session file, or gone since the directory was read.
func skipped(err error) bool {
	return errors.Is(err, ErrNotSession) || errors.Is(err, fs.ErrNotExist)
}

// walk calls session for every .jsonl file under dir (recursively, in lexical order) and then
// record, when not nil, for every failure record under it. session may return fs.SkipAll to
// stop the walk. A missing dir has no files.
func walk(dir string, session func(path string) error, record func(path string) error) error {
	var records []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		switch {
		case d.IsDir():
		case strings.HasSuffix(path, ".jsonl"):
			return session(path)
		case record != nil && strings.HasSuffix(path, ".pm-failure.json"):
			records = append(records, path)
		}
		return nil
	})
	if err != nil || record == nil {
		return err
	}
	for _, path := range records {
		if err := record(path); err != nil {
			return err
		}
	}
	return nil
}

// Candidate is one session under a directory, known before its entries are read: a session
// file by its header, or a failure record whose session file omp never wrote.
type Candidate struct {
	path      string
	startedAt time.Time
	orphan    *File
}

// Parse derives the session's File: the session file's entries, or the failure record alone.
// ok is false when the file turned out to be no session to list (see skipped).
func (c Candidate) Parse() (f *File, ok bool, err error) {
	if c.orphan != nil {
		return c.orphan, true, nil
	}
	f, err = ParseFile(c.path)
	if skipped(err) {
		return nil, false, nil
	}
	return f, err == nil, err
}

// Newest lists the sessions under dir (recursively) newest first by header timestamp, reading
// only the header of each session file; other .jsonl files are skipped. A failure record whose
// session file omp never wrote is a session of its own, so a session killed before its first
// write still lists. A missing dir yields none.
func Newest(dir string) ([]Candidate, error) {
	var out []Candidate
	lr := newLineReader(headerBuf)
	err := walk(dir, func(path string) error {
		h, err := readHeader(path, lr)
		if skipped(err) {
			return nil
		}
		if err != nil {
			return err
		}
		out = append(out, Candidate{path: path, startedAt: h.startedAt})
		return nil
	}, func(recPath string) error {
		f, ok, err := orphanFile(recPath)
		if ok {
			out = append(out, Candidate{path: f.Path, startedAt: f.StartedAt, orphan: f})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].startedAt.After(out[j].startedAt) })
	return out, nil
}

// Matching returns the sessions under dir whose latest pm-meta keep accepts. Of a session file
// keep rejects only the header and the pm-meta entries are decoded; a failure record whose
// session file omp never wrote is judged by the identity it records.
func Matching(dir string, keep func(*Meta) bool) ([]*File, error) {
	var out []*File
	lr := newLineReader(entryBuf)
	err := walk(dir, func(path string) error {
		m, err := readMeta(path, lr)
		if skipped(err) || err == nil && (m == nil || !keep(m)) {
			return nil
		}
		if err != nil {
			return err
		}
		f, err := ParseFile(path)
		if skipped(err) {
			return nil
		}
		if err != nil {
			return err
		}
		out = append(out, f)
		return nil
	}, func(recPath string) error {
		f, ok, err := orphanFile(recPath)
		if ok && keep(f.Meta) {
			out = append(out, f)
		}
		return err
	})
	return out, err
}

// orphanFile derives the session the failure record at recPath names when its session file
// does not exist. It reports false when the record is old-format (no id) or the session
// file exists after all; either way there is no extra entry.
func orphanFile(recPath string) (*File, bool, error) {
	sessionFile := strings.TrimSuffix(recPath, ".pm-failure.json") + ".jsonl"
	if _, err := os.Stat(sessionFile); err == nil {
		return nil, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	b, err := os.ReadFile(recPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var rec failureRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, false, fmt.Errorf("%s: %w", recPath, err)
	}
	f := &File{Path: sessionFile, ToolOK: map[string]time.Time{}, Failure: rec.Reason, FailedAt: rec.At}
	if !backfillMeta(f, rec) {
		return nil, false, nil
	}
	return f, true, nil
}

// backfillMeta fills f.Meta from rec when the session file carries no pm-meta entry.
// It reports whether the record named a session at all.
func backfillMeta(f *File, rec failureRecord) bool {
	if rec.ID == "" || rec.Kind == "" {
		return false
	}
	f.Meta = &Meta{
		Kind: rec.Kind, Base: rec.Base, Ref: rec.Ref, Seat: rec.Seat,
		Ask: rec.Ask, Tier: rec.Tier, Model: rec.Model,
	}
	if f.ID == "" {
		f.ID = rec.ID
	}
	if f.StartedAt.IsZero() {
		f.StartedAt = rec.StartedAt
	}
	if f.LastAt.IsZero() {
		f.LastAt = rec.StartedAt
	}
	return true
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
