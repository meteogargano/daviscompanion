package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "weather.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testLoopData() *LOOPData {
	return &LOOPData{
		OutsideTempC:     21.5,
		OutsideTempF:     70.7,
		OutsideHumidity:  68,
		PressureHPa:      1012.3,
		PressureInHg:     29.893,
		WindU:            1.2,
		WindV:            -3.4,
		WindSpeed10MinMs: 4.5,
		HasGust:          true,
		GustSpeedMs:      5.6,
		YearRainClicks:   123,
	}
}

func payloadSensor(payload map[string]any) string {
	sensor, _ := payload["sensor"].(string)
	return sensor
}

func loadDeadMeasurements(t *testing.T, db *sql.DB) []DeadMeasurementRow {
	t.Helper()
	rows, err := db.Query(`SELECT id, payload, error, created_at, rejected_at FROM dead_measurements ORDER BY id ASC`)
	if err != nil {
		t.Fatalf("query dead_measurements: %v", err)
	}
	defer rows.Close()

	var result []DeadMeasurementRow
	for rows.Next() {
		var row DeadMeasurementRow
		var payload string
		if err := rows.Scan(&row.ID, &payload, &row.Error, &row.CreatedAt, &row.RejectedAt); err != nil {
			t.Fatalf("scan dead_measurements: %v", err)
		}
		if err := json.Unmarshal([]byte(payload), &row.Payload); err != nil {
			t.Fatalf("unmarshal dead_measurements payload: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate dead_measurements: %v", err)
	}
	return result
}

func TestBuildMeasurementsDropsHumiditySentinel(t *testing.T) {
	data := testLoopData()
	data.OutsideHumidity = 0xFF

	measurements := buildMeasurements(data, time.Unix(1_700_000_000, 0).UTC(), 0.2)
	for _, measurement := range measurements {
		if payloadSensor(measurement) == "humidity" {
			t.Fatalf("expected humidity sentinel to be dropped, got payload: %#v", measurement)
		}
	}
}

func TestBuildMeasurementsDropsPressureSentinel(t *testing.T) {
	data := testLoopData()
	data.PressureInHg = 0
	data.PressureHPa = 0

	measurements := buildMeasurements(data, time.Unix(1_700_000_000, 0).UTC(), 0.2)
	for _, measurement := range measurements {
		if payloadSensor(measurement) == "pressure" {
			t.Fatalf("expected pressure sentinel to be dropped, got payload: %#v", measurement)
		}
	}
}

func TestRunVoria2TickDrainsBacklogBeforeLive(t *testing.T) {
	db := openTestDB(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	pendingPayloads := []map[string]any{
		{"sensor": "temperature", "value": 1.0, "timestamp": now.Add(-2 * time.Minute).Format(time.RFC3339)},
		{"sensor": "humidity", "value": 55.0, "timestamp": now.Add(-90 * time.Second).Format(time.RFC3339)},
	}
	if err := storePending(db, pendingPayloads); err != nil {
		t.Fatalf("storePending pendingPayloads: %v", err)
	}

	var captured [][]map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var batch []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		captured = append(captured, batch)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"count":2,"results":[{"index":0,"ok":true},{"index":1,"ok":true}]}`))
	}))
	defer server.Close()

	client := newAPIClient(server.URL, "test-key")
	runVoria2Tick(testLoopData(), now, db, client, 0.2)

	if len(captured) != 1 {
		t.Fatalf("expected 1 request, got %d", len(captured))
	}
	if got, want := len(captured[0]), 2; got != want {
		t.Fatalf("expected batch of %d pending items, got %d", want, got)
	}
	if !reflect.DeepEqual(captured[0], pendingPayloads) {
		t.Fatalf("expected backlog-only batch %#v, got %#v", pendingPayloads, captured[0])
	}

	remaining, err := pendingCount(db)
	if err != nil {
		t.Fatalf("pendingCount: %v", err)
	}
	if remaining == 0 {
		t.Fatal("expected current readings to remain buffered behind backlog")
	}

	stored, err := loadPending(db, 20)
	if err != nil {
		t.Fatalf("loadPending: %v", err)
	}
	if got, want := len(stored), len(buildMeasurements(testLoopData(), now, 0.2)); got != want {
		t.Fatalf("expected %d buffered current rows, got %d", want, got)
	}
}

func TestRunVoria2TickMovesRejectedRowsToDeadLetter(t *testing.T) {
	db := openTestDB(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	pendingPayloads := []map[string]any{
		{"sensor": "temperature", "value": 1.0, "timestamp": now.Add(-2 * time.Minute).Format(time.RFC3339)},
		{"sensor": "humidity", "value": 255.0, "timestamp": now.Add(-90 * time.Second).Format(time.RFC3339)},
	}
	if err := storePending(db, pendingPayloads); err != nil {
		t.Fatalf("storePending pendingPayloads: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"count":1,"results":[{"index":0,"ok":true},{"index":1,"ok":false,"error":"value: humidity cannot exceed 100%"}]}`))
	}))
	defer server.Close()

	client := newAPIClient(server.URL, "test-key")
	runVoria2Tick(testLoopData(), now, db, client, 0.2)

	remaining, err := pendingCount(db)
	if err != nil {
		t.Fatalf("pendingCount: %v", err)
	}
	if remaining == 0 {
		t.Fatal("expected current readings buffered after backlog drain")
	}

	deadRows := loadDeadMeasurements(t, db)
	if len(deadRows) != 1 {
		t.Fatalf("expected 1 dead-letter row, got %d", len(deadRows))
	}
	if got := payloadSensor(deadRows[0].Payload); got != "humidity" {
		t.Fatalf("expected humidity dead-letter payload, got %q", got)
	}
	if deadRows[0].Error == "" {
		t.Fatal("expected rejection error to be stored in dead-letter row")
	}
	if !reflect.DeepEqual(deadRows[0].Payload, pendingPayloads[1]) {
		t.Fatalf("expected dead-letter payload %#v, got %#v", pendingPayloads[1], deadRows[0].Payload)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
