package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
)

// TestTwoPhase_DisableExploreSkipsExplorePhase — the per-call escape
// hatch lets a caller opt out of the explore phase even when an
// explorer is configured.
func TestTwoPhase_DisableExploreSkipsExplorePhase(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{finalResp("direct answer")},
	}
	s := New(rec).WithExplorer(exp)

	_, out, err := s.Consult(context.Background(), nil, ConsultInput{
		SessionID:      "s1",
		Prompt:         "answer this directly",
		DisableExplore: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 1 {
		t.Errorf("expected exactly 1 upstream call when disable_explore is set, got %d", len(rec.requests))
	}
	if len(rec.requests[0].Tools) != 0 {
		t.Errorf("synth-only call should advertise 0 tools, got %d", len(rec.requests[0].Tools))
	}
	if out.ExplorerModel != "" {
		t.Errorf("explorer_model = %q, want empty when explore was disabled", out.ExplorerModel)
	}
	if out.ExplorerToolCalls != 0 {
		t.Errorf("explorer_tool_calls = %d, want 0", out.ExplorerToolCalls)
	}
}

// TestTwoPhase_ExplorerFailureAbortsCall — an error during the explore
// phase aborts the whole Consult and leaves the session's durable
// history unchanged so a retry doesn't double-send the user prompt.
func TestTwoPhase_ExplorerFailureAbortsCall(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{err: errors.New("503 from explorer")}
	s := New(rec).WithExplorer(exp)

	_, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error from failing explore phase")
	}
	if !strings.Contains(err.Error(), "explore") {
		t.Errorf("error should identify explore phase, got: %v", err)
	}

	// Retry with the explorer working: session should be empty, so the
	// new explore phase sees exactly one message (the user prompt).
	rec.err = nil
	rec.responses = []*deepseek.ChatCompletionResponse{
		finalResp("No exploration needed."),
		finalResp("hi back"),
	}
	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// Index 1 here = the first request after the failure (rec.requests[1]).
	if got := len(rec.requests[1].Messages); got != 1 {
		t.Errorf("retry's explore phase sent %d messages, want 1 (rollback did not happen)", got)
	}
}

// TestTwoPhase_HonorsExplorerModelOverride — explorer_model on the
// input picks which model runs the explore phase.
func TestTwoPhase_HonorsExplorerModelOverride(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			finalResp("No exploration needed."),
			finalResp("synth answer"),
		},
	}
	s := New(rec).WithExplorer(exp)

	_, out, err := s.Consult(context.Background(), nil, ConsultInput{
		SessionID:     "s1",
		Prompt:        "hi",
		ExplorerModel: "custom-explorer-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.requests[0].Model != "custom-explorer-model" {
		t.Errorf("explore-phase model = %q, want %q", rec.requests[0].Model, "custom-explorer-model")
	}
	if rec.requests[1].Model != ModelV4Pro {
		t.Errorf("synth-phase model = %q, want %q", rec.requests[1].Model, ModelV4Pro)
	}
	if out.ExplorerModel != "custom-explorer-model" {
		t.Errorf("out.ExplorerModel = %q, want %q", out.ExplorerModel, "custom-explorer-model")
	}
}

// TestTwoPhase_ExplorerSystemPromptScoped — the explorer's system
// prompt is what we configured for the explore phase, not the
// synthesizer's. Per-call override beats the server default.
func TestTwoPhase_ExplorerSystemPromptScoped(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			finalResp("No exploration needed."),
			finalResp("synth"),
		},
	}
	s := New(rec).
		WithSystemPrompt("SYNTH-SYS").
		WithExplorerSystemPrompt("EXPLORER-SYS").
		WithExplorer(exp)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.requests[0].Messages[0].Content; got != "EXPLORER-SYS" {
		t.Errorf("explore-phase system prompt = %q, want EXPLORER-SYS", got)
	}
	// The synth prompt is the configured synth persona (SYNTH-SYS), not the
	// explorer's — plus the conditional tool note, since the explore phase ran
	// and the synth carries tools.
	if got := rec.requests[1].Messages[0].Content; !strings.HasPrefix(got, "SYNTH-SYS") || strings.Contains(got, "EXPLORER-SYS") {
		t.Errorf("synth-phase system prompt = %q, want it scoped to the SYNTH-SYS persona", got)
	}
}

// TestTwoPhase_NoExplorerSystemPromptFallsBackToSynth — if no explorer
// system prompt is configured, the explore phase reuses the
// synthesizer's system prompt rather than sending no system message.
func TestTwoPhase_NoExplorerSystemPromptFallsBackToSynth(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			finalResp("No exploration needed."),
			finalResp("synth"),
		},
	}
	s := New(rec).
		WithSystemPrompt("ONLY-SYS").
		WithExplorer(exp)
	// no WithExplorerSystemPrompt

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.requests[0].Messages[0].Content; got != "ONLY-SYS" {
		t.Errorf("explore-phase system prompt = %q, want fallback to synth's ONLY-SYS", got)
	}
}
