package server

import (
	"context"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
	"github.com/tobert/dpal/internal/explorer"
)

// reasoningEffortOf pulls the reasoning_effort value out of a captured
// request's ExtraFields, or "" if it was never set.
func reasoningEffortOf(req *deepseek.ChatCompletionRequest) string {
	if req == nil || req.ExtraFields == nil {
		return ""
	}
	v, _ := req.ExtraFields["reasoning_effort"].(string)
	return v
}

func TestConsultOneshot_ReasoningEffortPassedThrough(t *testing.T) {
	fake := &fakeClient{resp: makeResp("ok", "")}
	s := New(fake)

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{
		Prompt:          "ponder this",
		ReasoningEffort: "max",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := reasoningEffortOf(fake.lastReq); got != "max" {
		t.Errorf("reasoning_effort = %q, want %q", got, "max")
	}
}

func TestConsultOneshot_NoReasoningEffortByDefault(t *testing.T) {
	fake := &fakeClient{resp: makeResp("ok", "")}
	s := New(fake)

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := reasoningEffortOf(fake.lastReq); got != "" {
		t.Errorf("reasoning_effort = %q, want empty (unset)", got)
	}
}

func TestConsult_ReasoningEffortReachesSynthesizer(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{makeResp("answer", "thinking")},
	}
	s := New(rec)

	_, _, err := s.Consult(context.Background(), nil, ConsultInput{
		SessionID:       "s1",
		Prompt:          "hello",
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := reasoningEffortOf(rec.requests[0]); got != "high" {
		t.Errorf("synth reasoning_effort = %q, want %q", got, "high")
	}
}

// The explore phase is pattern-match-and-load, never deep reasoning, so a
// reasoning_effort knob on the Consult call must NOT leak onto the explorer
// request — only the synthesizer gets it.
func TestConsult_ReasoningEffortNotAppliedToExplorer(t *testing.T) {
	exp, err := explorer.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// First response is the explorer's (no tool calls → stripped); second
	// is the synthesizer's.
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			makeResp("explorer text", ""),
			makeResp("final answer", "thinking"),
		},
	}
	s := New(rec).WithExplorer(exp)

	_, _, err = s.Consult(context.Background(), nil, ConsultInput{
		SessionID:       "s2",
		Prompt:          "look around",
		ReasoningEffort: "max",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("expected 2 upstream calls (explore + synth), got %d", len(rec.requests))
	}
	if got := reasoningEffortOf(rec.requests[0]); got != "" {
		t.Errorf("explorer reasoning_effort = %q, want empty", got)
	}
	if got := reasoningEffortOf(rec.requests[1]); got != "max" {
		t.Errorf("synth reasoning_effort = %q, want %q", got, "max")
	}
}
