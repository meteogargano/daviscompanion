// protocol.go – Davis Vantage Pro serial protocol implementation.
//
// Protocol reference: Davis Instruments Serial Communication Reference Manual
// Rev 2.6.1 (March 29, 2013).
package main

import (
	"encoding/binary"
	"fmt"
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

// LOOP packet field byte offsets.
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

	// Wind – 10-minute average (used as gust proxy for API)
	WindSpeed10MinMs  float64 // m/s
	WindSpeed10MinMph float64 // mph (raw)

	// Rain
	YearRainClicks uint16  // raw clicks  (0.2 mm per click)
	YearRainMM     float64 // mm
}

// parseLoop decodes a 99-byte LOOP1 packet.
func parseLoop(raw []byte) (*LOOPData, error) {
	if len(raw) != loopPacketLen {
		return nil, fmt.Errorf("expected %d bytes, got %d", loopPacketLen, len(raw))
	}

	// ── Header check ────────────────────────────────────────────────────────
	if raw[0] != 'L' || raw[1] != 'O' || raw[2] != 'O' {
		return nil, fmt.Errorf("bad LOOP header: %02X %02X %02X", raw[0], raw[1], raw[2])
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

// ─────────────────────────────────────────────────────────────────────────────
// Unit conversion helpers
// ─────────────────────────────────────────────────────────────────────────────

func fToC(f float64) float64      { return (f - 32.0) * 5.0 / 9.0 }
func inHgToHPa(v float64) float64 { return v * 33.8639 }
func mphToMs(v float64) float64   { return v * 0.44704 }

// ─────────────────────────────────────────────────────────────────────────────
// Serial helpers
// ─────────────────────────────────────────────────────────────────────────────

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

// fetchLOOP sends "LOOP 1\n", waits for ACK, then reads the 99-byte packet.
func fetchLOOP(port serial.Port) (*LOOPData, error) {
	// Send command (single '\n' terminator – required for VP2).
	if _, err := port.Write([]byte("LOOP 1\n")); err != nil {
		return nil, fmt.Errorf("send LOOP command: %w", err)
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
		return nil, fmt.Errorf("console returned NAK (0x21) – bad command parameters")
	default:
		return nil, fmt.Errorf("unexpected response to LOOP command: 0x%02X", ack[0])
	}

	// Read exactly 99 bytes of packet data.
	raw := make([]byte, loopPacketLen)
	if err := readFull(port, raw, 5*time.Second); err != nil {
		return nil, fmt.Errorf("reading LOOP packet: %w", err)
	}

	return parseLoop(raw)
}
