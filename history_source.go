package main

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

type sqlTokenLogSource struct {
	db   *sqlx.DB
	isPG bool
}

type tokenDeltaRow struct {
	ModelName        string `db:"model_name"`
	PromptTokens     int64  `db:"prompt_tokens"`
	CompletionTokens int64  `db:"completion_tokens"`
	FirstSeenAt      int64  `db:"first_seen_at"`
	LastSeenAt       int64  `db:"last_seen_at"`
}

// Snapshot is safe for concurrent calls and returns a fixed high-watermark range.
func (s *sqlTokenLogSource) Snapshot(ctx context.Context, afterID int64) (tokenLogSnapshot, error) {
	if s.db == nil {
		return tokenLogSnapshot{}, fmt.Errorf("token log database is required")
	}

	var throughID int64
	highWatermarkQuery := s.rebind(`SELECT COALESCE(MAX(id), ?) FROM logs`)
	if err := s.db.GetContext(ctx, &throughID, highWatermarkQuery, afterID); err != nil {
		return tokenLogSnapshot{}, fmt.Errorf("read token log high watermark: %w", err)
	}
	if throughID <= afterID {
		return tokenLogSnapshot{ThroughID: throughID}, nil
	}

	query := s.rebind(`
		SELECT model_name,
			COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
			COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
			MIN(created_at) AS first_seen_at,
			MAX(created_at) AS last_seen_at
		FROM logs
		WHERE id > ? AND id <= ?
			AND type IN (2, 5)
			AND model_name IS NOT NULL AND model_name != ''
			AND created_at IS NOT NULL
		GROUP BY model_name`)
	rows := []tokenDeltaRow{}
	if err := s.db.SelectContext(ctx, &rows, query, afterID, throughID); err != nil {
		return tokenLogSnapshot{}, fmt.Errorf("aggregate token log range: %w", err)
	}

	deltas := make([]modelTokenDelta, 0, len(rows))
	for _, row := range rows {
		deltas = append(deltas, modelTokenDelta{
			ModelName:        row.ModelName,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			FirstSeenAt:      row.FirstSeenAt,
			LastSeenAt:       row.LastSeenAt,
		})
	}
	return tokenLogSnapshot{ThroughID: throughID, Deltas: deltas}, nil
}

func (s *sqlTokenLogSource) rebind(query string) string {
	if s.isPG {
		return sqlx.Rebind(sqlx.DOLLAR, query)
	}
	return query
}
