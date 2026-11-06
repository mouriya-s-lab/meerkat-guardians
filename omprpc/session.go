package omprpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// SessionSpec is one headless omp session held until one of its goal tools succeeds.
type SessionSpec struct {
	Options            Options
	Opening            string   // first prompt
	Continue           string   // prompt sent after each prompt_result that arrives before the goal
	GoalTools          []string // any one succeeding completes the session
	MaxContinuePrompts int
	ReadyTimeout       time.Duration // bounds the wait for the ready frame
	ExitGrace          time.Duration // after stdin closes, the process is killed if it has not exited by then
	// OnReady, if set, is called once the session id and file are known, before the opening prompt.
	OnReady func(sessionID, sessionFile string)
}

// SessionResult is a session whose goal tool succeeded; Args are that call's arguments.
type SessionResult struct {
	SessionID, SessionFile, ToolName string
	Args                             json.RawMessage
}

// SessionFailure is a session that ended without a goal tool succeeding.
type SessionFailure struct{ Reason string }

func (f *SessionFailure) Error() string { return f.Reason }

// RunSession starts omp, sends the opening prompt and holds the session until a goal tool
// succeeds, then closes stdin and waits for exit. It fails when omp exits first, a prompt_result
// reports a non-retryable error, MaxContinuePrompts continue prompts did not reach the goal, or
// ctx ends. LLM retries are omp's; RunSession never retries a turn itself.
func RunSession(ctx context.Context, s SessionSpec) (SessionResult, error) {
	fail := func(reason string) (SessionResult, error) { return SessionResult{}, &SessionFailure{Reason: reason} }

	readyCtx, cancel := context.WithTimeout(ctx, s.ReadyTimeout)
	proc, err := Start(readyCtx, s.Options)
	cancel()
	if err != nil {
		return fail(err.Error())
	}
	res, err := hold(ctx, proc, s)
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
	}
	return res, err
}

func hold(ctx context.Context, proc *Process, s SessionSpec) (SessionResult, error) {
	fail := func(reason string) (SessionResult, error) { return SessionResult{}, &SessionFailure{Reason: reason} }

	data, err := proc.Call(ctx, map[string]any{"type": "get_state"})
	if err != nil {
		return fail(err.Error())
	}
	var state struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
	}
	if err := json.Unmarshal(data, &state); err != nil || state.SessionID == "" {
		return fail(fmt.Sprintf("get_state without sessionId: %s", data))
	}
	if s.OnReady != nil {
		s.OnReady(state.SessionID, state.SessionFile)
	}
	goal := make(map[string]bool, len(s.GoalTools))
	for _, t := range s.GoalTools {
		goal[t] = true
	}

	if err := proc.Prompt(ctx, s.Opening); err != nil {
		return fail(err.Error())
	}
	starts := map[string]json.RawMessage{}
	continues := 0
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err().Error())
		case e, ok := <-proc.Events():
			if !ok {
				return fail(fmt.Sprintf("omp exited before a goal tool %v succeeded: %s", s.GoalTools, proc.StderrTail()))
			}
			if v, ok := DecodeToolStart(e); ok && goal[v.ToolName] {
				starts[v.ToolCallID] = v.Args
				continue
			}
			if v, ok := DecodeToolEnd(e); ok && goal[v.ToolName] && !v.IsError {
				args, ok := starts[v.ToolCallID]
				if !ok {
					return fail(fmt.Sprintf("%s ended without a start event for %s", v.ToolName, v.ToolCallID))
				}
				if err := proc.CloseStdin(); err != nil {
					return fail(err.Error())
				}
				waitExit(proc, s.ExitGrace)
				return SessionResult{SessionID: state.SessionID, SessionFile: state.SessionFile, ToolName: v.ToolName, Args: args}, nil
			}
			if v, ok := DecodePromptResult(e); ok {
				if v.Status == "error" && v.Error != nil && !v.Error.Retryable {
					return fail(v.Error.Message)
				}
				if continues >= s.MaxContinuePrompts {
					return fail("continue limit reached")
				}
				continues++
				if err := proc.Prompt(ctx, s.Continue); err != nil {
					return fail(err.Error())
				}
			}
		}
	}
}

// waitExit waits for a process whose stdin is closed, killing it after grace.
func waitExit(proc *Process, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		_ = proc.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		_ = proc.Kill()
		<-done
	}
}
