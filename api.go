// api.go – Voria2 Station Ingest API client.
//
// API reference: INGEST_API.md
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Client
// ─────────────────────────────────────────────────────────────────────────────

// APIClient sends measurements to the Voria2 ingest API.
type APIClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// newAPIClient creates a client. baseURL should be the scheme+host only
// (e.g. "https://api.voria2.io"); trailing slashes are stripped.
func newAPIClient(baseURL, apiKey string) *APIClient {
	return &APIClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Error types
// ─────────────────────────────────────────────────────────────────────────────

// HTTPError represents a non-2xx response from the API.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// isPermanent returns true for 4xx errors – these indicate configuration
// problems (bad API key, unknown sensor slug) that won't self-heal.
// Network errors and 5xx are transient and should be retried.
func isPermanent(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode >= 400 && he.StatusCode < 500
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// API responses
// ─────────────────────────────────────────────────────────────────────────────

// BulkResult is the response body from POST /api/v1/ingest/bulk.
type BulkResult struct {
	OK      bool         `json:"ok"`
	Count   int          `json:"count"`
	Results []ItemResult `json:"results"`
}

// ItemResult is the per-measurement result within a BulkResult.
type ItemResult struct {
	Index int    `json:"index"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Endpoints
// ─────────────────────────────────────────────────────────────────────────────

// verifyKey calls POST /api/v1/ingest/verify and returns the station name.
func (c *APIClient) verifyKey() (string, error) {
	req, err := http.NewRequest("POST", c.baseURL+"/api/v1/ingest/verify", nil)
	if err != nil {
		return "", fmt.Errorf("create verify request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var result struct {
		OK          bool   `json:"ok"`
		StationName string `json:"station_name"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode verify response: %w", err)
	}
	return result.StationName, nil
}

// bulkPost sends batch to POST /api/v1/ingest/bulk.
//
// Return values:
//   - (result, nil)        → HTTP 200; inspect result.Results for per-item failures
//   - (_, *HTTPError 4xx)  → permanent config error; do NOT store in SQLite
//   - (_, *HTTPError 5xx)  → transient server error; store in SQLite and retry
//   - (_, other error)     → network failure; store in SQLite and retry
func (c *APIClient) bulkPost(batch []map[string]any) (BulkResult, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return BulkResult{}, fmt.Errorf("marshal batch: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/api/v1/ingest/bulk",
		bytes.NewReader(body))
	if err != nil {
		return BulkResult{}, fmt.Errorf("create bulk request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		// Network-level failure (DNS, refused, timeout) → transient.
		return BulkResult{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return BulkResult{}, &HTTPError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var result BulkResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return BulkResult{}, fmt.Errorf("decode bulk response: %w", err)
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Measurement builders
// ─────────────────────────────────────────────────────────────────────────────

// buildMeasurements converts a parsed LOOP reading into the five API payload
// objects that will be submitted via bulkPost. t must be in UTC.
//
// Sensor slugs are the hardcoded defaults agreed upon at setup time:
//
//	temperature  → outside temperature (°C)
//	humidity     → outside humidity (%)
//	pressure     → barometric pressure (hPa)
//	wind         → wind (u/v/gust in m/s)
//	rain         → cumulative year-to-date rain (mm)
// gustMs returns the best available gust speed in m/s: the LOOP2 10-min
// rolling maximum when available, otherwise the LOOP1 10-min average proxy.
func gustMs(d *LOOPData) float64 {
	if d.HasGust {
		return d.GustSpeedMs
	}
	return d.WindSpeed10MinMs
}

func buildMeasurements(d *LOOPData, t time.Time, bucketMM float64) []map[string]any {
	ts := t.UTC().Format(time.RFC3339)
	return []map[string]any{
		{
			"sensor":    "temperature",
			"value":     d.OutsideTempC,
			"timestamp": ts,
		},
		{
			"sensor":    "humidity",
			"value":     float64(d.OutsideHumidity),
			"timestamp": ts,
		},
		{
			"sensor":    "pressure",
			"value":     d.PressureHPa,
			"timestamp": ts,
		},
		{
			// Gust from LOOP2 10-min rolling maximum; falls back to LOOP1 10-min
			// avg when LOOP2 is unavailable (firmware too old or console cold-start).
			"sensor":    "wind",
			"u":         d.WindU,
			"v":         d.WindV,
			"gust":      gustMs(d),
			"timestamp": ts,
		},
		{
			// cumulative_mm is the field the server reads for cumulative rain;
			// the year-to-date counter resets each January 1st on the console.
			// bucketMM converts raw clicks to mm (0.2 metric, 0.254 imperial).
			"sensor":        "rain",
			"cumulative_mm": float64(d.YearRainClicks) * bucketMM,
			"timestamp":     ts,
		},
	}
}
