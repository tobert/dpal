package server

import (
	"context"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
)

// In the disable_explore / direct path there is no explorer pass: the
// caller has curated the context, so the synth call advertises no tools
// and sends tool_choice:"none" to keep it from emitting tool-call markup.
func TestConsult_DirectSynthSetsToolChoiceNone(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{makeResp("ok", "")}}
	s := New(rec)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{
		SessionID: "s1", Prompt: "hi", DisableExplore: true,
	}); err != nil {
		t.Fatal(err)
	}
	if rec.requests[0].ToolChoice != "none" {
		t.Errorf("direct synth tool_choice = %v, want \"none\"", rec.requests[0].ToolChoice)
	}
}

// When the explore phase runs, NEITHER call is constrained with
// tool_choice:"none": the explorer drives the tool loop, and the synth is
// now free to fetch beyond the report (auto tool_choice), so it must not
// be pinned to "none".
func TestConsult_NeitherPhaseGetsToolChoiceNoneWhenExploreRan(t *testing.T) {
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
	if rec.requests[1].ToolChoice == "none" {
		t.Errorf("synth tool_choice = %v, want unset (synth may fetch beyond the report)", rec.requests[1].ToolChoice)
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
