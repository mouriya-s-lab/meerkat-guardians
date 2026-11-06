package omprpc

import "encoding/json"

// Event is one stdout frame of an omp RPC process that is not the response to a Call.
// Type is the frame's "type" field; Raw is the whole frame.
type Event struct {
	Type string
	Raw  json.RawMessage
}

// ToolStart is a tool_execution_start frame. omp 18.17 carries the tool arguments only here.
type ToolStart struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
}

// ToolEnd is a tool_execution_end frame. It carries no arguments; correlate with ToolStart by ToolCallID.
type ToolEnd struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
	Result     struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

// PromptResult is the prompt_result frame that completes one accepted prompt.
// Status is "completed", "aborted" or "error"; Error is set only with "error".
type PromptResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

// DecodeToolStart decodes a tool_execution_start event.
func DecodeToolStart(e Event) (ToolStart, bool) {
	var v ToolStart
	return v, e.Type == "tool_execution_start" && json.Unmarshal(e.Raw, &v) == nil
}

// DecodeToolEnd decodes a tool_execution_end event.
func DecodeToolEnd(e Event) (ToolEnd, bool) {
	var v ToolEnd
	return v, e.Type == "tool_execution_end" && json.Unmarshal(e.Raw, &v) == nil
}

// DecodePromptResult decodes a prompt_result event.
func DecodePromptResult(e Event) (PromptResult, bool) {
	var v PromptResult
	return v, e.Type == "prompt_result" && json.Unmarshal(e.Raw, &v) == nil
}
