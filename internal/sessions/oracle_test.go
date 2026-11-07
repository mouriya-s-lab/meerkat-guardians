package sessions

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-guardians/internal/config"
	"github.com/oh-my-omp/pm-guardians/internal/sessions/synth"
)

// The readers that decode every line of every file (oh-my-omp/pm-guardians@e890de1) are kept
// here as the oracle: the narrowed readers must derive what they derived.

type oracleEntry struct {
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

func oracleParseFile(path string) (*File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	f := &File{Path: path, ToolOK: map[string]time.Time{}}
	for sc.Scan() {
		var e oracleEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
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

func oracleScanDir(dir string) ([]*File, error) {
	var files []*File
	var orphans []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".jsonl"):
			f, err := oracleParseFile(path)
			if errors.Is(err, ErrNotSession) || errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			files = append(files, f)
		case strings.HasSuffix(path, ".pm-failure.json"):
			orphans = append(orphans, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, recPath := range orphans {
		f, ok, err := orphanFile(recPath)
		if err != nil {
			return nil, err
		}
		if ok {
			files = append(files, f)
		}
	}
	return files, nil
}

// oracleNewest is the old order of List and bases.scan: every file parsed, newest first.
func oracleNewest(t *testing.T, dir string) []*File {
	t.Helper()
	files, err := oracleScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].StartedAt.After(files[j].StartedAt) })
	return files
}

func oracleFindFile(t *testing.T, dir, id string) (string, bool) {
	t.Helper()
	for _, f := range oracleNewestUnsorted(t, dir) {
		if f.ID != id {
			continue
		}
		if _, err := os.Stat(f.Path); err == nil {
			return f.Path, true
		}
	}
	return "", false
}

func oracleNewestUnsorted(t *testing.T, dir string) []*File {
	t.Helper()
	files, err := oracleScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// edgeDir writes session files that probe the line rules: lines before the header, equal
// header timestamps, forks, failure records, malformed and oddly typed lines.
func edgeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	put := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hdr := func(id, ts string) string {
		return `{"type":"session","version":3,"id":"` + id + `","timestamp":"` + ts + `","cwd":"/c"}`
	}
	meta := func(kind string, ask int, ts string) string {
		return fmt.Sprintf(`{"type":"custom","customType":"pm-meta","data":{"kind":%q,"ask":%d,"base":"b","model":"m"},"id":"x","timestamp":%q}`, kind, ask, ts)
	}
	ok := func(tool, ts string) string {
		return `{"type":"message","id":"r","timestamp":"` + ts + `","message":{"role":"toolResult","toolName":"` + tool + `","isError":false}}`
	}
	const t0 = "2026-11-07T01:00:00.000Z"
	for _, name := range []string{"testdata/base.jsonl", "testdata/proxy.jsonl", "testdata/other.jsonl", "fixtures/parent/base.jsonl", "fixtures/parent/fork.jsonl"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		put("real/"+strings.ReplaceAll(name, "/", "_"), strings.TrimSuffix(string(b), "\n"))
	}
	put("a/corrupt-before-header.jsonl", `{"type":"title","title":""}`, `{"type":"sess`, hdr("e1", t0), meta("query", 1, t0), ok("pm_reply", "2026-11-07T01:00:02.000Z"))
	// Equal header timestamps, in different directories and orders.
	put("b/tie-1.jsonl", hdr("tie-1", t0), meta("query", 2, t0))
	put("a/tie-2.jsonl", hdr("tie-2", t0), meta("query", 2, t0), ok("pm_reply", "2026-11-07T01:00:03.000Z"))
	put("c/tie-3.jsonl", hdr("tie-3", t0), meta("proxy", 0, t0))
	put("c/empty.jsonl")
	put("c/title-only.jsonl", `{"type":"title","title":""}`)
	put("c/empty-id.jsonl", hdr("", "2026-11-07T00:00:00.000Z"), `{"type":"title"}`, hdr("eid", "2026-11-07T02:00:00.000Z"), meta("base", 0, t0))
	put("c/empty-id-then-entry.jsonl", hdr("", t0), meta("base", 0, t0))
	put("d/odd-lines.jsonl", hdr("odd", "2026-11-07T03:00:00.000Z"),
		meta("query", 3, "2026-11-07T03:00:01.000Z"),
		`{"type":"custom","customType":"pm-meta","id":"x","timestamp":"2026-11-07T03:00:02.000Z"}`,
		`{"type":"custom","customType":"pm-meta","data":"not a meta","id":"x","timestamp":"2026-11-07T03:00:03.000Z"}`,
		`{"type":"message","id":"m","timestamp":"2026-11-07T03:00:04.000Z","message":null}`,
		`{"type":"message","id":5,"timestamp":"2026-11-07T03:00:05.000Z","message":{"role":"toolResult","toolName":"typed-id","isError":false}}`,
		`{"type":"message","id":"m","parentSession":7,"timestamp":"2026-11-07T03:00:06.000Z","message":{"role":"toolResult","toolName":"typed-parent","isError":false}}`,
		`{"type":"message","id":"m","timestamp":"2026-11-07T03:00:07.000Z","message":{"role":"assistant","errorMessage":"boom"}}`,
		`{"type":"message","id":"m","timestamp":"2026-11-07T03:00:08.000Z","message":"text"}`,
		ok("pm_escalate", "2026-11-07T03:00:09.000Z"),
		`{"type":"message","id":"m","timestamp":"2026-11-07T03:00:10.000Z","message":{"role":"toolResult","toolName":"pm_escalate","isError":true}}`,
		`{"type":"custom","customType":"pm-meta","data":null,"id":"x","timestamp":"2026-11-07T03:00:11.000Z"}`,
		`{"type":"custom","customType":"other","data":{"note":"\"pm-meta\""},"id":"x","timestamp":"2026-11-07T03:00:12.000Z"}`,
		`{"type":"message","id":"m","timestamp":"2026-11-07T03:00:13`)
	put("d/null-meta-last.jsonl", hdr("nullmeta", "2026-11-07T03:30:00.000Z"),
		meta("query", 4, "2026-11-07T03:30:01.000Z"),
		`{"type":"custom","customType":"pm-meta","data":null,"id":"x","timestamp":"2026-11-07T03:30:02.000Z"}`)
	// Line endings: CRLF, blank lines, and a last line without a newline.
	if err := os.WriteFile(filepath.Join(dir, "d/crlf.jsonl"), []byte(hdr("crlf", "2026-11-07T03:40:00.000Z")+"\r\n\r\n"+
		meta("query", 7, "2026-11-07T03:40:01.000Z")+"\r\n\n"+ok("pm_reply", "2026-11-07T03:40:02.000Z")), 0o644); err != nil {
		t.Fatal(err)
	}
	put("d/fork.jsonl", hdr("fork", "2026-11-07T04:00:00.000Z"),
		meta("base", 0, "2026-11-07T00:00:00.000Z"), ok("pm_base_ready", "2026-11-07T00:00:01.000Z"),
		meta("query", 5, "2026-11-07T04:00:01.000Z"), ok("read", "2026-11-07T04:00:02.000Z"))
	// Failure records: beside a file, orphan (new format, ties a file's start), old format.
	failed := put("e/failed.jsonl", hdr("failed", "2026-11-07T05:00:00.000Z"), meta("query", 6, "2026-11-07T05:00:01.000Z"))
	if err := writeFailure(failed, failureRecord{Reason: "omp exited", At: time.Date(2026, 11, 7, 5, 0, 9, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if err := writeFailure(filepath.Join(dir, "e/orphan.jsonl"), failureRecord{
		Reason: "start failed", At: time.Date(2026, 11, 7, 1, 0, 5, 0, time.UTC),
		ID: "orphan", Kind: KindQuery, Ask: 2, Base: "b", Model: "m", StartedAt: time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeFailure(filepath.Join(dir, "e/old.jsonl"), failureRecord{Reason: "omp exited"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func synthDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := synth.WriteDir(dir, synth.Layout{
		// Lines both shorter and longer than the 64 KiB reader buffer.
		Base: "g.o__code.m", Model: "m", N: 4, FileBytes: 512 << 10, LineBytes: 96 << 10, AskFrom: 1000,
		Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func oracleDirs(t *testing.T) map[string]string {
	return map[string]string{"edge": edgeDir(t), "synth": synthDir(t)}
}

func TestOracleParseFile(t *testing.T) {
	for name, dir := range oracleDirs(t) {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return err
			}
			got, gotErr := ParseFile(path)
			want, wantErr := oracleParseFile(path)
			if !errors.Is(gotErr, wantErr) || !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: ParseFile = %+v, %v; oracle %+v, %v", name, path, got, gotErr, want, wantErr)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Newest parsed in order is the old fully parsed, sorted list: List takes its first rows and
// bases.scan its newest and first ready entries from it.
func TestOracleNewest(t *testing.T) {
	for name, dir := range oracleDirs(t) {
		cands, err := Newest(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got []*File
		for _, c := range cands {
			f, ok, err := c.Parse()
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				got = append(got, f)
			}
		}
		if want := oracleNewest(t, dir); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: newest order differs:\n got %v\nwant %v", name, ids(got), ids(want))
		}
	}
}

func TestOracleList(t *testing.T) {
	goals := map[Kind][]string{KindBase: {"pm_base_ready"}, KindProxy: {"pm_return_facts"}, KindQuery: {"pm_reply", "pm_replace", "pm_escalate"}}
	for name, dir := range oracleDirs(t) {
		all := oracleNewest(t, dir)
		r := NewRegistry(config.LaunchLink{})
		for limit := 1; limit <= len(all)+1; limit++ {
			got, err := r.List(dir, goals, limit)
			if err != nil {
				t.Fatal(err)
			}
			if want := r.summaries(all[:min(limit, len(all))], goals); !reflect.DeepEqual(got, want) {
				t.Errorf("%s limit %d:\n got %+v\nwant %+v", name, limit, got, want)
			}
		}
	}
}

func TestOracleFindFile(t *testing.T) {
	for name, dir := range oracleDirs(t) {
		queries := []string{"missing", "orphan", ""}
		for _, f := range oracleNewest(t, dir) {
			queries = append(queries, f.ID)
		}
		for _, id := range queries {
			got, gotOK, err := FindFile(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			want, wantOK := oracleFindFile(t, dir, id)
			if got != want || gotOK != wantOK {
				t.Errorf("%s FindFile(%q) = %q %v, oracle %q %v", name, id, got, gotOK, want, wantOK)
			}
		}
	}
}

// Matching keeps exactly the files the old claim loop kept: those whose latest pm-meta matches.
func TestOracleMatching(t *testing.T) {
	for name, dir := range oracleDirs(t) {
		all := oracleNewestUnsorted(t, dir)
		asks := map[int64]bool{0: true, 99999: true}
		for _, f := range all {
			if f.Meta != nil {
				asks[f.Meta.Ask] = true
			}
		}
		for _, kind := range []Kind{KindQuery, KindProxy, KindBase} {
			for ask := range asks {
				keep := func(m *Meta) bool { return m.Kind == kind && m.Ask == ask }
				got, err := Matching(dir, keep)
				if err != nil {
					t.Fatal(err)
				}
				var want []*File
				for _, f := range all {
					if f.Meta != nil && keep(f.Meta) {
						want = append(want, f)
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s Matching(%s, %d) = %v, oracle %v", name, kind, ask, ids(got), ids(want))
				}
			}
		}
	}
}

// A file the query does not need is not read past its header, so a broken failure record
// beside it no longer fails the lookup of another session (the old full scan returned the
// record's error for every query).
func TestFindFileIgnoresUnrelatedFailureRecord(t *testing.T) {
	dir := edgeDir(t)
	if err := os.WriteFile(FailurePath(filepath.Join(dir, "a/tie-2.jsonl")), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := oracleScanDir(dir); err == nil {
		t.Fatal("oracle scan should fail on the broken record")
	}
	if path, ok, err := FindFile(dir, "fork"); err != nil || !ok || filepath.Base(path) != "fork.jsonl" {
		t.Fatalf("FindFile = %q %v %v", path, ok, err)
	}
}

func ids(files []*File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.ID
	}
	return out
}

// lineReader yields the lines bufio.Scanner yields, across files read one after another with
// one lineReader, short and long lines alike.
func TestLineReaderMatchesScanner(t *testing.T) {
	long := strings.Repeat("x", 3*entryBuf+17)
	inputs := []string{
		"", "\n", "a", "a\n", "a\r\n", "a\r", "\r\n\r\n", "a\n\nb", "a\nb\n",
		long, long + "\n" + "b", "b\n" + long + "\r\n" + long[:entryBuf] + "\n", strings.Repeat(long+"\n", 3),
	}
	dir := t.TempDir()
	l := newLineReader(entryBuf)
	for i, in := range inputs {
		path := filepath.Join(dir, fmt.Sprintf("%d.jsonl", i))
		if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
			t.Fatal(err)
		}
		var want []string
		sc := bufio.NewScanner(strings.NewReader(in))
		sc.Buffer(make([]byte, entryBuf), maxLineBytes)
		for sc.Scan() {
			want = append(want, sc.Text())
		}
		fh, err := l.open(path)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for l.scan() {
			got = append(got, string(l.bytes()))
		}
		fh.Close()
		if sc.Err() != nil || l.failed() != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("input %d: got %d lines (%v), want %d (%v)", i, len(got), l.failed(), len(want), sc.Err())
		}
	}
}
