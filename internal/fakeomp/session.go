package fakeomp

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// sessionFile is the omp session file a session-dir run writes. The header follows omp's
// v3 form; each tool call appends the entries omp records for one (assistant toolCall
// message, tool_execution_start, tool_execution_end, toolResult message). The file exists
// from before the ready frame, so a session that never reaches its goal still has it
// (acceptance row 2), and get_state answers with its path (row 1).
type sessionFile struct {
	path string // the .jsonl path get_state reports
	id   string // the session id in the header and get_state
	fh   *os.File
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
	Version int    `json:"version,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
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

// openSessionFile creates the session file under dir in omp's naming and writes the
// session header, then meta (when non-empty) as the pm-meta custom entry the council
// plugin appends at session start.
func openSessionFile(dir string, meta json.RawMessage) (*sessionFile, error) {
	id, err := sessionID()
	if err != nil {
		return nil, err
	}
	name := nowStamp() + "_" + id + ".jsonl"
	path := filepath.Join(dir, name)
	fh, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("fake-omp: session file: %w", err)
	}
	s := &sessionFile{path: path, id: id, fh: fh}
	if err := s.write(sessionEntry{Type: "session", Version: 3, ID: id, Timestamp: nowStamp(),
		Runtime: runtimeInfo{Version: "fake-omp"}, Cwd: cwd()}); err != nil {
		fh.Close()
		return nil, err
	}
	if len(meta) > 0 {
		if err := s.write(sessionEntry{Type: "custom", CustomType: "pm-meta", ID: shortID(), Timestamp: nowStamp(),
			Runtime: runtimeInfo{Version: "fake-omp"}, Data: meta}); err != nil {
			fh.Close()
			return nil, err
		}
	}
	return s, nil
}

// appendToolCall records one tool execution: the assistant message whose content carries
// the toolCall, the tool_execution_start and tool_execution_end custom events, and the
// toolResult message that closes the call — omp's chain, replayed.
func (s *sessionFile) appendToolCall(callID, name, argsJSON, resultJSON string) {
	parent := ""
	appendOne := func(e sessionEntry) {
		e.ID = shortID()
		e.ParentID = parent
		e.Timestamp = nowStamp()
		parent = e.ID
		s.write(e)
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

// close flushes and closes the file.
func (s *sessionFile) close() { s.fh.Close() }

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
