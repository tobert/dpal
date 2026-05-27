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
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "deepseek.chat" {
		t.Errorf("span name = %q, want %q", span.Name, "deepseek.chat")
	}

	attrs := attrIndex(span)
	want := map[string]any{
		"gen_ai.system":                 "deepseek",
		"gen_ai.operation.name":         "chat",
		"gen_ai.request.model":          deepseek.DeepSeekReasoner,
		"gen_ai.request.message_count":  int64(1),
		"gen_ai.response.model":         "deepseek-reasoner",
		"gen_ai.usage.input_tokens":     int64(12),
		"gen_ai.usage.output_tokens":    int64(7),
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
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Status.Code.String() != "Error" {
		t.Errorf("span status = %s, want Error", span.Status.Code)
	}
	if len(span.Events) == 0 {
		t.Error("expected error event recorded on span, got none")
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

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if got := attrIndex(spans[0])["gen_ai.request.message_count"]; got != int64(1) {
		t.Errorf("first span message_count = %v, want 1", got)
	}
	if got := attrIndex(spans[1])["gen_ai.request.message_count"]; got != int64(3) {
		t.Errorf("second span message_count = %v, want 3 (user/assistant/user)", got)
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
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	// SimpleSpanProcessor exports in End order: child first, then parent.
	child, parentSpan := spans[0], spans[1]
	if child.Parent.SpanID() != parentSpan.SpanContext.SpanID() {
		t.Errorf("child.parent_span_id = %s, want %s (parent span)",
			child.Parent.SpanID(), parentSpan.SpanContext.SpanID())
	}
}
