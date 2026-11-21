package osm2mysql

import (
	"bufio"
	"container/heap"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// extSortFanIn caps how many run files are merged in a single k-way pass, so
// the merge never needs an unbounded number of open files / buffers. Set high
// enough that planet-scale sorts (~1100+ runs at -sort-mem 8 GB) merge in a
// single pass, avoiding the extra transient disk + I/O of intermediate fold
// runs (which would briefly ~double the spill, dangerous on a tight disk).
// At 1 MB per run reader that's ~2 GB of merge buffers at full fan-in.
// If a sort still produces more runs than this, merge() folds in extra passes.
const extSortFanIn = 2048

// ExtSorter is a bounded-memory external merge sort over fixed-size binary
// records. Records are accumulated into in-RAM buffers; when a buffer fills it
// is sorted and flushed to a "run" file on disk. merge() then performs a
// (possibly multi-pass) k-way merge of the runs, deleting each run as it is
// consumed so transient disk use falls as the merge proceeds.
//
// It is generic over the record bytes: the caller supplies recSize and a
// comparator over raw record slices. The same type is used in pass 2 keyed by
// node_id and then by (way_id, seq).
//
// Concurrency: the run registry is mutex-guarded, but record ingest goes
// through per-shard single-producer writers (see concurrentAdder) so the hot
// extraction path stays parallel; only run-file registration is locked.
type ExtSorter struct {
	dir         string
	prefix      string
	recSize     int
	keyBytes    []int // record byte offsets, least-significant first (radix order)
	less        func(a, b []byte) bool
	perBufBytes int // per-writer in-RAM buffer size (multiple of recSize)
	logf        func(format string, args ...any)

	mu     sync.Mutex
	runs   []string
	runSeq int
	err    error // first write/close failure; see Err
}

// newExtSorter creates a sorter spilling into dir. memBudget is the total
// in-RAM budget for run generation, split across `shards` writers. keyBytes
// lists the record's key byte offsets from least- to most-significant; the
// run-generation radix sort uses it, and `less` must impose the same order
// (it drives the k-way merge).
func newExtSorter(dir, prefix string, recSize int, memBudget int64, shards int, keyBytes []int, less func(a, b []byte) bool) *ExtSorter {
	if shards < 1 {
		shards = 1
	}
	// Halve the budget: the radix sort needs a ping-pong scratch buffer the
	// same size as the record buffer.
	perRecs := memBudget / int64(shards) / 2 / int64(recSize)
	if perRecs < 1<<12 { // floor: keep runs >= any single way batch (~2000 refs)
		perRecs = 1 << 12
	}
	return &ExtSorter{
		dir:         dir,
		prefix:      prefix,
		recSize:     recSize,
		keyBytes:    keyBytes,
		less:        less,
		perBufBytes: int(perRecs) * recSize,
	}
}

// newRunFile creates a uniquely-named run file. It does NOT add it to the
// registry: callers that produce a primary run call registerRun afterwards;
// intermediate merge outputs are tracked by the merge loop itself.
func (s *ExtSorter) newRunFile() (string, *os.File, error) {
	s.mu.Lock()
	name := filepath.Join(s.dir, fmt.Sprintf("%s.run.%05d.bin", s.prefix, s.runSeq))
	s.runSeq++
	s.mu.Unlock()
	f, err := os.Create(name)
	if err != nil {
		return "", nil, err
	}
	return name, f, nil
}

func (s *ExtSorter) registerRun(name string) {
	s.mu.Lock()
	s.runs = append(s.runs, name)
	s.mu.Unlock()
}

// setErr records the first failure from a writer. Writers run inside parallel
// callbacks, so the error is stored and returned later by merge or Err.
func (s *ExtSorter) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Err reports the first failure recorded while writing or flushing runs.
func (s *ExtSorter) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// sortWriter is a single-producer ingest handle. Not safe for concurrent use;
// wrap with a lock (concurrentAdder does this) when feeding from parallel
// callbacks.
type sortWriter struct {
	s       *ExtSorter
	buf     []byte
	scratch []byte // radix ping-pong buffer, lazily sized to match a flush
}

func (s *ExtSorter) newWriter() *sortWriter {
	return &sortWriter{s: s, buf: make([]byte, 0, s.perBufBytes)}
}

// addBatch appends one or more concatenated records (a multiple of recSize).
// A way's node refs are emitted as a single batch so we lock once per way, not
// once per ref.
func (w *sortWriter) addBatch(recs []byte) {
	if len(recs) == 0 {
		return
	}
	if len(w.buf)+len(recs) > cap(w.buf) {
		w.flush()
	}
	if len(recs) > cap(w.buf) {
		// Batch larger than the whole buffer (huge way + tiny budget): write
		// it as its own sorted run directly.
		w.flushBytes(recs)
		return
	}
	w.buf = append(w.buf, recs...)
}

func (w *sortWriter) flush() {
	if len(w.buf) == 0 {
		return
	}
	w.flushBytes(w.buf)
	w.buf = w.buf[:0]
}

// flushBytes sorts the records in data with an LSD byte radix sort (stable
// counting sort per key byte, least-significant first) and writes the sorted
// run sequentially. O(n) in the record count: no comparisons, no per-flush
// index allocation.
func (w *sortWriter) flushBytes(data []byte) {
	rs := w.s.recSize
	n := len(data) / rs
	if n == 0 {
		return
	}
	if cap(w.scratch) < len(data) {
		w.scratch = make([]byte, len(data))
	}
	scratch := w.scratch[:len(data)]

	src, dst := data, scratch
	var count [256]int
	for _, b := range w.s.keyBytes {
		for i := range count {
			count[i] = 0
		}
		for i := 0; i < n; i++ {
			count[src[i*rs+b]]++
		}
		if count[src[b]] == n {
			continue
		}
		sum := 0
		for i := 0; i < 256; i++ {
			c := count[i]
			count[i] = sum
			sum += c
		}
		for i := 0; i < n; i++ {
			key := src[i*rs+b]
			pos := count[key]
			count[key]++
			copy(dst[pos*rs:(pos+1)*rs], src[i*rs:(i+1)*rs])
		}
		src, dst = dst, src
	}
	// After the passes, src holds the fully sorted records (it's data or
	// scratch depending on the number of non-constant key bytes).

	name, f, err := w.s.newRunFile()
	if err != nil {
		w.s.setErr(fmt.Errorf("create sort run: %w", err))
		return
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	if _, err := bw.Write(src[:n*rs]); err != nil {
		f.Close()
		os.Remove(name)
		w.s.setErr(fmt.Errorf("write sort run: %w", err))
		return
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(name)
		w.s.setErr(fmt.Errorf("flush sort run: %w", err))
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		w.s.setErr(fmt.Errorf("close sort run: %w", err))
		return
	}
	w.s.registerRun(name)
}

func (w *sortWriter) close() {
	w.flush()
	// Closed writers are never used again. Release both radix ping-pong
	// buffers before later pipeline stages allocate their own sort writers.
	w.buf = nil
	w.scratch = nil
}

// concurrentAdder fans parallel callbacks across `shards` single-producer
// writers, each guarded by its own mutex, picked round-robin. This keeps lock
// contention low (one lock per way, sharded) while preserving the parallelism
// that keeps extraction CPU-bound.
type concurrentAdder struct {
	writers []*sortWriter
	mus     []sync.Mutex
	rr      atomic.Uint32
}

func (s *ExtSorter) concurrentAdder(shards int) *concurrentAdder {
	if shards < 1 {
		shards = 1
	}
	c := &concurrentAdder{
		writers: make([]*sortWriter, shards),
		mus:     make([]sync.Mutex, shards),
	}
	for i := range c.writers {
		c.writers[i] = s.newWriter()
	}
	return c
}

func (c *concurrentAdder) addBatch(recs []byte) {
	i := int(c.rr.Add(1)) % len(c.writers)
	c.mus[i].Lock()
	c.writers[i].addBatch(recs)
	c.mus[i].Unlock()
}

func (c *concurrentAdder) close() {
	for _, w := range c.writers {
		w.close()
	}
}

// --- k-way merge ---

type runReader struct {
	f    *os.File
	br   *bufio.Reader
	path string
}

func openRunReader(path string) (*runReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &runReader{f: f, br: bufio.NewReaderSize(f, 1<<20), path: path}, nil
}

// advance reads the next record into dst. Returns ok=false at clean EOF.
func (r *runReader) advance(dst []byte) (bool, error) {
	_, err := io.ReadFull(r.br, dst)
	if err == io.EOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *runReader) close(remove bool) {
	r.f.Close()
	if remove {
		os.Remove(r.path)
	}
}

type mergeItem struct {
	rec []byte // owns recSize bytes
	rdr int    // index into readers
}

type mergeHeap struct {
	items []*mergeItem
	less  func(a, b []byte) bool
}

func (h *mergeHeap) Len() int           { return len(h.items) }
func (h *mergeHeap) Less(i, j int) bool { return h.less(h.items[i].rec, h.items[j].rec) }
func (h *mergeHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *mergeHeap) Push(x any)         { h.items = append(h.items, x.(*mergeItem)) }
func (h *mergeHeap) Pop() any {
	n := len(h.items)
	it := h.items[n-1]
	h.items = h.items[:n-1]
	return it
}

// mergeReader streams a globally sorted record stream over a set of run files,
// deleting each run as it is exhausted.
type mergeReader struct {
	recSize int
	readers []*runReader
	h       *mergeHeap
	out     []byte // stable buffer returned by next()
}

func (s *ExtSorter) openMergeOf(paths []string) (*mergeReader, error) {
	m := &mergeReader{
		recSize: s.recSize,
		readers: make([]*runReader, 0, len(paths)),
		h:       &mergeHeap{less: s.less},
		out:     make([]byte, s.recSize),
	}
	heap.Init(m.h)
	for _, p := range paths {
		r, err := openRunReader(p)
		if err != nil {
			m.closeAll()
			return nil, err
		}
		idx := len(m.readers)
		m.readers = append(m.readers, r)
		rec := make([]byte, s.recSize)
		ok, err := r.advance(rec)
		if err != nil {
			m.closeAll()
			return nil, err
		}
		if ok {
			heap.Push(m.h, &mergeItem{rec: rec, rdr: idx})
		} else {
			r.close(true)
		}
	}
	return m, nil
}

// next returns the next record in sorted order. The returned slice is valid
// only until the following call to next.
func (m *mergeReader) next() ([]byte, bool, error) {
	if m.h.Len() == 0 {
		return nil, false, nil
	}
	it := heap.Pop(m.h).(*mergeItem)
	copy(m.out, it.rec)
	r := m.readers[it.rdr]
	ok, err := r.advance(it.rec)
	if err != nil {
		return nil, false, err
	}
	if ok {
		heap.Push(m.h, it)
	} else {
		r.close(true)
	}
	return m.out, true, nil
}

func (m *mergeReader) closeAll() {
	for _, r := range m.readers {
		if r != nil {
			r.close(false)
		}
	}
}

// merge folds all runs down to a single sorted stream, using multiple k-way
// passes if there are more than extSortFanIn runs.
func (s *ExtSorter) merge() (*mergeReader, error) {
	if err := s.Err(); err != nil {
		return nil, err
	}
	runs := append([]string(nil), s.runs...)
	pass := 0
	for len(runs) > extSortFanIn {
		pass++
		if s.logf != nil {
			s.logf("extsort[%s]: merge pass %s folding %s runs", s.prefix, formatNumber(int64(pass)), formatNumber(int64(len(runs))))
		}
		var next []string
		for i := 0; i < len(runs); i += extSortFanIn {
			end := i + extSortFanIn
			if end > len(runs) {
				end = len(runs)
			}
			merged, err := s.mergeGroupToFile(runs[i:end])
			if err != nil {
				return nil, err
			}
			next = append(next, merged)
		}
		runs = next
	}
	return s.openMergeOf(runs)
}

// mergeGroupToFile merges a group of runs into one new run file (deleting the
// inputs as it goes) and returns the new path.
func (s *ExtSorter) mergeGroupToFile(group []string) (string, error) {
	mr, err := s.openMergeOf(group)
	if err != nil {
		return "", err
	}
	name, f, err := s.newRunFile()
	if err != nil {
		mr.closeAll()
		return "", err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	for {
		rec, ok, err := mr.next()
		if err != nil {
			f.Close()
			return "", err
		}
		if !ok {
			break
		}
		if _, err := bw.Write(rec); err != nil {
			f.Close()
			return "", err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	return name, nil
}
