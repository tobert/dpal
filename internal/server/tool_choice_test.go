package server

import (
	"context"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
)

// The synthesizer is handed the explorer's tool-call transcript but must not
// itself call tools. Relying on omitting `tools` to mean "don't call tools"
// is an inconsistent request (the history references tool calls) and primes
// the model to emit tool-call markup that leaks into Content. The protocol's
// purpose-built knob is tool_choice:"none"; assert the synth sends it.
func TestConsult_SynthSetsToolChoiceNone(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{makeResp("ok", "")}}
	s := New(rec)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{
		SessionID: "s1", Prompt: "hi", DisableExplore: true,
	}); err != nil {
		t.Fatal(err)
	}
	if rec.requests[0].ToolChoice != "none" {
		t.Errorf("synth tool_choice = %v, want \"none\"", rec.requests[0].ToolChoice)
	}
}

// The explorer phase is the whole point of the tool loop, so it must NOT be
// constrained with tool_choice:"none" — only the synth call is.
func TestConsult_ExplorerDoesNotGetToolChoiceNone(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{
		makeResp("explorer's draft answer", ""), // explore phase ends immediately (no tool calls)
		makeResp("synth answer", ""),
	}}
	s := New(rec).WithExplorer(newSandboxExplorer(t))

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{
		SessionID: "s1", Prompt: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("got %d requests, want 2 (explore + synth)", len(rec.requests))
	}
	if rec.requests[0].ToolChoice == "none" {
		t.Errorf("explorer tool_choice = %v, want unset (explorer must be free to call tools)", rec.requests[0].ToolChoice)
	}
	if rec.requests[1].ToolChoice != "none" {
		t.Errorf("synth tool_choice = %v, want \"none\"", rec.requests[1].ToolChoice)
	}
}

// The oneshot path makes a single direct call with no tools and no explore
// phase, so there is no tool-call history to be inconsistent with — it must
// not send tool_choice:"none".
func TestConsultOneshot_NoToolChoice(t *testing.T) {
	fake := &fakeClient{resp: makeResp("ok", "")}
	s := New(fake)

	if _, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if fake.lastReq.ToolChoice == "none" {
		t.Errorf("oneshot tool_choice = %v, want unset", fake.lastReq.ToolChoice)
	}
}
