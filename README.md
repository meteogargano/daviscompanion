# daviscompanion

A Go daemon that reads current conditions from a **Davis Vantage Pro / Pro2 / Vue**
console via the WeatherLink serial interface and continuously uploads measurements
to the **Voria2 Ingest API**, with automatic local buffering when offline.

Implements the Davis Serial Communication Reference Manual, Rev 2.6.1.

## What it measures

| Field | Unit | Source | Notes |
|---|---|---|---|
| Outside temperature | °C | LOOP1 | Sent to API; °F retained for display |
| Inside temperature | °C | LOOP1 | Display only |
| Outside humidity | % | LOOP1 | Sent to API |
| Inside humidity | % | LOOP1 | Display only |
| Barometric pressure | hPa | LOOP1 | Sent to API; inHg retained for display |
| Wind speed (instantaneous) | m/s | LOOP1 | Decomposed into U/V |
| Wind direction | degrees (meteorological) | LOOP1 | 0 = calm |
| Wind U component (eastward) | m/s | LOOP1 | Sent to API |
| Wind V component (northward) | m/s | LOOP1 | Sent to API |
| Wind gust speed (10-min rolling max) | m/s | LOOP2 | Sent to API as `gust` |
| Wind gust direction | degrees (meteorological) | LOOP2 | Display only |
| Year-to-date rainfall | mm | LOOP1 | Cumulative; converted from clicks via `--bucket` |

Wind U/V follow standard meteorological convention: direction is **from**,
so a North wind (360°) gives `U=0, V=-speed`.

When LOOP2 is unavailable (older firmware or console cold-start), gust falls
back to the LOOP1 10-minute average speed — no data is lost.

## Requirements

- A Davis WeatherLink data logger connected to a serial / USB-serial port
- **VP2 firmware ≥1.90** or a **Vantage Vue** — required for LOOP2 / LPS command
  (older firmware still works but gust will fall back to the 10-min average)
- A Voria2 station API key (`vsk_…`) — not required for `--dry-run` or `--only-legacy-api`

## Usage

### Test the serial connection (dry-run)

Connects to the console, reads one LOOP1 + one LOOP2 packet, prints all values
and exits. No API key or database needed.

```bash
./davisweather --port /dev/ttyUSB0 --dry-run

# With an imperial (0.01 in) rain gauge
./davisweather --port /dev/ttyUSB0 --dry-run --bucket 0.01in
```

### Daemon mode (continuous upload to Voria2)

Polls the console every `--interval`, uploads to the Voria2 Ingest API, and
buffers undeliverable readings locally in SQLite for automatic retry.

```bash
./davisweather \
  --port    /dev/ttyUSB0 \
  --api-url https://api.voria2.io \
  --api-key vsk_xxxxxxxxxxxxxxxxxxxx

# All options
./davisweather \
  --port     /dev/ttyUSB0 \
  --api-url  https://api.voria2.io \
  --api-key  vsk_xxxxxxxxxxxxxxxxxxxx \
  --interval 5m \
  --db       /var/lib/davis/weather.db \
  --bucket   0.01in \
  --baud     9600
```

### Daemon mode with legacy API

Use `--with-legacy-api` to send to both Voria2 and the legacy API simultaneously,
or `--only-legacy-api` to skip Voria2 entirely.

```bash
# Both Voria2 and legacy API
./davisweather \
  --port               /dev/ttyUSB0 \
  --api-url            https://api.voria2.io \
  --api-key            vsk_xxxxxxxxxxxxxxxxxxxx \
  --with-legacy-api \
  --legacy-api-url     https://legacy.example.com/ingest \
  --legacy-api-key     mylegacykey \
  --legacy-api-station MYSTATION

# Legacy API only (no Voria2 key required)
./davisweather \
  --port               /dev/ttyUSB0 \
  --only-legacy-api \
  --legacy-api-url     https://legacy.example.com/ingest \
  --legacy-api-key     mylegacykey \
  --legacy-api-station MYSTATION
```

Stop cleanly with **Ctrl+C** or `SIGTERM`.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--port` | *(required)* | Serial port, e.g. `/dev/ttyUSB0` or `COM3` |
| `--api-url` | *(required unless `--only-legacy-api`)* | Voria2 API base URL, e.g. `https://api.voria2.io` |
| `--api-key` | *(required unless `--only-legacy-api`)* | Voria2 station API key (`vsk_…`) |
| `--dry-run` | `false` | Read one packet, print values and exit (no upload, no DB) |
| `--bucket` | `0.2mm` | Rain bucket size per click: `0.2mm` (metric) or `0.01in` (imperial/US) |
| `--interval` | `2m` | Poll interval as a Go duration: `30s`, `2m`, `5m`, … |
| `--db` | `./weather.db` | SQLite offline buffer path |
| `--baud` | `19200` | Baud rate (Davis default is 19200) |
| `--with-legacy-api` | `false` | Send to both Voria2 **and** the legacy API |
| `--only-legacy-api` | `false` | Send **only** to the legacy API; skip Voria2 entirely |
| `--legacy-api-url` | *(required if legacy enabled)* | Legacy API base URL |
| `--legacy-api-key` | *(required if legacy enabled)* | Legacy API authorization key |
| `--legacy-api-station` | *(required if legacy enabled)* | Station identifier appended as a URL path segment |

`--with-legacy-api` and `--only-legacy-api` are mutually exclusive.

## Legacy API mode

When `--with-legacy-api` or `--only-legacy-api` is set, each poll tick also
sends a pipe-delimited measurement packet to the configured legacy endpoint.

### Packet format

```
unix_timestamp|temp_c|humidity_pct|baro_hpa|windspeed_kmh|windgust_kmh|winddir_deg|rain_mm_ytd|EE
```

Example: `1774954968|10.8|80.0|1004.9|3.5|4.0|41|245.4|EE`

| Position | Field | Unit | Source | Format |
|---|---|---|---|---|
| 1 | Unix timestamp (UTC) | seconds | `time.Now().UTC().Unix()` | integer |
| 2 | Outside temperature | °C | `OutsideTempC` | `%.1f` |
| 3 | Outside humidity | % | `OutsideHumidity` | `%.1f` |
| 4 | Barometric pressure | hPa | `PressureHPa` | `%.1f` |
| 5 | Wind speed (instantaneous) | km/h | `WindSpeedMs × 3.6` | `%.1f` |
| 6 | Wind gust (10-min rolling max) | km/h | `gustMs(d) × 3.6` | `%.1f` |
| 7 | Wind direction | degrees | `WindDir` | integer |
| 8 | Year-to-date rainfall | mm | `YearRainClicks × bucketMM` | `%.1f` |
| 9 | End marker | — | literal | `EE` |

### Transport

Each packet is sent as a single `POST` request:

```
POST {legacy-api-url}/{legacy-api-station}
Authorization: <legacy-api-key>
Content-Type: text/plain

1774954968|10.8|80.0|1004.9|3.5|4.0|41|245.4|EE
```

### Mode selection

| Flag combination | Voria2 | Legacy |
|---|---|---|
| *(neither)* | enabled | disabled |
| `--with-legacy-api` | enabled | enabled |
| `--only-legacy-api` | disabled | enabled |

### Offline buffering (legacy)

Because the legacy API accepts one packet per request, the daemon drains up to
**50 buffered packets per tick** during recovery (vs 995 for Voria2). Both
buffers are independent — a Voria2 failure does not prevent legacy delivery, and
vice versa.

**Error classification** follows the same rules as Voria2:

| Condition | Buffered? |
|---|---|
| Network error / HTTP 5xx | Yes — retry next tick |
| HTTP 4xx | No — permanent config error |

**Inspect the legacy buffer:**

```bash
sqlite3 weather.db "SELECT count(*) FROM pending_legacy;"
sqlite3 weather.db "SELECT id, created_at, packet FROM pending_legacy LIMIT 10;"
```

## Rain bucket sizes

The Davis console reports rain as raw **clicks**. The mm value depends on which
tipping-bucket gauge is installed:

| Flag value | Gauge type | mm per click |
|---|---|---|
| `0.2mm` *(default)* | Metric | 0.2000 mm |
| `0.01in` | Imperial / US | 0.2540 mm (= 0.01 × 25.4) |

The year-to-date counter resets on the console each January 1st. The API receives
the value as `cumulative_mm`.

## Sensor slugs

The following slugs are hardcoded and must exist in your Voria2 station:

| Slug | Measurement type | Value sent |
|---|---|---|
| `temperature` | temperature | Outside temperature (°C) |
| `humidity` | humidity | Outside humidity (%) |
| `pressure` | pressure | Barometric pressure (hPa) |
| `wind` | wind | U/V components + 10-min rolling gust (m/s) |
| `rain` | rain (cumulative) | Year-to-date rain (mm, bucket-adjusted) |

All timestamps are sent in **UTC** (`2006-01-02T15:04:05Z`).

## Offline buffering

When the API cannot be reached, each tick's readings (5 rows) are stored in the
local SQLite database. On the next successful tick the daemon automatically drains
up to **995 buffered rows** alongside the current reading in a single bulk request
(API limit: 1000 items per call).

**Error classification:**

| Condition | Buffered? | Reason |
|---|---|---|
| Network error / HTTP 5xx | Yes — retry next tick | Transient |
| HTTP 401 / 403 | No | Bad API key won't self-heal |
| HTTP 422 | No | Unknown sensor slug is a config error |
| HTTP 200, item `ok: false` | Removed from buffer | Permanent rejection |

The daemon **never needs to be restarted** to recover — the ticker runs regardless
of API health. The SQLite file persists across restarts; no buffered data is lost.

**Inspect the buffer:**

```bash
sqlite3 weather.db "SELECT count(*) FROM pending_measurements;"
sqlite3 weather.db "SELECT id, created_at, json_extract(payload,'$.sensor') FROM pending_measurements LIMIT 10;"
```

## legacyserver

`legacyserver` is a companion HTTP server that receives packets from
`davisweather --with-legacy-api` / `--only-legacy-api` and writes each one to
a `.txt` file on disk. It lives in the `legacyserver/` subdirectory and is a
separate Go module with no external dependencies.

### Build

```bash
cd legacyserver
go build -o legacyserver .
```

### Run

```bash
./legacyserver \
  --api-key  mysecretkey \
  --data-dir /var/lib/weather/packets

# All options
./legacyserver \
  --api-key  mysecretkey \
  --data-dir /var/lib/weather/packets \
  --addr     :9000 \
  --workers  8 \
  --queue    2048
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--api-key` | *(required)* | Key expected in the `Authorization` header |
| `--data-dir` | *(required)* | Directory where `.txt` files are written (created if absent) |
| `--addr` | `:8080` | Listen address |
| `--workers` | `4` | Background file-writer goroutines |
| `--queue` | `1024` | In-memory write queue capacity |

### Endpoint

```
POST /{station}
Authorization: <api-key>
Content-Type: text/plain

1774954968|10.8|80.0|1004.9|3.5|4.0|41|245.4|EE
```

The station name is taken from the **last path segment** of the URL, so both
`/MYSTATION` and `/api/v1/MYSTATION` work — configure the client's
`--legacy-api-url` accordingly.

Multiple packets can be sent in one request body (one per line). Each valid
line is written as a separate file; invalid lines are rejected individually and
reported in the response without affecting the rest of the batch.

### Output files

Each accepted packet is saved as:

```
{data-dir}/meteo_{station}_{unix_timestamp}.txt
```

The unix timestamp is taken from **field 1 of the packet** (the weather
station's own recording time). File content is the raw packet string followed
by a newline.

Example:

```
meteo_MYSTATION_1774954968.txt  → "1774954968|10.8|80.0|1004.9|3.5|4.0|41|245.4|EE\n"
```

If a packet with the same station and timestamp is received again (e.g. a
client retry), the file is overwritten with identical content.

### Response

```json
{"ok": true,  "accepted": 1, "rejected": 0, "errors": []}
{"ok": true,  "accepted": 2, "rejected": 1, "errors": [{"line": 3, "message": "expected 9 pipe-delimited fields, got 5"}]}
{"ok": false, "accepted": 0, "rejected": 1, "errors": [{"line": 1, "message": "expected EE terminator, got \"XX\""}]}
```

HTTP status: `200` when at least one packet was accepted; `422` when all
packets were rejected; `401` for bad key; `405` for non-POST methods.

### Architecture

File writes are dispatched to a pool of `--workers` goroutines via a buffered
channel (`--queue` capacity). This decouples HTTP response latency from disk
I/O — the handler returns as soon as jobs are enqueued, and writers flush to
disk in the background. On `SIGINT`/`SIGTERM`:

1. The HTTP server stops accepting new connections and waits for in-flight
   handlers to complete.
2. The write queue is closed and all workers drain their remaining jobs.
3. The process exits only after every queued file has been written.

## Protocol notes

1. **Wake-up** — sends `\n`, waits up to 1.2 s for `\n\r` response; retries 3×.
   Automatically re-attempted if a fetch fails mid-run.
2. **LPS command** — sends `LPS 3 2\n` (bitmask `3` = LOOP1 + LOOP2, count `2`),
   waits for ACK (`0x06`), then reads two sequential 99-byte packets ~2 s apart.
   LOOP2 failure is non-fatal; the daemon continues with LOOP1 data only.
3. **CRC** — Davis CRC-16 (polynomial `0x1021`) over all 99 bytes; result must be zero.
   Verified independently for each packet.

## Packet offsets used

### LOOP1 (PacketType = 0)

```
 7  uint16 LE  Barometer          (inHg / 1000)
 9  int16  LE  Inside temp        (tenths °F)
11  uint8      Inside humidity    (%)
12  int16  LE  Outside temp       (tenths °F)
14  uint8      Wind speed         (mph, instantaneous)
15  uint8      Wind speed 10-min  (mph) ← fallback gust when LOOP2 unavailable
16  uint16 LE  Wind direction     (1–360 °, 0 = calm)
33  uint8      Outside humidity   (%)
54  uint16 LE  Year rain          (clicks; multiply by bucket size for mm)
```

### LOOP2 (PacketType = 1) — VP2 firmware ≥1.90 / Vantage Vue only

```
21  uint16 LE  Wind gust speed    (1/10 mph, 10-min rolling max; 0x7FFF = not yet available)
23  uint16 LE  Wind gust direction(1–360 °, 0 = calm)
```
