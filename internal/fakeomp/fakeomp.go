// Package fakeomp speaks the omp RPC protocol as a deterministic fake omp, driven by
// environment variables (pm-guardians#20):
//
//	FAKE_OMP_PLAN      comma-separated steps goal/text/retry/fatal/loop/stall/die; the last repeats
//	FAKE_OMP_GOAL      goal tool name; default: the first entry of the comma list the starting
//	                   program hands its plugin, PM_GOAL_TOOL (pm-guardians) or PM_GOAL_TOOLS
//	                   (pm-council)
//	FAKE_OMP_ARGS      path of a JSON file with the goal tool's arguments
//	FAKE_OMP_DELAY_MS  delay before each step, in milliseconds
//
// Steps: goal calls the goal tool (its arguments from FAKE_OMP_ARGS, else the prompt text
// as {"facts": …}); text completes the turn with an assistant text message and no tool
// call; retry and fatal end the turn with a retryable resp. non-retryable prompt_result
// error; loop keeps calling the non-goal tool read and never completes the turn; stall
// acknowledges the prompt, then writes nothing and outputs nothing until killed; die writes
// stderr and exits 3.
//
// The session file follows omp's timing (pm-guardians#26): a fresh session's file is created
// with its first assistant message, so a session that stalls or fails before one has none;
// --fork <source> creates it before the ready frame, its header's parentSession the source
// header's id (the source's entries are not copied).
//
// PM_META, when set, is written as the session's pm-meta custom entry right after the
// header, as the council plugin does at session start (pm-guardians#22); a PM_META that is
// not JSON fails the run before the ready frame.
//
// The omprpc test binary (TestMain) and cmd/fake-omp both call Main; there is no second
// copy. fakeomp imports nothing from omprpc: it is the server side of the protocol.
package fakeomp

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Main runs one fake omp process over the given streams and returns its exit code. args
// are the run's own arguments (--session-dir and --fork are read from them, the rest
// ignored); nothing else is read from the process beyond the control environment variables.
// A die as the plan's first step writes stderr and exits 3 before the ready frame, so Start
// fails at startup with the stderr tail (acceptance row 3).
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	plan, err := parsePlan(os.Getenv("FAKE_OMP_PLAN"))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(plan) > 0 && plan[0] == stepDie {
		fmt.Fprintln(stderr, "fatal: model not found: nope/nope")
		return 3
	}
	if err := run(args, stdin, stdout, stderr, plan); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// run speaks the RPC protocol: one stderr line above the ready frame, then one response
// per newline-delimited command until stdin ends, prompts driving the plan's steps in
// order with the last step repeating.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, plan []step) error {
	meta, err := pmMeta()
	if err != nil {
		return err
	}
	dir, ok := parseFlag(args, "--session-dir")
	if !ok {
		return errors.New("fake-omp: --session-dir is required")
	}
	parent := ""
	source, fork := parseFlag(args, "--fork")
	if fork {
		if parent, err = forkSourceID(source); err != nil {
			return err
		}
	}
	sess, err := newSessionFile(dir, meta, parent)
	if err != nil {
		return err
	}
	defer sess.close()
	if fork {
		if err := sess.start(); err != nil {
			return err
		}
	}

	fmt.Fprintln(stderr, "booting") // stderr-tail coverage above the ready frame

	delay, _ := strconv.Atoi(os.Getenv("FAKE_OMP_DELAY_MS"))
	fake := engine{plan: plan, goal: goalTool(), delay: time.Duration(delay) * time.Millisecond}

	enc := json.NewEncoder(stdout)
	_ = enc.Encode(map[string]any{"type": "ready"})

	sc := bufio.NewScanner(stdin)
	prompts := 0
	for sc.Scan() {
		var cmd command
		if json.Unmarshal(sc.Bytes(), &cmd) != nil {
			continue
		}
		switch cmd.Type {
		case "get_state":
			_ = enc.Encode(map[string]any{"type": "response", "id": cmd.ID, "command": "get_state",
				"success": true, "data": sess.state()})
		case "prompt":
			_ = enc.Encode(map[string]any{"type": "response", "id": cmd.ID, "command": "prompt", "success": true})
			if err := fake.turn(min(prompts, len(plan)-1), cmd, sess, enc); err != nil {
				return err
			}
			prompts++
		default:
			_ = enc.Encode(map[string]any{"type": "response", "id": cmd.ID, "command": cmd.Type,
				"success": false, "error": "Unknown command"})
		}
	}
	return sc.Err()
}

// pmMeta is PM_META as raw JSON; empty when unset. The council plugin parses the same
// variable at load and fails on invalid JSON, so the fake does too.
func pmMeta() (json.RawMessage, error) {
	v := os.Getenv("PM_META")
	if v == "" {
		return nil, nil
	}
	if !json.Valid([]byte(v)) {
		return nil, fmt.Errorf("fake-omp: PM_META is not JSON: %s", v)
	}
	return json.RawMessage(v), nil
}

// engine drives one run: the parsed plan, the goal tool and the per-step delay.
type engine struct {
	plan  []step
	goal  string
	delay time.Duration
}

// step names one plan entry.
type step int

const (
	stepGoal step = iota
	stepText
	stepRetry
	stepFatal
	stepLoop
	stepStall
	stepDie
)

// parsePlan reads the comma-separated plan; the empty plan is a single turn without a goal
// call. An unknown name is an error up front, before the ready frame.
func parsePlan(s string) ([]step, error) {
	names := []string{"text"}
	if s != "" {
		names = strings.Split(s, ",")
	}
	byName := map[string]step{
		"goals": stepGoal, "goal": stepGoal,
		"text": stepText, "retry": stepRetry, "fatal": stepFatal,
		"loop": stepLoop, "stall": stepStall, "die": stepDie,
	}
	plan := make([]step, len(names))
	for i, n := range names {
		st, ok := byName[strings.TrimSpace(n)]
		if !ok {
			return nil, fmt.Errorf("fake-omp: unknown plan step %q", strings.TrimSpace(n))
		}
		plan[i] = st
	}
	return plan, nil
}

// goalTool reads the goal tool name: FAKE_OMP_GOAL, else the first entry of the comma
// list the starting program hands its plugin (pm-guardians sets PM_GOAL_TOOL, pm-council
// PM_GOAL_TOOLS; a program sets only its own), else a default that still names a goal tool.
func goalTool() string {
	if g := os.Getenv("FAKE_OMP_GOAL"); g != "" {
		return g
	}
	list := os.Getenv("PM_GOAL_TOOL")
	if list == "" {
		list = os.Getenv("PM_GOAL_TOOLS")
	}
	for _, t := range strings.Split(list, ",") {
		if t = strings.TrimSpace(t); t != "" {
			return t
		}
	}
	return "pm_return_facts"
}

// command is one command line of the omp RPC protocol.
type command struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

// turn performs the plan step i for the accepted prompt cmd; the prompt acknowledgment is
// already written when this is called. It fails only when the session file cannot be written.
func (e engine) turn(i int, cmd command, sess *sessionFile, enc *json.Encoder) error {
	if e.delay > 0 {
		time.Sleep(e.delay)
	}
	switch e.plan[i] {
	case stepGoal:
		if err := e.call(sess, enc, e.goal, goalArgs(cmd.Message), `{"content":[{"type":"text","text":"recorded"}]}`); err != nil {
			return err
		}
		_ = enc.Encode(promptResult(cmd.ID, "completed", nil))
	case stepText:
		// Text streams as message_update frames while the client is busy: a burst
		// exercises the unbounded event queue between the stdout reader and callers
		// that wait on a Call response. The frames carry no text; the finished message
		// records the fixed reply "done".
		for i := range 500 {
			_ = enc.Encode(map[string]any{"type": "message_update", "n": i})
		}
		if err := sess.appendText("done"); err != nil {
			return err
		}
		_ = enc.Encode(promptResult(cmd.ID, "completed", nil))
	case stepRetry:
		_ = enc.Encode(promptResult(cmd.ID, "error", &promptError{Message: "429 slow down", Retryable: true}))
	case stepFatal:
		_ = enc.Encode(promptResult(cmd.ID, "error", &promptError{Message: "400 model rejected the request"}))
	case stepLoop:
		// A turn that never completes: one non-goal tool call after another.
		for {
			if err := e.call(sess, enc, "read", map[string]any{"path": "store.go"}, ""); err != nil {
				return err
			}
			time.Sleep(20 * time.Millisecond)
		}
	case stepStall:
		// A turn stuck before its first assistant message: nothing more on stdout or in the
		// session file, stdin left unread, until the process is killed. Sleeping rather than
		// blocking on a channel keeps the runtime's deadlock detector quiet.
		for {
			time.Sleep(time.Hour)
		}
	case stepDie:
		// die after the ready frame: drop stdout and exit; stdin stays unread.
		os.Exit(3)
	}
	return nil
}

// call emits one tool execution: the RPC frames, then the omp session entries (the
// assistant toolCall message, the tool_execution_start/end events and the toolResult
// message that closes the call).
func (e engine) call(sess *sessionFile, enc *json.Encoder, name string, args map[string]any, resultJSON string) error {
	id := callID()

	_ = enc.Encode(map[string]any{"type": "tool_execution_start", "toolCallId": id, "toolName": name, "args": args})
	result := json.RawMessage(resultJSON)
	if len(result) == 0 {
		result = json.RawMessage(`{"content":[]}`)
	}
	_ = enc.Encode(map[string]any{"type": "tool_execution_end", "toolCallId": id, "toolName": name,
		"isError": false, "result": result})

	argsJSON, _ := json.Marshal(args)
	return sess.appendToolCall(id, name, string(argsJSON), resultJSON)
}

// promptResult is a prompt_result frame.
func promptResult(id, status string, perr *promptError) map[string]any {
	r := map[string]any{"type": "prompt_result", "id": id, "agentInvoked": true, "status": status}
	if perr != nil {
		r["error"] = perr
	}
	return r
}

// promptError is a prompt_result error payload; only retryable errors retry.
type promptError struct {
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// goalArgs resolves the goal tool's arguments: the FAKE_OMP_ARGS JSON file's object, or
// the prompt text as {"facts": …} when no file is named.
func goalArgs(prompt string) map[string]any {
	if path := os.Getenv("FAKE_OMP_ARGS"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var v map[string]any
			if json.Unmarshal(b, &v) == nil {
				return v
			}
		}
	}
	return map[string]any{"facts": prompt}
}

// parseFlag reads name X or name=X from args, the last one wins; every other argument is
// ignored.
func parseFlag(args []string, name string) (string, bool) {
	val := ""
	ok := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == name:
			if i+1 < len(args) {
				val, ok = args[i+1], true
				i++
			}
		case strings.HasPrefix(a, name+"="):
			val, ok = strings.TrimPrefix(a, name+"="), true
		}
	}
	return val, ok
}

// callID is a fresh RFC 4122 version-4 UUID string for a tool call.
func callID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails on a working system
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sessionID is a fresh RFC 4122 version-7 UUID string, omp's session id form.
func sessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// version 7: timestamp ms in the top 48 bits, version nibble, random, variant, random.
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// nowStamp is omp's session-file timestamp: UTC, RFC 3339, milliseconds, Z.
func nowStamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
