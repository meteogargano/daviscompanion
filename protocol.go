// protocol.go – Davis Vantage Pro serial protocol implementation.
//
// Protocol reference: Davis Instruments Serial Communication Reference Manual
// Rev 2.6.1 (March 29, 2013).
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"time"

	"go.bug.st/serial"
)

// ─────────────────────────────────────────────────────────────────────────────
// Davis CRC-16 (polynomial 0x1021, CCITT variant used by Davis)
// Spec §XII – CRC calculation.
//
// Verification: compute the CRC over the entire 99-byte block (data + the two
// CRC bytes appended by the console). If the transmission was clean the result
// is zero.
// ─────────────────────────────────────────────────────────────────────────────

var crcTable [256]uint16

func init() {
	const poly = 0x1021
	for i := 0; i < 256; i++ {
		crc := uint16(i) << 8
		for j := 0; j < 8; j++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ poly
			} else {
				crc <<= 1
			}
		}
		crcTable[i] = crc
	}
}

// calcCRC computes the Davis CRC-16 over data. Pass the full 99-byte LOOP
// packet; the result must be zero for a valid packet.
func calcCRC(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc = crcTable[(crc>>8)^uint16(b)] ^ (crc << 8)
	}
	return crc
}

// ─────────────────────────────────────────────────────────────────────────────
// LOOP packet – offsets from spec §X.1 (Table: Contents of the LOOP packet)
// All multi-byte values are little-endian; CRC bytes are big-endian (MSB first).
//
// Offset  Size  Field
//
//	 0      3    "LOO" header
//	 3      1    BarTrend  (signed, °3-hr trend)
//	 4      1    PacketType (0 = LOOP1, 1 = LOOP2)
//	 5      2    NextRecord
//	 7      2    Barometer         uint16 LE  (in Hg / 1000)
//	 9      2    InsideTemperature int16  LE  (tenths of °F)
//	11      1    InsideHumidity    uint8      (%)
//	12      2    OutsideTemperature int16 LE  (tenths of °F)
//	14      1    WindSpeed         uint8      (mph)
//	15      1    WindSpeed10Min    uint8      (mph)
//	16      2    WindDirection     uint16 LE  (1–360 °, 0 = no data/calm)
//	18      7    ExtraTemperatures
//	25      4    SoilTemperatures
//	29      4    LeafTemperatures
//	33      1    OutsideHumidity   uint8      (%)
//	34      7    ExtraHumidities
//	41      2    RainRate          uint16 LE  (rain clicks / hour)
//	43      1    UV                uint8      (UV index)
//	44      2    SolarRadiation    uint16 LE  (W/m²)
//	46      2    StormRain         uint16 LE  (rain clicks)
//	48      2    StartDateStorm    uint16 LE
//	50      2    RainDay           uint16 LE  (rain clicks, 0.2 mm each)
//	52      2    RainMonth         uint16 LE  (rain clicks)
//	54      2    RainYear          uint16 LE  (rain clicks)  ← we read this
//	56      2    ETDay             uint16 LE
//	58      2    ETMonth           uint16 LE
//	60      2    ETYear            uint16 LE
//	62      4    SoilMoistures
//	66      4    LeafWetness
//	70      2    InsideAlarms
//	72      1    RainAlarms
//	73      2    OutsideAlarms
//	75      2    OutsideHumAlarms
//	77      8    ExtraTempHumAlarms
//	85      4    SoilLeafAlarms
//	89      1    TransmitterBatteryStatus
//	90      2    ConsoleBatteryVoltage     uint16 LE  ((raw * 300) / 512 / 100 V)
//	92      1    ForecastIcons
//	93      1    ForecastRuleNumber
//	94      2    Sunrise           uint16 LE  (hour * 100 + minute)
//	96      2    Sunset            uint16 LE  (hour * 100 + minute)
//	98      1    '\n' (0x0A)  – end-of-record marker
//	          ── 99 bytes total; CRC is verified over ALL 99 bytes (result = 0)
// ─────────────────────────────────────────────────────────────────────────────

const loopPacketLen = 99

// LOOP1 packet field byte offsets (spec §X.1, PacketType byte 4 == 0).
const (
	offBarometer       = 7
	offInsideTemp      = 9
	offInsideHum       = 11
	offOutsideTemp     = 12
	offWindSpeed       = 14
	offWindSpeed10Min  = 15
	offWindDir         = 16
	offOutsideHum      = 33
	offRainRate        = 41
	offUV              = 43
	offSolarRad        = 44
	offRainDay         = 50
	offRainMonth       = 52
	offRainYear        = 54
	offConsoleBatt     = 90
	offForecastIcons   = 92
	offForecastRuleNum = 93
	offSunrise         = 94
	offSunset          = 96
)

// LOOP2 packet field byte offsets (spec §X.2, PacketType byte 4 == 1).
// Available on VP2 firmware ≥1.90 and Vantage Vue.
const (
	offL2GustSpeed10Min = 21 // uint16 LE, 1/10 mph — 10-min rolling gust
	offL2GustDir10Min   = 23 // uint16 LE, degrees
)

// gustSentinel is sent by the console when it has not yet accumulated a full
// 10-minute window (e.g. immediately after a power cycle).
const gustSentinel uint16 = 0x7FFF

// LOOPData holds the parsed, unit-converted fields we care about.
type LOOPData struct {
	// Temperature
	OutsideTempC float64 // °C
	OutsideTempF float64 // °F (raw, for reference)
	InsideTempC  float64 // °C
	InsideTempF  float64 // °F

	// Humidity
	OutsideHumidity uint8 // %
	InsideHumidity  uint8 // %

	// Pressure
	PressureHPa  float64 // hPa
	PressureInHg float64 // inHg (raw, for reference)

	// Wind – instantaneous
	WindSpeedMs  float64 // m/s  (instantaneous)
	WindSpeedMph float64 // mph  (raw)
	WindDir      uint16  // degrees met (0 = calm/no data, 1–360)
	// UV-decomposed wind components (meteorological convention)
	// U > 0 → wind blowing toward the east
	// V > 0 → wind blowing toward the north
	// Direction is where the wind is blowing FROM, so components are negated.
	WindU float64 // m/s  eastward
	WindV float64 // m/s  northward

	// Wind – 10-minute average (LOOP1; used as gust fallback when LOOP2 unavailable)
	WindSpeed10MinMs  float64 // m/s
	WindSpeed10MinMph float64 // mph (raw)

	// Wind – 10-minute rolling gust (from LOOP2).
	// HasGust is false when LOOP2 is absent or the console returns sentinel 0x7FFF
	// (i.e. the console has not yet accumulated a full 10-minute window).
	GustSpeedMs  float64
	GustSpeedMph float64
	GustDir      uint16  // meteorological degrees; 0 = calm/no data
	GustU        float64 // m/s eastward
	GustV        float64 // m/s northward
	HasGust      bool

	// Rain
	YearRainClicks uint16  // raw clicks  (0.2 mm per click)
	YearRainMM     float64 // mm
}

// parseLoop1 decodes a 99-byte LOOP1 packet (PacketType == 0).
func parseLoop1(raw []byte) (*LOOPData, error) {
	if len(raw) != loopPacketLen {
		return nil, fmt.Errorf("expected %d bytes, got %d", loopPacketLen, len(raw))
	}

	// ── Header check ────────────────────────────────────────────────────────
	if raw[0] != 'L' || raw[1] != 'O' || raw[2] != 'O' {
		return nil, fmt.Errorf("bad LOOP header: %02X %02X %02X", raw[0], raw[1], raw[2])
	}

	// ── Packet type check ───────────────────────────────────────────────────
	if raw[4] != 0 {
		return nil, fmt.Errorf("expected LOOP1 (type 0), got type %d", raw[4])
	}

	// ── CRC verification ────────────────────────────────────────────────────
	// Per spec §XII: compute CRC over the full block; valid result is 0.
	if crc := calcCRC(raw); crc != 0 {
		return nil, fmt.Errorf("CRC check failed (got 0x%04X, want 0x0000)", crc)
	}

	d := &LOOPData{}

	// ── Barometer ───────────────────────────────────────────────────────────
	// uint16 LE, units: in Hg / 1000
	baroRaw := binary.LittleEndian.Uint16(raw[offBarometer:])
	d.PressureInHg = float64(baroRaw) / 1000.0
	d.PressureHPa = inHgToHPa(d.PressureInHg)

	// ── Inside temperature ───────────────────────────────────────────────────
	// int16 LE, units: tenths of °F
	d.InsideTempF = float64(int16(binary.LittleEndian.Uint16(raw[offInsideTemp:]))) / 10.0
	d.InsideTempC = fToC(d.InsideTempF)

	// ── Inside humidity ──────────────────────────────────────────────────────
	d.InsideHumidity = raw[offInsideHum]

	// ── Outside temperature ──────────────────────────────────────────────────
	// int16 LE, units: tenths of °F
	d.OutsideTempF = float64(int16(binary.LittleEndian.Uint16(raw[offOutsideTemp:]))) / 10.0
	d.OutsideTempC = fToC(d.OutsideTempF)

	// ── Wind (instantaneous) ─────────────────────────────────────────────────
	// Speed: uint8, mph. 0 means calm or lost sync.
	d.WindSpeedMph = float64(raw[offWindSpeed])
	d.WindSpeedMs = mphToMs(d.WindSpeedMph)

	// Direction: uint16 LE, 1–360 °. 0 = calm / no data.
	d.WindDir = binary.LittleEndian.Uint16(raw[offWindDir:])

	// UV-decompose only when we have a direction.
	// Meteorological convention: direction is the bearing the wind is coming FROM.
	//   U = −speed · sin(dir)   (positive eastward)
	//   V = −speed · cos(dir)   (positive northward)
	if d.WindDir > 0 {
		dirRad := float64(d.WindDir) * math.Pi / 180.0
		d.WindU = -d.WindSpeedMs * math.Sin(dirRad)
		d.WindV = -d.WindSpeedMs * math.Cos(dirRad)
	}

	// ── Wind (10-minute average) – used as gust proxy ─────────────────────────
	// uint8, mph.
	d.WindSpeed10MinMph = float64(raw[offWindSpeed10Min])
	d.WindSpeed10MinMs = mphToMs(d.WindSpeed10MinMph)

	// ── Outside humidity ─────────────────────────────────────────────────────
	d.OutsideHumidity = raw[offOutsideHum]

	// ── Year rain ────────────────────────────────────────────────────────────
	// uint16 LE, units: rain clicks (0.2 mm per click).
	d.YearRainClicks = binary.LittleEndian.Uint16(raw[offRainYear:])
	d.YearRainMM = float64(d.YearRainClicks) * 0.2

	return d, nil
}

// parseLoop2 extracts the 10-minute rolling gust fields from a LOOP2 packet
// and merges them into d. Non-fatal: if the console has not yet accumulated
// 10 minutes of data it sends the sentinel 0x7FFF; we set HasGust=false in
// that case so callers can display "n/a" rather than a garbage value.
func parseLoop2(raw []byte, d *LOOPData) error {
	if len(raw) != loopPacketLen {
		return fmt.Errorf("expected %d bytes, got %d", loopPacketLen, len(raw))
	}
	if raw[0] != 'L' || raw[1] != 'O' || raw[2] != 'O' {
		return fmt.Errorf("bad LOOP header: %02X %02X %02X", raw[0], raw[1], raw[2])
	}
	if raw[4] != 1 {
		return fmt.Errorf("expected LOOP2 (type 1), got type %d", raw[4])
	}
	if crc := calcCRC(raw); crc != 0 {
		return fmt.Errorf("CRC check failed (got 0x%04X, want 0x0000)", crc)
	}

	gustRaw := binary.LittleEndian.Uint16(raw[offL2GustSpeed10Min:])
	if gustRaw == gustSentinel {
		d.HasGust = false
		return nil
	}

	// Gust speed is stored in 1/10 mph units.
	gustMph := float64(gustRaw) / 10.0
	d.GustSpeedMph = gustMph
	d.GustSpeedMs = mphToMs(gustMph)
	d.GustDir = binary.LittleEndian.Uint16(raw[offL2GustDir10Min:])
	if d.GustDir > 0 {
		rad := float64(d.GustDir) * math.Pi / 180.0
		d.GustU = -d.GustSpeedMs * math.Sin(rad)
		d.GustV = -d.GustSpeedMs * math.Cos(rad)
	}
	d.HasGust = true
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit conversion helpers
// ─────────────────────────────────────────────────────────────────────────────

func fToC(f float64) float64      { return (f - 32.0) * 5.0 / 9.0 }
func inHgToHPa(v float64) float64 { return v * 33.8639 }
func mphToMs(v float64) float64   { return v * 0.44704 }

// openSerialPort opens a serial port with the specified baud rate and configures
// it for Davis console communication (8N1 with read timeout).
func openSerialPort(portName string, baud int) (serial.Port, error) {
	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(portName, mode)
	if err != nil {
		return nil, err
	}
	if err := port.SetReadTimeout(200 * time.Millisecond); err != nil {
		port.Close()
		return nil, err
	}
	return port, nil
}

// wakeConsole performs the Davis console wake-up procedure (spec §V).
// It sends '\n' and waits up to 1.2 s for '\n\r'. Retries up to 3 times.
func wakeConsole(port serial.Port) error {
	buf := make([]byte, 64)
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := port.Write([]byte("\n")); err != nil {
			return fmt.Errorf("write wake byte: %w", err)
		}
		// The console may echo back other data before '\n\r'; scan up to
		// 1.2 s worth of bytes looking for the expected two-byte sequence.
		deadline := time.Now().Add(1200 * time.Millisecond)
		var acc []byte
		for time.Now().Before(deadline) {
			n, _ := port.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
				for i := 0; i < len(acc)-1; i++ {
					if acc[i] == '\n' && acc[i+1] == '\r' {
						return nil // awake
					}
				}
			}
		}
	}
	return fmt.Errorf("console did not respond to wake-up after 3 attempts")
}

// readFull reads exactly len(dst) bytes, accumulating partial reads.
// A per-call deadline prevents hanging forever.
func readFull(port serial.Port, dst []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	got := 0
	for got < len(dst) {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout: need %d bytes, got %d after %s",
				len(dst), got, timeout)
		}
		n, err := port.Read(dst[got:])
		got += n
		if err != nil && got < len(dst) {
			return fmt.Errorf("read error after %d/%d bytes: %w", got, len(dst), err)
		}
	}
	return nil
}

// fetchLPS sends "LPS 3 2\n" and reads one LOOP1 followed by one LOOP2 packet.
//
// LPS bitmask 3 = bit0 (LOOP1) | bit1 (LOOP2), count 2 = one of each.
// The console delivers them sequentially, ~2 s apart.
// Requires VP2 firmware ≥1.90 or Vantage Vue; returns NAK on older firmware.
//
// LOOP2 failure is non-fatal: if the read or parse fails, the function returns
// the valid LOOP1 data with HasGust=false and logs a warning.
func fetchLPS(port serial.Port) (*LOOPData, error) {
	if _, err := port.Write([]byte("LPS 3 2\n")); err != nil {
		return nil, fmt.Errorf("send LPS command: %w", err)
	}

	// Expect ACK (0x06).
	ack := make([]byte, 1)
	if err := readFull(port, ack, 3*time.Second); err != nil {
		return nil, fmt.Errorf("waiting for ACK: %w", err)
	}
	switch ack[0] {
	case 0x06:
		// good
	case 0x21:
		return nil, fmt.Errorf("console returned NAK (0x21) – LPS rejected, VP2 firmware ≥1.90 required")
	default:
		return nil, fmt.Errorf("unexpected response to LPS command: 0x%02X", ack[0])
	}

	// ── Packet 1: LOOP1 ───────────────────────────────────────────────────
	raw1 := make([]byte, loopPacketLen)
	if err := readFull(port, raw1, 5*time.Second); err != nil {
		return nil, fmt.Errorf("reading LOOP1: %w", err)
	}
	d, err := parseLoop1(raw1)
	if err != nil {
		return nil, err
	}
	log.Println("LOOP1 OK")

	// ── Packet 2: LOOP2 ───────────────────────────────────────────────────
	// Console sleeps ~2 s between packets; allow 6 s total.
	raw2 := make([]byte, loopPacketLen)
	if err := readFull(port, raw2, 6*time.Second); err != nil {
		log.Printf("warning: LOOP2 read failed: %v (gust will be unavailable)", err)
		return d, nil
	}
	if err := parseLoop2(raw2, d); err != nil {
		log.Printf("warning: LOOP2 parse failed: %v (gust will be unavailable)", err)
		return d, nil
	}
	log.Println("LOOP2 OK")

	return d, nil
}
