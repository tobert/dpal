package server

import (
	"context"
	"errors"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
)

// recordingClient captures every request and replays a scripted sequence
// of responses so tests can assert on the history sent across multiple
// turns of the same session.
type recordingClient struct {
	requests []*deepseek.ChatCompletionRequest
	responses []*deepseek.ChatCompletionResponse
	err      error
	idx      int
}

func (r *recordingClient) CreateChatCompletion(ctx context.Context, req *deepseek.ChatCompletionRequest) (*deepseek.ChatCompletionResponse, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return nil, r.err
	}
	if r.idx >= len(r.responses) {
		return nil, errors.New("recordingClient: ran out of scripted responses")
	}
	resp := r.responses[r.idx]
	r.idx++
	return resp, nil
}

func TestConsult_FirstCallSendsSingleUserMessage(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{makeResp("hi there", "thinking…")},
	}
	s := New(rec)

	_, out, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(rec.requests[0].Messages); got != 1 {
		t.Fatalf("first request had %d messages, want 1", got)
	}
	if rec.requests[0].Messages[0].Content != "hello" {
		t.Errorf("first message content = %q, want %q", rec.requests[0].Messages[0].Content, "hello")
	}
	if out.TurnCount != 1 {
		t.Errorf("turn_count = %d, want 1", out.TurnCount)
	}
	if out.SessionID != "s1" {
		t.Errorf("session_id = %q, want %q", out.SessionID, "s1")
	}
	if out.ReasoningContent != "thinking…" {
		t.Errorf("reasoning_content = %q, want %q", out.ReasoningContent, "thinking…")
	}
}

func TestConsult_SecondCallIncludesPriorHistory(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			makeResp("first answer", "first thoughts"),
			makeResp("second answer", "second thoughts"),
		},
	}
	s := New(rec)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "Q1"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, out, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "Q2"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	second := rec.requests[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request had %d messages, want 3 (user, assistant, user)", len(second))
	}
	if second[0].Role != deepseek.ChatMessageRoleUser || second[0].Content != "Q1" {
		t.Errorf("msg[0] = {%q, %q}, want user/Q1", second[0].Role, second[0].Content)
	}
	if second[1].Role != deepseek.ChatMessageRoleAssistant || second[1].Content != "first answer" {
		t.Errorf("msg[1] = {%q, %q}, want assistant/first answer", second[1].Role, second[1].Content)
	}
	if second[2].Role != deepseek.ChatMessageRoleUser || second[2].Content != "Q2" {
		t.Errorf("msg[2] = {%q, %q}, want user/Q2", second[2].Role, second[2].Content)
	}
	if out.TurnCount != 2 {
		t.Errorf("turn_count = %d, want 2", out.TurnCount)
	}
}

func TestConsult_OmitsReasoningContentFromHistory(t *testing.T) {
	// DeepSeek explicitly instructs callers NOT to echo reasoning_content
	// back in subsequent requests. This regression test guards that.
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			makeResp("a", "should-not-be-resent"),
			makeResp("b", ""),
		},
	}
	s := New(rec)

	_, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "Q1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "Q2"}); err != nil {
		t.Fatal(err)
	}

	for _, m := range rec.requests[1].Messages {
		if m.ReasoningContent != "" {
			t.Fatalf("reasoning_content leaked into history: %q", m.ReasoningContent)
		}
	}
}

func TestConsult_SessionsAreIsolated(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			makeResp("a-reply", ""),
			makeResp("b-reply", ""),
		},
	}
	s := New(rec)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "a", Prompt: "for-a"}); err != nil {
		t.Fatal(err)
	}
	_, out, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "b", Prompt: "for-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.requests[1].Messages) != 1 {
		t.Errorf("second session's first request had %d messages, want 1 (sessions bled together)", len(rec.requests[1].Messages))
	}
	if out.TurnCount != 1 {
		t.Errorf("session b turn_count = %d, want 1", out.TurnCount)
	}
}

func TestConsult_RejectsEmptySessionID(t *testing.T) {
	rec := &recordingClient{}
	s := New(rec)
	_, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "  ", Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error for empty session_id")
	}
	if len(rec.requests) != 0 {
		t.Error("client should not be called when session_id is missing")
	}
}

func TestConsult_RejectsEmptyPrompt(t *testing.T) {
	rec := &recordingClient{}
	s := New(rec)
	_, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: ""})
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestConsult_FailedCallRollsBackHistory(t *testing.T) {
	// A failed API call should not leave the speculatively-appended user
	// turn in history, so a retry with the same prompt doesn't double-send.
	rec := &recordingClient{err: errors.New("503")}
	s := New(rec)

	_, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hello"})
	if err == nil {
		t.Fatal("expected error from failing client")
	}

	rec.err = nil
	rec.responses = []*deepseek.ChatCompletionResponse{makeResp("hi", "")}
	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hello"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(rec.requests[1].Messages) != 1 {
		t.Errorf("retry sent %d messages, want 1 (rollback did not happen)", len(rec.requests[1].Messages))
	}
}

func TestConsult_HonorsModelOverride(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{makeResp("ok", "")},
	}
	s := New(rec)

	_, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "hi", Model: deepseek.DeepSeekChat})
	if err != nil {
		t.Fatal(err)
	}
	if rec.requests[0].Model != deepseek.DeepSeekChat {
		t.Errorf("request model = %q, want %q", rec.requests[0].Model, deepseek.DeepSeekChat)
	}
}
