package main

import (
	"context"
	"testing"
	"time"
)

func TestDiscoverHostInfoAppNameRetriesUntilAvailable(t *testing.T) {
	original := hostInfoAppNameProbe
	t.Cleanup(func() { hostInfoAppNameProbe = original })

	attempts := 0
	hostInfoAppNameProbe = func(context.Context) string {
		attempts++
		if attempts == 3 {
			return "n8nstarter1786476319764"
		}
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := discoverHostInfoAppName(ctx, time.Millisecond, time.Millisecond)
	if got != "n8nstarter1786476319764" {
		t.Fatalf("app name = %q", got)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}
