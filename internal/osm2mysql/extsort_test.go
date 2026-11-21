package osm2mysql

import (
	"encoding/binary"
	"math/rand"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

func BenchmarkRadixWayNodeRefs(b *testing.B) {
	const n = 250000
	template := make([]byte, n*wnrefSize)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint64(template[i*wnrefSize:], rng.Uint64()&((1<<34)-1))
		binary.LittleEndian.PutUint64(template[i*wnrefSize+8:], uint64(i))
	}
	dir := b.TempDir()
	sorter := newExtSorter(dir, "bench", wnrefSize, int64(len(template))*2, 1, keyNodeID, lessNodeID)
	writer := sorter.newWriter()
	data := make([]byte, len(template))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(data, template)
		writer.flushBytes(data)
	}
}

func BenchmarkConcurrentAdder(b *testing.B) {
	dir := b.TempDir()
	data := make([]byte, 200*wnrefSize)
	for i := 0; i < 200; i++ {
		binary.LittleEndian.PutUint64(data[i*wnrefSize:], uint64(200-i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := newExtSorter(dir, "concurrent", wnrefSize, 4096*wnrefSize*2, 1, keyNodeID, lessNodeID)
		adder := s.concurrentAdder(1)
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < 500; k++ {
					adder.addBatch(data)
				}
			}()
		}
		wg.Wait()
		adder.close()
	}
}

func TestRadixConstantPasses(t *testing.T) {
	for _, keys := range [][]uint64{
		{5, 5, 5},
		{2, 1, 2, 0},
		{0x102, 0x201, 0x101, 0x102},
		{0x10002, 0x20101, 0x10101, 0x10002},
	} {
		s := newExtSorter(t.TempDir(), "radix", 18, 4096*18*2, 1, keyNodeID, lessNodeID)
		w := s.newWriter()
		expected := make([]int, len(keys))
		for i, k := range keys {
			var rec [18]byte
			putKey(rec[:], k, i)
			w.addBatch(rec[:])
			expected[i] = i
		}
		sort.SliceStable(expected, func(i, j int) bool { return keys[expected[i]] < keys[expected[j]] })
		w.close()
		reader, err := s.merge()
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range expected {
			rec, ok, err := reader.next()
			if err != nil || !ok {
				t.Fatalf("read: %v, %v", ok, err)
			}
			if got := binary.LittleEndian.Uint64(rec[8:]); got != uint64(want) {
				t.Fatalf("keys %v: got payload %d, want %d", keys, got, want)
			}
		}
		if _, ok, err := reader.next(); ok || err != nil {
			t.Fatalf("expected end of stream, got %v, %v", ok, err)
		}
	}
}

// putKey writes an 18-byte record whose first 8 bytes are the sort key.
func putKey(rec []byte, key uint64, i int) {
	binary.LittleEndian.PutUint64(rec[0:], key)
	binary.LittleEndian.PutUint64(rec[8:], uint64(i))
	binary.LittleEndian.PutUint16(rec[16:], uint16(i))
}

// TestExtSorterManyRunsMultiPass feeds enough records, with a tiny memory
// budget, to force many runs and a multi-pass (fan-in > extSortFanIn) merge.
// Verifies global ordering, exact count, and that all run files are deleted.
func TestExtSorterManyRunsMultiPass(t *testing.T) {
	dir := t.TempDir()
	const recSize = 18
	// Tiny budget over 600k records, producing many runs > extSortFanIn (multi-pass).
	s := newExtSorter(dir, "t", recSize, 4096*recSize, 1, keyNodeID, lessNodeID)
	w := s.newWriter()

	const N = 600000
	rng := rand.New(rand.NewSource(1))
	keys := make([]uint64, N)
	var rec [recSize]byte
	for i := 0; i < N; i++ {
		k := rng.Uint64() >> 1 // keep within positive int64 range
		keys[i] = k
		putKey(rec[:], k, i)
		w.addBatch(rec[:])
	}
	w.close()
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	mr, err := s.merge()
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	got := 0
	var prev uint64
	for {
		r, ok, err := mr.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		k := binary.LittleEndian.Uint64(r[0:])
		if got > 0 && k < prev {
			t.Fatalf("out of order at %d: %d < %d", got, k, prev)
		}
		if k != keys[got] {
			t.Fatalf("key mismatch at %d: got %d want %d", got, k, keys[got])
		}
		prev = k
		got++
	}
	if got != N {
		t.Fatalf("count: got %d want %d", got, N)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "t.run.*.bin"))
	if len(files) != 0 {
		t.Fatalf("run files not cleaned up: %v", files)
	}
}

// TestExtSorterConcurrent feeds the sorter from multiple goroutines via a
// concurrentAdder and verifies the merged stream is globally sorted with the
// right count.
func TestExtSorterConcurrent(t *testing.T) {
	dir := t.TempDir()
	const recSize = 18
	s := newExtSorter(dir, "c", recSize, 4096*recSize*8, 8, keyNodeID, lessNodeID)
	ca := s.concurrentAdder(8)

	const G, per = 8, 50000
	var wg sync.WaitGroup
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed)))
			var rec [recSize]byte
			for i := 0; i < per; i++ {
				putKey(rec[:], rng.Uint64()>>1, i)
				ca.addBatch(rec[:])
			}
		}(g)
	}
	wg.Wait()
	ca.close()

	mr, err := s.merge()
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	got := 0
	var prev uint64
	for {
		r, ok, err := mr.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		k := binary.LittleEndian.Uint64(r[0:])
		if got > 0 && k < prev {
			t.Fatalf("out of order at %d", got)
		}
		prev = k
		got++
	}
	if got != G*per {
		t.Fatalf("count: got %d want %d", got, G*per)
	}
}

// TestExtSorterCompositeKey verifies the radix sort honours a composite key
// (primary way_id, secondary seq), the resolved-record layout used in Pass 3.
func TestExtSorterCompositeKey(t *testing.T) {
	dir := t.TempDir()
	const recSize = resolvedSize // way_id u64 | seq u16 | lat i32 | lon i32
	s := newExtSorter(dir, "k", recSize, 4096*recSize, 1, keyResolved, lessResolved)
	w := s.newWriter()

	type wk struct {
		way uint64
		seq uint16
	}
	const N = 200000
	rng := rand.New(rand.NewSource(7))
	want := make([]wk, 0, N)
	var rec [recSize]byte
	for i := 0; i < N; i++ {
		way := uint64(rng.Intn(128))<<56 | uint64(rng.Intn(256))
		seq := uint16(rng.Intn(2000))
		want = append(want, wk{way, seq})
		binary.LittleEndian.PutUint64(rec[0:], way)
		binary.LittleEndian.PutUint16(rec[8:], seq)
		w.addBatch(rec[:])
	}
	w.close()
	sort.Slice(want, func(i, j int) bool {
		if want[i].way != want[j].way {
			return want[i].way < want[j].way
		}
		return want[i].seq < want[j].seq
	})

	mr, err := s.merge()
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	for i := 0; ; i++ {
		r, ok, err := mr.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			if i != N {
				t.Fatalf("count: got %d want %d", i, N)
			}
			break
		}
		way := binary.LittleEndian.Uint64(r[0:])
		seq := binary.LittleEndian.Uint16(r[8:])
		if way != want[i].way || seq != want[i].seq {
			t.Fatalf("at %d: got (%d,%d) want (%d,%d)", i, way, seq, want[i].way, want[i].seq)
		}
	}
}

// TestExtSorterEmpty verifies an empty sorter yields an empty stream.
func TestExtSorterEmpty(t *testing.T) {
	dir := t.TempDir()
	s := newExtSorter(dir, "e", 18, 4096*18, 1, keyNodeID, lessNodeID)
	s.newWriter().close()
	mr, err := s.merge()
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, ok, err := mr.next(); ok || err != nil {
		t.Fatalf("expected empty stream, got ok=%v err=%v", ok, err)
	}
}
