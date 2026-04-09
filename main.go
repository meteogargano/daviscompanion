// davisweather – continuous daemon that reads a Davis Vantage Pro / Pro2 / Vue
// console via serial, uploads measurements to the Voria2 Ingest API, and
// buffers undeliverable readings in a local SQLite database for automatic
// retry.
//
// Usage:
//
//	davisweather --port /dev/ttyUSB0 --api-url https://api.voria2.io \
//	             --api-key vsk_xxxx
//	davisweather --port /dev/ttyUSB0 --api-url https://api.voria2.io \
//	             --api-key vsk_xxxx --interval 5m --db /var/lib/davis/wx.db
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
)

func main() {
	// ── Flags ────────────────────────────────────────────────────────────────
	portName := flag.String("port", "", "Serial port, e.g. /dev/ttyUSB0 or COM3 (required)")
	baud := flag.Int("baud", 19200, "Baud rate (Davis default: 19200)")
	apiURL := flag.String("api-url", "", "Voria2 API base URL, e.g. https://api.voria2.io (required in daemon mode)")
	apiKey := flag.String("api-key", "", "Voria2 API key starting with vsk_ (required in daemon mode)")
	intervalStr := flag.String("interval", "2m", "Poll interval, e.g. 30s, 2m, 5m")
	dbPath := flag.String("db", "./weather.db", "SQLite buffer database path")
	bucket := flag.String("bucket", "0.2mm", "Rain bucket size per click: 0.2mm or 0.01in")
	dryRun := flag.Bool("dry-run", false, "Read one packet, print values and exit (no upload, no DB)")

	withLegacyAPI := flag.Bool("with-legacy-api", false, "Send to both Voria2 and the legacy API (mutually exclusive with --only-legacy-api)")
	onlyLegacyAPI := flag.Bool("only-legacy-api", false, "Send only to the legacy API, skipping Voria2 (mutually exclusive with --with-legacy-api)")
	legacyAPIURL := flag.String("legacy-api-url", "", "Legacy API base URL (required with --with-legacy-api / --only-legacy-api)")
	legacyAPIKey := flag.String("legacy-api-key", "", "Legacy API authorization key (required with --with-legacy-api / --only-legacy-api)")
	legacyAPIStation := flag.String("legacy-api-station", "", "Legacy station identifier appended to the URL path (required with --with-legacy-api / --only-legacy-api)")
	flag.Parse()

	if *portName == "" {
		log.Fatal("--port is required.  Example: --port /dev/ttyUSB0")
	}
	var bucketMM float64
	switch *bucket {
	case "0.2mm":
		bucketMM = 0.2
	case "0.01in":
		bucketMM = 0.01 * 25.4 // 0.254 mm/click
	default:
		log.Fatalf("invalid --bucket %q: accepted values are 0.2mm or 0.01in", *bucket)
	}

	// ── Dry-run: fetch one packet, print and exit ─────────────────────────────
	if *dryRun {
		port, err := openSerialPort(*portName, *baud)
		if err != nil {
			log.Fatalf("open serial port %s: %v", *portName, err)
		}
		defer port.Close()

		if err := wakeConsole(port); err != nil {
			log.Fatalf("wake console: %v", err)
		}

		data, err := fetchLPS(port)
		if err != nil {
			log.Fatalf("fetch LPS: %v", err)
		}
		printResults(data, bucketMM)
		return
	}

	// ── Daemon-mode validation ────────────────────────────────────────────────
	if *withLegacyAPI && *onlyLegacyAPI {
		log.Fatal("--with-legacy-api and --only-legacy-api are mutually exclusive")
	}
	legacyEnabled := *withLegacyAPI || *onlyLegacyAPI

	if !*onlyLegacyAPI {
		if *apiURL == "" {
			log.Fatal("--api-url is required.  Example: --api-url https://api.voria2.io")
		}
		if *apiKey == "" {
			log.Fatal("--api-key is required.  Example: --api-key vsk_xxxxxxxxxxxxxxxxxxxx")
		}
	}
	if legacyEnabled {
		if *legacyAPIURL == "" {
			log.Fatal("--legacy-api-url is required with --with-legacy-api / --only-legacy-api")
		}
		if *legacyAPIKey == "" {
			log.Fatal("--legacy-api-key is required with --with-legacy-api / --only-legacy-api")
		}
		if *legacyAPIStation == "" {
			log.Fatal("--legacy-api-station is required with --with-legacy-api / --only-legacy-api")
		}
	}
	interval, err := time.ParseDuration(*intervalStr)
	if err != nil || interval <= 0 {
		log.Fatalf("invalid --interval %q: must be a positive Go duration (e.g. 2m)", *intervalStr)
	}
	log.Printf("Rain bucket: %s (%.4f mm/click)", *bucket, bucketMM)

	// ── SQLite ───────────────────────────────────────────────────────────────
	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", *dbPath, err)
	}
	defer db.Close()
	log.Printf("SQLite buffer: %s", *dbPath)

	// ── API clients ───────────────────────────────────────────────────────────
	var client *APIClient
	if !*onlyLegacyAPI {
		client = newAPIClient(*apiURL, *apiKey)
		if station, err := client.verifyKey(); err != nil {
			log.Printf("Voria2 key verification failed: %v (continuing – readings will buffer locally)", err)
		} else {
			log.Printf("Voria2 station verified: %s", station)
		}
	}

	var legacyClient *LegacyClient
	if legacyEnabled {
		legacyClient = newLegacyClient(*legacyAPIURL, *legacyAPIKey, *legacyAPIStation)
		log.Printf("Legacy API enabled: %s (station: %s)", *legacyAPIURL, *legacyAPIStation)
	}

	// ── Graceful shutdown ────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Run loop ─────────────────────────────────────────────────────────────
	log.Printf("Polling every %s. Press Ctrl+C to stop.", interval)

	runTick(*portName, *baud, db, client, legacyClient, bucketMM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runTick(*portName, *baud, db, client, legacyClient, bucketMM)
		case <-ctx.Done():
			log.Println("Received shutdown signal, exiting.")
			return
		}
	}
}

// runTick is called on every poll interval. It reads one LOOP packet then
// dispatches to the Voria2 and/or legacy delivery paths independently.
func runTick(portName string, baud int, db *sql.DB, client *APIClient, legacyClient *LegacyClient, bucketMM float64) {
	// ── Open serial port ─────────────────────────────────────────────────────
	port, err := openSerialPort(portName, baud)
	if err != nil {
		log.Printf("open serial port %s: %v (will retry on next tick)", portName, err)
		return
	}
	defer port.Close()

	// ── Wake console ─────────────────────────────────────────────────────────
	if err := wakeConsole(port); err != nil {
		log.Printf("wake console: %v (will retry on next tick)", err)
		return
	}

	// ── Fetch reading ────────────────────────────────────────────────────────
	data, err := fetchLPS(port)
	if err != nil {
		log.Printf("fetchLPS error: %v (will retry on next tick)", err)
		return
	}

	now := time.Now().UTC()

	if client != nil {
		runVoria2Tick(data, now, db, client, bucketMM)
	}
	if legacyClient != nil {
		runLegacyTick(data, now, db, legacyClient, bucketMM)
	}
}

// runVoria2Tick handles the Voria2 bulk-upload path for one poll tick.
func runVoria2Tick(data *LOOPData, now time.Time, db *sql.DB, client *APIClient, bucketMM float64) {
	current := buildMeasurements(data, now, bucketMM)

	// ── Load buffered rows ───────────────────────────────────────────────────
	// Fill the 1000-item batch limit with oldest pending rows first; the
	// current readings occupy the tail of the batch.
	const batchMax = 1000
	pending, err := loadPending(db, batchMax-len(current))
	if err != nil {
		log.Printf("loadPending error: %v (proceeding without buffered rows)", err)
		pending = nil
	}

	// ── Build combined batch ─────────────────────────────────────────────────
	batch := make([]map[string]any, 0, len(pending)+len(current))
	for _, row := range pending {
		batch = append(batch, row.Payload)
	}
	batch = append(batch, current...)

	// ── Upload ───────────────────────────────────────────────────────────────
	result, err := client.bulkPost(batch)
	if err != nil {
		if isPermanent(err) {
			// 4xx: bad API key or unknown sensor slug – won't self-heal.
			// Do NOT buffer; logging is the only useful action.
			log.Printf("Voria2 permanent error (check --api-key and sensor slugs): %v", err)
		} else {
			// Transient: network down or server 5xx – buffer current readings.
			log.Printf("Voria2 upload failed (transient, will retry next tick): %v", err)
			if storeErr := storePending(db, current); storeErr != nil {
				log.Printf("storePending error: %v", storeErr)
			} else {
				n, _ := pendingCount(db)
				log.Printf("Voria2: buffered %d new rows; total pending: %d", len(current), n)
			}
		}
		return
	}

	// ── Process per-item results ──────────────────────────────────────────────
	// HTTP 200 means the server processed the batch. Individual items may still
	// have failed permanently (e.g. unknown sensor slug). We delete ALL sent
	// pending rows regardless of per-item outcome – permanent failures will
	// never self-heal and must not accumulate in the buffer indefinitely.
	okCount := 0
	for _, r := range result.Results {
		if r.OK {
			okCount++
		} else {
			if r.Index < len(pending) {
				log.Printf("Voria2: buffered row id=%d rejected permanently: %s (removed from buffer)",
					pending[r.Index].ID, r.Error)
			} else {
				ci := r.Index - len(pending)
				log.Printf("Voria2: current measurement index %d rejected permanently: %s",
					ci, r.Error)
			}
		}
	}

	// Delete the pending rows we included in this batch.
	if len(pending) > 0 {
		pendingIDs := make([]int64, len(pending))
		for i, row := range pending {
			pendingIDs[i] = row.ID
		}
		if delErr := deletePending(db, pendingIDs); delErr != nil {
			log.Printf("deletePending error: %v", delErr)
		}
	}

	remaining, _ := pendingCount(db)
	log.Printf("Voria2 tick OK: sent %d items (%d pending drained), API accepted %d/%d, buffer remaining: %d",
		len(batch), len(pending), okCount, len(batch), remaining)
}

// legacyDrainMax is the maximum number of buffered legacy packets to drain per
// tick. The legacy API is one-per-POST, so a large burst would delay the next
// serial read; drain progressively across ticks instead.
const legacyDrainMax = 50

// runLegacyTick handles the legacy pipe-delimited packet delivery path for one
// poll tick. It drains buffered packets oldest-first before sending the current
// reading. Both paths are independent: a failure here does not affect Voria2.
func runLegacyTick(data *LOOPData, now time.Time, db *sql.DB, lc *LegacyClient, bucketMM float64) {
	packet := buildLegacyPacket(data, now, bucketMM)

	// ── Drain buffered packets ────────────────────────────────────────────────
	pending, err := loadLegacyPending(db, legacyDrainMax)
	if err != nil {
		log.Printf("loadLegacyPending error: %v (proceeding without buffered packets)", err)
		pending = nil
	}

	var drainedIDs []int64
	for _, row := range pending {
		if sendErr := lc.sendPacket(row.Packet); sendErr != nil {
			if isPermanent(sendErr) {
				// 4xx: bad key or station – remove this row, keep draining.
				log.Printf("Legacy: buffered packet id=%d rejected permanently: %v (removed from buffer)", row.ID, sendErr)
				drainedIDs = append(drainedIDs, row.ID)
			} else {
				// Transient: stop draining, retry remaining rows next tick.
				log.Printf("Legacy drain interrupted (transient): %v", sendErr)
				break
			}
		} else {
			drainedIDs = append(drainedIDs, row.ID)
		}
	}

	if len(drainedIDs) > 0 {
		if delErr := deleteLegacyPending(db, drainedIDs); delErr != nil {
			log.Printf("deleteLegacyPending error: %v", delErr)
		}
	}

	// ── Send current packet ───────────────────────────────────────────────────
	if sendErr := lc.sendPacket(packet); sendErr != nil {
		if isPermanent(sendErr) {
			log.Printf("Legacy permanent error (check --legacy-api-key / --legacy-api-station): %v", sendErr)
		} else {
			log.Printf("Legacy upload failed (transient, will retry next tick): %v", sendErr)
			if storeErr := storeLegacyPending(db, []string{packet}); storeErr != nil {
				log.Printf("storeLegacyPending error: %v", storeErr)
			} else {
				n, _ := legacyPendingCount(db)
				log.Printf("Legacy: buffered 1 packet; total pending: %d", n)
			}
		}
		return
	}

	remaining, _ := legacyPendingCount(db)
	log.Printf("Legacy tick OK: sent current packet + drained %d buffered; buffer remaining: %d",
		len(drainedIDs), remaining)
}

func printResults(d *LOOPData, bucketMM float64) {
	windDirStr := "calm"
	if d.WindDir > 0 {
		windDirStr = fmt.Sprintf("%d°", d.WindDir)
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetTitle("Davis Vantage Pro – Current Data")
	t.AppendHeader(table.Row{"Field", "Value"})

	t.AppendRow(table.Row{"Captured (UTC)", time.Now().UTC().Format("2006-01-02 15:04:05")})
	t.AppendSeparator()

	t.AppendRow(table.Row{"Outside Temp", fmt.Sprintf("%.1f °C  (%.1f °F)", d.OutsideTempC, d.OutsideTempF)})
	t.AppendRow(table.Row{"Inside Temp", fmt.Sprintf("%.1f °C  (%.1f °F)", d.InsideTempC, d.InsideTempF)})
	t.AppendSeparator()

	t.AppendRow(table.Row{"Outside Humidity", fmt.Sprintf("%d%%", d.OutsideHumidity)})
	t.AppendRow(table.Row{"Inside Humidity", fmt.Sprintf("%d%%", d.InsideHumidity)})
	t.AppendSeparator()

	t.AppendRow(table.Row{"Pressure", fmt.Sprintf("%.2f hPa  (%.3f inHg)", d.PressureHPa, d.PressureInHg)})
	t.AppendSeparator()

	t.AppendRow(table.Row{"Wind Speed", fmt.Sprintf("%.2f m/s  (%.1f mph)", d.WindSpeedMs, d.WindSpeedMph)})
	t.AppendRow(table.Row{"Wind Direction", windDirStr})
	t.AppendRow(table.Row{"Wind U (eastward)", fmt.Sprintf("%+.3f m/s", d.WindU)})
	t.AppendRow(table.Row{"Wind V (northward)", fmt.Sprintf("%+.3f m/s", d.WindV)})
	t.AppendSeparator()

	if d.HasGust {
		var gustDirStr string
		switch {
		case d.GustDir == 0:
			gustDirStr = "calm"
		case d.GustDir > 360:
			gustDirStr = "n/a"
		default:
			gustDirStr = fmt.Sprintf("%d°", d.GustDir)
		}
		t.AppendRow(table.Row{"Gust Speed (10-min)", fmt.Sprintf("%.2f m/s  (%.1f mph)", d.GustSpeedMs, d.GustSpeedMph)})
		t.AppendRow(table.Row{"Gust Direction", gustDirStr})
		t.AppendRow(table.Row{"Gust U (eastward)", fmt.Sprintf("%+.3f m/s", d.GustU)})
		t.AppendRow(table.Row{"Gust V (northward)", fmt.Sprintf("%+.3f m/s", d.GustV)})
	} else {
		t.AppendRow(table.Row{"Wind Gust (10-min)", "n/a (LOOP2 unavailable or console <10 min data)"})
	}
	t.AppendSeparator()

	rainMM := float64(d.YearRainClicks) * bucketMM
	t.AppendRow(table.Row{"Year Rain", fmt.Sprintf("%.1f mm  (%d clicks × %.4f mm)", rainMM, d.YearRainClicks, bucketMM)})

	t.SetStyle(table.StyleRounded)
	t.Render()
}
