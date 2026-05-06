package main

import (
	"context"
	"testing"
	"time"
)

// testCtx returns a context that auto-cancels at the end of the test.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
