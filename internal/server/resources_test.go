package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestInfoJSON_ReflectsServerState(t *testing.T) {
	s := New(&fakeClient{}).WithVersion("9.9.9")

	body, err := s.infoJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got InfoPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "dpal" {
		t.Errorf("name = %q, want dpal", got.Name)
	}
	if got.Version != "9.9.9" {
		t.Errorf("version = %q, want 9.9.9", got.Version)
	}
	if got.DefaultModel != deepseek.DeepSeekReasoner {
		t.Errorf("default_model = %q, want %q", got.DefaultModel, deepseek.DeepSeekReasoner)
	}
	if got.SessionCap != defaultMaxSessions {
		t.Errorf("session_cap = %d, want %d", got.SessionCap, defaultMaxSessions)
	}
	if got.ActiveSessions != 0 {
		t.Errorf("active_sessions = %d, want 0", got.ActiveSessions)
	}
}

func TestInfoJSON_ActiveSessionsCountsCorrectly(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{
		makeResp("a", ""), makeResp("b", ""),
	}}
	s := New(rec).WithVersion("0")
	_, _, _ = s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "Q1"})
	_, _, _ = s.Consult(context.Background(), nil, ConsultInput{SessionID: "s2", Prompt: "Q2"})

	body, _ := s.infoJSON()
	var got InfoPayload
	_ = json.Unmarshal(body, &got)
	if got.ActiveSessions != 2 {
		t.Errorf("active_sessions = %d, want 2", got.ActiveSessions)
	}
}

func TestSessionsJSON_ListsSessionsInLRUOrder(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{
		makeResp("a", ""), makeResp("b", ""), makeResp("c", ""),
	}}
	s := New(rec).WithVersion("0")
	_, _, _ = s.Consult(context.Background(), nil, ConsultInput{SessionID: "first", Prompt: "Q"})
	_, _, _ = s.Consult(context.Background(), nil, ConsultInput{SessionID: "second", Prompt: "Q"})
	_, _, _ = s.Consult(context.Background(), nil, ConsultInput{SessionID: "third", Prompt: "Q"})

	body, err := s.sessionsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got SessionsPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 3 {
		t.Fatalf("count = %d, want 3", got.Count)
	}
	wantIDs := []string{"first", "second", "third"}
	for i, w := range wantIDs {
		if got.Sessions[i].ID != w {
			t.Errorf("Sessions[%d].id = %q, want %q", i, got.Sessions[i].ID, w)
		}
	}
	// Each session has 1 user turn + 1 assistant turn = 2 messages.
	for _, sess := range got.Sessions {
		if sess.MessageCount != 2 {
			t.Errorf("session %q message_count = %d, want 2", sess.ID, sess.MessageCount)
		}
		if sess.UserTurns != 1 {
			t.Errorf("session %q user_turns = %d, want 1", sess.ID, sess.UserTurns)
		}
	}
}

func TestSessionJSON_ReturnsFullTranscript(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{
		makeResp("answer A", ""),
		makeResp("answer B", ""),
	}}
	s := New(rec).WithVersion("0")

	_, _, _ = s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "question A"})
	_, _, _ = s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "question B"})

	body, err := s.sessionJSON("s1")
	if err != nil {
		t.Fatal(err)
	}
	var got SessionPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "s1" {
		t.Errorf("id = %q, want s1", got.ID)
	}
	want := []TranscriptMessage{
		{Role: "user", Content: "question A"},
		{Role: "assistant", Content: "answer A"},
		{Role: "user", Content: "question B"},
		{Role: "assistant", Content: "answer B"},
	}
	if len(got.Messages) != len(want) {
		t.Fatalf("len(messages) = %d, want %d", len(got.Messages), len(want))
	}
	for i, w := range want {
		if got.Messages[i] != w {
			t.Errorf("messages[%d] = %+v, want %+v", i, got.Messages[i], w)
		}
	}
}

func TestSessionJSON_MissingSessionErrors(t *testing.T) {
	s := New(&fakeClient{}).WithVersion("0")
	if _, err := s.sessionJSON("nope"); err == nil {
		t.Fatal("expected error for unknown session_id, got nil")
	}
}

func TestParseSessionURI(t *testing.T) {
	tests := []struct {
		uri     string
		wantID  string
		wantErr bool
	}{
		{"dpal://session/abc", "abc", false},
		{"dpal://session/abc-123_x", "abc-123_x", false},
		{"dpal://session/", "", true},
		{"dpal://sessions", "", true},
		{"dpal://info", "", true},
	}
	for _, tc := range tests {
		id, err := parseSessionURI(tc.uri)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseSessionURI(%q) err = %v, wantErr = %v", tc.uri, err, tc.wantErr)
		}
		if id != tc.wantID {
			t.Errorf("parseSessionURI(%q) id = %q, want %q", tc.uri, id, tc.wantID)
		}
	}
}

func TestHandleSession_ReturnsResourceResult(t *testing.T) {
	rec := &recordingClient{responses: []*deepseek.ChatCompletionResponse{makeResp("ok", "")}}
	s := New(rec).WithVersion("0")
	_, _, _ = s.Consult(context.Background(), nil, ConsultInput{SessionID: "alpha", Prompt: "hi"})

	res, err := s.handleSession(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "dpal://session/alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("Contents len = %d, want 1", len(res.Contents))
	}
	c := res.Contents[0]
	if c.URI != "dpal://session/alpha" {
		t.Errorf("uri = %q", c.URI)
	}
	if c.MIMEType != jsonMIME {
		t.Errorf("mime = %q", c.MIMEType)
	}
	if !strings.Contains(c.Text, `"alpha"`) {
		t.Errorf("body did not contain session id: %s", c.Text)
	}
}

func TestHandleSession_RejectsBadURI(t *testing.T) {
	s := New(&fakeClient{}).WithVersion("0")
	_, err := s.handleSession(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "dpal://info"},
	})
	if err == nil {
		t.Fatal("expected error for non-session URI")
	}
}
