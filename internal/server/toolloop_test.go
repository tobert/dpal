package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"

	"github.com/tobert/dpal/internal/explorer"
)

// toolCallResp builds a scripted assistant response that wants to call
// one tool and returns no terminal content.
func toolCallResp(name, argsJSON, callID string) *deepseek.ChatCompletionResponse {
	return &deepseek.ChatCompletionResponse{
		Model: "deepseek-chat",
		Choices: []deepseek.Choice{{
			FinishReason: "tool_calls",
			Message: deepseek.Message{
				Role: deepseek.ChatMessageRoleAssistant,
				ToolCalls: []deepseek.ToolCall{{
					ID:   callID,
					Type: "function",
					Function: deepseek.ToolCallFunction{
						Name:      name,
						Arguments: argsJSON,
					},
				}},
			},
		}},
	}
}

func finalResp(content string) *deepseek.ChatCompletionResponse {
	return &deepseek.ChatCompletionResponse{
		Model: "deepseek-reasoner",
		Choices: []deepseek.Choice{{
			FinishReason: "stop",
			Message: deepseek.Message{
				Role:    deepseek.ChatMessageRoleAssistant,
				Content: content,
			},
		}},
	}
}

func newSandboxExplorer(t *testing.T) *explorer.Explorer {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello there"), 0o644); err != nil {
		t.Fatal(err)
	}
	exp, err := explorer.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return exp
}

// TestExplorePhase_DispatchesToolCallAndRecalls covers the inner chat()
// loop as used by Consult's explore phase: tool_call -> dispatched ->
// result fed back -> recall -> final text -> stripped before synth.
func TestExplorePhase_DispatchesToolCallAndRecalls(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			// Explore phase: tool call, then final text (will be stripped).
			toolCallResp("read_file", `{"path":"note.txt"}`, "call-1"),
			finalResp("Exploration complete."),
			// Synth phase: writes the actual answer.
			finalResp("Note says: hello there."),
		},
	}
	s := New(rec).WithExplorer(exp)

	_, out, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "Please read note.txt"})
	if err != nil {
		t.Fatalf("Consult: %v", err)
	}
	if !strings.Contains(out.Content, "hello there") {
		t.Errorf("final content does not reflect tool result: %q", out.Content)
	}
	if out.ExplorerToolCalls != 1 {
		t.Errorf("explorer_tool_calls = %d, want 1", out.ExplorerToolCalls)
	}
	if len(rec.requests) != 3 {
		t.Fatalf("expected 3 upstream calls (explore-1, explore-2, synth), got %d", len(rec.requests))
	}

	// Explore-1: just the user prompt.
	if got := len(rec.requests[0].Messages); got != 1 {
		t.Errorf("explore-1 messages = %d, want 1", got)
	}

	// Explore-2: user + assistant(tool_calls) + tool(result) = 3.
	exploreRecall := rec.requests[1].Messages
	if len(exploreRecall) != 3 {
		t.Fatalf("explore-2 messages = %d, want 3", len(exploreRecall))
	}
	if exploreRecall[1].Role != deepseek.ChatMessageRoleAssistant || len(exploreRecall[1].ToolCalls) != 1 {
		t.Errorf("explore-2[1] not an assistant tool_calls message: %+v", exploreRecall[1])
	}
	if exploreRecall[2].Role != "tool" || exploreRecall[2].ToolCallID != "call-1" {
		t.Errorf("explore-2[2] not the tool result with call_id=call-1: %+v", exploreRecall[2])
	}
	if !strings.Contains(exploreRecall[2].Content, "hello there") {
		t.Errorf("tool result did not include file body: %q", exploreRecall[2].Content)
	}

	// Synth: same user + tool exchanges WITHOUT the explorer's text answer.
	synth := rec.requests[2].Messages
	if len(synth) != 3 {
		t.Fatalf("synth messages = %d, want 3 (user, assistant-tool_calls, tool); explorer's text should have been stripped", len(synth))
	}
	for _, m := range synth {
		if m.Role == deepseek.ChatMessageRoleAssistant && m.Content == "Exploration complete." {
			t.Errorf("explorer's text synthesis leaked into synth request: %+v", m)
		}
	}
}

// TestExplorePhase_NoToolsAdvertisedDuringSynth verifies the synth call
// has Tools=nil. The synthesizer should be reading exploration results,
// not running further tool calls itself.
func TestExplorePhase_NoToolsAdvertisedDuringSynth(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			finalResp("No exploration needed."), // explorer makes zero tool calls
			finalResp("synth answer"),
		},
	}
	s := New(rec).WithExplorer(exp)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(rec.requests))
	}
	if len(rec.requests[0].Tools) != 4 {
		t.Errorf("explore phase Tools = %d, want 4", len(rec.requests[0].Tools))
	}
	if len(rec.requests[1].Tools) != 0 {
		t.Errorf("synth phase Tools = %d, want 0 (no tools in synth)", len(rec.requests[1].Tools))
	}
}

// TestExplorePhase_TrivialPromptStillPaysForSynth is the deliberate
// no-shortcut guarantee: even when the explorer makes zero tool calls,
// we still run the synthesizer so the answer's voice is consistent.
func TestExplorePhase_TrivialPromptStillPaysForSynth(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			finalResp("No exploration needed."),
			finalResp("4"),
		},
	}
	s := New(rec).WithExplorer(exp)

	_, out, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "what is 2+2?"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 2 {
		t.Errorf("expected 2 upstream calls (explore + synth) even for trivial prompts, got %d", len(rec.requests))
	}
	if out.Content != "4" {
		t.Errorf("content = %q, want %q (must come from synth, not explorer)", out.Content, "4")
	}
	if out.ExplorerToolCalls != 0 {
		t.Errorf("explorer_tool_calls = %d, want 0", out.ExplorerToolCalls)
	}
}

// TestExplorePhase_ToolsAttachedOnlyWhenExplorerPresent — the no-explore
// case (Server.explorer == nil) still works and just runs the synth.
func TestExplorePhase_NoExplorerSkipsExploreEntirely(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{finalResp("ok")}}
	s := New(rec) // no explorer

	_, out, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 1 {
		t.Errorf("expected exactly 1 upstream call when explorer is nil, got %d", len(rec.requests))
	}
	if len(rec.requests[0].Tools) != 0 {
		t.Errorf("Tools should be empty without explorer, got %d", len(rec.requests[0].Tools))
	}
	if out.ExplorerModel != "" {
		t.Errorf("explorer_model = %q, want empty when explore phase did not run", out.ExplorerModel)
	}
}

// TestExplorePhase_OneshotRejectsToolCalls — ConsultOneshot does not
// advertise tools and does not configure an explorer, so a model that
// nonetheless emits tool_calls must fail loudly rather than be silently
// dropped.
func TestExplorePhase_OneshotRejectsToolCalls(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			toolCallResp("read_file", `{"path":"x"}`, "call-1"),
		},
	}
	s := New(rec) // no explorer

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error when oneshot upstream emits tool_calls")
	}
	if !strings.Contains(err.Error(), "no explorer") {
		t.Errorf("error message should mention missing explorer, got: %v", err)
	}
}

// TestExplorePhase_HitsMaxIterationsAndErrors — the explore phase honors
// the tool-iteration cap and aborts the whole Consult call without ever
// reaching the synth phase.
func TestExplorePhase_HitsMaxIterationsAndErrors(t *testing.T) {
	exp := newSandboxExplorer(t)
	infinite := []*deepseek.ChatCompletionResponse{}
	for range 20 {
		infinite = append(infinite, toolCallResp("list_directory", `{"path":"."}`, "x"))
	}
	rec := &recordingClient{responses: infinite}
	s := New(rec).WithExplorer(exp)
	s.maxToolIterations = 3

	_, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "loop forever"})
	if err == nil {
		t.Fatal("expected error when loop exceeds iteration cap")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error message should mention exceeded iterations, got: %v", err)
	}
	if len(rec.requests) != 3 {
		t.Errorf("expected 3 upstream calls before hitting cap, got %d (synth phase must not run)", len(rec.requests))
	}
}

// TestConsult_PersistsToolExchangesInHistory — Amy's explicit design
// call: session.messages holds [user, assistant(tool_calls), tool, ...,
// synth-answer]. Next turn's request includes that full history.
func TestConsult_PersistsToolExchangesInHistory(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			// Turn 1 explore: one tool call, then text (stripped).
			toolCallResp("read_file", `{"path":"note.txt"}`, "c1"),
			finalResp("Exploration complete."),
			// Turn 1 synth: writes the answer.
			finalResp("file says hello there"),
			// Turn 2 explore: no tool calls.
			finalResp("No exploration needed."),
			// Turn 2 synth.
			finalResp("you're welcome"),
		},
	}
	s := New(rec).WithExplorer(exp)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "read note.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "thanks"}); err != nil {
		t.Fatal(err)
	}

	// After turn 1, sess.messages = [u1, a1-tool_calls, t1-result, a1-synth] = 4.
	// Turn 2 explore sees those 4 + u2 = 5.
	turn2Explore := rec.requests[3].Messages
	if len(turn2Explore) != 5 {
		t.Fatalf("turn 2 explore messages = %d, want 5 (u1, a-tool, tool, synth, u2)", len(turn2Explore))
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant", "user"}
	for i, want := range wantRoles {
		if turn2Explore[i].Role != want {
			gotRoles := make([]string, len(turn2Explore))
			for j, m := range turn2Explore {
				gotRoles[j] = m.Role
			}
			t.Fatalf("turn 2 explore role[%d] = %q, want %q; full: %v", i, turn2Explore[i].Role, want, gotRoles)
		}
	}
	if turn2Explore[2].ToolCallID != "c1" {
		t.Errorf("tool message tool_call_id = %q, want c1", turn2Explore[2].ToolCallID)
	}
}
