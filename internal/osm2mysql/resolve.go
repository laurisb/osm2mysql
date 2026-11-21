package osm2mysql

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Record layouts (little-endian, fixed size). seq/refCount are uint16: OSM
// ways are capped at ~2000 nodes, well under 65535.
const (
	wnrefSize    = 18 // node_id u64 [0:8] | way_id u64 [8:16] | seq u16 [16:18]
	wayspanSize  = 11 // way_id u64 [0:8] | refCount u16 [8:10] | firstEqLast u8 [10]
	resolvedSize = 18 // way_id u64 [0:8] | seq u16 [8:10] | latE7 i32 [10:14] | lonE7 i32 [14:18]
)

// numWnrefBuckets partitions way-node refs into node-ID ranges for the join.
const numWnrefBuckets = 64

func addIDDelta(id, delta int64) (int64, bool) {
	if delta > 0 && id > math.MaxInt64-delta || delta < 0 && id < math.MinInt64-delta {
		return 0, false
	}
	return id + delta, true
}

// resolveResult carries the counters Parse needs for its final summary.
type resolveResult struct {
	centroidsWritten int64
	droppedWays      int64
	invalidWays      int64
}

func lessNodeID(a, b []byte) bool { // wnref, by node_id
	return binary.LittleEndian.Uint64(a[0:]) < binary.LittleEndian.Uint64(b[0:])
}

func lessWayID(a, b []byte) bool { // wayspan, by way_id
	return binary.LittleEndian.Uint64(a[0:]) < binary.LittleEndian.Uint64(b[0:])
}

func lessResolved(a, b []byte) bool { // resolved, by (way_id, seq)
	wa, wb := binary.LittleEndian.Uint64(a[0:]), binary.LittleEndian.Uint64(b[0:])
	if wa != wb {
		return wa < wb
	}
	return binary.LittleEndian.Uint16(a[8:]) < binary.LittleEndian.Uint16(b[8:])
}

// Radix key byte offsets, least-significant first. Each must impose the same
// order as the matching less* comparator above (little-endian integer keys).
var (
	keyNodeID   = []int{0, 1, 2, 3, 4, 5, 6, 7}
	keyWayID    = []int{0, 1, 2, 3, 4, 5, 6, 7}
	keyResolved = []int{8, 9, 0, 1, 2, 3, 4, 5, 6, 7}
)

// refExtractor collects selected way references and spans during discovery.
type refExtractor struct {
	wnrefBuckets []*ExtSorter       // one per node-id range
	wnrefAdders  []*concurrentAdder // parallel ingest, one per bucket
	bucketWidth  int64              // node ids per bucket

	wayspanSorter *ExtSorter
	wayspanAdder  *concurrentAdder
	refsSpilled   atomic.Int64
	maxWayID      atomic.Uint64
}

func (p *Parser) newRefExtractor(bucketCount int) *refExtractor {
	logf := func(format string, args ...any) { p.log.Log(fmt.Sprintf(format, args...)) }
	e := &refExtractor{
		wnrefBuckets: make([]*ExtSorter, bucketCount),
		wnrefAdders:  make([]*concurrentAdder, bucketCount),
	}
	// All buckets are live during extraction, so split the sort budget across
	// them. Each bucket sorts only its own node-id range.
	per := p.sortMem / int64(bucketCount)
	for b := 0; b < bucketCount; b++ {
		s := newExtSorter(p.tempDir, fmt.Sprintf("wnref%02d", b), wnrefSize, per, 1, keyNodeID, lessNodeID)
		e.wnrefBuckets[b] = s
		e.wnrefAdders[b] = s.concurrentAdder(1)
	}
	ws := newExtSorter(p.tempDir, "wayspan", wayspanSize, p.sortMem/4, p.threads, keyWayID, lessWayID)
	ws.logf = logf
	e.wayspanSorter = ws
	e.wayspanAdder = ws.concurrentAdder(p.threads)
	return e
}

// bucketOf maps a node id to its range bucket.
func (e *refExtractor) bucketOf(nodeID int64) int {
	if nodeID < 0 {
		return 0
	}
	b := nodeID / e.bucketWidth
	if b >= int64(len(e.wnrefBuckets)) {
		return len(e.wnrefBuckets) - 1
	}
	return int(b)
}

// wayPartition maps the observed way-id space into contiguous ranges. Keeping
// each worker in a range preserves ordered centroid writes while distributing
// the expensive resolved-vertex sorts across workers.
func wayPartition(wayID, maxWayID uint64, workers int) int {
	if workers <= 1 || maxWayID == 0 {
		return 0
	}
	hi, lo := bits.Mul64(wayID, uint64(workers))
	bucket, _ := bits.Div64(hi, lo, maxWayID+1)
	b := int(bucket)
	if b >= workers {
		return workers - 1
	}
	return b
}

// splitWaySpans turns the small, globally sorted span stream into one ordered
// file per Pass 3 range. Doing this before the centroid workers start avoids
// making higher ranges wait while a dispatcher drains every preceding range.
// At 11 bytes per way this extra sequential I/O is tiny beside resolved verts.
func splitWaySpans(m *mergeReader, dir string, maxWayID uint64, workers int) ([]string, error) {
	paths := make([]string, workers)
	files := make([]*os.File, workers)
	writers := make([]*bufio.Writer, workers)
	cleanup := func(remove bool) {
		for i, f := range files {
			if f != nil {
				f.Close()
			}
			if remove && paths[i] != "" {
				os.Remove(paths[i])
			}
		}
	}
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("wayspan.part.%02d.bin", i))
		f, err := os.Create(paths[i])
		if err != nil {
			cleanup(true)
			return nil, err
		}
		files[i] = f
		writers[i] = bufio.NewWriterSize(f, 1<<20)
	}
	for {
		rec, ok, err := m.next()
		if err != nil {
			cleanup(true)
			return nil, err
		}
		if !ok {
			break
		}
		wayID := binary.LittleEndian.Uint64(rec[0:])
		if _, err := writers[wayPartition(wayID, maxWayID, workers)].Write(rec); err != nil {
			cleanup(true)
			return nil, err
		}
	}
	for i, w := range writers {
		if err := w.Flush(); err != nil {
			cleanup(true)
			return nil, err
		}
		if err := files[i].Close(); err != nil {
			files[i] = nil
			cleanup(true)
			return nil, err
		}
		files[i] = nil
	}
	return paths, nil
}

// onWay spills one (node_id, way_id, seq) record per node ref, routed to the
// bucket for that node id, plus one (way_id, refCount, firstEqLast) span per
// way. Safe for concurrent callers.
func (e *refExtractor) onWay(w *Way) error {
	n := len(w.NodeIDs)
	if w.ID < 0 || n >= math.MaxUint16 {
		return fmt.Errorf("way %d exceeds supported ID or reference limit", w.ID)
	}
	if n > 0 {
		// Build this way's ref records contiguously, then ship them to buckets
		// in runs of consecutive same-bucket refs. A way's nodes almost always
		// share one node-id range, so this is ~one locked addBatch per way
		// rather than one per ref, which is what keeps extraction fast.
		batch := make([]byte, n*wnrefSize)
		wid := uint64(w.ID)
		for {
			old := e.maxWayID.Load()
			if wid <= old || e.maxWayID.CompareAndSwap(old, wid) {
				break
			}
		}
		for i, nid := range w.NodeIDs {
			off := i * wnrefSize
			binary.LittleEndian.PutUint64(batch[off:], uint64(nid))
			binary.LittleEndian.PutUint64(batch[off+8:], wid)
			binary.LittleEndian.PutUint16(batch[off+16:], uint16(i))
		}
		start, curBucket := 0, e.bucketOf(w.NodeIDs[0])
		for i := 1; i < n; i++ {
			if b := e.bucketOf(w.NodeIDs[i]); b != curBucket {
				e.wnrefAdders[curBucket].addBatch(batch[start*wnrefSize : i*wnrefSize])
				start, curBucket = i, b
			}
		}
		e.wnrefAdders[curBucket].addBatch(batch[start*wnrefSize : n*wnrefSize])
		e.refsSpilled.Add(int64(n))
	}
	var span [wayspanSize]byte
	binary.LittleEndian.PutUint64(span[0:], uint64(w.ID))
	binary.LittleEndian.PutUint16(span[8:], uint16(n))
	if n >= 1 && w.NodeIDs[0] == w.NodeIDs[n-1] {
		span[10] = 1
	}
	e.wayspanAdder.addBatch(span[:])
	return nil
}

// Err reports the first spill failure across the reference sorters.
func (e *refExtractor) Err() error {
	for _, s := range e.wnrefBuckets {
		if err := s.Err(); err != nil {
			return err
		}
	}
	return e.wayspanSorter.Err()
}

func (e *refExtractor) close() {
	for _, a := range e.wnrefAdders {
		a.close()
	}
	e.wayspanAdder.close()
}
