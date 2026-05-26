package server

import (
	"context"
	"fmt"
	"strings"

	deepseek "github.com/cohesion-org/deepseek-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tobert/dpal/internal/explorer"
)

const defaultMaxToolIterations = 10

// DeepSeekClient is the subset of the deepseek-go client surface dpal uses.
// Defined as an interface so handlers can be tested without network access.
type DeepSeekClient interface {
	CreateChatCompletion(ctx context.Context, req *deepseek.ChatCompletionRequest) (*deepseek.ChatCompletionResponse, error)
}

type Server struct {
	client            DeepSeekClient
	defaultModel      string
	sessions          *Sessions
	tracer            trace.Tracer
	version           string
	explorer          *explorer.Explorer
	maxToolIterations int
}

func New(client DeepSeekClient) *Server {
	return &Server{
		client:            client,
		defaultModel:      deepseek.DeepSeekReasoner,
		sessions:          NewSessions(defaultMaxSessions),
		tracer:            otel.Tracer("dpal"),
		version:           "unknown",
		maxToolIterations: defaultMaxToolIterations,
	}
}

// WithExplorer enables DeepSeek-driven exploration of the given
// sandboxed directory by attaching the explorer's tool schemas to
// every chat request and dispatching tool calls during the response
// loop. Pass nil (the default) to disable exploration.
func (s *Server) WithExplorer(e *explorer.Explorer) *Server {
	s.explorer = e
	return s
}

// WithTracer overrides the OTel tracer used to instrument upstream
// calls. Returns the receiver so it can be chained off New. Primarily
// for tests; production code should configure the global TracerProvider
// via otelinit.Bootstrap.
func (s *Server) WithTracer(t trace.Tracer) *Server {
	s.tracer = t
	return s
}

// WithVersion records the build version exposed via the dpal://info
// resource. Returns the receiver for chaining.
func (s *Server) WithVersion(v string) *Server {
	s.version = v
	return s
}

// chat orchestrates one or more DeepSeek calls, dispatching any tool
// calls the model requests through the configured Explorer until the
// model returns a final answer (FinishReason != "tool_calls") or the
// loop hits maxToolIterations. The returned slice is a fresh allocation
// containing the full message history including tool exchanges; the
// caller's input is never mutated.
func (s *Server) chat(
	ctx context.Context,
	model string,
	messages []deepseek.ChatCompletionMessage,
) (*deepseek.ChatCompletionResponse, []deepseek.ChatCompletionMessage, error) {
	msgs := make([]deepseek.ChatCompletionMessage, len(messages))
	copy(msgs, messages)

	var tools []deepseek.Tool
	if s.explorer != nil {
		tools = s.explorer.ToolDefinitions()
	}

	maxIter := s.maxToolIterations
	if maxIter <= 0 {
		maxIter = 1
	}

	for iter := 0; iter < maxIter; iter++ {
		resp, err := s.callOnce(ctx, model, msgs, tools, iter)
		if err != nil {
			return nil, nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, nil, fmt.Errorf("deepseek returned no choices")
		}
		choice := resp.Choices[0]

		hasToolCalls := len(choice.Message.ToolCalls) > 0
		if !hasToolCalls {
			// Final answer. Append assistant content (no reasoning) to
			// the durable history and return.
			msgs = append(msgs, deepseek.ChatCompletionMessage{
				Role:    deepseek.ChatMessageRoleAssistant,
				Content: choice.Message.Content,
			})
			return resp, msgs, nil
		}

		if s.explorer == nil {
			return nil, nil, fmt.Errorf("deepseek requested tools but no explorer is configured")
		}

		// Record the assistant turn that carries the tool calls.
		msgs = append(msgs, deepseek.ChatCompletionMessage{
			Role:      deepseek.ChatMessageRoleAssistant,
			Content:   choice.Message.Content,
			ToolCalls: choice.Message.ToolCalls,
		})

		// Dispatch each tool call and append its result.
		for _, tc := range choice.Message.ToolCalls {
			result := s.dispatchToolCall(ctx, tc)
			msgs = append(msgs, deepseek.ChatCompletionMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}

	return nil, nil, fmt.Errorf("tool-call loop exceeded %d iterations", maxIter)
}

// callOnce performs a single DeepSeek invocation, instrumented with one
// deepseek.chat span carrying gen_ai attributes and token usage.
func (s *Server) callOnce(
	ctx context.Context,
	model string,
	messages []deepseek.ChatCompletionMessage,
	tools []deepseek.Tool,
	iter int,
) (*deepseek.ChatCompletionResponse, error) {
	ctx, span := s.tracer.Start(ctx, "deepseek.chat",
		trace.WithAttributes(
			attribute.String("gen_ai.system", "deepseek"),
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.request.model", model),
			attribute.Int("gen_ai.request.message_count", len(messages)),
			attribute.Int("dpal.tool_iteration", iter),
			attribute.Int("dpal.tools_offered", len(tools)),
		),
	)
	defer span.End()

	req := &deepseek.ChatCompletionRequest{
		Model:    model,
		Messages: messages,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}

	resp, err := s.client.CreateChatCompletion(ctx, req)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, err
	}

	span.SetAttributes(
		attribute.String("gen_ai.response.model", resp.Model),
		attribute.Int("gen_ai.usage.input_tokens", resp.Usage.PromptTokens),
		attribute.Int("gen_ai.usage.output_tokens", resp.Usage.CompletionTokens),
		attribute.Int("deepseek.usage.cache_hit_tokens", resp.Usage.PromptCacheHitTokens),
		attribute.Int("deepseek.usage.cache_miss_tokens", resp.Usage.PromptCacheMissTokens),
	)
	if len(resp.Choices) > 0 {
		span.SetAttributes(
			attribute.String("gen_ai.response.finish_reason", resp.Choices[0].FinishReason),
			attribute.Int("dpal.response.tool_call_count", len(resp.Choices[0].Message.ToolCalls)),
		)
	}
	return resp, nil
}

func (s *Server) dispatchToolCall(ctx context.Context, tc deepseek.ToolCall) string {
	_, span := s.tracer.Start(ctx, "explorer.tool_call",
		trace.WithAttributes(
			attribute.String("tool.name", tc.Function.Name),
			attribute.String("tool.call_id", tc.ID),
		),
	)
	defer span.End()
	out := s.explorer.Dispatch(tc.Function.Name, tc.Function.Arguments)
	span.SetAttributes(attribute.Int("tool.result_bytes", len(out)))
	return out
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

	resp, _, err := s.chat(ctx, model, []deepseek.ChatCompletionMessage{
		{Role: deepseek.ChatMessageRoleUser, Content: in.Prompt},
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

	// Build the candidate transcript without mutating sess.messages —
	// chat() takes a defensive copy, but assembling the candidate
	// fresh means a failed call leaves the durable session untouched.
	candidate := make([]deepseek.ChatCompletionMessage, 0, len(sess.messages)+1)
	candidate = append(candidate, sess.messages...)
	candidate = append(candidate, deepseek.ChatCompletionMessage{
		Role:    deepseek.ChatMessageRoleUser,
		Content: in.Prompt,
	})

	resp, updated, err := s.chat(ctx, model, candidate)
	if err != nil {
		return nil, ConsultOutput{}, fmt.Errorf("deepseek call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, ConsultOutput{}, fmt.Errorf("deepseek returned no choices")
	}

	sess.messages = updated
	msg := resp.Choices[0].Message

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

	mcpServer.AddResource(&mcp.Resource{
		URI:         infoURI,
		Name:        "info",
		Description: "dpal service info (version, defaults, session counts)",
		MIMEType:    jsonMIME,
	}, s.handleInfo)
	mcpServer.AddResource(&mcp.Resource{
		URI:         sessionsURI,
		Name:        "sessions",
		Description: "list of active dpal conversation sessions with metadata",
		MIMEType:    jsonMIME,
	}, s.handleSessions)
	mcpServer.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: sessionTemplate,
		Name:        "session",
		Description: "full transcript of one session",
		MIMEType:    jsonMIME,
	}, s.handleSession)
}
