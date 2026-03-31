// legacyserver – HTTP server that receives pipe-delimited weather packets from
// the davisweather legacy API client, verifies the API key, and writes each
// packet to a .txt file on disk.
//
// Usage:
//
//	legacyserver --api-key mysecretkey --data-dir /var/lib/weather/packets
//	legacyserver --api-key mysecretkey --data-dir ./packets --addr :9000 --workers 8
//
// Packet format (per line in the request body):
//
//	unix_ts|temp_c|hum_pct|baro_hpa|wind_kmh|gust_kmh|wind_deg|rain_mm|EE
//
// Each accepted packet is written to:
//
//	{data-dir}/meteo_{station}_{unix_ts}.txt
//
// The request body may contain multiple packets separated by newlines.
// Each line is validated and written independently; partial batches are
// accepted.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// writeJob carries everything needed to flush one packet to disk.
type writeJob struct {
	station   string
	timestamp int64
	packet    string
}

// srv holds state shared across HTTP handlers.
type srv struct {
	apiKey     string
	dataDir    string
	writeQueue chan writeJob
	wg         sync.WaitGroup
}

// lineError is included in the JSON response for rejected lines.
type lineError struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Validation
// ─────────────────────────────────────────────────────────────────────────────

// stationRe restricts station names to safe filename characters.
var stationRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// parsePacket validates a pipe-delimited packet and returns its unix timestamp.
//
// Expected format: unix_ts|temp_c|hum|baro|wind_kmh|gust_kmh|wind_deg|rain_mm|EE
func parsePacket(line string) (int64, error) {
	fields := strings.Split(line, "|")
	if len(fields) != 9 {
		return 0, fmt.Errorf("expected 9 pipe-delimited fields, got %d", len(fields))
	}
	if fields[8] != "EE" {
		return 0, fmt.Errorf("expected EE terminator, got %q", fields[8])
	}
	ts, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid unix timestamp %q: %w", fields[0], err)
	}
	return ts, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP handler
// ─────────────────────────────────────────────────────────────────────────────

func (s *srv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// ── Auth ──────────────────────────────────────────────────────────────────
	if r.Header.Get("Authorization") != s.apiKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// ── Station from last URL path segment ────────────────────────────────────
	// Supports both /STATION and /some/prefix/STATION.
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	station := parts[len(parts)-1]
	if !stationRe.MatchString(station) {
		http.Error(w, "invalid station name in URL path (allowed: A-Z a-z 0-9 _ -)", http.StatusBadRequest)
		return
	}

	// ── Read body (max 1 MiB) ─────────────────────────────────────────────────
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "body read error", http.StatusBadRequest)
		return
	}
	if len(raw) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// ── Parse and enqueue one job per valid line ───────────────────────────────
	var (
		accepted int
		errs     []lineError
	)
	lineNum := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lineNum++
		ts, parseErr := parsePacket(line)
		if parseErr != nil {
			errs = append(errs, lineError{Line: lineNum, Message: parseErr.Error()})
			continue
		}
		select {
		case s.writeQueue <- writeJob{station: station, timestamp: ts, packet: line}:
			accepted++
		default:
			errs = append(errs, lineError{Line: lineNum, Message: "write queue full, try again later"})
		}
	}

	if accepted == 0 && lineNum == 0 {
		http.Error(w, "no packets found in body", http.StatusBadRequest)
		return
	}

	// ── Log and respond ───────────────────────────────────────────────────────
	log.Printf("POST /%s from %s: accepted=%d rejected=%d",
		station, r.RemoteAddr, accepted, len(errs))

	status := http.StatusOK
	if accepted == 0 {
		// All lines rejected (bad format / full queue).
		status = http.StatusUnprocessableEntity
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"ok":       status == http.StatusOK,
		"accepted": accepted,
		"rejected": len(errs),
		"errors":   errs,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Background file writer
// ─────────────────────────────────────────────────────────────────────────────

// writeWorker drains the write queue until it is closed, writing one file
// per job. Multiple workers run concurrently to overlap disk I/O.
func (s *srv) writeWorker() {
	defer s.wg.Done()
	for job := range s.writeQueue {
		s.writeFile(job)
	}
}

func (s *srv) writeFile(job writeJob) {
	filename := fmt.Sprintf("meteo_%s_%d.txt", job.station, job.timestamp)
	path := filepath.Join(s.dataDir, filename)
	if err := os.WriteFile(path, []byte(job.packet+"\n"), 0644); err != nil {
		log.Printf("error writing %s: %v", filename, err)
	} else {
		log.Printf("saved %s", filename)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	addr      := flag.String("addr",     ":8080", "Listen address, e.g. :8080 or 0.0.0.0:9000")
	apiKey    := flag.String("api-key",  "",      "API key expected in the Authorization header (required)")
	dataDir   := flag.String("data-dir", "",      "Directory where packet .txt files are written (required)")
	workers   := flag.Int("workers",     4,       "Number of background file-writer goroutines")
	queueSize := flag.Int("queue",       1024,    "In-memory write queue capacity (packets)")
	flag.Parse()

	if *apiKey == "" {
		log.Fatal("--api-key is required")
	}
	if *dataDir == "" {
		log.Fatal("--data-dir is required")
	}

	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		log.Fatalf("create data-dir %s: %v", *dataDir, err)
	}

	s := &srv{
		apiKey:     *apiKey,
		dataDir:    *dataDir,
		writeQueue: make(chan writeJob, *queueSize),
	}

	for i := 0; i < *workers; i++ {
		s.wg.Add(1)
		go s.writeWorker()
	}

	httpServer := &http.Server{
		Addr:         *addr,
		Handler:      s,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("legacyserver listening on %s  data-dir=%s  workers=%d  queue=%d",
			*addr, *dataDir, *workers, *queueSize)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutdown signal received, draining in-flight requests…")

	// Stop accepting new requests and wait for active handlers to finish.
	// Only then close the queue — handlers must not send to a closed channel.
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}

	// Drain the write queue and wait for workers to finish.
	close(s.writeQueue)
	s.wg.Wait()
	log.Println("All writes flushed, exiting.")
}
