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
		Model: "deepseek-reasoner",
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

func TestChat_DispatchesToolCallAndRecalls(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			toolCallResp("read_file", `{"path":"note.txt"}`, "call-1"),
			finalResp("I read the file and saw: hello there."),
		},
	}
	s := New(rec).WithExplorer(exp)

	_, out, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "Please read note.txt"})
	if err != nil {
		t.Fatalf("ConsultOneshot: %v", err)
	}
	if !strings.Contains(out.Content, "hello there") {
		t.Errorf("final content does not reflect tool result: %q", out.Content)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(rec.requests))
	}
	// First request: only the user prompt.
	if len(rec.requests[0].Messages) != 1 {
		t.Errorf("first request messages = %d, want 1", len(rec.requests[0].Messages))
	}
	// Second request: user + assistant(tool_calls) + tool(result) = 3.
	second := rec.requests[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request messages = %d, want 3", len(second))
	}
	if second[1].Role != deepseek.ChatMessageRoleAssistant || len(second[1].ToolCalls) != 1 {
		t.Errorf("expected assistant turn with tool_calls at index 1, got %+v", second[1])
	}
	if second[2].Role != "tool" || second[2].ToolCallID != "call-1" {
		t.Errorf("expected tool result at index 2 with call_id=call-1, got %+v", second[2])
	}
	if !strings.Contains(second[2].Content, "hello there") {
		t.Errorf("tool result content did not include file body: %q", second[2].Content)
	}
}

func TestChat_ToolsOnlyAttachedWhenExplorerPresent(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{finalResp("ok")}}
	s := New(rec) // no explorer

	if _, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.requests[0].Tools) != 0 {
		t.Errorf("Tools should be empty without explorer, got %d", len(rec.requests[0].Tools))
	}
}

func TestChat_ToolsAttachedWhenExplorerPresent(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{finalResp("ok")}}
	s := New(rec).WithExplorer(exp)

	if _, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.requests[0].Tools) != 3 {
		t.Errorf("Tools attached = %d, want 3 (list_directory, read_file, search_project)", len(rec.requests[0].Tools))
	}
}

func TestChat_RejectsToolCallWithoutExplorer(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			toolCallResp("read_file", `{"path":"x"}`, "call-1"),
		},
	}
	s := New(rec) // no explorer

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error when DeepSeek requests a tool but no explorer is configured")
	}
	if !strings.Contains(err.Error(), "no explorer") {
		t.Errorf("error message should mention missing explorer, got: %v", err)
	}
}

func TestChat_HitsMaxIterationsAndErrors(t *testing.T) {
	exp := newSandboxExplorer(t)
	// Build a client that always responds with another tool call.
	infinite := []*deepseek.ChatCompletionResponse{}
	for range 20 {
		infinite = append(infinite, toolCallResp("list_directory", `{"path":"."}`, "x"))
	}
	rec := &recordingClient{responses: infinite}
	s := New(rec).WithExplorer(exp)
	s.maxToolIterations = 3

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "loop forever"})
	if err == nil {
		t.Fatal("expected error when loop exceeds iteration cap")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error message should mention exceeded iterations, got: %v", err)
	}
	if len(rec.requests) != 3 {
		t.Errorf("expected 3 upstream calls before hitting cap, got %d", len(rec.requests))
	}
}

func TestConsult_PersistsToolExchangesInHistory(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			toolCallResp("read_file", `{"path":"note.txt"}`, "c1"),
			finalResp("file says hello there"),
			finalResp("follow-up answer"),
		},
	}
	s := New(rec).WithExplorer(exp)

	// First turn: triggers a tool call.
	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "read note.txt"}); err != nil {
		t.Fatal(err)
	}

	// Second turn: history should already include user + assistant(tool_calls) + tool(result) + assistant(final).
	// The next call should resend all of that as context.
	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "thanks"}); err != nil {
		t.Fatal(err)
	}

	// After turn 1: history = [u1, a1-tool, t1, a1-final] = 4 msgs.
	// Turn 2's request body should be history + new user prompt = 5.
	third := rec.requests[2].Messages
	if len(third) != 5 {
		t.Fatalf("third request messages = %d, want 5 (u, a-tool, tool, a-final, u-2); got: %+v", len(third), third)
	}
	if third[0].Role != "user" || third[1].Role != "assistant" || third[2].Role != "tool" ||
		third[3].Role != "assistant" || third[4].Role != "user" {
		got := []string{third[0].Role, third[1].Role, third[2].Role, third[3].Role, third[4].Role}
		t.Errorf("role sequence = %v, want [user assistant tool assistant user]", got)
	}
	if third[2].ToolCallID != "c1" {
		t.Errorf("tool message tool_call_id = %q, want c1", third[2].ToolCallID)
	}
}
