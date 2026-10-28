# daviscompanion

A Go daemon (`davisweather`) that reads current conditions from a
**Davis Vantage Pro / Pro2 / Vue** console via the WeatherLink serial interface
and continuously uploads measurements to the **Voria2 Ingest API**, with
automatic local buffering when offline (can be disabled with `--no-db` for a
fully stateless setup).

Runs on Linux, macOS and Windows directly, and is packaged for
**OpenWrt 25.12+** with a LuCI configuration page (see
[OpenWrt](#openwrt)).

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
- A Voria2 station API key (`vsk_…`) — not required for `--dry-run`

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

Stop cleanly with **Ctrl+C** or `SIGTERM`.

### Stateless mode (no database)

Add `--no-db` to any daemon-mode invocation to disable the SQLite buffer
entirely — no database file is opened or created. Each tick uploads the fresh
readings; if the API is unreachable the readings are **dropped** and the next
tick tries again with new data. This is the default mode on OpenWrt.

```bash
./davisweather \
  --port    /dev/ttyUSB0 \
  --api-url https://api.voria2.io \
  --api-key vsk_xxxxxxxxxxxxxxxxxxxx \
  --no-db
```

| Event | Behavior |
|---|---|
| Upload OK | Readings accepted, tick logged |
| Network error / HTTP 5xx | Readings dropped, warning logged, retried next tick with fresh data |
| HTTP 4xx (bad key, bad slug) | Readings dropped, warning logged |

## Flags

| Flag | Default | Description |
|---|---|---|
| `--port` | *(required)* | Serial port, e.g. `/dev/ttyUSB0` or `COM3` |
| `--api-url` | *(required)* | Voria2 API base URL, e.g. `https://api.voria2.io` |
| `--api-key` | *(required)* | Voria2 station API key (`vsk_…`) |
| `--version` | — | Print version and exit (version is injected at build time) |
| `--dry-run` | `false` | Read one packet, print values and exit (no upload, no DB) |
| `--bucket` | `0.2mm` | Rain bucket size per click: `0.2mm` (metric) or `0.01in` (imperial/US) |
| `--interval` | `2m` | Poll interval as a Go duration: `30s`, `2m`, `5m`, … |
| `--db` | `./weather.db` | SQLite offline buffer path (ignored with `--no-db`) |
| `--no-db` | `false` | Stateless mode: disable SQLite entirely; failed uploads are dropped and retried with fresh data on the next tick |
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
| `wind` | wind | U/V components + 10-min rolling gust (m/s) |
| `rain` | rain (cumulative) | Year-to-date rain (mm, bucket-adjusted) |

All timestamps are sent in **UTC** (`2006-01-02T15:04:05Z`).

## Offline buffering

*This section applies to default (buffered) mode. With `--no-db` there is no
buffer — failed uploads are dropped (see [Stateless mode](#stateless-mode-no-database)).*

When the API cannot be reached, each tick's readings (up to 5 rows) are stored in
the local SQLite database. On the next successful tick the daemon automatically
drains up to **995 buffered rows** alongside the current reading in a single bulk
request (API limit: 1000 items per call).

**Error classification:**

| Condition | Buffered? | Reason |
|---|---|---|
| Network error / HTTP 5xx | Yes — retry next tick | Transient |
| HTTP 401 / 403 | No | Bad API key won't self-heal |
| HTTP 422 | No | Unknown sensor slug is a config error |
| HTTP 200, item `ok: false` | Moved to dead-letter table | Permanent rejection |

The daemon **never needs to be restarted** to recover — the ticker runs regardless
of API health. The SQLite file persists across restarts; no buffered data is lost.

**Inspect the buffer:**

```bash
sqlite3 weather.db "SELECT count(*) FROM pending_measurements;"
sqlite3 weather.db "SELECT id, created_at, json_extract(payload,'$.sensor') FROM pending_measurements LIMIT 10;"
```

## OpenWrt

`davisweather` is packaged for **OpenWrt 25.12 and newer** (the apk-based
release; 24.10 with opkg is *not* supported because its Go toolchain is older
than this project requires). The packaging lives in [`openwrt/`](openwrt/):

- `davisweather` — the daemon, a UCI config (`/etc/config/davisweather`) and a
  procd init script with automatic restart on config changes
- `luci-app-davisweather` — a LuCI configuration page under **Services → Davis
  Weather** (client-side JS, no Lua)

### Install from GitHub Releases

Prebuilt `.apk` packages for `aarch64_generic` (Pi 3/4/5, most ARM64
routers) and `arm_cortex-a7_neon-vfpv4` (Pi 2, many ARM routers) are attached
to each [release](https://github.com/meteogargano/daviscompanion/releases);
more targets can be enabled in
[`.github/workflows/openwrt.yml`](.github/workflows/openwrt.yml). Download
the `.apk` for your architecture plus the LuCI app (arch-independent), copy
them to the router and install:

```sh
scp davisweather-0.6-r1-<arch>.apk luci-app-davisweather-0.6-r1-all.apk root@192.168.1.1:/tmp/
ssh root@192.168.1.1
apk add --allow-untrusted /tmp/davisweather-*.apk /tmp/luci-app-davisweather-*.apk
```

> The packages are unsigned (personal build), hence `--allow-untrusted`.

Requirements on the router:

- **Free flash space:** the Go binary is ~15 MB — budget ≥ 20 MB free
  (`df -h /overlay`). Not installable on 8/16 MB NOR devices without extroot.
- **USB serial driver** for your USB-serial adapter (the package suggests them,
  but does not force-install): `kmod-usb-serial-ftdi`, `kmod-usb-serial-pl2303`,
  `kmod-usb-serial-ch341` or `kmod-usb-serial-cp210x`
- Working NTP time sync (timestamps come from the system clock)

### Configure

Either via **LuCI → Services → Davis Weather**, or via UCI:

```sh
uci set davisweather.general.enabled='1'
uci set davisweather.general.port='/dev/ttyUSB0'
uci set davisweather.general.interval='5m'
uci set davisweather.general.bucket='0.01in'
uci set davisweather.voria2.api_url='https://api.voria2.io'
uci set davisweather.voria2.api_key='vsk_xxxxxxxxxxxxxxxxxxxx'
uci commit davisweather
service davisweather start
```

UCI options (all map 1:1 to daemon flags):

| Section | Option | Default | Description |
|---|---|---|---|
| `general` | `enabled` | `0` | Autostart the daemon |
| `general` | `port` | `/dev/ttyUSB0` | Serial device |
| `general` | `baud` | `19200` | Baud rate |
| `general` | `interval` | `2m` | Poll interval (Go duration) |
| `general` | `bucket` | `0.2mm` | `0.2mm` or `0.01in` |
| `general` | `buffering` | `none` | `none` (stateless, `--no-db`) or `sqlite` (`--db`) |
| `general` | `db_path` | `/etc/davisweather/weather.db` | SQLite path (with `buffering='sqlite'`) |
| `voria2` | `api_url` | `https://api.voria2.io` | API base URL |
| `voria2` | `api_key` | *(empty)* | `vsk_…` key — required to start |

Changing any option (LuCI **Save & Apply** or `uci commit`) automatically
restarts the service via its procd reload trigger.

**Logs:** `logread -e davisweather` (the daemon logs to syslog via procd).

Uninstall: `apk del luci-app-davisweather davisweather` — `/etc/config/davisweather`
is preserved as a conffile.

### Building the packages yourself

Use the [OpenWrt SDK](https://downloads.openwrt.org/releases/25.12.0/) for your
target and add this repository's `openwrt/` directory as a feed:

```sh
git clone https://github.com/meteogargano/daviscompanion.git
cd /path/to/sdk

echo "src-link davis /path/to/daviscompanion/openwrt" >> feeds.conf
./scripts/feeds update davis
./scripts/feeds install -a -p davis

make menuconfig   # Utilities -> davisweather, LuCI -> Applications -> luci-app-davisweather
make package/davisweather/compile V=s
make package/luci-app-davisweather/compile V=s
```

Binaries land in `bin/packages/<arch>/davis/`.

## Building from source

```bash
go build -o davisweather .
./davisweather --version   # "dev" unless built with -ldflags "-X main.version=…"
```

Cross-compile for Raspberry Pi (armv7 / arm64):

```bash
./compile-pi.sh   # writes dist/davisweather-armv7l and dist/davisweather-aarch64
```

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

## Changelog

- **v0.6** — MIT license; `--version` flag; OpenWrt 25.12 packaging with LuCI
  app; **removed the legacy API mode and `legacyserver`** (preserved on the
  `legacy` branch)
- **v0.1–v0.5** — serial protocol hardening, offline buffer with dead-letter
  queue, `--no-db` stateless mode, backlog replay hardening

## License

[MIT](LICENSE) © meteogargano
