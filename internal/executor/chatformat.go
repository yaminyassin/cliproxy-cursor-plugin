package executor

// chatCompletionsRequest is the minimal subset of the OpenAI chat
// completions request format this executor accepts (fact-r2-host-
// conventions: chat-completions is CLIProxyAPI's declared executor input
// format).
type chatCompletionsRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
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
	Error   *chatErrorInfo `json:"error,omitempty"`
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
