// davisweather – continuous daemon that reads a Davis Vantage Pro / Pro2 / Vue
// console via serial, uploads measurements to the Voria2 Ingest API, and
// buffers undeliverable readings in a local SQLite database for automatic
// retry. With --no-db the SQLite buffer is disabled: uploads become
// fire-and-forget, failed readings are dropped, and the next interval tick
// retries with fresh data.
//
// Usage:
//
//	davisweather --port /dev/ttyUSB0 --api-url https://api.voria2.io \
//	             --api-key vsk_xxxx
//	davisweather --port /dev/ttyUSB0 --api-url https://api.voria2.io \
//	             --api-key vsk_xxxx --interval 5m --db /var/lib/davis/wx.db
//	davisweather --port /dev/ttyUSB0 --api-url https://api.voria2.io \
//	             --api-key vsk_xxxx --no-db
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
)

// version is overridden at build time via:
//
//	go build -ldflags "-X main.version=0.6"
var version = "dev"

func main() {
	// ── Flags ────────────────────────────────────────────────────────────────
	portName := flag.String("port", "", "Serial port, e.g. /dev/ttyUSB0 or COM3 (required)")
	baud := flag.Int("baud", 19200, "Baud rate (Davis default: 19200)")
	apiURL := flag.String("api-url", "", "Voria2 API base URL, e.g. https://api.voria2.io (required in daemon mode)")
	apiKey := flag.String("api-key", "", "Voria2 API key starting with vsk_ (required in daemon mode)")
	intervalStr := flag.String("interval", "2m", "Poll interval, e.g. 30s, 2m, 5m")
	dbPath := flag.String("db", "./weather.db", "SQLite buffer database path")
	noDB := flag.Bool("no-db", false, "Stateless mode: no SQLite database and no offline buffering. Failed uploads are dropped; the next interval retries with fresh data (ignores --db)")
	bucket := flag.String("bucket", "0.2mm", "Rain bucket size per click: 0.2mm or 0.01in")
	dryRun := flag.Bool("dry-run", false, "Read one packet, print values and exit (no upload, no DB)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("davisweather %s\n", version)
		return
	}

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
			if errors.Is(err, errISSLinkLost) {
				log.Fatalf("discarded LOOP packet: %v", err)
			}
			log.Fatalf("fetch LPS: %v", err)
		}
		printResults(data, bucketMM)
		return
	}

	// ── Daemon-mode validation ────────────────────────────────────────────────
	if *apiURL == "" {
		log.Fatal("--api-url is required.  Example: --api-url https://api.voria2.io")
	}
	if *apiKey == "" {
		log.Fatal("--api-key is required.  Example: --api-key vsk_xxxxxxxxxxxxxxxxxxxx")
	}
	interval, err := time.ParseDuration(*intervalStr)
	if err != nil || interval <= 0 {
		log.Fatalf("invalid --interval %q: must be a positive Go duration (e.g. 2m)", *intervalStr)
	}
	log.Printf("Rain bucket: %s (%.4f mm/click)", *bucket, bucketMM)

	// ── SQLite ───────────────────────────────────────────────────────────────
	var db *sql.DB
	if *noDB {
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "db" {
				log.Println("--db is ignored in --no-db mode")
			}
		})
		log.Println("SQLite buffer: disabled (--no-db) – failed uploads are dropped")
	} else {
		db, err = openDB(*dbPath)
		if err != nil {
			log.Fatalf("open db %s: %v", *dbPath, err)
		}
		defer db.Close()
		log.Printf("SQLite buffer: %s", *dbPath)
	}

	// ── API client ────────────────────────────────────────────────────────────
	client := newAPIClient(*apiURL, *apiKey)
	if station, err := client.verifyKey(); err != nil {
		if *noDB {
			log.Printf("Voria2 key verification failed: %v (continuing – failed uploads will be dropped)", err)
		} else {
			log.Printf("Voria2 key verification failed: %v (continuing – readings will buffer locally)", err)
		}
	} else {
		log.Printf("Voria2 station verified: %s", station)
	}

	// ── Graceful shutdown ────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Run loop ─────────────────────────────────────────────────────────────
	log.Printf("Polling every %s. Press Ctrl+C to stop.", interval)

	runTick(*portName, *baud, db, client, bucketMM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runTick(*portName, *baud, db, client, bucketMM)
		case <-ctx.Done():
			log.Println("Received shutdown signal, exiting.")
			return
		}
	}
}

// runTick is called on every poll interval. It reads one LOOP packet and
// uploads it to the Voria2 API.
func runTick(portName string, baud int, db *sql.DB, client *APIClient, bucketMM float64) {
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
		if errors.Is(err, errISSLinkLost) {
			log.Printf("discarding LOOP packet while ISS link is down: %v (waiting for next good packet)", err)
			return
		}
		log.Printf("fetchLPS error: %v (will retry on next tick)", err)
		return
	}

	now := time.Now().UTC()

	if db == nil {
		runVoria2TickSimple(data, now, client, bucketMM)
	} else {
		runVoria2Tick(data, now, db, client, bucketMM)
	}
}

// runVoria2Tick handles the Voria2 bulk-upload path for one poll tick.
func runVoria2Tick(data *LOOPData, now time.Time, db *sql.DB, client *APIClient, bucketMM float64) {
	current := buildMeasurements(data, now, bucketMM)

	// ── Load buffered rows ───────────────────────────────────────────────────
	// Fill the 1000-item batch limit with oldest pending rows first; the
	// current readings occupy the tail of the batch.
	const batchMax = 1000
	pendingCountBefore, err := pendingCount(db)
	if err != nil {
		log.Printf("pendingCount error: %v (assuming empty buffer)", err)
		pendingCountBefore = 0
	}

	pendingLimit := batchMax
	if pendingCountBefore == 0 {
		pendingLimit = batchMax - len(current)
	}

	pending, err := loadPending(db, pendingLimit)
	if err != nil {
		log.Printf("loadPending error: %v (proceeding without buffered rows)", err)
		pending = nil
	}

	backlogMode := len(pending) > 0
	var bufferedCurrentRows []PendingRow
	if backlogMode {
		bufferedCurrentRows, err = storePendingRows(db, current)
		if err != nil {
			log.Printf("storePending error while preserving current readings behind backlog: %v", err)
			return
		}
	}

	// ── Build combined batch ─────────────────────────────────────────────────
	batch := make([]map[string]any, 0, len(pending)+len(current))
	for _, row := range pending {
		batch = append(batch, row.Payload)
	}
	if !backlogMode {
		batch = append(batch, current...)
	}

	if len(batch) == 0 {
		remaining, _ := pendingCount(db)
		log.Printf("Voria2 tick idle: no measurements to send, buffer remaining: %d", remaining)
		return
	}

	// ── Upload ───────────────────────────────────────────────────────────────
	result, err := client.bulkPost(batch)
	if err != nil {
		if isPermanent(err) {
			// 4xx: bad API key or unknown sensor slug – won't self-heal.
			// Remove freshly buffered current rows if we pre-buffered them behind backlog.
			if len(bufferedCurrentRows) > 0 {
				bufferedIDs := make([]int64, 0, len(bufferedCurrentRows))
				deadRows := make([]DeadMeasurementInsert, 0, len(bufferedCurrentRows))
				for _, row := range bufferedCurrentRows {
					bufferedIDs = append(bufferedIDs, row.ID)
					deadRows = append(deadRows, DeadMeasurementInsert{Payload: row.Payload, Error: err.Error()})
				}
				if delErr := deletePending(db, bufferedIDs); delErr != nil {
					log.Printf("deletePending buffered current rows error: %v", delErr)
				}
				if deadErr := storeDeadMeasurements(db, deadRows); deadErr != nil {
					log.Printf("storeDeadMeasurements buffered current rows error: %v", deadErr)
				}
			}
			log.Printf("Voria2 permanent error (check --api-key and sensor slugs): %v", err)
		} else {
			// Transient: network down or server 5xx – buffer current readings.
			log.Printf("Voria2 upload failed (transient, will retry next tick): %v", err)
			if !backlogMode {
				if storeErr := storePending(db, current); storeErr != nil {
					log.Printf("storePending error: %v", storeErr)
				} else {
					n, _ := pendingCount(db)
					log.Printf("Voria2: buffered %d new rows; total pending: %d", len(current), n)
				}
			} else {
				n, _ := pendingCount(db)
				log.Printf("Voria2: backlog preserved, total pending: %d", n)
			}
		}
		return
	}

	// ── Process per-item results ──────────────────────────────────────────────
	okCount := 0
	var pendingAckIDs []int64
	var pendingRejected []RejectedPendingRow
	var currentRejected []DeadMeasurementInsert
	for _, r := range result.Results {
		if r.OK {
			okCount++
			if r.Index < len(pending) {
				pendingAckIDs = append(pendingAckIDs, pending[r.Index].ID)
			}
			continue
		}

		if r.Index < len(pending) {
			log.Printf("Voria2: buffered row id=%d rejected permanently: %s (moving to dead-letter)",
				pending[r.Index].ID, r.Error)
			pendingRejected = append(pendingRejected, RejectedPendingRow{Row: pending[r.Index], Error: r.Error})
			continue
		}

		if backlogMode {
			continue
		}

		ci := r.Index - len(pending)
		if ci >= 0 && ci < len(current) {
			log.Printf("Voria2: current measurement index %d rejected permanently: %s (moving to dead-letter)",
				ci, r.Error)
			currentRejected = append(currentRejected, DeadMeasurementInsert{Payload: current[ci], Error: r.Error})
		}
	}

	if len(pendingAckIDs) > 0 {
		if delErr := deletePending(db, pendingAckIDs); delErr != nil {
			log.Printf("deletePending error: %v", delErr)
		}
	}

	if len(pendingRejected) > 0 {
		if moveErr := movePendingToDead(db, pendingRejected); moveErr != nil {
			log.Printf("movePendingToDead error: %v", moveErr)
		}
	}

	if len(currentRejected) > 0 {
		if deadErr := storeDeadMeasurements(db, currentRejected); deadErr != nil {
			log.Printf("storeDeadMeasurements error: %v", deadErr)
		}
	}

	remaining, _ := pendingCount(db)
	log.Printf("Voria2 tick OK: sent %d items (%d pending drained), API accepted %d/%d, buffer remaining: %d",
		len(batch), len(pendingAckIDs), okCount, len(batch), remaining)
	return
}

// ─────────────────────────────────────────────────────────────────────────────
// No-DB mode (--no-db)
// ─────────────────────────────────────────────────────────────────────────────

// runVoria2TickSimple is the --no-db variant of runVoria2Tick: it uploads only
// the current readings, with no SQLite buffer and no backlog logic. Any
// failure drops the readings; the next interval tick starts fresh.
func runVoria2TickSimple(data *LOOPData, now time.Time, client *APIClient, bucketMM float64) {
	current := buildMeasurements(data, now, bucketMM)

	result, err := client.bulkPost(current)
	if err != nil {
		if isPermanent(err) {
			log.Printf("Voria2 permanent error (check --api-key and sensor slugs), dropping %d measurements: %v", len(current), err)
		} else {
			log.Printf("Voria2 upload failed (transient), dropping %d measurements until next tick: %v", len(current), err)
		}
		return
	}

	okCount := 0
	for _, r := range result.Results {
		if r.OK {
			okCount++
			continue
		}
		log.Printf("Voria2: measurement index %d rejected permanently, dropped: %s", r.Index, r.Error)
	}
	log.Printf("Voria2 tick OK: sent %d measurements, API accepted %d/%d", len(current), okCount, len(current))
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
