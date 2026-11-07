package fakeomp

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// sessionFile is the omp session file a session-dir run writes, created when omp creates
// it: a fresh session's file appears with its first assistant message, a --fork session's
// before the ready frame. get_state answers with the path either way (pm-guardians#26). The
// header follows omp's v3 form; each tool call appends the entries omp records for one
// (assistant toolCall message, tool_execution_start, tool_execution_end, toolResult
// message).
type sessionFile struct {
	path   string          // the .jsonl path get_state reports
	id     string          // the session id in the header and get_state
	parent string          // the fork source's session id, the header's parentSession; empty for a fresh session
	meta   json.RawMessage // PM_META, written as the pm-meta entry right after the header
	fh     *os.File        // nil until start creates the file
}

// sessionEntry is what every non-title line of an omp session file carries; header-only
// and message-only fields stay empty elsewhere.
type sessionEntry struct {
	Type       string          `json:"type"`
	ID         string          `json:"id,omitempty"`
	ParentID   string          `json:"parentId,omitempty"`
	Timestamp  string          `json:"timestamp"`
	Runtime    runtimeInfo     `json:"runtime"`
	CustomType string          `json:"customType,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Message    *entryMessage   `json:"message,omitempty"`
	// session header only
	Version       int    `json:"version,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	ParentSession string `json:"parentSession,omitempty"`
}

// runtimeInfo is the runtime object every entry carries.
type runtimeInfo struct {
	Version string `json:"version"`
}

// entryMessage is the message object of a message entry.
type entryMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
	Timestamp  int64           `json:"timestamp,omitempty"`
}

// newSessionFile names the session file under dir in omp's naming without touching the
// disk; parent is the fork source's session id, empty for a fresh session.
func newSessionFile(dir string, meta json.RawMessage, parent string) (*sessionFile, error) {
	id, err := sessionID()
	if err != nil {
		return nil, err
	}
	return &sessionFile{path: filepath.Join(dir, nowStamp()+"_"+id+".jsonl"), id: id, parent: parent, meta: meta}, nil
}

// start creates the file and writes the session header, then meta (when non-empty) as the
// pm-meta custom entry the council plugin appends at session start.
func (s *sessionFile) start() error {
	fh, err := os.Create(s.path)
	if err != nil {
		return fmt.Errorf("fake-omp: session file: %w", err)
	}
	s.fh = fh
	if err := s.write(sessionEntry{Type: "session", Version: 3, ID: s.id, Timestamp: nowStamp(),
		Runtime: runtimeInfo{Version: "fake-omp"}, Cwd: cwd(), ParentSession: s.parent}); err != nil {
		return err
	}
	if len(s.meta) > 0 {
		return s.write(sessionEntry{Type: "custom", CustomType: "pm-meta", ID: shortID(), Timestamp: nowStamp(),
			Runtime: runtimeInfo{Version: "fake-omp"}, Data: s.meta})
	}
	return nil
}

// forkSourceID reads the session id from the header line of the fork source file.
func forkSourceID(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("fake-omp: --fork: %w", err)
	}
	defer fh.Close()
	var h struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(fh).Decode(&h); err != nil || h.Type != "session" || h.ID == "" {
		return "", fmt.Errorf("fake-omp: --fork %s: first line is not a session header", path)
	}
	return h.ID, nil
}

// appendToolCall records one tool execution: the assistant message whose content carries
// the toolCall, the tool_execution_start and tool_execution_end custom events, and the
// toolResult message that closes the call — omp's chain, replayed. The first assistant
// message of a fresh session creates the file.
func (s *sessionFile) appendToolCall(callID, name, argsJSON, resultJSON string) error {
	if s.fh == nil {
		if err := s.start(); err != nil {
			return err
		}
	}
	parent := ""
	var err error
	appendOne := func(e sessionEntry) {
		e.ID = shortID()
		e.ParentID = parent
		e.Timestamp = nowStamp()
		parent = e.ID
		if err == nil {
			err = s.write(e)
		}
	}
	appendOne(sessionEntry{Type: "message", Runtime: runtimeInfo{Version: "fake-omp"},
		Message: &entryMessage{Role: "assistant", Content: json.RawMessage(fmt.Sprintf(
			`[{"type":"toolCall","id":%q,"name":%q,"arguments":%s,"partialArgs":%q,"streamIndex":0,"intent":%q}]`,
			callID, name, argsJSON, argsJSON, name))}})
	appendOne(sessionEntry{Type: "custom", CustomType: "tool_execution_start", Runtime: runtimeInfo{Version: "fake-omp"},
		Data: json.RawMessage(fmt.Sprintf(`{"toolCallId":%q,"toolName":%q,"startedAt":%q}`, callID, name, nowStamp()))})
	appendOne(sessionEntry{Type: "custom", CustomType: "tool_execution_end", Runtime: runtimeInfo{Version: "fake-omp"},
		Data: json.RawMessage(fmt.Sprintf(`{"toolCallId":%q,"toolName":%q,"startedAt":%q,"endedAt":%q,"isError":false}`,
			callID, name, nowStamp(), nowStamp()))})
	appendOne(sessionEntry{Type: "message", Runtime: runtimeInfo{Version: "fake-omp"},
		Message: &entryMessage{Role: "toolResult", ToolCallID: callID, ToolName: name,
			Content:   json.RawMessage(resultContent(resultJSON)),
			IsError:   false,
			Timestamp: time.Now().UnixMilli()}})
	return err
}

// write appends one entry line.
func (s *sessionFile) write(e sessionEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := s.fh.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// resultContent unwraps the tool_execution_end result JSON to a toolResult message's
// content array; an empty result is a call with nothing to read back.
func resultContent(resultJSON string) string {
	if resultJSON == "" {
		return ""
	}
	var r struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal([]byte(resultJSON), &r) == nil && len(r.Content) > 0 {
		return string(r.Content)
	}
	return resultJSON
}

// close closes the file, if start created it.
func (s *sessionFile) close() {
	if s.fh != nil {
		s.fh.Close()
	}
}

// state is what get_state answers: the session id and file path.
func (s *sessionFile) state() map[string]string {
	return map[string]string{"sessionId": s.id, "sessionFile": s.path}
}

// cwd is the working directory recorded in the session header; an unreadable one stays empty.
func cwd() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}

// shortID is an 8-hex id like omp's entry ids.
func shortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}
