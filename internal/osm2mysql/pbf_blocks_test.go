package osm2mysql

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func BenchmarkDecodeZlibBlob(b *testing.B) {
	raw := make([]byte, 256<<10)
	x := uint32(1)
	for i := range raw {
		x = x*1664525 + 1013904223
		raw[i] = byte(x >> 27)
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		b.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	blob := appendVarintField(nil, 2, uint64(len(raw)))
	blob = appendBytesField(blob, 3, compressed.Bytes())
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decoded, err := decodeBlob(blob)
		if err != nil || len(decoded) != len(raw) {
			b.Fatalf("decode: %d bytes, %v", len(decoded), err)
		}
	}
}

func TestDecodeZlibBlobSizeHints(t *testing.T) {
	raw := make([]byte, 7000)
	x := uint32(1)
	for i := range raw {
		x = x*1664525 + 1013904223
		raw[i] = byte(x >> 27)
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, size := range []uint64{0, 1, uint64(len(raw)), uint64(len(raw) * 2), 1 << 32} {
		blob := appendVarintField(nil, 2, size)
		blob = appendBytesField(blob, 3, compressed.Bytes())
		got, err := decodeBlob(blob)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("raw_size %d: decoded %d bytes, error %v", size, len(got), err)
		}
	}
	corrupt := append([]byte(nil), compressed.Bytes()...)
	corrupt[len(corrupt)-1] ^= 0xff
	blob := appendVarintField(nil, 2, uint64(len(raw)))
	blob = appendBytesField(blob, 3, corrupt)
	if _, err := decodeBlob(blob); err == nil {
		t.Fatal("corrupt zlib checksum accepted")
	}
}

func TestDecodeZlibCompatibility(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for _, size := range []int{0, 1, 100, 65536, 1 << 20} {
		raw := make([]byte, size)
		if _, err := rng.Read(raw); err != nil {
			t.Fatal(err)
		}
		for _, level := range []int{zlib.NoCompression, zlib.BestSpeed, zlib.DefaultCompression, zlib.BestCompression} {
			var compressed bytes.Buffer
			writer, err := zlib.NewWriterLevel(&compressed, level)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			blob := appendVarintField(nil, 2, uint64(len(raw)))
			blob = appendBytesField(blob, 3, compressed.Bytes())
			got, err := decodeBlob(blob)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("size=%d level=%d: decoded %d bytes, error %v", size, level, len(got), err)
			}
			for _, invalid := range [][]byte{compressed.Bytes()[:1], compressed.Bytes()[:len(compressed.Bytes())-1], {0, 1, 2}} {
				bad := appendBytesField(nil, 3, invalid)
				if _, err := decodeBlob(bad); err == nil {
					t.Fatalf("size=%d level=%d: accepted malformed stream", size, level)
				}
			}
		}
	}
}

func BenchmarkNodeRange(b *testing.B) {
	const n = 8000
	ids := make([]uint64, n)
	coordinates := make([]uint64, n)
	tags := make([]uint64, 0, n*3)
	for i := range ids {
		ids[i] = zigzag(1)
		coordinates[i] = zigzag(1)
		tags = append(tags, 1, 2, 0)
	}
	table := appendBytesField(nil, 1, nil)
	table = appendBytesField(table, 1, []byte("amenity"))
	table = appendBytesField(table, 1, []byte("cafe"))
	dense := appendBytesField(nil, 1, packedValues(ids...))
	dense = appendBytesField(dense, 8, packedValues(coordinates...))
	dense = appendBytesField(dense, 9, packedValues(coordinates...))
	dense = appendBytesField(dense, 10, packedValues(tags...))
	block := appendBytesField(nil, 1, table)
	block = appendBytesField(block, 2, appendBytesField(nil, 2, dense))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		minID, maxID, err := nodeRange(block)
		if err != nil || minID != 1 || maxID != n {
			b.Fatalf("nodeRange = %d, %d, %v", minID, maxID, err)
		}
	}
}

func TestNodeRangeMatchesFullDecode(t *testing.T) {
	node := appendVarintField(nil, 1, zigzag(9))
	node = appendVarintField(node, 8, zigzag(1))
	node = appendVarintField(node, 9, zigzag(1))
	dense := appendBytesField(nil, 1, packedValues(zigzag(4), zigzag(8), zigzag(-6)))
	dense = appendBytesField(dense, 8, packedValues(0, 0, 0))
	dense = appendBytesField(dense, 9, packedValues(0, 0, 0))
	group := appendBytesField(nil, 1, node)
	group = appendBytesField(group, 2, dense)
	for _, block := range [][]byte{nil, appendBytesField(nil, 2, group)} {
		minID, maxID, err := nodeRange(block)
		wantMin, wantMax, wantErr := parsePrimitiveBlock(block, nil, func(int64, int32, int32, map[string]string) {})
		if err != wantErr || minID != wantMin || maxID != wantMax {
			t.Fatalf("range = %d, %d, %v; full decode = %d, %d, %v", minID, maxID, err, wantMin, wantMax, wantErr)
		}
	}
}

func TestRangeOnlyDefersDenseFieldValidation(t *testing.T) {
	dense := appendBytesField(nil, 1, packedValues(zigzag(1)))
	dense = appendBytesField(dense, 8, []byte{0x80})
	block := appendBytesField(nil, 2, appendBytesField(nil, 2, dense))
	if _, _, err := nodeRange(block); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parsePrimitiveBlock(block, nil, func(int64, int32, int32, map[string]string) {}); err == nil {
		t.Fatal("node decode accepted malformed packed coordinates")
	}
}

func TestNodeBlockIndexRejectsUnsupportedIDs(t *testing.T) {
	for _, tc := range []struct {
		id      int64
		wantErr bool
	}{
		{0, false},
		{1<<34 - 1, false},
		{1 << 34, false},
		{math.MaxInt64, false},
		{-1, true},
	} {
		node := appendVarintField(nil, 1, zigzag(tc.id))
		node = appendVarintField(node, 8, 0)
		node = appendVarintField(node, 9, 0)
		block := appendBytesField(nil, 2, appendBytesField(nil, 1, node))
		data := addTestBlob(nil, "OSMData", block)
		path := filepath.Join(t.TempDir(), "node.osm.pbf")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := buildNodeBlockIndex(path, 1)
		if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "exceed supported range")) {
			t.Fatalf("node ID %d: expected range error, got %v", tc.id, err)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("node ID %d: %v", tc.id, err)
		}
	}
}

func TestNodeBlockIndexAndDecode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.osm.pbf")

	stringTable := appendBytesField(nil, 1, nil)
	stringTable = appendBytesField(stringTable, 1, []byte("amenity"))
	stringTable = appendBytesField(stringTable, 1, []byte("cafe"))
	dense := appendBytesField(nil, 1, packedValues(zigzag(100), zigzag(1), zigzag(2)))
	dense = appendBytesField(dense, 8, packedValues(zigzag(10), zigzag(1), zigzag(1)))
	dense = appendBytesField(dense, 9, packedValues(zigzag(20), zigzag(1), zigzag(1)))
	dense = appendBytesField(dense, 10, packedValues(1, 2, 0, 1, 2, 0, 1, 2, 0))
	group := appendBytesField(nil, 2, dense)
	primitive := appendBytesField(nil, 1, stringTable)
	primitive = appendBytesField(primitive, 2, group)
	primitive = appendVarintField(primitive, 17, 100)
	primitive = appendVarintField(primitive, 19, uint64(50000000000))
	primitive = appendVarintField(primitive, 20, uint64(10000000000))
	blob := appendBytesField(nil, 1, primitive)
	header := appendBytesField(nil, 1, []byte("OSMData"))
	header = appendVarintField(header, 3, uint64(len(blob)))

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(header)))
	if _, err := file.Write(length[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(header); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(blob); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	index, err := buildNodeBlockIndex(path, 2)
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	if len(index.blocks) != 1 || index.blocks[0].minID != 100 || index.blocks[0].maxID != 103 {
		t.Fatalf("index blocks = %#v", index.blocks)
	}

	var mu sync.Mutex
	var got []*Node
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, bounds := range [][2]int64{{100, 101}, {102, 103}} {
		wg.Add(1)
		go func(minID, maxID int64) {
			defer wg.Done()
			if err := index.decodeRange(path, minID, maxID, func(node *Node) {
				mu.Lock()
				got = append(got, node)
				mu.Unlock()
			}); err != nil {
				errs <- err
			}
		}(bounds[0], bounds[1])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("decode node range: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
	if len(got) != 3 || got[0].ID != 100 || got[1].ID != 101 || got[2].ID != 103 {
		t.Fatalf("decoded range IDs = %#v", got)
	}
	if math.Abs(got[0].Lat-50.000001) > 1e-12 || math.Abs(got[0].Lon-10.000002) > 1e-12 || got[0].Tags["amenity"] != "cafe" {
		t.Fatalf("decoded node = %#v", got[0])
	}
}

func appendBytesField(data []byte, number int, value []byte) []byte {
	data = protowire.AppendVarint(data, uint64(number<<3|2))
	data = protowire.AppendVarint(data, uint64(len(value)))
	return append(data, value...)
}

func appendVarintField(data []byte, number int, value uint64) []byte {
	data = protowire.AppendVarint(data, uint64(number<<3))
	return protowire.AppendVarint(data, value)
}

func packedValues(values ...uint64) []byte {
	var data []byte
	for _, value := range values {
		data = protowire.AppendVarint(data, value)
	}
	return data
}

func zigzag(value int64) uint64 {
	return uint64(value<<1) ^ uint64(value>>63)
}
