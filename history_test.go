package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
)

type stubTokenLogSource struct {
	snapshots []tokenLogSnapshot
}

func (s *stubTokenLogSource) Snapshot(_ context.Context, _ int64) (tokenLogSnapshot, error) {
	if len(s.snapshots) == 0 {
		return tokenLogSnapshot{}, nil
	}
	snapshot := s.snapshots[0]
	s.snapshots = s.snapshots[1:]
	return snapshot, nil
}

func openTokenTestSource(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatalf("open source database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close source database: %v", err)
		}
	})
	_, err = db.Exec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY,
		created_at INTEGER NOT NULL,
		type INTEGER NOT NULL,
		model_name TEXT,
		prompt_tokens INTEGER,
		completion_tokens INTEGER
	)`)
	if err != nil {
		t.Fatalf("create source logs: %v", err)
	}
	return db
}

func closeTokenHistoryAfterTest(t *testing.T, history *tokenHistory) {
	t.Helper()
	t.Cleanup(func() {
		if err := history.Close(); err != nil {
			t.Errorf("close token history: %v", err)
		}
	})
}

func TestTokenHistoryCollectsIncrementallyAcrossRestart(t *testing.T) {
	sourceDB := openTokenTestSource(t)
	_, err := sourceDB.Exec(`INSERT INTO logs VALUES
		(1, 1000, 2, 'test-model', 100, 20),
		(2, 1010, 5, 'test-model', 5, 0),
		(3, 1020, 1, 'test-model', 999, 999)`)
	if err != nil {
		t.Fatalf("insert initial logs: %v", err)
	}

	path := filepath.Join(t.TempDir(), "history.db")
	source := &sqlTokenLogSource{db: sourceDB}
	history, err := openTokenHistory(path, source)
	if err != nil {
		t.Fatalf("open initial token history: %v", err)
	}
	if err := history.Collect(context.Background()); err != nil {
		t.Fatalf("initial Collect() error = %v", err)
	}
	if err := history.Close(); err != nil {
		t.Fatalf("close initial token history: %v", err)
	}

	_, err = sourceDB.Exec(`INSERT INTO logs VALUES (4, 1030, 2, 'test-model', 7, 3)`)
	if err != nil {
		t.Fatalf("insert incremental log: %v", err)
	}
	history, err = openTokenHistory(path, source)
	if err != nil {
		t.Fatalf("reopen token history: %v", err)
	}
	closeTokenHistoryAfterTest(t, history)
	if err := history.Collect(context.Background()); err != nil {
		t.Fatalf("incremental Collect() error = %v", err)
	}
	if err := history.Collect(context.Background()); err != nil {
		t.Fatalf("idempotent Collect() error = %v", err)
	}

	totals, err := history.Totals(context.Background(), []string{"test-model"})
	if err != nil {
		t.Fatalf("Totals() error = %v", err)
	}
	if totals[0].RetainedTokens != 135 {
		t.Fatalf("RetainedTokens = %d, want 135", totals[0].RetainedTokens)
	}
}

func TestTokenHistorySurvivesSourceLogCleanup(t *testing.T) {
	sourceDB := openTokenTestSource(t)
	_, err := sourceDB.Exec(`INSERT INTO logs VALUES (9, 1000, 2, 'test-model', 40, 10)`)
	if err != nil {
		t.Fatalf("insert source log: %v", err)
	}

	history, err := openTokenHistory(
		filepath.Join(t.TempDir(), "history.db"),
		&sqlTokenLogSource{db: sourceDB},
	)
	if err != nil {
		t.Fatalf("open token history: %v", err)
	}
	closeTokenHistoryAfterTest(t, history)
	if err := history.Collect(context.Background()); err != nil {
		t.Fatalf("initial Collect() error = %v", err)
	}
	if _, err := sourceDB.Exec(`DELETE FROM logs`); err != nil {
		t.Fatalf("clear source logs: %v", err)
	}
	if err := history.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() after source cleanup error = %v", err)
	}

	totals, err := history.Totals(context.Background(), []string{"test-model"})
	if err != nil {
		t.Fatalf("Totals() error = %v", err)
	}
	if totals[0].RetainedTokens != 50 {
		t.Fatalf("RetainedTokens = %d, want 50", totals[0].RetainedTokens)
	}
}

func TestTokenHistoryCollectsRetainedLogs(t *testing.T) {
	source := &stubTokenLogSource{snapshots: []tokenLogSnapshot{{
		ThroughID: 12,
		Deltas: []modelTokenDelta{{
			ModelName:        "test-model",
			PromptTokens:     120,
			CompletionTokens: 30,
			FirstSeenAt:      1_000,
			LastSeenAt:       1_100,
		}},
	}}}
	history, err := openTokenHistory(filepath.Join(t.TempDir(), "history.db"), source)
	if err != nil {
		t.Fatalf("openTokenHistory() error = %v", err)
	}
	closeTokenHistoryAfterTest(t, history)

	if err := history.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	totals, err := history.Totals(context.Background(), []string{"test-model"})
	if err != nil {
		t.Fatalf("Totals() error = %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("len(totals) = %d, want 1", len(totals))
	}
	if totals[0].RetainedTokens != 150 {
		t.Fatalf("RetainedTokens = %d, want 150", totals[0].RetainedTokens)
	}
}
