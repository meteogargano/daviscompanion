// store.go – SQLite-backed offline measurement buffer.
//
// When the Voria2 API is unreachable, measurements are persisted here and
// drained automatically on the next successful upload tick.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// PendingRow is a row from the pending_measurements table.
type PendingRow struct {
	ID      int64
	Payload map[string]any
}

// openDB opens (or creates) the SQLite database at path and applies the schema.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS pending_measurements (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		payload    TEXT    NOT NULL,
		created_at TEXT    NOT NULL
	)`)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// storePending inserts measurements into the pending table within a single
// transaction. Each measurement is stored as its own JSON row.
func storePending(db *sql.DB, measurements []map[string]any) error {
	if len(measurements) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO pending_measurements (payload, created_at) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, m := range measurements {
		b, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal measurement: %w", err)
		}
		if _, err := stmt.Exec(string(b), now); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}
	return tx.Commit()
}

// loadPending returns up to limit rows ordered oldest-first.
func loadPending(db *sql.DB, limit int) ([]PendingRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT id, payload FROM pending_measurements ORDER BY id ASC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending: %w", err)
	}
	defer rows.Close()

	var result []PendingRow
	for rows.Next() {
		var id int64
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			return nil, fmt.Errorf("unmarshal row %d: %w", id, err)
		}
		result = append(result, PendingRow{ID: id, Payload: m})
	}
	return result, rows.Err()
}

// deletePending removes rows by ID. Called after a successful (or permanently
// failed) bulk upload so the buffer does not grow unbounded.
func deletePending(db *sql.DB, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	query := "DELETE FROM pending_measurements WHERE id IN (" + placeholders + ")"

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := db.Exec(query, args...); err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}
	return nil
}

// pendingCount returns the number of rows currently buffered.
func pendingCount(db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRow(`SELECT COUNT(*) FROM pending_measurements`).Scan(&n)
	return n, err
}
