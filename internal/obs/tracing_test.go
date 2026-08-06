package obs

import (
	"context"
	"testing"
)

func TestSetupTracingEmptyEndpoint(t *testing.T) {
	ctx := context.Background()

	shutdown, err := SetupTracing(ctx, "penstock-test", "")
	if err != nil {
		t.Fatalf("SetupTracing with empty endpoint: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected a non-nil shutdown function")
	}

	// The no-op provider must still hand out usable tracers.
	_, span := Tracer().Start(ctx, "test-span")
	span.End()

	if err := shutdown(ctx); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
}
