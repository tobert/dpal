package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	deepseek "github.com/cohesion-org/deepseek-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tobert/dpal/internal/explorer"
)

const defaultMaxToolIterations = 10

// Retry policy for transient upstream failures (429 + 5xx). Exponential
// backoff with jitter, bounded total attempts. Per-call only — the
// tool-call loop is not retried as a unit; each upstream HTTP request
// is its own retry budget.
const (
	defaultRetryMax      = 4 // 5 attempts total
	defaultRetryBaseWait = 250 * time.Millisecond
	defaultRetryMaxWait  = 8 * time.Second
)

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
	systemPrompt      string // composed at startup; per-call SystemPrompt overrides

	retryMax      int
	retryBaseWait time.Duration
	retryMaxWait  time.Duration

	usageMu sync.Mutex
	usage   map[string]*ModelUsage // keyed by response model (what DeepSeek actually billed)
}

// ModelUsage is the cumulative token tally for a single model since
// process start. Surfaced in dpal://info.
type ModelUsage struct {
	Model           string `json:"model"`
	Calls           uint64 `json:"calls"`
	InputTokens     uint64 `json:"input_tokens"`
	OutputTokens    uint64 `json:"output_tokens"`
	CacheHitTokens  uint64 `json:"cache_hit_tokens"`
	CacheMissTokens uint64 `json:"cache_miss_tokens"`
}

func New(client DeepSeekClient) *Server {
	return &Server{
		client:            client,
		defaultModel:      deepseek.DeepSeekReasoner,
		sessions:          NewSessions(defaultMaxSessions),
		tracer:            otel.Tracer("dpal"),
		version:           "unknown",
		maxToolIterations: defaultMaxToolIterations,
		retryMax:          defaultRetryMax,
		retryBaseWait:     defaultRetryBaseWait,
		retryMaxWait:      defaultRetryMaxWait,
		usage:             make(map[string]*ModelUsage),
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

// WithSystemPrompt installs a default system prompt that gets prepended
// to every DeepSeek call as a {role: "system"} message. Per-call
// SystemPrompt on ConsultInput / OneshotInput overrides this default
// for that single call. Empty string means no system message.
func (s *Server) WithSystemPrompt(p string) *Server {
	s.systemPrompt = p
	return s
}

// chat orchestrates one or more DeepSeek calls, dispatching any tool
// calls the model requests through the configured Explorer until the
// model returns a final answer (FinishReason != "tool_calls") or the
// loop hits maxToolIterations. The returned slice is a fresh allocation
// containing the full message history including tool exchanges; the
// caller's input is never mutated.
//
// systemPrompt, if non-empty, is prepended to each upstream HTTP
// request as a {role: "system"} message — but it is NOT stored in the
// returned msgs slice, so session histories stay user/assistant/tool
// only and the active prompt can change between turns without
// rewriting history.
func (s *Server) chat(
	ctx context.Context,
	model string,
	systemPrompt string,
	exp *explorer.Explorer,
	messages []deepseek.ChatCompletionMessage,
) (*deepseek.ChatCompletionResponse, []deepseek.ChatCompletionMessage, error) {
	msgs := make([]deepseek.ChatCompletionMessage, len(messages))
	copy(msgs, messages)

	var tools []deepseek.Tool
	if exp != nil {
		tools = exp.ToolDefinitions()
	}

	maxIter := s.maxToolIterations
	if maxIter <= 0 {
		maxIter = 1
	}

	for iter := 0; iter < maxIter; iter++ {
		resp, err := s.callOnce(ctx, model, systemPrompt, msgs, tools, iter)
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

		if exp == nil {
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
			result := s.dispatchToolCall(ctx, exp, tc)
			msgs = append(msgs, deepseek.ChatCompletionMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}

	return nil, nil, fmt.Errorf("tool-call loop exceeded %d iterations", maxIter)
}

// callOnce performs a single DeepSeek invocation (one logical request,
// possibly multiple HTTP attempts under the retry policy). Each attempt
// emits its own deepseek.chat span so retries are visible in traces.
func (s *Server) callOnce(
	ctx context.Context,
	model string,
	systemPrompt string,
	messages []deepseek.ChatCompletionMessage,
	tools []deepseek.Tool,
	iter int,
) (*deepseek.ChatCompletionResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= s.retryMax; attempt++ {
		if attempt > 0 {
			wait := backoffDelay(s.retryBaseWait, s.retryMaxWait, attempt)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := s.callAttempt(ctx, model, systemPrompt, messages, tools, iter, attempt)
		if err == nil {
			return resp, nil
		}
		if !isRetryable(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("deepseek: exhausted %d retries: %w", s.retryMax, lastErr)
}

// isRetryable returns true for transient upstream failures: 429
// (rate-limited) and any 5xx. Anything else — including auth/permission
// errors and malformed requests — is final.
func isRetryable(err error) bool {
	var apiErr *deepseek.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 429 || (apiErr.StatusCode >= 500 && apiErr.StatusCode < 600) {
			return true
		}
	}
	return false
}

// backoffDelay returns base * 2^(attempt-1), capped at max, with full
// jitter (uniform random in [0, computed]) so concurrent retries don't
// dogpile a recovering upstream.
func backoffDelay(base, max time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base << (attempt - 1)
	if d <= 0 || d > max {
		d = max
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

func (s *Server) callAttempt(
	ctx context.Context,
	model string,
	systemPrompt string,
	messages []deepseek.ChatCompletionMessage,
	tools []deepseek.Tool,
	iter int,
	attempt int,
) (*deepseek.ChatCompletionResponse, error) {
	outbound := messages
	if systemPrompt != "" {
		outbound = make([]deepseek.ChatCompletionMessage, 0, len(messages)+1)
		outbound = append(outbound, deepseek.ChatCompletionMessage{
			Role:    deepseek.ChatMessageRoleSystem,
			Content: systemPrompt,
		})
		outbound = append(outbound, messages...)
	}

	ctx, span := s.tracer.Start(ctx, "deepseek.chat",
		trace.WithAttributes(
			attribute.String("gen_ai.system", "deepseek"),
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.request.model", model),
			attribute.Int("gen_ai.request.message_count", len(outbound)),
			attribute.Int("dpal.tool_iteration", iter),
			attribute.Int("dpal.tools_offered", len(tools)),
			attribute.Int("dpal.retry_attempt", attempt),
			attribute.Bool("dpal.system_prompt_present", systemPrompt != ""),
		),
	)
	defer span.End()

	req := &deepseek.ChatCompletionRequest{
		Model:    model,
		Messages: outbound,
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
	s.recordUsage(resp.Model, resp.Usage)
	if len(resp.Choices) > 0 {
		span.SetAttributes(
			attribute.String("gen_ai.response.finish_reason", resp.Choices[0].FinishReason),
			attribute.Int("dpal.response.tool_call_count", len(resp.Choices[0].Message.ToolCalls)),
		)
	}
	return resp, nil
}

// recordUsage adds one call's tokens to the per-model tally. The key
// is the response model (what DeepSeek actually billed), which differs
// from the request model for aliases like "deepseek-chat".
func (s *Server) recordUsage(model string, u deepseek.Usage) {
	if model == "" {
		model = "unknown"
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	mu, ok := s.usage[model]
	if !ok {
		mu = &ModelUsage{Model: model}
		s.usage[model] = mu
	}
	mu.Calls++
	mu.InputTokens += uint64(u.PromptTokens)
	mu.OutputTokens += uint64(u.CompletionTokens)
	mu.CacheHitTokens += uint64(u.PromptCacheHitTokens)
	mu.CacheMissTokens += uint64(u.PromptCacheMissTokens)
}

// UsageSnapshot returns the per-model token tally sorted by model name
// for stable JSON output.
func (s *Server) UsageSnapshot() []ModelUsage {
	s.usageMu.Lock()
	out := make([]ModelUsage, 0, len(s.usage))
	for _, mu := range s.usage {
		out = append(out, *mu)
	}
	s.usageMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func (s *Server) dispatchToolCall(ctx context.Context, exp *explorer.Explorer, tc deepseek.ToolCall) string {
	_, span := s.tracer.Start(ctx, "explorer.tool_call",
		trace.WithAttributes(
			attribute.String("tool.name", tc.Function.Name),
			attribute.String("tool.call_id", tc.ID),
		),
	)
	defer span.End()
	out := exp.Dispatch(tc.Function.Name, tc.Function.Arguments)
	span.SetAttributes(attribute.Int("tool.result_bytes", len(out)))
	return out
}

// explorerForRequest decides which Explorer to use for a single tool
// invocation. Preference order:
//
//  1. If the MCP client advertises roots via roots/list, use the first
//     one (logging the rest).
//  2. Otherwise fall back to the Explorer configured at process start
//     via --root.
//
// Returns nil when explore is disabled entirely (--no-explore).
// Errors during ListRoots or URI parsing are logged and treated as
// "client doesn't advertise roots"; the --root fallback applies.
func (s *Server) explorerForRequest(ctx context.Context, req *mcp.CallToolRequest) *explorer.Explorer {
	if s.explorer == nil {
		return nil
	}
	if req == nil || req.Session == nil {
		return s.explorer
	}
	res, err := req.Session.ListRoots(ctx, nil)
	if err != nil {
		// Common: client doesn't implement the roots capability. Quietly fall back.
		return s.explorer
	}
	if len(res.Roots) == 0 {
		return s.explorer
	}
	if len(res.Roots) > 1 {
		extras := make([]string, 0, len(res.Roots)-1)
		for _, r := range res.Roots[1:] {
			extras = append(extras, r.URI)
		}
		log.Printf("dpal: client advertised %d roots; using first (%s), ignoring %v",
			len(res.Roots), res.Roots[0].URI, extras)
	}
	path, err := fileURIToPath(res.Roots[0].URI)
	if err != nil {
		log.Printf("dpal: invalid root URI %q: %v; falling back to --root", res.Roots[0].URI, err)
		return s.explorer
	}
	exp, err := explorer.New(path)
	if err != nil {
		log.Printf("dpal: explorer.New(%q): %v; falling back to --root", path, err)
		return s.explorer
	}
	return exp
}

// fileURIToPath converts a "file://..." MCP root URI to a local
// filesystem path. The MCP spec currently mandates the file:// scheme.
func fileURIToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("unsupported root URI scheme %q (want file://)", u.Scheme)
	}
	// file:///abs/path     -> u.Path = /abs/path
	// file://host/abs/path -> reject; we don't do remote roots
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("remote roots not supported (host=%q)", u.Host)
	}
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}

type OneshotInput struct {
	Prompt       string `json:"prompt" jsonschema:"the question or instruction to send to DeepSeek"`
	Model        string `json:"model,omitempty" jsonschema:"optional model override; defaults to deepseek-reasoner"`
	SystemPrompt string `json:"system_prompt,omitempty" jsonschema:"optional system prompt override for this call; takes precedence over the dpal-configured default"`
}

type OneshotOutput struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Model            string `json:"model"`
}

func (s *Server) ConsultOneshot(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in OneshotInput,
) (*mcp.CallToolResult, OneshotOutput, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, OneshotOutput{}, fmt.Errorf("prompt is required")
	}

	model := in.Model
	if model == "" {
		model = s.defaultModel
	}

	sysPrompt := s.systemPrompt
	if in.SystemPrompt != "" {
		sysPrompt = in.SystemPrompt
	}

	exp := s.explorerForRequest(ctx, req)

	resp, _, err := s.chat(ctx, model, sysPrompt, exp, []deepseek.ChatCompletionMessage{
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
	SessionID    string `json:"session_id" jsonschema:"identifier for this conversation; reuse to continue, choose any new string to start fresh"`
	Prompt       string `json:"prompt" jsonschema:"the user message to send"`
	Model        string `json:"model,omitempty" jsonschema:"optional model override; defaults to deepseek-reasoner"`
	SystemPrompt string `json:"system_prompt,omitempty" jsonschema:"optional system prompt override for this call; takes precedence over the dpal-configured default. Not stored in session history, so changing it between calls is safe."`
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
	req *mcp.CallToolRequest,
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

	sysPrompt := s.systemPrompt
	if in.SystemPrompt != "" {
		sysPrompt = in.SystemPrompt
	}

	exp := s.explorerForRequest(ctx, req)

	resp, updated, err := s.chat(ctx, model, sysPrompt, exp, candidate)
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

	if msg.ReasoningContent != "" {
		sess.reasoning = append(sess.reasoning, ReasoningEntry{
			TurnIndex: turnCount,
			Content:   msg.ReasoningContent,
		})
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
