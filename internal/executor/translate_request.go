package executor

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// buildAgentRunRequest translates a chat-completions request into a
// Cursor AgentRunRequest: the last user message becomes the turn's
// UserMessageAction text, and the model id maps to ModelDetails/
// RequestedModel. Prior turns (system/assistant/tool history) are out of
// scope for this v1 translation pass — see docs/PROTOCOL_SCOPE.md — the
// chat-completions-relevant subset (single-turn user message -> Cursor
// response, including tool-call round-trip via a fresh request per
// fact-r5-tool-roundtrip) is what CLIProxyAPI's chat-completions executor
// contract requires.
func buildAgentRunRequest(req chatCompletionsRequest) (*gen.AgentRunRequest, error) {
	userText := lastUserMessageText(req.Messages)
	messageID, err := newMessageID()
	if err != nil {
		return nil, err
	}

	userMessage := &gen.UserMessage{
		Text:      userText,
		MessageId: messageID,
	}

	action := &gen.ConversationAction{
		Action: &gen.ConversationAction_UserMessageAction{
			UserMessageAction: &gen.UserMessageAction{
				UserMessage: userMessage,
			},
		},
	}

	return &gen.AgentRunRequest{
		ConversationState: &gen.ConversationStateStructure{},
		Action:            action,
		ModelDetails:      &gen.ModelDetails{ModelId: req.Model},
	}, nil
}

func lastUserMessageText(messages []chatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

func newMessageID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
