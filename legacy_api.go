// legacy_api.go – Legacy weather API client.
//
// Sends a single pipe-delimited packet per POST request to a third-party
// "legacy" weather ingest endpoint. The packet format is:
//
//	unix_timestamp|temp_c|humidity_pct|baro_hpa|windspeed_kmh|windgust_kmh|winddir_deg|rain_mm_ytd|EE
//
// Station identity is appended as the final path segment of the configured URL:
//
//	POST {legacy-api-url}/{station}
//	Authorization: <key>
package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LegacyClient sends measurements to a legacy weather ingest endpoint.
type LegacyClient struct {
	baseURL string // scheme+host+path, trailing slash stripped
	apiKey  string
	station string // appended as a URL path segment
	http    *http.Client
}

// newLegacyClient creates a client. baseURL should not have a trailing slash.
func newLegacyClient(baseURL, apiKey, station string) *LegacyClient {
	return &LegacyClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		station: station,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// buildLegacyPacket formats a single pipe-delimited measurement packet from a
// parsed LOOP reading. now must be UTC.
//
// Format: unix_ts|temp_c|hum_pct|baro_hpa|wind_kmh|gust_kmh|wind_deg|rain_mm|EE
func buildLegacyPacket(d *LOOPData, now time.Time, bucketMM float64) string {
	return fmt.Sprintf("%d|%.1f|%.1f|%.1f|%.1f|%.1f|%d|%.1f|EE",
		now.UTC().Unix(),
		d.OutsideTempC,
		float64(d.OutsideHumidity),
		d.PressureHPa,
		d.WindSpeedMs*3.6,
		gustMs(d)*3.6,
		d.WindDir,
		float64(d.YearRainClicks)*bucketMM,
	)
}

// sendPacket POSTs a single pipe-delimited packet to {baseURL}/{station}.
//
// Return values:
//   - nil             → HTTP 2xx; delivery confirmed
//   - *HTTPError 4xx  → permanent config error; do NOT buffer
//   - *HTTPError 5xx  → transient server error; buffer and retry
//   - other error     → network failure; buffer and retry
func (c *LegacyClient) sendPacket(packet string) error {
	url := c.baseURL + "/" + c.station
	req, err := http.NewRequest("POST", url, strings.NewReader(packet))
	if err != nil {
		return fmt.Errorf("create legacy request: %w", err)
	}
	req.Header.Set("Authorization", c.apiKey)
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.http.Do(req)
	if err != nil {
		// Network-level failure (DNS, refused, timeout) → transient.
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return nil
}
