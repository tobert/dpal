//go:build e2e

// End-to-end tests against the live DeepSeek API. These are opt-in via
// the `e2e` build tag, and skip cleanly when DEEPSEEK_API_KEY is unset.
//
//	DEEPSEEK_API_KEY=... go test -tags=e2e -v ./internal/server/
package server_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"

	"github.com/tobert/dpal/internal/explorer"
	"github.com/tobert/dpal/internal/server"
)

func requireKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping live e2e test")
	}
	return key
}

func TestE2E_ReasonerSurfacesReasoningContent(t *testing.T) {
	client := deepseek.NewClient(requireKey(t))
	srv := server.New(client)

	_, out, err := srv.ConsultOneshot(context.Background(), nil, server.OneshotInput{
		Prompt: "What is 2 + 3? Answer with just the number.",
	})
	if err != nil {
		t.Fatalf("ConsultOneshot: %v", err)
	}
	if !strings.Contains(out.Content, "5") {
		t.Errorf("content did not contain '5', got %q", out.Content)
	}
	if out.ReasoningContent == "" {
		t.Error("expected non-empty reasoning_content from deepseek-reasoner")
	}
	t.Logf("model=%s", out.Model)
	t.Logf("content (%d chars): %q", len(out.Content), out.Content)
	preview := out.ReasoningContent
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	t.Logf("reasoning (%d chars): %q", len(out.ReasoningContent), preview)
}

func TestE2E_StatefulConsultRemembersPriorTurn(t *testing.T) {
	client := deepseek.NewClient(requireKey(t))
	srv := server.New(client)

	ctx := context.Background()
	const sessionID = "e2e-stateful-1"

	if _, _, err := srv.Consult(ctx, nil, server.ConsultInput{
		SessionID: sessionID,
		Prompt:    "Please remember the number 47. Reply with just 'noted'.",
		Model:     deepseek.DeepSeekChat, // cheaper non-reasoning call
	}); err != nil {
		t.Fatalf("first turn: %v", err)
	}

	_, out, err := srv.Consult(ctx, nil, server.ConsultInput{
		SessionID: sessionID,
		Prompt:    "What number did I ask you to remember? Reply with just the number.",
		Model:     deepseek.DeepSeekChat,
	})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if !strings.Contains(out.Content, "47") {
		t.Errorf("model did not recall '47' from session history, got %q", out.Content)
	}
	if out.TurnCount != 2 {
		t.Errorf("turn_count = %d, want 2", out.TurnCount)
	}
	t.Logf("recall content: %q (turn_count=%d)", out.Content, out.TurnCount)
}

func TestE2E_ToolLoopReadsSandboxedFile(t *testing.T) {
	client := deepseek.NewClient(requireKey(t))

	dir := t.TempDir()
	const magic = "PINEAPPLE_42"
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"),
		[]byte("the magic password is "+magic+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exp, err := explorer.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(client).WithExplorer(exp)

	_, out, err := srv.ConsultOneshot(context.Background(), nil, server.OneshotInput{
		Prompt: "There is a file named secret.txt in the current directory. Use the read_file tool to read it, then tell me what the magic password is. Reply with just the password, nothing else.",
		Model:  deepseek.DeepSeekChat, // V3 has more reliable tool calling than R1
	})
	if err != nil {
		t.Fatalf("ConsultOneshot with tools: %v", err)
	}
	if !strings.Contains(out.Content, magic) {
		t.Errorf("model did not surface the password from the file; got %q", out.Content)
	}
	t.Logf("tool-loop final content: %q", out.Content)
}
