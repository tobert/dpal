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

const defaultMaxToolIterations = 25

// toolChoiceNone is the OpenAI-compatible tool_choice value that forbids
// the model from calling any tool. dpal sets it on the synthesizer call so
// the synth answers from the explorer's loaded context instead of trying to
// continue the tool loop (which, with no tools advertised, would leak raw
// tool-call markup into the response content).
const toolChoiceNone = "none"

// V4 model IDs. deepseek-go v1.3.4 only exposes the legacy DeepSeekChat
// ("deepseek-chat") and DeepSeekReasoner ("deepseek-reasoner") aliases.
// Those aliases route to deepseek-v4-flash (non-thinking/thinking
// respectively) today, but DeepSeek announced they will be retired on
// 2026-07-24. dpal addresses V4 models by explicit ID so it survives
// the alias retirement, and so we can pick V4-Pro (the strong reasoner)
// for synthesis instead of V4-Flash with thinking.
const (
	ModelV4Pro   = "deepseek-v4-pro"
	ModelV4Flash = "deepseek-v4-flash"
)

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
	client               DeepSeekClient
	defaultModel         string
	defaultExplorerModel string // model used for the explore phase of two-phase Consult
	sessions             *Sessions
	tracer               trace.Tracer
	version              string
	explorer             *explorer.Explorer
	maxToolIterations    int
	systemPrompt         string // composed at startup; per-call SystemPrompt overrides
	explorerSystemPrompt string // system prompt used during the explore phase

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
		client:               client,
		defaultModel:         ModelV4Pro,
		defaultExplorerModel: ModelV4Flash,
		sessions:             NewSessions(defaultMaxSessions),
		tracer:               otel.Tracer("dpal"),
		version:              "unknown",
		maxToolIterations:    defaultMaxToolIterations,
		retryMax:             defaultRetryMax,
		retryBaseWait:        defaultRetryBaseWait,
		retryMaxWait:         defaultRetryMaxWait,
		usage:                make(map[string]*ModelUsage),
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

// WithExplorerSystemPrompt installs the system prompt used during the
// explore phase of two-phase Consult. Per-call ExplorerSystemPrompt on
// ConsultInput overrides this for a single call. Empty string means no
// dedicated explorer prompt — the synthesizer's system prompt is used
// for the explore phase as a fallback.
func (s *Server) WithExplorerSystemPrompt(p string) *Server {
	s.explorerSystemPrompt = p
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
	enableThinking bool,
	reasoningEffort string,
	toolChoice string,
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
		resp, err := s.callOnce(ctx, model, systemPrompt, msgs, tools, iter, enableThinking, reasoningEffort, toolChoice)
		if err != nil {
			return nil, nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, nil, fmt.Errorf("deepseek returned no choices")
		}
		choice := resp.Choices[0]

		hasToolCalls := len(choice.Message.ToolCalls) > 0
		if !hasToolCalls {
			// Final answer. Append assistant content to the durable
			// history. ReasoningContent is preserved per V4's
			// multi-turn-with-tools requirement (V3/R1 used to require
			// the opposite — see CLAUDE.md).
			msgs = append(msgs, deepseek.ChatCompletionMessage{
				Role:             deepseek.ChatMessageRoleAssistant,
				Content:          choice.Message.Content,
				ReasoningContent: choice.Message.ReasoningContent,
			})
			return resp, msgs, nil
		}

		if exp == nil {
			return nil, nil, fmt.Errorf("deepseek requested tools but no explorer is configured")
		}

		// Record the assistant turn that carries the tool calls.
		// ReasoningContent is preserved here too: if a thinking-enabled
		// model emits reasoning_content alongside tool_calls, V4
		// requires it on subsequent requests.
		msgs = append(msgs, deepseek.ChatCompletionMessage{
			Role:             deepseek.ChatMessageRoleAssistant,
			Content:          choice.Message.Content,
			ReasoningContent: choice.Message.ReasoningContent,
			ToolCalls:        choice.Message.ToolCalls,
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
	enableThinking bool,
	reasoningEffort string,
	toolChoice string,
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
		resp, err := s.callAttempt(ctx, model, systemPrompt, messages, tools, iter, attempt, enableThinking, reasoningEffort, toolChoice)
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
	enableThinking bool,
	reasoningEffort string,
	toolChoice string,
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
			attribute.Bool("dpal.thinking_enabled", enableThinking),
		),
	)
	defer span.End()
	if reasoningEffort != "" {
		span.SetAttributes(attribute.String("dpal.reasoning_effort", reasoningEffort))
	}
	if toolChoice != "" {
		span.SetAttributes(attribute.String("dpal.tool_choice", toolChoice))
	}

	req := &deepseek.ChatCompletionRequest{
		Model:          model,
		Messages:       outbound,
		EnableThinking: enableThinking,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	// tool_choice lets the caller forbid tool use ("none") even when the
	// message history references prior tool calls. The synth phase uses
	// this: it is fed the explorer's tool-call transcript but must answer,
	// not call tools. Omitting it (empty) leaves the model's default.
	if toolChoice != "" {
		req.ToolChoice = toolChoice
	}
	// reasoning_effort isn't a first-class field in deepseek-go; pass it
	// through ExtraFields, which the SDK merges into the top-level request
	// payload. Only set it when non-empty so the model default stands.
	if reasoningEffort != "" {
		req.ExtraFields = map[string]any{"reasoning_effort": reasoningEffort}
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
	Model        string `json:"model,omitempty" jsonschema:"optional model override; defaults to deepseek-v4-pro"`
	SystemPrompt string `json:"system_prompt,omitempty" jsonschema:"optional system prompt override for this call; takes precedence over the dpal-configured default"`
	Thinking     *bool  `json:"thinking,omitempty" jsonschema:"optional override for V4 thinking mode; defaults to true (V4-Pro is a thinking-mode model). Set false to get fast non-thinking responses."`
	// ReasoningEffort tunes how much the thinking-mode model deliberates.
	// V4 documents "high" and "max"; the value is passed through to
	// DeepSeek unmodified, so any value the API accepts works. Empty means
	// "use the model default" (dpal sends no reasoning_effort field).
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"optional reasoning_effort for thinking mode (e.g. \"high\" or \"max\"); passed through to DeepSeek. Omit to use the model default."`
}

type OneshotOutput struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Model            string `json:"model"`
}

// ConsultOneshot is the non-agentic, single-call, stateless tool. It
// makes exactly one DeepSeek request — no tools are advertised and no
// explore phase runs, so the caller is responsible for providing all
// the context the model needs in the prompt. Mirrors gpal's batch-style
// call. Use Consult instead when you want dpal's two-phase agentic
// default.
func (s *Server) ConsultOneshot(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in OneshotInput,
) (_ *mcp.CallToolResult, _ OneshotOutput, retErr error) {
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

	// V4 thinking mode: default on for the oneshot path since the
	// default model is V4-Pro (a thinking-mode model). Callers can
	// flip this off when they pick a non-thinking model.
	enableThinking := true
	if in.Thinking != nil {
		enableThinking = *in.Thinking
	}

	// Operation span: parents the single upstream deepseek.chat so a oneshot
	// is one connected trace, and surfaces failures at the operation level.
	ctx, span := s.tracer.Start(ctx, "consult_deepseek_oneshot",
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "consult_oneshot"),
			attribute.String("gen_ai.request.model", model),
		),
	)
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	resp, _, err := s.chat(ctx, model, sysPrompt, nil, []deepseek.ChatCompletionMessage{
		{Role: deepseek.ChatMessageRoleUser, Content: in.Prompt},
	}, enableThinking, in.ReasoningEffort, "")
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
	SessionID            string `json:"session_id" jsonschema:"identifier for this conversation; reuse to continue, choose any new string to start fresh"`
	Prompt               string `json:"prompt" jsonschema:"the user message to send"`
	Model                string `json:"model,omitempty" jsonschema:"optional synthesizer model override; defaults to deepseek-v4-pro. This is the model that writes the final answer."`
	SystemPrompt         string `json:"system_prompt,omitempty" jsonschema:"optional system prompt override for this call; takes precedence over the dpal-configured default. Not stored in session history, so changing it between calls is safe."`
	ExplorerModel        string `json:"explorer_model,omitempty" jsonschema:"optional explore-phase model override; defaults to deepseek-v4-flash. Used only when an explorer is configured (i.e. not --no-explore)."`
	ExplorerSystemPrompt string `json:"explorer_system_prompt,omitempty" jsonschema:"optional system prompt override for the explore phase; takes precedence over the dpal-configured explorer default."`
	DisableExplore       bool   `json:"disable_explore,omitempty" jsonschema:"set true to skip the explore phase for this call and send the prompt directly to the synthesizer model. Useful when the caller has already curated context."`
	Thinking             *bool  `json:"thinking,omitempty" jsonschema:"optional override for V4 thinking mode on the synthesizer call; defaults to true. Set false for fast non-thinking synthesis. The explorer phase never uses thinking mode regardless of this setting."`
	// ReasoningEffort tunes the synthesizer's thinking-mode deliberation.
	// Applies only to the synth call; the explorer never gets it (explore
	// is pattern-match-and-load, not deep reasoning). Passed through to
	// DeepSeek unmodified; empty means the model default.
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"optional reasoning_effort for the synthesizer's thinking mode (e.g. \"high\" or \"max\"); passed through to DeepSeek. Applies to the synth call only, never the explorer. Omit to use the model default."`
}

type ConsultOutput struct {
	Content           string `json:"content"`
	ReasoningContent  string `json:"reasoning_content,omitempty"`
	Model             string `json:"model"`
	ExplorerModel     string `json:"explorer_model,omitempty"` // empty when the explore phase did not run
	ExplorerToolCalls int    `json:"explorer_tool_calls"`      // number of tool calls the explorer issued
	SessionID         string `json:"session_id"`
	TurnCount         int    `json:"turn_count"`
}

// Consult is the stateful, agentic chat tool. By default it runs two
// phases: a cheap explorer model (deepseek-v4-flash, non-thinking)
// drives the tool loop to load relevant code and context, then a
// stronger synthesizer model (deepseek-v4-pro, thinking) takes the
// loaded message history and writes the final answer. The explorer's
// own text-only synthesis attempt is stripped before the synthesizer
// is called so the synthesizer doesn't just defer to it.
//
// If --no-explore is set or DisableExplore is true on the input, the
// explore phase is skipped and the synthesizer is called directly.
// Errors during either phase abort the whole call and leave the
// session's durable history untouched.
func (s *Server) Consult(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in ConsultInput,
) (_ *mcp.CallToolResult, _ ConsultOutput, retErr error) {
	if strings.TrimSpace(in.SessionID) == "" {
		return nil, ConsultOutput{}, fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, ConsultOutput{}, fmt.Errorf("prompt is required")
	}

	synthModel := in.Model
	if synthModel == "" {
		synthModel = s.defaultModel
	}

	explorerModel := in.ExplorerModel
	if explorerModel == "" {
		explorerModel = s.defaultExplorerModel
	}

	// Operation span: the root that the explore/synth phase spans (and every
	// upstream call and tool call beneath them) hang off, so one consult is
	// one connected trace rather than scattered orphan spans.
	ctx, opSpan := s.tracer.Start(ctx, "consult_deepseek",
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "consult"),
			attribute.String("gen_ai.request.model", synthModel),
			attribute.String("dpal.session_id", in.SessionID),
			attribute.Bool("dpal.explore_disabled", in.DisableExplore),
		),
	)
	defer func() {
		if retErr != nil {
			opSpan.SetStatus(codes.Error, retErr.Error())
		}
		opSpan.End()
	}()

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

	synthSysPrompt := s.systemPrompt
	if in.SystemPrompt != "" {
		synthSysPrompt = in.SystemPrompt
	}

	exp := s.explorerForRequest(ctx, req)

	// Phase 1: explore. Skipped if there's no explorer (--no-explore) or
	// the caller opted out. The explorer's tool exchanges land in
	// `candidate` for the synthesizer to read; the explorer's own final
	// text answer is dropped so the synthesizer doesn't anchor on it.
	//
	// The explorer never uses V4 thinking mode — exploration is
	// pattern-match-and-load, not deep reasoning, and thinking-mode
	// tokens on the explorer would be wasted spend.
	var explorerToolCalls int
	usedExplorer := ""
	if exp != nil && !in.DisableExplore {
		explorerSys := s.explorerSystemPrompt
		if in.ExplorerSystemPrompt != "" {
			explorerSys = in.ExplorerSystemPrompt
		}
		if explorerSys == "" {
			// Fall back to synthesizer's system prompt if no explorer
			// prompt is configured. Better than sending no system
			// message at all for the explore phase.
			explorerSys = synthSysPrompt
		}

		exploreCtx, exploreSpan := s.tracer.Start(ctx, "dpal.explore",
			trace.WithAttributes(attribute.String("gen_ai.request.model", explorerModel)))
		_, withExploration, err := s.chat(exploreCtx, explorerModel, explorerSys, exp, candidate, false, "", "")
		if err != nil {
			exploreSpan.SetStatus(codes.Error, err.Error())
			exploreSpan.End()
			return nil, ConsultOutput{}, fmt.Errorf("deepseek explore phase failed: %w", err)
		}

		candidate, explorerToolCalls = stripExplorerSynthesis(withExploration, len(candidate))
		exploreSpan.SetAttributes(attribute.Int("dpal.explorer_tool_calls", explorerToolCalls))
		exploreSpan.End()
		usedExplorer = explorerModel
	}

	// V4 thinking mode for the synth call: on by default (the default
	// model is V4-Pro, which exists precisely to do thinking-mode
	// synthesis). Callers can flip it off when they explicitly pick a
	// non-thinking model and want fast responses.
	enableThinking := true
	if in.Thinking != nil {
		enableThinking = *in.Thinking
	}

	// Phase 2: synthesize. No tools advertised — the synthesizer reads
	// whatever the explorer loaded into the candidate transcript and
	// writes a final answer. tool_choice="none" forbids it from trying to
	// continue the tool loop: the candidate carries the explorer's
	// tool-call transcript, which otherwise primes the model to emit a
	// tool call that (with no tools advertised) leaks as raw markup into
	// the answer content.
	synthCtx, synthSpan := s.tracer.Start(ctx, "dpal.synthesize",
		trace.WithAttributes(attribute.String("gen_ai.request.model", synthModel)))
	resp, updated, err := s.chat(synthCtx, synthModel, synthSysPrompt, nil, candidate, enableThinking, in.ReasoningEffort, toolChoiceNone)
	if err != nil {
		synthSpan.SetStatus(codes.Error, err.Error())
		synthSpan.End()
		return nil, ConsultOutput{}, fmt.Errorf("deepseek synthesize phase failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		synthSpan.End()
		return nil, ConsultOutput{}, fmt.Errorf("deepseek returned no choices")
	}
	synthSpan.End()

	sess.messages = updated
	msg := resp.Choices[0].Message

	turnCount := 0
	for _, m := range sess.messages {
		if m.Role == deepseek.ChatMessageRoleUser {
			turnCount++
		}
	}

	opSpan.SetAttributes(
		attribute.Int("dpal.explorer_tool_calls", explorerToolCalls),
		attribute.Int("dpal.turn_count", turnCount),
		attribute.String("dpal.explorer_model", usedExplorer),
	)

	if msg.ReasoningContent != "" {
		sess.reasoning = append(sess.reasoning, ReasoningEntry{
			TurnIndex: turnCount,
			Content:   msg.ReasoningContent,
		})
	}

	out := ConsultOutput{
		Content:           msg.Content,
		ReasoningContent:  msg.ReasoningContent,
		Model:             synthModel,
		ExplorerModel:     usedExplorer,
		ExplorerToolCalls: explorerToolCalls,
		SessionID:         in.SessionID,
		TurnCount:         turnCount,
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg.Content}},
	}, out, nil
}

// stripExplorerSynthesis drops the explorer's final text-only assistant
// message (which is its attempt at answering — we want the synthesizer
// to write the answer instead) while keeping every tool_call/tool pair
// the explorer issued. originalLen is the length of the candidate
// before the explorer ran; messages at that index or later are
// explorer-produced and subject to stripping.
//
// Also returns the count of tool calls the explorer made, summed across
// all of its assistant turns.
func stripExplorerSynthesis(msgs []deepseek.ChatCompletionMessage, originalLen int) ([]deepseek.ChatCompletionMessage, int) {
	toolCalls := 0
	for i := originalLen; i < len(msgs); i++ {
		if msgs[i].Role == deepseek.ChatMessageRoleAssistant {
			toolCalls += len(msgs[i].ToolCalls)
		}
	}
	// chat() always appends a final text-only assistant message when
	// the loop terminates normally (FinishReason != tool_calls). Strip
	// it. Defensive: only strip if it actually matches that shape.
	if n := len(msgs); n > 0 {
		last := msgs[n-1]
		if last.Role == deepseek.ChatMessageRoleAssistant && len(last.ToolCalls) == 0 {
			return msgs[:n-1], toolCalls
		}
	}
	return msgs, toolCalls
}

func (s *Server) Register(mcpServer *mcp.Server) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "consult_deepseek_oneshot",
		Description: "Single stateless DeepSeek call — no session, no tools, no exploration. " +
			"Exactly one upstream request: your prompt in, model's answer out. " +
			"You are responsible for any context the model needs. " +
			"Use consult_deepseek instead for dpal's agentic two-phase default.",
	}, s.ConsultOneshot)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "consult_deepseek",
		Description: "Agentic, stateful DeepSeek V4 consultation. Two phases by default: " +
			"a cheap explorer model (deepseek-v4-flash, non-thinking) reads files via tool calls to load context, " +
			"then a stronger synthesizer model (deepseek-v4-pro with thinking) writes the answer. " +
			"BOTH MODELS ARE BILLED. Conversation is keyed by session_id; reuse to continue. " +
			"Set disable_explore=true to skip the explore phase, or use consult_deepseek_oneshot for a single direct call.",
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
