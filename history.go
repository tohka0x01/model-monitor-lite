package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const tokenHistorySource = "newapi_logs"

type modelTokenDelta struct {
	ModelName        string
	PromptTokens     int64
	CompletionTokens int64
	FirstSeenAt      int64
	LastSeenAt       int64
}

type tokenLogSnapshot struct {
	ThroughID int64
	Deltas    []modelTokenDelta
}

type tokenLogSource interface {
	Snapshot(context.Context, int64) (tokenLogSnapshot, error)
}

type modelTokenTotal struct {
	ModelName      string `json:"model_name"`
	RetainedTokens int64  `json:"retained_tokens"`
	FirstSeenAt    int64  `json:"first_seen_at,omitempty"`
	LastSeenAt     int64  `json:"last_seen_at,omitempty"`
}

type tokenHistory struct {
	db      *sql.DB
	source  tokenLogSource
	collect sync.Mutex
}

// openTokenHistory returns a store safe for concurrent collection and reads.
func openTokenHistory(path string, source tokenLogSource) (*tokenHistory, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("token history path is required")
	}
	if source == nil {
		return nil, errors.New("token log source is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create token history directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open token history database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := initializeTokenHistory(db); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("close token history after initialization failure: %w", closeErr))
		}
		return nil, err
	}
	return &tokenHistory{db: db, source: source}, nil
}

func initializeTokenHistory(db *sql.DB) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS model_token_totals (
			model_name TEXT PRIMARY KEY,
			prompt_tokens INTEGER NOT NULL CHECK (prompt_tokens >= 0),
			completion_tokens INTEGER NOT NULL CHECK (completion_tokens >= 0),
			first_seen_at INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS collector_checkpoint (
			source TEXT PRIMARY KEY,
			last_log_id INTEGER NOT NULL CHECK (last_log_id >= 0)
		)`,
		`INSERT INTO collector_checkpoint (source, last_log_id)
		 VALUES ('newapi_logs', 0)
		 ON CONFLICT(source) DO NOTHING`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("initialize token history database: %w", err)
		}
	}
	return nil
}

// Collect is safe for concurrent calls; collection is serialized per store.
func (h *tokenHistory) Collect(ctx context.Context) error {
	h.collect.Lock()
	defer h.collect.Unlock()

	checkpoint, err := h.checkpoint(ctx)
	if err != nil {
		return err
	}
	snapshot, err := h.source.Snapshot(ctx, checkpoint)
	if err != nil {
		return fmt.Errorf("read token log snapshot: %w", err)
	}
	if err := validateTokenSnapshot(snapshot, checkpoint); err != nil {
		return err
	}
	if snapshot.ThroughID == checkpoint {
		return nil
	}
	return h.applySnapshot(ctx, snapshot)
}

func (h *tokenHistory) checkpoint(ctx context.Context) (int64, error) {
	var checkpoint int64
	err := h.db.QueryRowContext(ctx,
		`SELECT last_log_id FROM collector_checkpoint WHERE source = ?`,
		tokenHistorySource,
	).Scan(&checkpoint)
	if err != nil {
		return 0, fmt.Errorf("read token history checkpoint: %w", err)
	}
	return checkpoint, nil
}

func validateTokenSnapshot(snapshot tokenLogSnapshot, checkpoint int64) error {
	if snapshot.ThroughID < checkpoint {
		return fmt.Errorf("token log id moved backwards: checkpoint=%d source=%d", checkpoint, snapshot.ThroughID)
	}
	if snapshot.ThroughID == checkpoint && len(snapshot.Deltas) > 0 {
		return errors.New("token log snapshot has deltas without advancing checkpoint")
	}
	for _, delta := range snapshot.Deltas {
		if strings.TrimSpace(delta.ModelName) == "" {
			return errors.New("token log snapshot contains an empty model name")
		}
		if delta.PromptTokens < 0 || delta.CompletionTokens < 0 {
			return fmt.Errorf("token log snapshot contains negative tokens for model %q", delta.ModelName)
		}
		if delta.FirstSeenAt > delta.LastSeenAt {
			return fmt.Errorf("token log snapshot has invalid time range for model %q", delta.ModelName)
		}
	}
	return nil
}

func (h *tokenHistory) applySnapshot(ctx context.Context, snapshot tokenLogSnapshot) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin token history transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, delta := range snapshot.Deltas {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO model_token_totals (
				model_name, prompt_tokens, completion_tokens, first_seen_at, last_seen_at
			) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(model_name) DO UPDATE SET
				prompt_tokens = prompt_tokens + excluded.prompt_tokens,
				completion_tokens = completion_tokens + excluded.completion_tokens,
				first_seen_at = MIN(first_seen_at, excluded.first_seen_at),
				last_seen_at = MAX(last_seen_at, excluded.last_seen_at)`,
			delta.ModelName,
			delta.PromptTokens,
			delta.CompletionTokens,
			delta.FirstSeenAt,
			delta.LastSeenAt,
		)
		if err != nil {
			return fmt.Errorf("update token history for model %q: %w", delta.ModelName, err)
		}
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE collector_checkpoint SET last_log_id = ? WHERE source = ?`,
		snapshot.ThroughID,
		tokenHistorySource,
	)
	if err != nil {
		return fmt.Errorf("update token history checkpoint: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read token history checkpoint update result: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("updated %d token history checkpoints, want 1", updated)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit token history transaction: %w", err)
	}
	return nil
}

// Totals is safe for concurrent calls and returns one result per requested model.
func (h *tokenHistory) Totals(ctx context.Context, models []string) ([]modelTokenTotal, error) {
	if len(models) == 0 {
		return []modelTokenTotal{}, nil
	}

	query := `SELECT model_name, prompt_tokens + completion_tokens, first_seen_at, last_seen_at
		FROM model_token_totals WHERE model_name IN (` + placeholders(len(models)) + `)`
	args := make([]any, len(models))
	for i, model := range models {
		args[i] = model
	}
	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query token history totals: %w", err)
	}
	defer rows.Close()

	byModel := make(map[string]modelTokenTotal, len(models))
	for rows.Next() {
		var total modelTokenTotal
		if err := rows.Scan(&total.ModelName, &total.RetainedTokens, &total.FirstSeenAt, &total.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan token history total: %w", err)
		}
		byModel[total.ModelName] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate token history totals: %w", err)
	}

	totals := make([]modelTokenTotal, 0, len(models))
	for _, model := range models {
		total := byModel[model]
		total.ModelName = model
		totals = append(totals, total)
	}
	return totals, nil
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// Close must be called after the collector has stopped.
func (h *tokenHistory) Close() error {
	if err := h.db.Close(); err != nil {
		return fmt.Errorf("close token history database: %w", err)
	}
	return nil
}
