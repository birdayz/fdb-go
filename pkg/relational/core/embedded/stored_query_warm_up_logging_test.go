package embedded

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestStoredQueryWarmUpRoutineLogging(t *testing.T) {
	t.Parallel()
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: level}))
			warmStoredQueries(context.Background(), NewRelationalPlanCache(nil), nil, logger)
			if level == slog.LevelInfo && out.Len() != 0 {
				t.Fatalf("routine open logged at default level: %s", out.String())
			}
			if level == slog.LevelDebug && !strings.Contains(out.String(), "level=DEBUG msg=\"OfflineStoredQueriesProcessor finished\"") {
				t.Fatalf("debug warm-up diagnostics missing: %s", out.String())
			}
		})
	}
}
