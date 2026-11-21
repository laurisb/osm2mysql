package osm2mysql

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// parseRequiredNodes keeps the required-node pipeline bounded. Way
// discovery and node block decoding are parallel, while the ordered node
// consumer preserves the ID order needed by the sorted-reference merge.
func (p *Parser) parseRequiredNodes() error {
	start := time.Now()
	passStart := time.Now()
	p.log.Log("Parsing in 4 passes")
	p.log.Log("Pass 1: discover area references")

	layout, err := discoverBlockLayout(p.pbfFile)
	if err != nil {
		return fmt.Errorf("index PBF blocks: %w", err)
	}
	// Allocate the live sort buffers before the allocation-heavy node index
	// pass; otherwise it runs with a tiny GC heap and collects excessively.
	nodeWorkers := min(max(p.threads, 1), max(len(layout.nodes), 1))
	ext := p.newRefExtractor(max(numWnrefBuckets, nodeWorkers))
	p.log.Log("Indexing PBF node blocks")
	index, err := buildNodeBlockIndexFor(p.pbfFile, nodeWorkers, layout.nodes)
	if err != nil {
		return fmt.Errorf("index node blocks: %w", err)
	}
	p.log.Log(fmt.Sprintf("Indexed %s node blocks", formatNumber(int64(len(index.blocks)))))
	var maxNodeID int64
	if len(index.blocks) > 0 {
		maxNodeID = index.blocks[len(index.blocks)-1].maxID
	}
	ext.bucketWidth = int64(uint64(maxNodeID)/uint64(len(ext.wnrefBuckets)) + 1)
	spool, err := newAreaSpool(p)
	if err != nil {
		return fmt.Errorf("create area spool: %w", err)
	}
	spoolClosed := false
	defer func() {
		if !spoolClosed {
			_ = spool.file.Close()
		}
	}()
	matches := newSelectionMatches(p.selections)
	wayStats := wayScanStats{matches: matches}
	p.log.Log("Discovering selected way references")
	var discoveryMu sync.Mutex
	var discoveryErr error
	recordDiscoveryError := func(err error) {
		discoveryMu.Lock()
		if discoveryErr == nil {
			discoveryErr = err
		}
		discoveryMu.Unlock()
	}
	if err := decodeSelectedWays(p.pbfFile, p.threads, layout.ways, p.selections, &wayStats, func(w *Way) {
		if err := ext.onWay(w); err != nil {
			recordDiscoveryError(err)
			return
		}
		if err := spool.addWay(w); err != nil {
			recordDiscoveryError(err)
		}
	}); err != nil {
		return fmt.Errorf("way discovery failed: %w", err)
	}
	ext.close()
	if discoveryErr != nil {
		return discoveryErr
	}
	if err := ext.Err(); err != nil {
		return fmt.Errorf("sort selected way references: %w", err)
	}
	p.log.Log(fmt.Sprintf("Matching ways: open %s, closed %s (including area=no)",
		formatNumber(wayStats.open.Load()), formatNumber(wayStats.closed.Load())))
	p.log.Log(fmt.Sprintf("Reference discovery: %s refs", formatNumber(ext.refsSpilled.Load())))
	p.log.Log(fmt.Sprintf("Pass 1: complete in %s", formatDuration(time.Since(passStart))))

	maxWayID := ext.maxWayID.Load()
	resolvedSorters := make([]*ExtSorter, p.centroidWorkers)
	resolvedAdders := make([]*concurrentAdder, p.centroidWorkers)
	for i := range resolvedSorters {
		resolvedSorters[i] = newExtSorter(p.tempDir, fmt.Sprintf("required_resolved%02d", i), resolvedSize,
			p.sortMem/int64(p.centroidWorkers), nodeWorkers, keyResolved, lessResolved)
		resolvedAdders[i] = resolvedSorters[i].concurrentAdder(nodeWorkers)
	}
	joinedRefs := &atomic.Int64{}
	csvCh, csvDone := p.requiredCSV()
	csvClosed := false
	closeCSV := func() error {
		if csvClosed {
			return nil
		}
		csvClosed = true
		close(csvCh)
		return <-csvDone
	}
	// Producers always finish before an early return, so draining here keeps
	// the writer goroutine from leaking.
	defer func() {
		if !csvClosed {
			close(csvCh)
			if err := <-csvDone; err != nil {
				p.log.Log("WARNING: " + err.Error())
			}
		}
	}()
	var nodesScanned, nodePois, nodeTags atomic.Int64

	p.log.Log("Pass 2: decode nodes and join references")
	nodeStart := time.Now()
	ranges := balanceNodeBuckets(index, ext.bucketWidth, len(ext.wnrefBuckets), nodeWorkers)
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		var previous int64
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				current := nodesScanned.Load()
				rate := float64(current-previous) / 30
				p.log.Log(fmt.Sprintf("Node scan progress: %s nodes, %s refs joined, %s nodes/s",
					formatNumber(current), formatNumber(joinedRefs.Load()), formatNumber(int64(rate))))
				previous = current
			}
		}
	}()

	var nodeWG sync.WaitGroup
	workerErrors := make(chan error, nodeWorkers)
	for _, workerRange := range ranges {
		bucketStart, bucketEnd := workerRange.start, workerRange.end
		joiner := newRequiredNodeJoiner(ext, resolvedAdders, maxWayID, bucketStart, bucketEnd, joinedRefs)
		minNodeID := int64(bucketStart) * ext.bucketWidth
		maxNodeID := int64(math.MaxInt64)
		if bucketEnd != len(ext.wnrefBuckets) {
			maxNodeID = int64(bucketEnd)*ext.bucketWidth - 1
		}
		nodeWG.Add(1)
		go func(joiner *requiredNodeJoiner, minNodeID, maxNodeID int64) {
			defer nodeWG.Done()
			var lastNodeID int64
			var haveLastNodeID bool
			err := index.decodeSelectedRange(p.pbfFile, minNodeID, maxNodeID, p.selections, func(n *Node) {
				if joiner.err != nil {
					return
				}
				if haveLastNodeID && n.ID < lastNodeID {
					joiner.err = fmt.Errorf("node range requires ascending IDs: saw %d after %d", n.ID, lastNodeID)
					return
				}
				lastNodeID, haveLastNodeID = n.ID, true
				nodesScanned.Add(1)
				if err := joiner.resolveThrough(n.ID, n.Lat, n.Lon); err != nil {
					joiner.err = err
					return
				}
				if !p.selections.matches(n.Tags) {
					return
				}
				matches.record(n.Tags)
				encoded, tagCount := formatPOITagsJSON(n.Tags)
				if tagCount == 0 {
					return
				}
				p.selections.forEachMatch(n.Tags, func(_ int, pair tagSelection) {
					nodePois.Add(1)
					nodeTags.Add(int64(tagCount))
					csvCh <- csvRecord{poiLine: formatPOILineJSON("node", n.ID, n.Lat, n.Lon, pair, encoded)}
				})
			})
			joiner.flushJoined()
			if err == nil {
				err = joiner.err
			}
			if err != nil {
				workerErrors <- err
			}
		}(joiner, minNodeID, maxNodeID)
	}
	nodeWG.Wait()
	close(progressDone)
	close(workerErrors)
	var pass2Err error
	for err := range workerErrors {
		if pass2Err == nil {
			pass2Err = err
		}
	}
	for _, adder := range resolvedAdders {
		adder.close()
	}
	if pass2Err != nil {
		return fmt.Errorf("node range decode failed: %w", pass2Err)
	}
	p.log.Log(fmt.Sprintf("Pass 2: complete: %s nodes read, %s refs joined in %s",
		formatNumber(nodesScanned.Load()), formatNumber(joinedRefs.Load()), formatDuration(time.Since(nodeStart))))

	p.log.Log("Pass 3: resolve area points")
	result, err := p.resolveRequiredCentroids(ext.wayspanSorter, resolvedSorters, spool, maxWayID)
	if err != nil {
		return err
	}
	p.incompleteAreas, p.invalidAreas = result.droppedWays, result.invalidWays

	passStart = time.Now()
	p.log.Log("Pass 4: emit area POIs")
	wayPois, wayTags, err := spool.emit(csvCh)
	if err != nil {
		return fmt.Errorf("way POI emit failed: %w", err)
	}
	p.log.Log(fmt.Sprintf("Pass 4: complete: %s way POIs in %s",
		formatNumber(wayPois), formatDuration(time.Since(passStart))))

	if err := closeCSV(); err != nil {
		return fmt.Errorf("write POI CSV: %w", err)
	}
	if err := spool.file.Close(); err != nil {
		return fmt.Errorf("close area tag spool: %w", err)
	}
	spoolClosed = true
	p.log.Log(fmt.Sprintf("Parse core complete: %s refs, %s centroids in %s",
		formatNumber(ext.refsSpilled.Load()), formatNumber(result.centroidsWritten), formatDuration(time.Since(start))))
	p.log.Log(fmt.Sprintf("POIs: nodes %s, areas %s",
		formatNumber(nodePois.Load()), formatNumber(wayPois)))
	p.log.Log(fmt.Sprintf("Tags: nodes %s, areas %s",
		formatNumber(nodeTags.Load()), formatNumber(wayTags)))
	p.log.Log(fmt.Sprintf("Dropped areas: incomplete %s, invalid or degenerate %s",
		formatNumber(result.droppedWays), formatNumber(result.invalidWays)))
	matches.logUnmatched(p.log)
	return nil
}

type nodeBucketRange struct{ start, end int }

func balanceNodeBuckets(index *nodeBlockIndex, width int64, buckets, workers int) []nodeBucketRange {
	weights := make([]int, buckets)
	for _, block := range index.blocks {
		first := min(block.minID/width, int64(buckets-1))
		last := min(block.maxID/width, int64(buckets-1))
		for i := int(first); i <= int(last); i++ {
			weights[i]++
		}
	}
	remainingWeight := 0
	for _, weight := range weights {
		remainingWeight += weight
	}
	result := make([]nodeBucketRange, workers)
	start := 0
	for i := range result {
		remainingWorkers := workers - i
		end := start
		weight := 0
		target := (remainingWeight + remainingWorkers - 1) / remainingWorkers
		for end < buckets-(remainingWorkers-1) && (end == start || weight < target) {
			weight += weights[end]
			end++
		}
		if remainingWorkers == 1 {
			end = buckets
		}
		result[i] = nodeBucketRange{start: start, end: end}
		remainingWeight -= weight
		start = end
	}
	return result
}

type requiredNodeJoiner struct {
	ext         *refExtractor
	adders      []*concurrentAdder
	maxWayID    uint64
	bucket      int
	bucketEnd   int
	merge       *mergeReader
	rec         []byte
	have        bool
	joined      int64
	published   int64
	joinedTotal *atomic.Int64
	err         error
}

func newRequiredNodeJoiner(ext *refExtractor, adders []*concurrentAdder, maxWayID uint64, bucketStart, bucketEnd int, joinedTotal *atomic.Int64) *requiredNodeJoiner {
	return &requiredNodeJoiner{
		ext: ext, adders: adders, maxWayID: maxWayID,
		bucket: bucketStart, bucketEnd: bucketEnd,
		joinedTotal: joinedTotal,
	}
}

func (j *requiredNodeJoiner) advance() error {
	for j.bucket < j.bucketEnd {
		if j.merge == nil {
			merge, err := j.ext.wnrefBuckets[j.bucket].merge()
			if err != nil {
				return fmt.Errorf("merge bucket %d: %w", j.bucket, err)
			}
			j.merge = merge
		}
		rec, ok, err := j.merge.next()
		if err != nil {
			return fmt.Errorf("read bucket %d: %w", j.bucket, err)
		}
		if ok {
			j.rec, j.have = append(j.rec[:0], rec...), true
			return nil
		}
		j.merge, j.bucket = nil, j.bucket+1
	}
	j.have = false
	return nil
}

func (j *requiredNodeJoiner) flushJoined() {
	if pending := j.joined - j.published; pending > 0 {
		j.joinedTotal.Add(pending)
		j.published = j.joined
	}
}

func (j *requiredNodeJoiner) resolveThrough(nodeID int64, lat, lon float64) error {
	if !j.have {
		if err := j.advance(); err != nil {
			return err
		}
	}
	var latE7, lonE7 int32
	coordsReady := false
	for j.have && int64(binary.LittleEndian.Uint64(j.rec[:8])) <= nodeID {
		refID := int64(binary.LittleEndian.Uint64(j.rec[:8]))
		if refID == nodeID {
			if !coordsReady {
				latE7 = int32(math.Round(lat * 1e7))
				lonE7 = int32(math.Round(lon * 1e7))
				coordsReady = true
			}
			id := binary.LittleEndian.Uint64(j.rec[8:])
			seq := binary.LittleEndian.Uint16(j.rec[16:])
			var resolved [resolvedSize]byte
			binary.LittleEndian.PutUint64(resolved[0:], id)
			binary.LittleEndian.PutUint16(resolved[8:], seq)
			binary.LittleEndian.PutUint32(resolved[10:], uint32(latE7))
			binary.LittleEndian.PutUint32(resolved[14:], uint32(lonE7))
			j.adders[wayPartition(id, j.maxWayID, len(j.adders))].addBatch(resolved[:])
			j.joined++
			if j.joined-j.published >= 1<<16 {
				j.flushJoined()
			}
		}
		if err := j.advance(); err != nil {
			return err
		}
	}
	return nil
}

// resolveRequiredCentroids is the bounded counterpart of Pass 3. Both input
// streams are externally sorted, so it retains coordinates for one way only.
func (p *Parser) resolveRequiredCentroids(spans *ExtSorter, resolved []*ExtSorter, centroids interface{ Add(int64, int32, int32) }, maxWayID uint64) (resolveResult, error) {
	start := time.Now()
	spanMerge, err := spans.merge()
	if err != nil {
		return resolveResult{}, fmt.Errorf("sort way spans: %w", err)
	}
	defer spanMerge.closeAll()
	spanPaths, err := splitWaySpans(spanMerge, p.tempDir, maxWayID, len(resolved))
	if err != nil {
		return resolveResult{}, fmt.Errorf("partition way spans: %w", err)
	}
	var result resolveResult
	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := make([]error, len(resolved))
	for i, sorter := range resolved {
		wg.Add(1)
		go func(i int, sorter *ExtSorter) {
			defer wg.Done()
			local, err := resolveWayRange(spanPaths[i], sorter, centroids)
			if err != nil {
				errs[i] = err
				return
			}
			mu.Lock()
			result.centroidsWritten += local.centroidsWritten
			result.droppedWays += local.droppedWays
			result.invalidWays += local.invalidWays
			mu.Unlock()
		}(i, sorter)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return resolveResult{}, err
		}
	}
	p.log.Log(fmt.Sprintf("Pass 3: complete: %s centroids in %s",
		formatNumber(result.centroidsWritten), formatDuration(time.Since(start))))
	return result, nil
}

// resolveWayRange resolves the area geometries of one way-id range.
func resolveWayRange(spanPath string, sorter *ExtSorter, centroids interface{ Add(int64, int32, int32) }) (resolveResult, error) {
	spanReader, err := openRunReader(spanPath)
	if err != nil {
		return resolveResult{}, fmt.Errorf("open way spans: %w", err)
	}
	defer spanReader.close(true)
	resolvedMerge, err := sorter.merge()
	if err != nil {
		return resolveResult{}, fmt.Errorf("sort resolved refs: %w", err)
	}
	var current [resolvedSize]byte
	haveCurrent := false
	advance := func() error {
		record, ok, err := resolvedMerge.next()
		if err != nil {
			return fmt.Errorf("read resolved refs: %w", err)
		}
		if ok {
			copy(current[:], record)
			haveCurrent = true
		} else {
			haveCurrent = false
		}
		return nil
	}
	if err := advance(); err != nil {
		return resolveResult{}, err
	}
	local := resolveResult{}
	verts := make([]coord, 0, 256)
	var span [wayspanSize]byte
	for {
		ok, err := spanReader.advance(span[:])
		if err != nil {
			return resolveResult{}, fmt.Errorf("read way spans: %w", err)
		}
		if !ok {
			break
		}
		wayID := binary.LittleEndian.Uint64(span[:8])
		refCount := int(binary.LittleEndian.Uint16(span[8:10]))
		firstEqLast := span[10] == 1
		verts = verts[:0]
		for haveCurrent && binary.LittleEndian.Uint64(current[:8]) == wayID {
			verts = append(verts, coord{latE7: int32(binary.LittleEndian.Uint32(current[10:14])), lonE7: int32(binary.LittleEndian.Uint32(current[14:18]))})
			if err := advance(); err != nil {
				return resolveResult{}, err
			}
		}
		if len(verts) != refCount {
			local.droppedWays++
			continue
		}
		if !firstEqLast {
			local.invalidWays++
			continue
		}
		lat, lon, ok := areaPoint(verts)
		if !ok {
			local.invalidWays++
			continue
		}
		centroids.Add(int64(wayID), lat, lon)
		local.centroidsWritten++
	}
	return local, nil
}
