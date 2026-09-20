package logging

import (
	"bytes"
	"encoding/binary"
	"math"
	"sort"
)

// msgpack.go — a minimal MessagePack encoder.
//
// Fluent Bit's forward protocol is MessagePack over TCP. Encoding the handful of
// types a log record needs (map, array, string, int, bool, nil, float64) is
// about 100 lines, which is cheaper than adding a dependency to a project whose
// entire dependency list is one YAML parser.
//
// Only encoding is implemented — this process is a client of the forward
// protocol and never parses the ack packets, because chunk/ack framing is not
// used.

// msgpack type markers we emit.
const (
	mpNil   = 0xc0
	mpFalse = 0xc2
	mpTrue  = 0xc3

	mpInt8   = 0xd0
	mpInt16  = 0xd1
	mpInt32  = 0xd2
	mpInt64  = 0xd3
	mpUint8  = 0xcc
	mpUint16 = 0xcd
	mpUint32 = 0xce
	mpUint64 = 0xcf

	mpFloat64 = 0xcb

	mpStr8  = 0xd9
	mpStr16 = 0xda
	mpStr32 = 0xdb

	mpArray16 = 0xdc
	mpArray32 = 0xdd

	mpMap16 = 0xde
	mpMap32 = 0xdf
)

// packForwardMessage appends a Fluent Bit forward-protocol Message to buf.
//
// Message mode is the msgpack array [tag, time, record] where time is a Unix
// timestamp. Fluent Bit also accepts an EventTime extension type; the plain
// integer form is understood by every version that matters.
func packForwardMessage(buf *bytes.Buffer, tag string, unixTime int64, record map[string]interface{}) {
	packArrayHeader(buf, 3)
	packString(buf, tag)
	packInt(buf, unixTime)
	packValue(buf, record)
}

// packValue encodes any supported Go value. Unsupported types are encoded as
// their fmt-less zero value (nil) rather than panicking — a log line is never
// worth crashing the proxy over.
func packValue(buf *bytes.Buffer, v interface{}) {
	switch t := v.(type) {
	case nil:
		buf.WriteByte(mpNil)
	case bool:
		if t {
			buf.WriteByte(mpTrue)
		} else {
			buf.WriteByte(mpFalse)
		}
	case string:
		packString(buf, t)
	case int:
		packInt(buf, int64(t))
	case int64:
		packInt(buf, t)
	case int32:
		packInt(buf, int64(t))
	case int16:
		packInt(buf, int64(t))
	case int8:
		packInt(buf, int64(t))
	case uint:
		packUint(buf, uint64(t))
	case uint64:
		packUint(buf, t)
	case uint32:
		packUint(buf, uint64(t))
	case float64:
		packFloat64(buf, t)
	case float32:
		packFloat64(buf, float64(t))
	case map[string]interface{}:
		packMap(buf, t)
	case []interface{}:
		packArrayHeader(buf, len(t))
		for _, item := range t {
			packValue(buf, item)
		}
	default:
		buf.WriteByte(mpNil)
	}
}

func packMap(buf *bytes.Buffer, m map[string]interface{}) {
	packMapHeader(buf, len(m))
	// Sort keys so identical records produce identical bytes; this makes the
	// output stable in tests and in any downstream dedup.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		packString(buf, k)
		packValue(buf, m[k])
	}
}

func packArrayHeader(buf *bytes.Buffer, n int) {
	switch {
	case n < 16:
		buf.WriteByte(byte(0x90 | n))
	case n <= math.MaxUint16:
		buf.WriteByte(mpArray16)
		writeUint16(buf, uint16(n))
	default:
		buf.WriteByte(mpArray32)
		writeUint32(buf, uint32(n))
	}
}

func packMapHeader(buf *bytes.Buffer, n int) {
	switch {
	case n < 16:
		buf.WriteByte(byte(0x80 | n))
	case n <= math.MaxUint16:
		buf.WriteByte(mpMap16)
		writeUint16(buf, uint16(n))
	default:
		buf.WriteByte(mpMap32)
		writeUint32(buf, uint32(n))
	}
}

func packString(buf *bytes.Buffer, s string) {
	n := len(s)
	switch {
	case n < 32:
		buf.WriteByte(byte(0xa0 | n))
	case n <= math.MaxUint8:
		buf.WriteByte(mpStr8)
		buf.WriteByte(byte(n))
	case n <= math.MaxUint16:
		buf.WriteByte(mpStr16)
		writeUint16(buf, uint16(n))
	default:
		buf.WriteByte(mpStr32)
		writeUint32(buf, uint32(n))
	}
	buf.WriteString(s)
}

// packInt encodes a signed integer using the narrowest representation that
// preserves it.
func packInt(buf *bytes.Buffer, v int64) {
	switch {
	case v >= 0:
		packUint(buf, uint64(v))
	case v >= -32:
		buf.WriteByte(byte(v)) // negative fixint
	case v >= math.MinInt8:
		buf.WriteByte(mpInt8)
		buf.WriteByte(byte(int8(v)))
	case v >= math.MinInt16:
		buf.WriteByte(mpInt16)
		writeUint16(buf, uint16(int16(v)))
	case v >= math.MinInt32:
		buf.WriteByte(mpInt32)
		writeUint32(buf, uint32(int32(v)))
	default:
		buf.WriteByte(mpInt64)
		writeUint64(buf, uint64(v))
	}
}

// packUint encodes an unsigned integer using the narrowest representation.
// Values that fit a positive fixint (0..127) use the single-byte form.
func packUint(buf *bytes.Buffer, v uint64) {
	switch {
	case v < 128:
		buf.WriteByte(byte(v))
	case v <= math.MaxUint8:
		buf.WriteByte(mpUint8)
		buf.WriteByte(byte(v))
	case v <= math.MaxUint16:
		buf.WriteByte(mpUint16)
		writeUint16(buf, uint16(v))
	case v <= math.MaxUint32:
		buf.WriteByte(mpUint32)
		writeUint32(buf, uint32(v))
	default:
		buf.WriteByte(mpUint64)
		writeUint64(buf, v)
	}
}

func packFloat64(buf *bytes.Buffer, v float64) {
	buf.WriteByte(mpFloat64)
	writeUint64(buf, math.Float64bits(v))
}

func writeUint16(buf *bytes.Buffer, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	buf.Write(b[:])
}

func writeUint32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

func writeUint64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}
