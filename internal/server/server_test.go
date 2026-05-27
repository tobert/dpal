package server

import (
	"context"
	"errors"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
)

type fakeClient struct {
	lastReq *deepseek.ChatCompletionRequest
	resp    *deepseek.ChatCompletionResponse
	err     error
}

func (f *fakeClient) CreateChatCompletion(ctx context.Context, req *deepseek.ChatCompletionRequest) (*deepseek.ChatCompletionResponse, error) {
	f.lastReq = req
	return f.resp, f.err
}

func makeResp(content, reasoning string) *deepseek.ChatCompletionResponse {
	return &deepseek.ChatCompletionResponse{
		Choices: []deepseek.Choice{
			{
				Message: deepseek.Message{
					Role:             deepseek.ChatMessageRoleAssistant,
					Content:          content,
					ReasoningContent: reasoning,
				},
			},
		},
	}
}

func TestConsultOneshot_SurfacesBothChannels(t *testing.T) {
	fake := &fakeClient{resp: makeResp("4", "Adding 2 and 2.")}
	s := New(fake)

	_, out, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "What is 2+2?"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Content != "4" {
		t.Errorf("Content = %q, want %q", out.Content, "4")
	}
	if out.ReasoningContent != "Adding 2 and 2." {
		t.Errorf("ReasoningContent = %q, want %q", out.ReasoningContent, "Adding 2 and 2.")
	}
}

func TestConsultOneshot_DefaultsToReasoner(t *testing.T) {
	fake := &fakeClient{resp: makeResp("ok", "")}
	s := New(fake)

	_, out, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastReq.Model != deepseek.DeepSeekReasoner {
		t.Errorf("request model = %q, want %q", fake.lastReq.Model, deepseek.DeepSeekReasoner)
	}
	if out.Model != deepseek.DeepSeekReasoner {
		t.Errorf("output model = %q, want %q", out.Model, deepseek.DeepSeekReasoner)
	}
}

func TestConsultOneshot_HonorsModelOverride(t *testing.T) {
	fake := &fakeClient{resp: makeResp("ok", "")}
	s := New(fake)

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi", Model: deepseek.DeepSeekChat})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastReq.Model != deepseek.DeepSeekChat {
		t.Errorf("request model = %q, want %q", fake.lastReq.Model, deepseek.DeepSeekChat)
	}
}

func TestConsultOneshot_RejectsEmptyPrompt(t *testing.T) {
	fake := &fakeClient{}
	s := New(fake)

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "   "})
	if err == nil {
		t.Fatal("expected error for empty prompt, got nil")
	}
	if fake.lastReq != nil {
		t.Error("client should not have been called for empty prompt")
	}
}

func TestConsultOneshot_PropagatesClientError(t *testing.T) {
	fake := &fakeClient{err: errors.New("boom")}
	s := New(fake)

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error to propagate, got nil")
	}
}

func TestConsultOneshot_RejectsEmptyChoices(t *testing.T) {
	fake := &fakeClient{resp: &deepseek.ChatCompletionResponse{}}
	s := New(fake)

	_, _, err := s.ConsultOneshot(context.Background(), nil, OneshotInput{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error for empty choices, got nil")
	}
}

func TestUsageSnapshot_AggregatesPerModel(t *testing.T) {
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			makeRespWithUsage("deepseek-chat", "a", "", 10, 20, 5, 5),
			makeRespWithUsage("deepseek-chat", "b", "", 100, 30, 80, 20),
			makeRespWithUsage("deepseek-reasoner", "c", "", 50, 200, 0, 50),
		},
	}
	s := New(rec)
	ctx := context.Background()
	for range 3 {
		if _, _, err := s.ConsultOneshot(ctx, nil, OneshotInput{Prompt: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	snap := s.UsageSnapshot()
	if len(snap) != 2 {
		t.Fatalf("got %d models, want 2", len(snap))
	}
	// Sort guarantees deepseek-chat is first.
	chat, reasoner := snap[0], snap[1]
	if chat.Model != "deepseek-chat" || reasoner.Model != "deepseek-reasoner" {
		t.Fatalf("unexpected order: %s, %s", chat.Model, reasoner.Model)
	}
	if chat.Calls != 2 || chat.InputTokens != 110 || chat.OutputTokens != 50 || chat.CacheHitTokens != 85 || chat.CacheMissTokens != 25 {
		t.Errorf("chat usage = %+v", chat)
	}
	if reasoner.Calls != 1 || reasoner.InputTokens != 50 || reasoner.OutputTokens != 200 {
		t.Errorf("reasoner usage = %+v", reasoner)
	}
}

func TestUsageSnapshot_NoCallsReturnsEmpty(t *testing.T) {
	s := New(&fakeClient{})
	snap := s.UsageSnapshot()
	if len(snap) != 0 {
		t.Errorf("expected empty snapshot, got %d entries", len(snap))
	}
}
