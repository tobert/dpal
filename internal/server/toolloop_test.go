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

	// Synth: the explorer's curated report is folded into the user prompt;
	// the raw tool transcript does NOT cross over.
	synth := rec.requests[2].Messages
	if len(synth) != 1 {
		t.Fatalf("synth messages = %d, want 1 (the user prompt with the report folded in)", len(synth))
	}
	if synth[0].Role != deepseek.ChatMessageRoleUser {
		t.Errorf("synth[0] role = %q, want user", synth[0].Role)
	}
	if !strings.Contains(synth[0].Content, "Please read note.txt") {
		t.Errorf("synth user message lost the original prompt: %q", synth[0].Content)
	}
	if !strings.Contains(synth[0].Content, "Exploration complete.") {
		t.Errorf("synth user message missing the explorer report: %q", synth[0].Content)
	}
	for _, m := range synth {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Errorf("raw tool transcript leaked into synth request: %+v", m)
		}
	}
}

// TestExplorePhase_SynthGetsToolsWhenExploreRan verifies the synth call
// is offered the explorer's tools (auto tool_choice) so it can fetch a
// precise span the report didn't quote. This is the Option-2 hybrid: the
// report is primary, the tools are the fallback.
func TestExplorePhase_SynthGetsToolsWhenExploreRan(t *testing.T) {
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
	if len(rec.requests[0].Tools) != 5 {
		t.Errorf("explore phase Tools = %d, want 5", len(rec.requests[0].Tools))
	}
	if len(rec.requests[1].Tools) != 5 {
		t.Errorf("synth phase Tools = %d, want 5 (synth may fetch beyond the report)", len(rec.requests[1].Tools))
	}
}

// TestConsult_SynthCanFetchBeyondReport — the Option-2 hybrid end to end:
// the explorer hands over a report that points at note.txt without
// quoting it, the synthesizer reads the file itself to get the exact
// text, and the answer reflects it. Crucially, the synth's own tool
// exchange does NOT persist — durable history stays lean [user, answer].
func TestConsult_SynthCanFetchBeyondReport(t *testing.T) {
	exp := newSandboxExplorer(t) // note.txt contains "hello there"
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			// Explore: load note.txt, then report a pointer (no full quote).
			toolCallResp("read_file", `{"path":"note.txt"}`, "c1"),
			finalResp("BEGIN EXPLORATION\nnote.txt:1 holds a greeting; read it for the exact wording\nEND OF EXPLORATION"),
			// Synth: pro decides it needs the exact text and reads the file.
			toolCallResp("read_file", `{"path":"note.txt"}`, "s1"),
			// Synth: final answer after the fetch.
			finalResp("The note says hello there."),
		},
	}
	s := New(rec).WithExplorer(exp)

	_, out, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "what exactly does the note say?"})
	if err != nil {
		t.Fatal(err)
	}

	// 4 upstream calls: explore-1 (tool), explore-2 (report), synth-1 (tool), synth-2 (final).
	if len(rec.requests) != 4 {
		t.Fatalf("expected 4 upstream calls, got %d", len(rec.requests))
	}
	if len(rec.requests[2].Tools) != 5 {
		t.Errorf("synth phase should advertise the explorer's 5 tools, got %d", len(rec.requests[2].Tools))
	}
	// The synth's fallback read was dispatched and fed back as a tool result.
	synth2 := rec.requests[3].Messages
	var sawFetch bool
	for _, m := range synth2 {
		if m.Role == "tool" && m.ToolCallID == "s1" && strings.Contains(m.Content, "hello there") {
			sawFetch = true
		}
	}
	if !sawFetch {
		t.Errorf("synth's fallback read was not dispatched back as a tool result: %+v", synth2)
	}
	if !strings.Contains(out.Content, "hello there") {
		t.Errorf("answer did not reflect the fetched text: %q", out.Content)
	}

	// Lean persistence: the synth tool exchange is ephemeral; history is
	// [user, final-answer] only.
	sess := s.sessions.byID["s1"]
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.messages) != 2 {
		t.Fatalf("session has %d messages, want 2 (lean [user, answer])", len(sess.messages))
	}
	for _, m := range sess.messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Errorf("synth tool exchange leaked into durable history: %+v", m)
		}
	}
	if sess.messages[1].Content != "The note says hello there." {
		t.Errorf("persisted answer = %q, want the synth final answer", sess.messages[1].Content)
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

// cappingClient models a WELL-BEHAVED model: it keeps requesting a tool
// every turn, but the moment the loop forces tool_choice:"none" (the
// graceful cap-hit) it returns a final text answer instead of more tool
// calls. This is what lets the loop wind down to a report rather than
// erroring out and discarding everything the model loaded.
type cappingClient struct {
	requests []*deepseek.ChatCompletionRequest
	final    string // content returned once tools are forbidden
}

func (c *cappingClient) CreateChatCompletion(_ context.Context, req *deepseek.ChatCompletionRequest) (*deepseek.ChatCompletionResponse, error) {
	c.requests = append(c.requests, req)
	if req.ToolChoice == toolChoiceNone {
		content := c.final
		if content == "" {
			content = "report from what I loaded"
		}
		return finalResp(content), nil
	}
	return toolCallResp("list_directory", `{"path":"."}`, "x"), nil
}

// TestChat_GracefulStopAtCap — the core of the cap-hit fix. When a model
// keeps requesting tools, the loop forces tool_choice:"none" on its final
// permitted turn so the model produces a report from what it loaded,
// instead of the loop exhausting its budget and erroring (which discarded
// every file the model had read).
func TestChat_GracefulStopAtCap(t *testing.T) {
	exp := newSandboxExplorer(t)
	c := &cappingClient{final: "forced report"}
	s := New(c).WithExplorer(exp)
	s.maxToolIterations = 3

	msgs := []deepseek.ChatCompletionMessage{{Role: deepseek.ChatMessageRoleUser, Content: "explore"}}
	resp, out, err := s.chat(context.Background(), "m", "sys", exp, msgs, true, "", "")
	if err != nil {
		t.Fatalf("cap-hit should wind down to a report, got error: %v", err)
	}
	if len(c.requests) != 3 {
		t.Fatalf("expected exactly maxIter=3 upstream calls, got %d", len(c.requests))
	}
	// The final permitted call must forbid tools so the model has to answer.
	if c.requests[2].ToolChoice != toolChoiceNone {
		t.Errorf("final call tool_choice = %q, want %q", c.requests[2].ToolChoice, toolChoiceNone)
	}
	// Earlier calls must NOT be pinned to none — the model is free to explore.
	if c.requests[0].ToolChoice == toolChoiceNone {
		t.Errorf("first call was pinned to none; the model must be free to call tools early")
	}
	if resp.Choices[0].Message.Content != "forced report" {
		t.Errorf("final content = %q, want the forced report", resp.Choices[0].Message.Content)
	}
	// The returned history ends on the report (assistant, no tool calls).
	last := out[len(out)-1]
	if last.Role != deepseek.ChatMessageRoleAssistant || len(last.ToolCalls) != 0 {
		t.Errorf("history should end on the report turn, got %+v", last)
	}
}

// TestChat_InjectsBudgetNotes — the model is told how much budget remains
// so it can self-pace, instead of crawling blind until it's cut off. The
// note rides as a trailing system message (kept off the cached prefix).
func TestChat_InjectsBudgetNotes(t *testing.T) {
	exp := newSandboxExplorer(t)
	c := &cappingClient{}
	s := New(c).WithExplorer(exp)
	s.maxToolIterations = 3

	msgs := []deepseek.ChatCompletionMessage{{Role: deepseek.ChatMessageRoleUser, Content: "explore"}}
	if _, _, err := s.chat(context.Background(), "m", "sys", exp, msgs, true, "", ""); err != nil {
		t.Fatal(err)
	}

	// A warning note appears while budget remains (iter 0: 2 calls left).
	warn := lastSystemNote(c.requests[0])
	if !strings.Contains(warn, "remain") || strings.Contains(warn, "final turn") {
		t.Errorf("first call should carry the budget-remaining note, trailing system msg = %q", warn)
	}
	// The final call carries the finalize note, not a "remaining" warning.
	final := lastSystemNote(c.requests[2])
	if !strings.Contains(final, "final turn") {
		t.Errorf("final call missing the finalize note, trailing system msg = %q", final)
	}
}

// lastSystemNote returns the content of the request's trailing message if
// it is a system message, else "". The budget note is injected at the tail
// so it never disturbs the cacheable system+history prefix.
func lastSystemNote(req *deepseek.ChatCompletionRequest) string {
	if len(req.Messages) == 0 {
		return ""
	}
	m := req.Messages[len(req.Messages)-1]
	if m.Role != deepseek.ChatMessageRoleSystem {
		return ""
	}
	return m.Content
}

// TestExplorePhase_MisbehavingModelStillErrorsAtCap — the graceful cap-hit
// relies on the model honoring tool_choice:"none". A model that IGNORES it
// and keeps emitting tool calls right through the final forced turn must
// still fail loudly rather than loop forever or return corrupt output.
// (recordingClient ignores tool_choice, so it stands in for that model.)
func TestExplorePhase_MisbehavingModelStillErrorsAtCap(t *testing.T) {
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
		t.Fatal("expected error when a model ignores forced tool_choice and never stops")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error message should mention exceeded iterations, got: %v", err)
	}
	if len(rec.requests) != 3 {
		t.Errorf("expected 3 upstream calls before hitting cap, got %d (synth phase must not run)", len(rec.requests))
	}
}

// TestConsult_PersistsLeanHistory — with the report hand-off, the session
// keeps only [user-prompt, synth-answer] per turn. The explorer's tool
// exchanges and its report are ephemeral: gathered for that turn's synth
// call, never stored. This replaces the old "persist tool exchanges"
// model, which only existed because the raw transcript WAS the hand-off;
// with reports there is nothing to persist.
func TestConsult_PersistsLeanHistory(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			// Turn 1 explore: one tool call, then a report.
			toolCallResp("read_file", `{"path":"note.txt"}`, "c1"),
			finalResp("Loaded note.txt; it says hello there."),
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

	// After turn 1, sess.messages = [u1, synth-answer] = 2 (no tool exchanges,
	// no report). Turn 2 explore sees those 2 + u2 = 3.
	turn2Explore := rec.requests[3].Messages
	if len(turn2Explore) != 3 {
		t.Fatalf("turn 2 explore messages = %d, want 3 (u1, synth-answer, u2)", len(turn2Explore))
	}
	wantRoles := []string{"user", "assistant", "user"}
	for i, want := range wantRoles {
		if turn2Explore[i].Role != want {
			gotRoles := make([]string, len(turn2Explore))
			for j, m := range turn2Explore {
				gotRoles[j] = m.Role
			}
			t.Fatalf("turn 2 explore role[%d] = %q, want %q; full: %v", i, turn2Explore[i].Role, want, gotRoles)
		}
	}
	// No tool exchanges should ever reach durable history.
	for _, m := range turn2Explore {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Errorf("tool exchange leaked into durable history: %+v", m)
		}
	}
	// The persisted assistant turn is the SYNTH answer, not the explorer's report.
	if turn2Explore[1].Content != "file says hello there" {
		t.Errorf("persisted assistant = %q, want the synth answer", turn2Explore[1].Content)
	}
}
