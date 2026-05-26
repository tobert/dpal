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
}

func New(client DeepSeekClient) *Server {
	return &Server{
		client:       client,
		defaultModel: deepseek.DeepSeekReasoner,
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

func (s *Server) Register(mcpServer *mcp.Server) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "consult_deepseek_oneshot",
		Description: "Send a single stateless prompt to DeepSeek and return the answer plus the model's reasoning_content (for R1).",
	}, s.ConsultOneshot)
}
