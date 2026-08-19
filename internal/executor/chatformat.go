package executor

import (
	"encoding/json"
	"fmt"
	"strings"
)

// chatCompletionsRequest is the minimal subset of the OpenAI chat
// completions request format this executor accepts (fact-r2-host-
// conventions: chat-completions is CLIProxyAPI's declared executor input
// format).
type chatCompletionsRequest struct {
	Model    string               `json:"model"`
	Stream   bool                 `json:"stream"`
	Messages []chatMessage        `json:"messages"`
	Tools    []chatToolDefinition `json:"tools,omitempty"`
}

// chatToolDefinition mirrors OpenAI's client-supplied tools[] entry
// shape ({"type":"function","function":{name,description,parameters}}):
// a custom tool/function set the local client wants Cursor's model able
// to call, translated into Cursor's AgentRunRequest.mcp_tools
// (McpToolDefinition) - see translate_request.go's buildMcpTools. This
// is distinct from Cursor's own native tools (shell/read/glob/...),
// which the model can request independent of what the client declares
// here.
type chatToolDefinition struct {
	Type     string                 `json:"type"`
	Function chatToolDefinitionFunc `json:"function"`
}

type chatToolDefinitionFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// UnmarshalJSON accepts BOTH OpenAI content forms for a message:
//
//	"content": "plain text"
//	"content": [{"type":"text","text":"..."}, ...]
//
// The array (content-parts) form is what real OpenAI-compatible clients
// send for multimodal or structured messages, and a live E2E run
// (2026-08-19) found that rejecting it broke every request from such a
// client with `cannot unmarshal array into Go struct field
// chatMessage.messages.content of type string`. Only text parts are
// carried through (Cursor's UserMessage.Text is plain text); non-text
// parts such as image_url are skipped rather than failing the whole
// request, so an image-bearing message still delivers its text instead
// of erroring out.
//
// Marshaling deliberately keeps default behavior (Content emitted as a
// plain string), which is the shape this plugin RETURNS.
func (m *chatMessage) UnmarshalJSON(data []byte) error {
	type chatMessageWire struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
	}
	var wire chatMessageWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	content, err := decodeChatContent(wire.Content)
	if err != nil {
		return err
	}
	m.Role = wire.Role
	m.Content = content
	m.ToolCalls = wire.ToolCalls
	m.ToolCallID = wire.ToolCallID
	return nil
}

// decodeChatContent flattens either OpenAI content form into plain text.
func decodeChatContent(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("cursor: message content must be a string or an array of content parts: %w", err)
	}

	var b strings.Builder
	for _, part := range parts {
		if part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(part.Text)
	}
	return b.String(), nil
}

// chatToolCall mirrors OpenAI's tool_calls entry shape: Cursor tool calls
// are surfaced this way per fact-r5-tool-roundtrip, batched per turn into
// one array rather than one entry per streamed oneof event.
type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatToolCallFunc `json:"function"`
}

type chatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatCompletionsResponse is the minimal non-streaming chat-completions
// response shape this executor emits.
type chatCompletionsResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []chatChoice   `json:"choices"`
	Usage   chatUsage      `json:"usage"`
	Error   *chatErrorInfo `json:"error,omitempty"`
}

// chatUsage is the OpenAI usage block. Real OpenAI-compatible clients
// treat a missing or all-zero usage block as a failed/anomalous response
// (a live E2E run on 2026-08-19 had a client reject otherwise-valid
// answers with "empty response with anomalously low token usage"), so
// this is always emitted. Completion tokens come from Cursor's own
// InteractionUpdate.TokenDelta stream; Cursor does not report prompt
// tokens, so they are estimated from the request text rather than
// reported as a bare zero.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// chatErrorInfo is a terminal-error signal for the mid-stream failure
// contract (never silent truncation): if Cursor's stream drops after
// partial content was already produced, this is set on the final
// response/chunk instead of returning a truncated success.
type chatErrorInfo struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// chatCompletionChunk is the OpenAI *streaming* response object. A
// streaming client parses choices[].delta (NOT choices[].message) and
// requires object == "chat.completion.chunk"; emitting a non-streaming
// chat.completion object on an SSE stream makes such a client accumulate
// nothing and report an empty response (observed live on 2026-08-19,
// where every streaming client got a silent empty answer even though the
// non-streaming surface returned correct content).
type chatCompletionChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *chatUsage        `json:"usage,omitempty"`
	Error   *chatErrorInfo    `json:"error,omitempty"`
}

type chatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        chatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type chatChunkDelta struct {
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

// toStreamChunks renders a completed turn as the OpenAI streaming chunk
// sequence: one content chunk carrying the assistant delta, then a
// terminal chunk carrying finish_reason and usage. The turn is still
// produced in full before the first chunk is emitted (the documented
// buffered-streaming scope boundary), but the WIRE FORMAT is now the
// streaming format clients actually parse.
func (r chatCompletionsResponse) toStreamChunks() []chatCompletionChunk {
	delta := chatChunkDelta{Role: "assistant"}
	finishReason := "stop"
	if len(r.Choices) > 0 {
		delta.Content = r.Choices[0].Message.Content
		delta.ToolCalls = r.Choices[0].Message.ToolCalls
		if r.Choices[0].FinishReason != "" {
			finishReason = r.Choices[0].FinishReason
		}
	}

	content := chatCompletionChunk{
		ID:      r.ID,
		Object:  "chat.completion.chunk",
		Model:   r.Model,
		Choices: []chatChunkChoice{{Index: 0, Delta: delta, FinishReason: nil}},
		Error:   r.Error,
	}
	usage := r.Usage
	terminal := chatCompletionChunk{
		ID:      r.ID,
		Object:  "chat.completion.chunk",
		Model:   r.Model,
		Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{}, FinishReason: &finishReason}},
		Usage:   &usage,
	}
	return []chatCompletionChunk{content, terminal}
}
