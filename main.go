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

	"go.bug.st/serial"
)

func main() {
	// ── Flags ────────────────────────────────────────────────────────────────
	portName    := flag.String("port",     "",             "Serial port, e.g. /dev/ttyUSB0 or COM3 (required)")
	baud        := flag.Int("baud",        19200,          "Baud rate (Davis default: 19200)")
	apiURL      := flag.String("api-url",  "",             "Voria2 API base URL, e.g. https://api.voria2.io (required)")
	apiKey      := flag.String("api-key",  "",             "Voria2 API key starting with vsk_ (required)")
	intervalStr := flag.String("interval", "2m",           "Poll interval, e.g. 30s, 2m, 5m")
	dbPath      := flag.String("db",       "./weather.db", "SQLite buffer database path")
	bucket      := flag.String("bucket",   "0.2mm",  "Rain bucket size per click: 0.2mm or 0.01in")
	dryRun      := flag.Bool("dry-run",   false,    "Read one packet, print values and exit (no upload, no DB)")
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

	// ── Serial port ──────────────────────────────────────────────────────────
	mode := &serial.Mode{
		BaudRate: *baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(*portName, mode)
	if err != nil {
		log.Fatalf("open %s: %v", *portName, err)
	}
	defer port.Close()

	if err := port.SetReadTimeout(200 * time.Millisecond); err != nil {
		log.Printf("warning: could not set read timeout: %v", err)
	}
	log.Printf("Opened %s at %d 8N1", *portName, *baud)

	// ── Wake console ─────────────────────────────────────────────────────────
	if err := wakeConsole(port); err != nil {
		log.Fatalf("wake console: %v", err)
	}
	log.Println("Console awake")

	// ── Dry-run: fetch one packet, print and exit ─────────────────────────────
	if *dryRun {
		data, err := fetchLOOP(port)
		if err != nil {
			log.Fatalf("fetch LOOP: %v", err)
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
	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", *dbPath, err)
	}
	defer db.Close()
	log.Printf("SQLite buffer: %s", *dbPath)

	// ── API client + key verification ────────────────────────────────────────
	client := newAPIClient(*apiURL, *apiKey)
	if station, err := client.verifyKey(); err != nil {
		log.Printf("API key verification failed: %v (continuing – readings will buffer locally)", err)
	} else {
		log.Printf("Station verified: %s", station)
	}

	// ── Graceful shutdown ────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Run loop ─────────────────────────────────────────────────────────────
	log.Printf("Polling every %s. Press Ctrl+C to stop.", interval)

	runTick(port, db, client, bucketMM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runTick(port, db, client, bucketMM)
		case <-ctx.Done():
			log.Println("Received shutdown signal, exiting.")
			return
		}
	}
}

// runTick is called on every poll interval. It reads one LOOP packet, builds
// the API payload, attempts a bulk upload (including any buffered rows), and
// falls back to SQLite storage on transient failures.
func runTick(port serial.Port, db *sql.DB, client *APIClient, bucketMM float64) {
	// ── Fetch reading ────────────────────────────────────────────────────────
	data, err := fetchLOOP(port)
	if err != nil {
		log.Printf("fetchLOOP error: %v; attempting console re-wake", err)
		if wakeErr := wakeConsole(port); wakeErr != nil {
			log.Printf("re-wake failed: %v", wakeErr)
		}
		return
	}

	now := time.Now().UTC()
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
			log.Printf("API permanent error (check --api-key and sensor slugs): %v", err)
		} else {
			// Transient: network down or server 5xx – buffer current readings.
			log.Printf("Upload failed (transient, will retry next tick): %v", err)
			if storeErr := storePending(db, current); storeErr != nil {
				log.Printf("storePending error: %v", storeErr)
			} else {
				n, _ := pendingCount(db)
				log.Printf("Buffered %d new rows; total pending in SQLite: %d", len(current), n)
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
				log.Printf("Buffered row id=%d rejected permanently by API: %s (removed from buffer)",
					pending[r.Index].ID, r.Error)
			} else {
				ci := r.Index - len(pending)
				log.Printf("Current measurement index %d rejected permanently by API: %s",
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
	log.Printf("Tick OK: sent %d items (%d pending drained), API accepted %d/%d, buffer remaining: %d",
		len(batch), len(pending), okCount, len(batch), remaining)
}

func printResults(d *LOOPData, bucketMM float64) {
	windDirStr := "calm"
	if d.WindDir > 0 {
		windDirStr = fmt.Sprintf("%d°", d.WindDir)
	}
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║        Davis Vantage Pro – Current Data          ║")
	fmt.Printf( "║  Captured (UTC)    : %-28s ║\n", time.Now().UTC().Format("2006-01-02 15:04:05"))
	fmt.Println("╠══════════════════════════════════════════════════╣")
	fmt.Printf( "║  Outside Temp      : %6.1f °C  (%6.1f °F)    ║\n", d.OutsideTempC, d.OutsideTempF)
	fmt.Printf( "║  Inside  Temp      : %6.1f °C  (%6.1f °F)    ║\n", d.InsideTempC, d.InsideTempF)
	fmt.Println("╠══════════════════════════════════════════════════╣")
	fmt.Printf( "║  Outside Humidity  : %3d %%                       ║\n", d.OutsideHumidity)
	fmt.Printf( "║  Inside  Humidity  : %3d %%                       ║\n", d.InsideHumidity)
	fmt.Println("╠══════════════════════════════════════════════════╣")
	fmt.Printf( "║  Pressure          : %8.2f hPa                ║\n", d.PressureHPa)
	fmt.Printf( "║                    ( %7.3f inHg )             ║\n", d.PressureInHg)
	fmt.Println("╠══════════════════════════════════════════════════╣")
	fmt.Printf( "║  Wind Speed        : %6.2f m/s  (%5.1f mph)   ║\n", d.WindSpeedMs, d.WindSpeedMph)
	fmt.Printf( "║  Wind Direction    : %-6s                     ║\n", windDirStr)
	fmt.Printf( "║  Wind U (eastward) : %+8.3f m/s              ║\n", d.WindU)
	fmt.Printf( "║  Wind V (northward): %+8.3f m/s              ║\n", d.WindV)
	fmt.Printf( "║  Wind 10-min avg   : %6.2f m/s  (%5.1f mph)   ║\n", d.WindSpeed10MinMs, d.WindSpeed10MinMph)
	fmt.Println("╠══════════════════════════════════════════════════╣")
	rainMM := float64(d.YearRainClicks) * bucketMM
	fmt.Printf( "║  Year Rain         : %8.1f mm               ║\n", rainMM)
	fmt.Printf( "║                    ( %5d clicks × %.4f mm ) ║\n", d.YearRainClicks, bucketMM)
	fmt.Println("╚══════════════════════════════════════════════════╝")
}
