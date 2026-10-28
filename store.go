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

// DeadMeasurementRow is a permanently rejected measurement moved out of the retry queue.
type DeadMeasurementRow struct {
	ID         int64
	Payload    map[string]any
	Error      string
	CreatedAt  string
	RejectedAt string
}

type DeadMeasurementInsert struct {
	Payload map[string]any
	Error   string
}

type RejectedPendingRow struct {
	Row   PendingRow
	Error string
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
		return nil, fmt.Errorf("migrate pending_measurements: %w", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS dead_measurements (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		payload     TEXT    NOT NULL,
		error       TEXT    NOT NULL,
		created_at  TEXT    NOT NULL,
		rejected_at TEXT    NOT NULL
	)`)
	if err != nil {
		return nil, fmt.Errorf("migrate dead_measurements: %w", err)
	}
	return db, nil
}

// storePending inserts measurements into the pending table within a single
// transaction. Each measurement is stored as its own JSON row.
func storePending(db *sql.DB, measurements []map[string]any) error {
	_, err := storePendingRows(db, measurements)
	return err
}

// storePendingRows inserts measurements and returns the created pending rows.
func storePendingRows(db *sql.DB, measurements []map[string]any) ([]PendingRow, error) {
	if len(measurements) == 0 {
		return nil, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO pending_measurements (payload, created_at) VALUES (?, ?) RETURNING id`)
	if err != nil {
		return nil, fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	rows := make([]PendingRow, 0, len(measurements))
	for _, m := range measurements {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("marshal measurement: %w", err)
		}
		var id int64
		if err := stmt.QueryRow(string(b), now).Scan(&id); err != nil {
			return nil, fmt.Errorf("insert: %w", err)
		}
		rows = append(rows, PendingRow{ID: id, Payload: m})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rows, nil
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

// movePendingToDead moves permanently rejected pending rows out of the retry queue.
func movePendingToDead(db *sql.DB, rows []RejectedPendingRow) error {
	if len(rows) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO dead_measurements (payload, error, created_at, rejected_at) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare dead insert: %w", err)
	}
	defer stmt.Close()

	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		body, err := json.Marshal(row.Row.Payload)
		if err != nil {
			return fmt.Errorf("marshal dead measurement %d: %w", row.Row.ID, err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := stmt.Exec(string(body), row.Error, now, now); err != nil {
			return fmt.Errorf("insert dead measurement %d: %w", row.Row.ID, err)
		}
		ids = append(ids, row.Row.ID)
	}

	if err := deletePendingTx(tx, ids); err != nil {
		return err
	}

	return tx.Commit()
}

func storeDeadMeasurements(db *sql.DB, rows []DeadMeasurementInsert) error {
	if len(rows) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := storeDeadMeasurementsTx(tx, rows); err != nil {
		return err
	}

	return tx.Commit()
}

func storeDeadMeasurementsTx(tx *sql.Tx, rows []DeadMeasurementInsert) error {
	stmt, err := tx.Prepare(`INSERT INTO dead_measurements (payload, error, created_at, rejected_at) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare dead insert: %w", err)
	}
	defer stmt.Close()

	for _, row := range rows {
		body, err := json.Marshal(row.Payload)
		if err != nil {
			return fmt.Errorf("marshal dead measurement: %w", err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := stmt.Exec(string(body), row.Error, now, now); err != nil {
			return fmt.Errorf("insert dead measurement: %w", err)
		}
	}

	return nil
}

func deletePendingTx(tx *sql.Tx, ids []int64) error {
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
	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}
	return nil
}
