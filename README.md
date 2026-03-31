# daviscompanion

A Go daemon that reads current conditions from a **Davis Vantage Pro / Pro2 / Vue**
console via the WeatherLink serial interface and continuously uploads measurements
to the **Voria2 Ingest API**, with automatic local buffering when offline.

Implements the Davis Serial Communication Reference Manual, Rev 2.6.1.

## What it measures

| Field | Unit | Notes |
|---|---|---|
| Outside temperature | °C | Sent to API; °F retained for display |
| Inside temperature | °C | Display only |
| Outside humidity | % | Sent to API |
| Inside humidity | % | Display only |
| Barometric pressure | hPa | Sent to API; inHg retained for display |
| Wind speed (instantaneous) | m/s | Decomposed into U/V |
| Wind direction | degrees (meteorological) | 0 = calm |
| Wind U component (eastward) | m/s | Sent to API |
| Wind V component (northward) | m/s | Sent to API |
| Wind 10-min average | m/s | Used as gust proxy (LOOP1 has no true gust) |
| Year-to-date rainfall | mm | Cumulative; converted from clicks via `--bucket` |

Wind U/V follow standard meteorological convention: direction is **from**,
so a North wind (360°) gives `U=0, V=-speed`.

## Requirements

- A Davis WeatherLink data logger connected to a serial / USB-serial port
- A Voria2 station API key (`vsk_…`) — not required for `--dry-run`

## Usage

### Test the serial connection (dry-run)

Connects to the console, reads one LOOP packet, prints all values and exits.
No API key or database needed.

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

Stop cleanly with **Ctrl+C** or `SIGTERM`.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--port` | *(required)* | Serial port, e.g. `/dev/ttyUSB0` or `COM3` |
| `--api-url` | *(required in daemon mode)* | Voria2 API base URL, e.g. `https://api.voria2.io` |
| `--api-key` | *(required in daemon mode)* | Voria2 station API key (`vsk_…`) |
| `--dry-run` | `false` | Read one packet, print values and exit (no upload, no DB) |
| `--bucket` | `0.2mm` | Rain bucket size per click: `0.2mm` (metric) or `0.01in` (imperial/US) |
| `--interval` | `2m` | Poll interval as a Go duration: `30s`, `2m`, `5m`, … |
| `--db` | `./weather.db` | SQLite offline buffer path |
| `--baud` | `19200` | Baud rate (Davis default is 19200) |

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
| `wind` | wind | U/V components + 10-min avg as gust (m/s) |
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

## Protocol notes

1. **Wake-up** — sends `\n`, waits up to 1.2 s for `\n\r` response; retries 3×.
   Automatically re-attempted if a fetch fails mid-run.
2. **LOOP command** — sends `LOOP 1\n`, waits for ACK (`0x06`), reads 99-byte packet.
3. **CRC** — Davis CRC-16 (polynomial `0x1021`) over all 99 bytes; result must be zero.

## LOOP packet offsets used

```
 7  uint16 LE  Barometer          (inHg / 1000)
 9  int16  LE  Inside temp        (tenths °F)
11  uint8      Inside humidity    (%)
12  int16  LE  Outside temp       (tenths °F)
14  uint8      Wind speed         (mph, instantaneous)
15  uint8      Wind speed 10-min  (mph) ← used as gust proxy
16  uint16 LE  Wind direction     (1–360 °, 0 = calm)
33  uint8      Outside humidity   (%)
54  uint16 LE  Year rain          (clicks; multiply by bucket size for mm)
```
