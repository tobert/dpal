package server

import (
	"context"
	"fmt"
	"strings"

	deepseek "github.com/cohesion-org/deepseek-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DeepSeekClient is the subset of the deepseek-go client surface dpal uses.
// Defined as an interface so handlers can be tested without network access.
type DeepSeekClient interface {
	CreateChatCompletion(ctx context.Context, req *deepseek.ChatCompletionRequest) (*deepseek.ChatCompletionResponse, error)
}

type Server struct {
	client       DeepSeekClient
	defaultModel string
	sessions     *Sessions
}

func New(client DeepSeekClient) *Server {
	return &Server{
		client:       client,
		defaultModel: deepseek.DeepSeekReasoner,
		sessions:     NewSessions(defaultSessionTTL, defaultMaxSessions),
	}
}

type OneshotInput struct {
	Prompt string `json:"prompt" jsonschema:"the question or instruction to send to DeepSeek"`
	Model  string `json:"model,omitempty" jsonschema:"optional model override; defaults to deepseek-reasoner"`
}

type OneshotOutput struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Model            string `json:"model"`
}

func (s *Server) ConsultOneshot(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in OneshotInput,
) (*mcp.CallToolResult, OneshotOutput, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, OneshotOutput{}, fmt.Errorf("prompt is required")
	}

	model := in.Model
	if model == "" {
		model = s.defaultModel
	}

	resp, err := s.client.CreateChatCompletion(ctx, &deepseek.ChatCompletionRequest{
		Model: model,
		Messages: []deepseek.ChatCompletionMessage{
			{Role: deepseek.ChatMessageRoleUser, Content: in.Prompt},
		},
	})
	if err != nil {
		return nil, OneshotOutput{}, fmt.Errorf("deepseek call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, OneshotOutput{}, fmt.Errorf("deepseek returned no choices")
	}

	msg := resp.Choices[0].Message
	out := OneshotOutput{
		Content:          msg.Content,
		ReasoningContent: msg.ReasoningContent,
		Model:            model,
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg.Content}},
	}, out, nil
}

type ConsultInput struct {
	SessionID string `json:"session_id" jsonschema:"identifier for this conversation; reuse to continue, choose any new string to start fresh"`
	Prompt    string `json:"prompt" jsonschema:"the user message to send"`
	Model     string `json:"model,omitempty" jsonschema:"optional model override; defaults to deepseek-reasoner"`
}

type ConsultOutput struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Model            string `json:"model"`
	SessionID        string `json:"session_id"`
	TurnCount        int    `json:"turn_count"`
}

// Consult is the stateful chat tool. It appends the user prompt to the
// session's history, calls DeepSeek with the full transcript, then appends
// the assistant reply (content only — reasoning_content is intentionally
// excluded from subsequent requests per DeepSeek's guidance).
func (s *Server) Consult(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in ConsultInput,
) (*mcp.CallToolResult, ConsultOutput, error) {
	if strings.TrimSpace(in.SessionID) == "" {
		return nil, ConsultOutput{}, fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, ConsultOutput{}, fmt.Errorf("prompt is required")
	}

	model := in.Model
	if model == "" {
		model = s.defaultModel
	}

	sess := s.sessions.Acquire(in.SessionID)
	sess.mu.Lock()
	defer sess.mu.Unlock()

	sess.messages = append(sess.messages, deepseek.ChatCompletionMessage{
		Role:    deepseek.ChatMessageRoleUser,
		Content: in.Prompt,
	})

	resp, err := s.client.CreateChatCompletion(ctx, &deepseek.ChatCompletionRequest{
		Model:    model,
		Messages: sess.messages,
	})
	if err != nil {
		// Roll back the user turn we speculatively appended so a retry
		// from the client doesn't double-send the same prompt.
		sess.messages = sess.messages[:len(sess.messages)-1]
		return nil, ConsultOutput{}, fmt.Errorf("deepseek call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		sess.messages = sess.messages[:len(sess.messages)-1]
		return nil, ConsultOutput{}, fmt.Errorf("deepseek returned no choices")
	}

	msg := resp.Choices[0].Message
	sess.messages = append(sess.messages, deepseek.ChatCompletionMessage{
		Role:    deepseek.ChatMessageRoleAssistant,
		Content: msg.Content,
	})

	turnCount := 0
	for _, m := range sess.messages {
		if m.Role == deepseek.ChatMessageRoleUser {
			turnCount++
		}
	}

	out := ConsultOutput{
		Content:          msg.Content,
		ReasoningContent: msg.ReasoningContent,
		Model:            model,
		SessionID:        in.SessionID,
		TurnCount:        turnCount,
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg.Content}},
	}, out, nil
}

func (s *Server) Register(mcpServer *mcp.Server) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "consult_deepseek_oneshot",
		Description: "Send a single stateless prompt to DeepSeek and return the answer plus the model's reasoning_content (for R1).",
	}, s.ConsultOneshot)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "consult_deepseek",
		Description: "Send a prompt to DeepSeek in a stateful conversation keyed by session_id. Subsequent calls with the same session_id continue the conversation.",
	}, s.Consult)
}
