package server

import (
	"context"
	"strings"
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

// The disable_explore synth runs with NO tools, so its system prompt must
// not describe file-exploration tools — a prompt that advertises read_file
// to a tools=nil call is exactly what primes the raw-tool-call leak (the
// model emits tool markup into content it can't execute). The conditional
// tool note is appended only when the call genuinely has tools.
func TestConsult_DirectSynthPromptOmitsToolNote(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{makeResp("ok", "")}}
	s := New(rec).WithExplorer(newSandboxExplorer(t)).WithSystemPrompt("BASE PERSONA")

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{
		SessionID: "s1", Prompt: "hi", DisableExplore: true,
	}); err != nil {
		t.Fatal(err)
	}
	sys := rec.requests[0].Messages[0]
	if sys.Role != deepseek.ChatMessageRoleSystem {
		t.Fatalf("request[0][0] role = %q, want system", sys.Role)
	}
	if strings.Contains(sys.Content, synthToolNote) {
		t.Errorf("direct synth prompt carries the tool note for a call with no tools:\n%s", sys.Content)
	}
}

// When the explore phase ran, the synth DOES get the explorer's tools, so
// the tool note is appended — telling the model it may fetch a span the
// report didn't quote. This is the conditional that keeps the leak fixed
// without hiding the genuinely-available tools.
func TestConsult_ExploreRanSynthPromptHasToolNote(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{
		makeResp("No exploration needed.", ""), // explorer makes zero tool calls
		makeResp("synth answer", ""),
	}}
	s := New(rec).WithExplorer(newSandboxExplorer(t)).WithSystemPrompt("BASE PERSONA")

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	synthSys := rec.requests[1].Messages[0]
	if !strings.Contains(synthSys.Content, synthToolNote) {
		t.Errorf("explore-ran synth prompt missing the tool note:\n%s", synthSys.Content)
	}
	if !strings.Contains(synthSys.Content, "BASE PERSONA") {
		t.Errorf("tool note replaced the base persona instead of appending:\n%s", synthSys.Content)
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
