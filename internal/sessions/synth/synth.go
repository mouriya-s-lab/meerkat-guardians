// Package synth writes synthetic omp session directories shaped like the ones omp writes under
// the program's sessionsDir, sized for load measurements of the session readers: every turn
// carries a large tool_execution_start data payload and a large tool result, as edit-heavy
// sessions do.
package synth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Layout is one sessionsDir: under bases/<Base> one ready base session (the newest) and N older
// base sessions that ended without meerkat_base_ready; under query/<Base> N query forks of the
// ready base, ask comment ids AskFrom..AskFrom+N-1, every other one settled with meerkat_reply;
// under proxy/<Base> N proxy forks, every other one done with meerkat_return_facts.
type Layout struct {
	Base      string    // base id, config.BaseID(guardian, repo, model)
	Model     string    // model recorded in meerkat-meta
	N         int       // historic base files, query files and proxy files, each
	FileBytes int       // approximate size of every session file
	LineBytes int       // approximate payload size of one large line
	AskFrom   int64     // first query ask comment id
	Start     time.Time // header timestamp of the oldest session; each next one is a minute later
	// Fingerprint is the ready base's meerkat-meta fingerprint (forks copy it).
	Fingerprint json.RawMessage
}

// Written is what WriteDir produced: the session ids it wrote, by role.
type Written struct {
	Ready   string   // the ready base session id
	Bases   []string // older base session ids, oldest first
	Queries []string // query session ids, oldest first
	Proxies []string // proxy session ids, oldest first
}

// WriteDir writes l under root (the sessionsDir).
func WriteDir(root string, l Layout) (Written, error) {
	var w Written
	at := l.Start
	next := func() time.Time { t := at; at = at.Add(time.Minute); return t }
	baseDir := filepath.Join(root, "bases", l.Base)
	for i := range l.N {
		s := session{kind: "base", id: fmt.Sprintf("0b%06d-0000-7000-8000-000000000000", i), started: next()}
		s.meta = l.meta(map[string]any{"kind": "base"})
		if err := write(baseDir, s, l); err != nil {
			return w, err
		}
		w.Bases = append(w.Bases, s.id)
	}
	ready := session{kind: "base", id: "0bffffff-0000-7000-8000-000000000000", started: next(), goal: "meerkat_base_ready"}
	ready.meta = l.meta(map[string]any{"kind": "base"})
	if err := write(baseDir, ready, l); err != nil {
		return w, err
	}
	w.Ready = ready.id
	for i := range l.N {
		s := session{kind: "query", id: fmt.Sprintf("0c%06d-0000-7000-8000-000000000000", i), started: next(), parent: &ready}
		s.meta = l.meta(map[string]any{"kind": "query", "ref": "mouriya-s-lab/meerkat-e2e-code#1", "ask": l.AskFrom + int64(i), "tier": "simple"})
		if i%2 == 0 {
			s.goal = "meerkat_reply"
		}
		if err := write(filepath.Join(root, "query", l.Base), s, l); err != nil {
			return w, err
		}
		w.Queries = append(w.Queries, s.id)
	}
	for i := range l.N {
		s := session{kind: "proxy", id: fmt.Sprintf("0d%06d-0000-7000-8000-000000000000", i), started: next(), parent: &ready}
		s.meta = l.meta(map[string]any{"kind": "proxy", "ref": "mouriya-s-lab/meerkat-e2e-agenda#1", "seat": "ClaudeHigh"})
		if i%2 == 0 {
			s.goal = "meerkat_return_facts"
		}
		if err := write(filepath.Join(root, "proxy", l.Base), s, l); err != nil {
			return w, err
		}
		w.Proxies = append(w.Proxies, s.id)
	}
	return w, nil
}

func (l Layout) meta(fields map[string]any) json.RawMessage {
	fields["base"] = l.Base
	fields["model"] = l.Model
	if len(l.Fingerprint) > 0 {
		fields["fingerprint"] = l.Fingerprint
	}
	b, err := json.Marshal(fields)
	if err != nil {
		panic(err) // fields are strings, numbers and raw JSON
	}
	return b
}

type session struct {
	kind    string
	id      string
	started time.Time
	meta    json.RawMessage
	goal    string   // goal tool that succeeds at the end; "" for a session that never reached it
	parent  *session // a fork copies the parent's history, entries timestamped before its header
}

// fileName is the name omp gives a session file: <header timestamp>_<session id>.jsonl.
func fileName(id string, started time.Time) string {
	return strings.NewReplacer(":", "-", ".", "-").Replace(started.UTC().Format("2006-01-02T15:04:05.000Z")) + "_" + id + ".jsonl"
}

func write(dir string, s session, l Layout) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	line := func(v any) {
		j, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	ts := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
	line(map[string]any{"type": "title", "v": 1, "title": "", "updatedAt": ts(s.started), "pad": strings.Repeat(" ", 160)})
	header := map[string]any{"type": "session", "version": 3, "id": s.id, "timestamp": ts(s.started), "cwd": "/checkouts/" + s.kind}
	if s.parent != nil {
		header["parentSession"] = s.parent.id
	}
	line(header)
	at := s.started
	if s.parent != nil {
		// Copied parent history: its meerkat-meta and a few turns, all before this header.
		pt := s.parent.started
		line(map[string]any{"type": "custom", "customType": "meerkat-meta", "data": s.parent.meta, "id": "p0", "timestamp": ts(pt)})
		for i := range 3 {
			pt = pt.Add(time.Second)
			turn(line, ts(pt), i, "read", l.LineBytes/4)
		}
		pt = pt.Add(time.Second)
		line(toolResult(ts(pt), "p-goal", s.parent.goal, false, 64))
		at = at.Add(time.Second)
	}
	line(map[string]any{"type": "custom", "customType": "meerkat-meta", "data": s.meta, "id": "m0", "timestamp": ts(at)})
	for i := 0; b.Len() < l.FileBytes; i++ {
		at = at.Add(time.Second)
		turn(line, ts(at), i, "edit", l.LineBytes)
	}
	at = at.Add(time.Second)
	if s.goal != "" {
		line(toolResult(ts(at), "goal", s.goal, false, 64))
	} else {
		line(map[string]any{"type": "message", "id": "e", "timestamp": ts(at), "message": map[string]any{
			"role": "assistant", "content": []any{}, "stopReason": "error", "errorMessage": "synthetic provider error"}})
	}
	return os.WriteFile(filepath.Join(dir, fileName(s.id, s.started)), []byte(b.String()), 0o644)
}

// turn writes one assistant tool call: the assistant message, the tool_execution_start custom
// entry carrying the call's arguments as data, and the tool result.
func turn(line func(any), ts string, i int, tool string, size int) {
	call := fmt.Sprintf("toolu_%06d", i)
	payload := filler(size)
	line(map[string]any{"type": "message", "id": fmt.Sprintf("a%d", i), "timestamp": ts, "message": map[string]any{
		"role": "assistant", "stopReason": "toolUse",
		"content": []any{map[string]any{"type": "toolCall", "id": call, "name": tool, "arguments": map[string]any{"path": "f.go"}}},
	}})
	line(map[string]any{"type": "custom", "customType": "tool_execution_start", "id": fmt.Sprintf("x%d", i), "timestamp": ts, "data": map[string]any{
		"toolCallId": call, "toolName": tool, "startedAt": ts,
		"args": map[string]any{"path": "f.go", "content": payload}, "intent": "edit",
	}})
	line(toolResult(ts, call, tool, false, size))
}

func toolResult(ts, call, tool string, isError bool, size int) map[string]any {
	return map[string]any{"type": "message", "id": "r" + call, "timestamp": ts, "message": map[string]any{
		"role": "toolResult", "toolCallId": call, "toolName": tool, "isError": isError,
		"content": []any{map[string]any{"type": "text", "text": filler(size)}},
	}}
}

// filler is size bytes of source-like text with escapes, so decoders do real string work.
func filler(size int) string {
	const chunk = "func f() {\n\treturn \"x\" // \u00e9\n}\n"
	var b strings.Builder
	b.Grow(size)
	for b.Len() < size {
		b.WriteString(chunk)
	}
	return b.String()
}
