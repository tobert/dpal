package server

import (
	"context"
	"testing"

	"github.com/tobert/dpal/internal/explorer"
)

func TestFileURIToPath_AbsolutePath(t *testing.T) {
	got, err := fileURIToPath("file:///home/atobey/proj")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/atobey/proj" {
		t.Errorf("got %q, want /home/atobey/proj", got)
	}
}

func TestFileURIToPath_LocalhostHost(t *testing.T) {
	got, err := fileURIToPath("file://localhost/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp" {
		t.Errorf("got %q, want /tmp", got)
	}
}

func TestFileURIToPath_PercentEscaped(t *testing.T) {
	got, err := fileURIToPath("file:///home/atobey/has%20space")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/atobey/has space" {
		t.Errorf("got %q, want unescaped", got)
	}
}

func TestFileURIToPath_RejectsRemoteHost(t *testing.T) {
	if _, err := fileURIToPath("file://otherhost/tmp"); err == nil {
		t.Fatal("expected error for remote host")
	}
}

func TestFileURIToPath_RejectsNonFileScheme(t *testing.T) {
	for _, uri := range []string{"http://example.com/", "ftp://x/", "/no-scheme"} {
		if _, err := fileURIToPath(uri); err == nil {
			t.Errorf("expected error for %q", uri)
		}
	}
}

func TestExplorerForRequest_NoExplorerReturnsNil(t *testing.T) {
	// --no-explore mode: nothing configured, ListRoots irrelevant.
	s := New(&fakeClient{})
	if got := s.explorerForRequest(context.Background(), nil); got != nil {
		t.Errorf("expected nil explorer when none configured, got %v", got)
	}
}

func TestExplorerForRequest_NilRequestFallsBackToServerExplorer(t *testing.T) {
	exp, err := explorer.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(&fakeClient{}).WithExplorer(exp)
	got := s.explorerForRequest(context.Background(), nil)
	if got != exp {
		t.Errorf("expected fallback to s.explorer when req is nil")
	}
}
