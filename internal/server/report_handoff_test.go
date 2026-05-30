package server

import (
	"context"
	"strings"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
)

// TestExtractExplorerReport covers the pure hand-off function: how the
// explorer's final text becomes the report the synthesizer sees.
func TestExtractExplorerReport(t *testing.T) {
	toolTurn := deepseek.ChatCompletionMessage{
		Role:      deepseek.ChatMessageRoleAssistant,
		ToolCalls: []deepseek.ToolCall{{ID: "c1"}},
	}
	toolResult := deepseek.ChatCompletionMessage{Role: deepseek.ChatMessageRoleTool, ToolCallID: "c1", Content: "data"}

	tests := []struct {
		name        string
		msgs        []deepseek.ChatCompletionMessage
		originalLen int
		wantReport  string
		wantCalls   int
	}{
		{
			name: "markers with leaked preamble and trailer",
			msgs: []deepseek.ChatCompletionMessage{
				{Role: deepseek.ChatMessageRoleUser, Content: "q"},
				toolTurn, toolResult,
				{Role: deepseek.ChatMessageRoleAssistant, Content: "Here is the report.\nBEGIN EXPLORATION\nthe body\nEND OF EXPLORATION\nthanks!"},
			},
			originalLen: 1,
			wantReport:  "the body",
			wantCalls:   1,
		},
		{
			name: "no markers uses whole trimmed content",
			msgs: []deepseek.ChatCompletionMessage{
				{Role: deepseek.ChatMessageRoleUser, Content: "q"},
				toolTurn, toolResult,
				{Role: deepseek.ChatMessageRoleAssistant, Content: "  loaded three files  "},
			},
			originalLen: 1,
			wantReport:  "loaded three files",
			wantCalls:   1,
		},
		{
			name: "zero tool calls means no report even with text",
			msgs: []deepseek.ChatCompletionMessage{
				{Role: deepseek.ChatMessageRoleUser, Content: "q"},
				{Role: deepseek.ChatMessageRoleAssistant, Content: "No exploration needed."},
			},
			originalLen: 1,
			wantReport:  "",
			wantCalls:   0,
		},
		{
			name: "begin marker but missing end keeps everything after begin",
			msgs: []deepseek.ChatCompletionMessage{
				{Role: deepseek.ChatMessageRoleUser, Content: "q"},
				toolTurn, toolResult,
				{Role: deepseek.ChatMessageRoleAssistant, Content: "preamble\nBEGIN EXPLORATION\nthe body and some trailing chatter"},
			},
			originalLen: 1,
			wantReport:  "the body and some trailing chatter",
			wantCalls:   1,
		},
		{
			name: "tool calls summed across multiple assistant turns",
			msgs: []deepseek.ChatCompletionMessage{
				{Role: deepseek.ChatMessageRoleUser, Content: "q"},
				toolTurn, toolResult,
				toolTurn, toolResult,
				{Role: deepseek.ChatMessageRoleAssistant, Content: "BEGIN EXPLORATION\ndone\nEND OF EXPLORATION"},
			},
			originalLen: 1,
			wantReport:  "done",
			wantCalls:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, calls := extractExplorerReport(tt.msgs, tt.originalLen)
			if report != tt.wantReport {
				t.Errorf("report = %q, want %q", report, tt.wantReport)
			}
			if calls != tt.wantCalls {
				t.Errorf("toolCalls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

// TestConsult_ReportMarkersStrippedBeforeSynth — when the explorer leaks a
// preamble/trailer around its BEGIN/END markers, only the marked body is
// folded into the synthesizer's prompt.
func TestConsult_ReportMarkersStrippedBeforeSynth(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			toolCallResp("read_file", `{"path":"note.txt"}`, "c1"),
			finalResp("I have everything I need. Here is the report.\nBEGIN EXPLORATION\nnote.txt:1 says hello there\nEND OF EXPLORATION\nlet me know if you need more"),
			finalResp("synth answer"),
		},
	}
	s := New(rec).WithExplorer(exp)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "what's in the note?"}); err != nil {
		t.Fatal(err)
	}

	synthUser := rec.requests[2].Messages[0].Content
	if !strings.Contains(synthUser, "note.txt:1 says hello there") {
		t.Errorf("synth prompt missing the report body: %q", synthUser)
	}
	for _, leaked := range []string{"I have everything I need", "let me know if you need more", "BEGIN EXPLORATION", "END OF EXPLORATION"} {
		if strings.Contains(synthUser, leaked) {
			t.Errorf("synth prompt leaked %q: %q", leaked, synthUser)
		}
	}
}

// TestConsult_NoReportInjectedWhenZeroToolCalls — when the explorer loads
// nothing, the synthesizer gets the bare prompt with no appended context.
func TestConsult_NoReportInjectedWhenZeroToolCalls(t *testing.T) {
	exp := newSandboxExplorer(t)
	rec := &recordingClient{
		responses: []*deepseek.ChatCompletionResponse{
			finalResp("No exploration needed."),
			finalResp("42"),
		},
	}
	s := New(rec).WithExplorer(exp)

	if _, _, err := s.Consult(context.Background(), nil, ConsultInput{SessionID: "s1", Prompt: "what is 6 times 7?"}); err != nil {
		t.Fatal(err)
	}

	synth := rec.requests[1].Messages
	if len(synth) != 1 {
		t.Fatalf("synth messages = %d, want 1", len(synth))
	}
	if synth[0].Content != "what is 6 times 7?" {
		t.Errorf("synth prompt = %q, want the bare prompt with no injected context", synth[0].Content)
	}
}
