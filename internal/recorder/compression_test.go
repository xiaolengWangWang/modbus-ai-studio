package recorder

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math/rand"
	"testing"
)

func TestEncodeDataCompressesAndRestoresLargePayload(t *testing.T) {
	data := bytes.Repeat([]byte("Modbus 故障分析：连接恢复后重新读取寄存器。\n"), 20000)
	original := bytes.Clone(data)
	encoded, codec, err := encodeData(data)
	if err != nil {
		t.Fatal(err)
	}
	if codec != codecZlib || len(encoded) >= len(data)/4 {
		t.Fatalf("repetitive payload was not compressed: codec=%d, stored=%d, raw=%d", codec, len(encoded), len(data))
	}
	decoded, err := decodeData(encoded, codec)
	if err != nil || !bytes.Equal(decoded, original) {
		t.Fatalf("payload did not round trip: decoded=%d, err=%v", len(decoded), err)
	}
	if !bytes.Equal(data, original) {
		t.Fatal("encoding changed the caller's payload")
	}
}

func TestEncodeDataKeepsSmallAndIncompressiblePayloadsPlain(t *testing.T) {
	random := make([]byte, 4096)
	if _, err := rand.New(rand.NewSource(42)).Read(random); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"nil":            nil,
		"empty":          {},
		"modbus packet":  {0, 1, 0, 0, 0, 6, 1, 3, 0, 0, 0, 1},
		"small repeated": bytes.Repeat([]byte{'x'}, 127),
		"random":         random,
	} {
		t.Run(name, func(t *testing.T) {
			encoded, codec, err := encodeData(data)
			if err != nil || codec != codecPlain || !bytes.Equal(encoded, data) {
				t.Fatalf("plain payload changed: codec=%d, err=%v", codec, err)
			}
			if (encoded == nil) != (data == nil) {
				t.Fatal("encoding changed nil/empty distinction")
			}
			decoded, err := decodeData(encoded, codec)
			if err != nil || !bytes.Equal(decoded, data) || (decoded == nil) != (data == nil) {
				t.Fatalf("plain payload did not round trip: err=%v", err)
			}
		})
	}
}

func TestDecodeDataDoesNotGuessCodecForLegacyPayloads(t *testing.T) {
	data := compressionFixture(t, []byte("legacy raw bytes that happen to be a zlib stream"))
	decoded, err := decodeData(data, codecPlain)
	if err != nil || !bytes.Equal(decoded, data) {
		t.Fatalf("legacy payload was transformed: err=%v", err)
	}
}

func TestDecodeDataRejectsUnknownCodec(t *testing.T) {
	for _, codec := range []int{-1, 2, 255} {
		if decoded, err := decodeData([]byte("data"), codec); err == nil || decoded != nil {
			t.Fatalf("codec %d was accepted: decoded=%q, err=%v", codec, decoded, err)
		}
	}
}

func TestDecodeDataRejectsDamagedCompressedPayload(t *testing.T) {
	valid := compressionFixture(t, bytes.Repeat([]byte("packet analysis"), 100))
	badChecksum := bytes.Clone(valid)
	badChecksum[len(badChecksum)-1] ^= 0xff
	for name, data := range map[string][]byte{
		"nil":          nil,
		"empty":        {},
		"invalid":      []byte("not a zlib stream"),
		"truncated":    valid[:len(valid)-2],
		"bad checksum": badChecksum,
	} {
		t.Run(name, func(t *testing.T) {
			if decoded, err := decodeData(data, codecZlib); err == nil || decoded != nil {
				t.Fatalf("damaged payload was accepted: decoded=%d, err=%v", len(decoded), err)
			}
		})
	}
}

func TestDecodeDataEnforcesExpandedSizeLimit(t *testing.T) {
	const limit = 1024
	for _, size := range []int{0, limit - 1, limit, limit + 1, limit * 16} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := bytes.Repeat([]byte{'x'}, size)
			encoded := compressionFixture(t, data)
			decoded, err := decodeDataLimit(encoded, codecZlib, limit)
			if size > limit {
				if err == nil || decoded != nil {
					t.Fatalf("expanded payload exceeded limit: decoded=%d, err=%v", len(decoded), err)
				}
			} else if err != nil || !bytes.Equal(decoded, data) {
				t.Fatalf("payload within limit was rejected: decoded=%d, err=%v", len(decoded), err)
			}
		})
	}
	legacy := bytes.Repeat([]byte{'x'}, limit+1)
	if decoded, err := decodeDataLimit(legacy, codecPlain, limit); err != nil || !bytes.Equal(decoded, legacy) {
		t.Fatalf("size limit changed legacy plain payload: err=%v", err)
	}
}

func TestEncodeDataOutputSurvivesWriterReuse(t *testing.T) {
	first := bytes.Repeat([]byte("first payload"), 1024)
	stored, codec, err := encodeData(first)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, _, err := encodeData(bytes.Repeat([]byte{byte(i)}, 4096)); err != nil {
			t.Fatal(err)
		}
	}
	decoded, err := decodeData(stored, codec)
	if err != nil || !bytes.Equal(decoded, first) {
		t.Fatalf("writer reuse changed an earlier payload: err=%v", err)
	}
}

func TestEncodeDataKeepsPayloadBeyondDecodeLimitPlain(t *testing.T) {
	data := make([]byte, (64<<20)+1)
	data[0], data[len(data)-1] = 1, 2
	encoded, codec, err := encodeData(data)
	if err != nil || codec != codecPlain || !bytes.Equal(encoded, data) {
		t.Fatalf("oversized payload must remain readable without decompression: codec=%d, err=%v", codec, err)
	}
}

func TestCompressionSupportsConcurrentCalls(t *testing.T) {
	for i := 0; i < 16; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			data := bytes.Repeat([]byte(fmt.Sprintf("session %d register analysis\n", i)), 300)
			for attempt := 0; attempt < 10; attempt++ {
				encoded, codec, err := encodeData(data)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := decodeData(encoded, codec)
				if err != nil || !bytes.Equal(decoded, data) {
					t.Fatalf("concurrent payload was corrupted: err=%v", err)
				}
			}
		})
	}
}

func compressionFixture(tb testing.TB, data []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		tb.Fatal(err)
	}
	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

var benchmarkCompressionData []byte

func BenchmarkEncodeData(b *testing.B) {
	randomPacket := make([]byte, 260)
	if _, err := rand.New(rand.NewSource(42)).Read(randomPacket); err != nil {
		b.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"packet_12B":    {0, 1, 0, 0, 0, 6, 1, 3, 0, 0, 0, 1},
		"packet_260B":   randomPacket,
		"analysis_4KiB": bytes.Repeat([]byte("Modbus register analysis; "), 160),
		"payload_1MiB":  bytes.Repeat([]byte{'x'}, 1<<20),
	} {
		b.Run(name, func(b *testing.B) {
			encoded, _, err := encodeData(data)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for b.Loop() {
				benchmarkCompressionData, _, err = encodeData(data)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(encoded))/float64(len(data)), "stored/raw")
		})
	}
}

func BenchmarkDecodeData(b *testing.B) {
	for name, data := range map[string][]byte{
		"packet_12B":    {0, 1, 0, 0, 0, 6, 1, 3, 0, 0, 0, 1},
		"analysis_4KiB": bytes.Repeat([]byte("Modbus register analysis; "), 160),
		"payload_1MiB":  bytes.Repeat([]byte{'x'}, 1<<20),
	} {
		b.Run(name, func(b *testing.B) {
			encoded, codec, err := encodeData(data)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for b.Loop() {
				benchmarkCompressionData, err = decodeData(encoded, codec)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
