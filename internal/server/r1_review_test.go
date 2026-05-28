package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
)

// TestExplorePhase_ToolErrorDoesNotStallLoop — flagged by R1's review:
// when a tool dispatch returns an error string (e.g. file not found),
// the explorer model should be able to recover and stop. The tool error
// must be visible to the model as the tool message Content (not
// swallowed), and a model that decides to stop in response should
// produce a clean end-to-end Consult.
func TestExplorePhase_ToolErrorDoesNotStallLoop(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			// Explorer asks for a file that doesn't exist.
			toolCallResp("read_file", `{"path":"does-not-exist.txt"}`, "c1"),
			// Sees the error result, decides to stop rather than retry.
			finalResp("Exploration aborted: target file missing."),
			// Synth answers from whatever context was loaded.
			finalResp("The requested file is not in the project."),
		},
	}
	s := New(rec).WithExplorer(exp)

	_, out, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "read the missing file"})
	if err != nil {
		t.Fatalf("Consult: %v", err)
	}
	if len(rec.requests) != 3 {
		t.Fatalf("expected 3 upstream calls (explore-1, explore-2, synth), got %d", len(rec.requests))
	}

	// The second upstream request is the explorer's recall. It must
	// carry the tool message with the error string as Content so the
	// model can see and react to it.
	recall := rec.requests[1].Messages
	var toolMsg *deepseek.ChatCompletionMessage
	for i := range recall {
		if recall[i].Role == "tool" && recall[i].ToolCallID == "c1" {
			toolMsg = &recall[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool message for c1 in recall request; got: %+v", recall)
	}
	if !strings.HasPrefix(toolMsg.Content, "error:") {
		t.Errorf("tool result should surface the error to the model; got: %q", toolMsg.Content)
	}

	if out.ExplorerToolCalls != 1 {
		t.Errorf("explorer_tool_calls = %d, want 1", out.ExplorerToolCalls)
	}
	if !strings.Contains(out.Content, "not in the project") {
		t.Errorf("synth content = %q, want it to reflect the missing-file outcome", out.Content)
	}
}

// TestConsult_ConcurrentSameSessionSerializesViaMutex — flagged by R1's
// review: two goroutines calling Consult on the same session_id at the
// same time must serialize via sess.mu. The session ends up with both
// turns in order, no corruption, both calls return without error.
//
// We rely on the recordingClient seeing requests in serial order (one
// goroutine per Consult, sess.mu held for the whole call) so its
// internal idx counter is safe without its own mutex.
func TestConsult_ConcurrentSameSessionSerializesViaMutex(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			makeResp("answer-1", "thinking-1"),
			makeResp("answer-2", "thinking-2"),
		},
	}
	s := New(rec)

	const sessID = "concurrent"
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.Consult(context.Background(), nil, ConsultInput{
				SessionID: sessID,
				Prompt:    fmt.Sprintf("Q%d", i),
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// After both turns: sess.messages = [user, assistant, user, assistant].
	// The user prompts may be Q0,Q1 or Q1,Q0 depending on which
	// goroutine won the mutex first — both orderings are correct, the
	// test just checks that we got exactly 4 messages and no corruption.
	sess := s.sessions.byID[sessID]
	if sess == nil {
		t.Fatal("session not stored")
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.messages) != 4 {
		t.Fatalf("session has %d messages, want 4 (u, a, u, a)", len(sess.messages))
	}
	wantRoles := []string{"user", "assistant", "user", "assistant"}
	for i, want := range wantRoles {
		if sess.messages[i].Role != want {
			t.Errorf("messages[%d].Role = %q, want %q", i, sess.messages[i].Role, want)
		}
	}
}

// TestConsult_ThinkingModeOnByDefaultForSynth — V4 behavior: the synth
// call uses thinking mode (EnableThinking: true) unless explicitly
// overridden. The explore phase always runs without thinking regardless.
func TestConsult_ThinkingModeOnByDefaultForSynth(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			finalResp("No exploration needed."),
			makeResp("synth", "synth-reasoning"),
		},
	}
	s := New(rec).WithExplorer(exp)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(rec.requests))
	}
	if rec.requests[0].EnableThinking {
		t.Errorf("explore phase EnableThinking = true, want false (exploration is not a thinking-mode workload)")
	}
	if !rec.requests[1].EnableThinking {
		t.Errorf("synth phase EnableThinking = false, want true (V4-Pro default)")
	}
}

// TestConsult_ThinkingModeOverride — the per-call thinking knob lets a
// caller turn off thinking on the synth phase when they want speed over
// depth.
func TestConsult_ThinkingModeOverride(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{makeResp("fast", "")},
	}
	s := New(rec)

	off := false
	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{
		SessionID: "s1",
		Prompt:    "be fast",
		Thinking:  &off,
	}); err != nil {
		t.Fatal(err)
	}
	if rec.requests[0].EnableThinking {
		t.Errorf("EnableThinking = true, want false (per-call override)")
	}
}
