package omprpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake omp: with FAKE_OMP set it speaks the RPC protocol on stdio.
func TestMain(m *testing.M) {
	switch os.Getenv("FAKE_OMP") {
	case "":
		os.Exit(m.Run())
	case "die":
		fmt.Fprintln(os.Stderr, "fatal: model not found: nope/nope")
		os.Exit(3)
	case "rpc":
		fakeRPC()
		os.Exit(0)
	case "session":
		fakeSession()
		os.Exit(0)
	}
}

func fakeRPC() {
	out := json.NewEncoder(os.Stdout)
	// A frame before ready is not part of the protocol; emit stderr noise instead.
	fmt.Fprintln(os.Stderr, "booting")
	_ = out.Encode(map[string]any{"type": "ready", "protocolVersion": 1})
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var cmd map[string]any
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			continue
		}
		id, _ := cmd["id"].(string)
		switch cmd["type"] {
		case "get_state":
			_ = out.Encode(map[string]any{"type": "response", "id": id, "command": "get_state", "success": true,
				"data": map[string]any{"sessionId": "s-1", "sessionFile": "/tmp/s-1.jsonl"}})
		case "prompt":
			_ = out.Encode(map[string]any{"type": "response", "id": id, "command": "prompt", "success": true})
			// Many events while the client is not reading: exercises the unbounded queue.
			for i := range 500 {
				_ = out.Encode(map[string]any{"type": "message_update", "n": i})
			}
			_ = out.Encode(map[string]any{"type": "tool_execution_start", "toolCallId": "t1", "toolName": "pm_return_facts",
				"args": map[string]any{"facts": "main.go:15"}})
			_ = out.Encode(map[string]any{"type": "tool_execution_end", "toolCallId": "t1", "toolName": "pm_return_facts",
				"isError": false, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "recorded"}}}})
			_ = out.Encode(map[string]any{"type": "prompt_result", "id": id, "agentInvoked": true, "status": "error",
				"error": map[string]any{"message": "429 rate limited", "retryable": true}})
		default:
			_ = out.Encode(map[string]any{"type": "response", "id": id, "command": cmd["type"], "success": false,
				"error": "Unknown command"})
		}
	}
}

func startFake(t *testing.T, mode string) (*Process, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Start(ctx, Options{Bin: exe, Env: []string{"FAKE_OMP=" + mode}})
}

func TestStartFailsWithStderrTail(t *testing.T) {
	_, err := startFake(t, "die")
	if err == nil || !strings.Contains(err.Error(), "model not found: nope/nope") {
		t.Fatalf("want error with stderr tail, got %v", err)
	}
}

func TestCallAndEvents(t *testing.T) {
	p, err := startFake(t, "rpc")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	data, err := p.Call(ctx, map[string]any{"type": "get_state"})
	if err != nil {
		t.Fatal(err)
	}
	var st struct{ SessionID string }
	if err := json.Unmarshal(data, &st); err != nil || st.SessionID != "s-1" {
		t.Fatalf("get_state data %s (%v)", data, err)
	}

	if _, err := p.Call(ctx, map[string]any{"type": "bogus"}); err == nil ||
		!strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "Unknown command") {
		t.Fatalf("want failure naming command and message, got %v", err)
	}

	if err := p.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	var (
		updates int
		start   ToolStart
		end     ToolEnd
		result  PromptResult
	)
	for e := range p.Events() {
		if e.Type == "response" {
			t.Fatalf("response leaked into events: %s", e.Raw)
		}
		if e.Type == "message_update" {
			updates++
		}
		if v, ok := DecodeToolStart(e); ok {
			start = v
		}
		if v, ok := DecodeToolEnd(e); ok {
			end = v
		}
		if v, ok := DecodePromptResult(e); ok {
			result = v
			break
		}
	}
	if updates != 500 {
		t.Fatalf("updates = %d", updates)
	}
	if start.ToolCallID != "t1" || start.ToolName != "pm_return_facts" || string(start.Args) != `{"facts":"main.go:15"}` {
		t.Fatalf("start = %+v", start)
	}
	if end.ToolCallID != "t1" || end.IsError || len(end.Result.Content) != 1 || end.Result.Content[0].Text != "recorded" {
		t.Fatalf("end = %+v", end)
	}
	if result.Status != "error" || result.Error == nil || !result.Error.Retryable || result.Error.Message != "429 rate limited" {
		t.Fatalf("result = %+v", result)
	}

	if err := p.CloseStdin(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(p.StderrTail(), "booting") {
		t.Fatalf("stderr tail = %q", p.StderrTail())
	}
}

func TestDecodersRejectOtherTypes(t *testing.T) {
	e := Event{Type: "tool_execution_end", Raw: json.RawMessage(`{"type":"tool_execution_end","toolCallId":"x"}`)}
	if _, ok := DecodeToolStart(e); ok {
		t.Fatal("DecodeToolStart accepted tool_execution_end")
	}
	if _, ok := DecodePromptResult(e); ok {
		t.Fatal("DecodePromptResult accepted tool_execution_end")
	}
	if v, ok := DecodeToolEnd(e); !ok || v.ToolCallID != "x" {
		t.Fatalf("DecodeToolEnd = %+v %v", v, ok)
	}
}
