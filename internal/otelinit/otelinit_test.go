package otelinit

import (
	"context"
	"testing"
)

func TestBootstrap_EmptyEndpointIsNoop(t *testing.T) {
	shutdown, err := Bootstrap(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Bootstrap with empty endpoint returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must not be nil even when OTel is disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}
}
