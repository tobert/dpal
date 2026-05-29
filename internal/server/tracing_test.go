package server

import (
	"context"
	"errors"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newTestTracer returns a tracer whose spans are synchronously exported
// into an InMemoryExporter, so tests can assert on them immediately.
func newTestTracer() (trace.Tracer, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	return tp.Tracer("dpal-test"), exporter
}

// attrIndex flattens a SpanStub's attribute list to a map for ergonomic lookups.
func attrIndex(span tracetest.SpanStub) map[string]any {
	out := map[string]any{}
	for _, kv := range span.Attributes {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

// spansNamed returns the exported spans whose name matches.
func spansNamed(spans tracetest.SpanStubs, name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// mustOne fetches the single span with the given name, failing otherwise.
func mustOne(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	got := spansNamed(spans, name)
	if len(got) != 1 {
		t.Fatalf("got %d %q spans, want 1", len(got), name)
	}
	return got[0]
}

func makeRespWithUsage(model, content, reasoning string, in, out, hit, miss int) *deepseek.ChatCompletionResponse {
	return &deepseek.ChatCompletionResponse{
		Model: model,
		Choices: []deepseek.Choice{{Message: deepseek.Message{
			Role:             deepseek.ChatMessageRoleAssistant,
			Content:          content,
			ReasoningContent: reasoning,
		}}},
		Usage: deepseek.Usage{
			PromptTokens:          in,
			CompletionTokens:      out,
			PromptCacheHitTokens:  hit,
			PromptCacheMissTokens: miss,
		},
	}
}

func TestOneshot_EmitsSpanWithGenAIAttributes(t *testing.T) {
	tracer, exporter := newTestTracer()
	rec := &fakeClient{resp: makeRespWithUsage("deepseek-reasoner", "hi", "thinking", 12, 7, 4, 8)}
	s := New(rec).WithTracer(tracer)

	if _, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hello"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exporter.GetSpans()
	// The oneshot wraps its single upstream call in a consult_deepseek_oneshot
	// operation span; the gen_ai attributes live on the inner deepseek.chat.
	if len(spansNamed(spans, "consult_deepseek_oneshot")) != 1 {
		t.Errorf("want one consult_deepseek_oneshot operation span; spans=%v", spans)
	}
	span := mustOne(t, spans, "deepseek.chat")

	attrs := attrIndex(span)
	want := map[string]any{
		"gen_ai.system":                    "deepseek",
		"gen_ai.operation.name":            "chat",
		"gen_ai.request.model":             ModelV4Pro,
		"gen_ai.request.message_count":     int64(1),
		"gen_ai.response.model":            "deepseek-reasoner",
		"gen_ai.usage.input_tokens":        int64(12),
		"gen_ai.usage.output_tokens":       int64(7),
		"deepseek.usage.cache_hit_tokens":  int64(4),
		"deepseek.usage.cache_miss_tokens": int64(8),
	}
	for k, v := range want {
		if got, ok := attrs[k]; !ok || got != v {
			t.Errorf("attr %q = %v (ok=%v), want %v", k, got, ok, v)
		}
	}
}

func TestChat_RecordsErrorOnFailedCall(t *testing.T) {
	tracer, exporter := newTestTracer()
	rec := &fakeClient{err: errors.New("503 upstream")}
	s := New(rec).WithTracer(tracer)

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hello"})
	if err == nil {
		t.Fatal("expected error from failing client")
	}

	spans := exporter.GetSpans()
	span := mustOne(t, spans, "deepseek.chat")
	if span.Status.Code.String() != "Error" {
		t.Errorf("span status = %s, want Error", span.Status.Code)
	}
	if len(span.Events) == 0 {
		t.Error("expected error event recorded on span, got none")
	}
	// The failure must propagate to the operation span so a trace search by
	// error surfaces the whole consult, not just the inner HTTP attempt.
	root := mustOne(t, spans, "consult_deepseek_oneshot")
	if root.Status.Code.String() != "Error" {
		t.Errorf("operation span status = %s, want Error", root.Status.Code)
	}
}

func TestConsult_EmitsOneSpanPerTurn(t *testing.T) {
	tracer, exporter := newTestTracer()
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			makeRespWithUsage("deepseek-reasoner", "a", "", 1, 1, 0, 1),
			makeRespWithUsage("deepseek-reasoner", "b", "", 2, 1, 0, 2),
		},
	}
	s := New(rec).WithTracer(tracer)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "Q1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "Q2"}); err != nil {
		t.Fatal(err)
	}

	// Each turn now nests deepseek.chat under a dpal.synthesize phase under a
	// consult_deepseek operation span; assert on the chat spans themselves.
	// SimpleSpanProcessor exports in End order, so chat[0] is the first turn.
	chat := spansNamed(exporter.GetSpans(), "deepseek.chat")
	if len(chat) != 2 {
		t.Fatalf("got %d deepseek.chat spans, want 2", len(chat))
	}
	if got := attrIndex(chat[0])["gen_ai.request.message_count"]; got != int64(1) {
		t.Errorf("first turn message_count = %v, want 1", got)
	}
	if got := attrIndex(chat[1])["gen_ai.request.message_count"]; got != int64(3) {
		t.Errorf("second turn message_count = %v, want 3 (user/assistant/user)", got)
	}
}

func TestChat_NestsUnderParentContext(t *testing.T) {
	tracer, exporter := newTestTracer()
	rec := &fakeClient{resp: makeRespWithUsage("deepseek-reasoner", "ok", "", 1, 1, 0, 1)}
	s := New(rec).WithTracer(tracer)

	parentCtx, parent := tracer.Start(context.Background(), "incoming.tool_call")
	if _, _, err := s.ConsultOneshot(parentCtx, nil, OneshotInput{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := exporter.GetSpans()
	incoming := mustOne(t, spans, "incoming.tool_call")
	oneshot := mustOne(t, spans, "consult_deepseek_oneshot")
	chat := mustOne(t, spans, "deepseek.chat")

	// The operation span hangs off the caller's span, and the upstream call
	// hangs off the operation span — a single connected trace.
	if oneshot.Parent.SpanID() != incoming.SpanContext.SpanID() {
		t.Errorf("oneshot.parent = %s, want incoming.tool_call %s",
			oneshot.Parent.SpanID(), incoming.SpanContext.SpanID())
	}
	if chat.Parent.SpanID() != oneshot.SpanContext.SpanID() {
		t.Errorf("chat.parent = %s, want consult_deepseek_oneshot %s",
			chat.Parent.SpanID(), oneshot.SpanContext.SpanID())
	}
}

// TestConsult_SpanTreeIsParented is the regression guard for the orphan-span
// bug: explore/synth phases and every tool call must hang off one consult
// operation span, not scatter as independent root traces.
func TestConsult_SpanTreeIsParented(t *testing.T) {
	tracer, exporter := newTestTracer()
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{
		toolCallResp("list_directory", `{"path":"."}`, "call_1"), // explore iter 0: call a tool
		makeResp("explorer draft", ""),                           // explore iter 1: finalize
		makeResp("synth answer", ""),                             // synth
	}}
	s := New(rec).WithTracer(tracer).WithExplorer(newSandboxExplorer(t))

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}

	spans := exporter.GetSpans()
	root := mustOne(t, spans, "consult_deepseek")
	explore := mustOne(t, spans, "dpal.explore")
	synth := mustOne(t, spans, "dpal.synthesize")
	toolCall := mustOne(t, spans, "explorer.tool_call")

	if explore.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("dpal.explore parent = %s, want consult_deepseek root", explore.Parent.SpanID())
	}
	if synth.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("dpal.synthesize parent = %s, want consult_deepseek root", synth.Parent.SpanID())
	}
	if toolCall.Parent.SpanID() != explore.SpanContext.SpanID() {
		t.Errorf("explorer.tool_call parent = %s, want dpal.explore (was an orphan root)", toolCall.Parent.SpanID())
	}
	if got := len(spansNamed(spans, "deepseek.chat")); got != 3 {
		t.Errorf("got %d deepseek.chat spans, want 3 (2 explore iters + 1 synth)", got)
	}
	if got := attrIndex(explore)["dpal.explorer_tool_calls"]; got != int64(1) {
		t.Errorf("dpal.explore explorer_tool_calls = %v, want 1", got)
	}
}
