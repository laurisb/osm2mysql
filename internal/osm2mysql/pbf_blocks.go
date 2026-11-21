package osm2mysql

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zlib"
	"google.golang.org/protobuf/encoding/protowire"
)

type nodeBlock struct {
	payloadOffset int64
	payloadSize   int64
	minID         int64
	maxID         int64
}

type nodeBlockIndex struct {
	blocks []nodeBlock
}

// buildNodeBlockIndex reads OSMData blobs and records the node-ID range and
// file location of each node-containing block so workers can decode disjoint
// ranges.
func buildNodeBlockIndex(path string, workers int) (*nodeBlockIndex, error) {
	layout, err := discoverBlockLayout(path)
	if err != nil {
		return nil, err
	}
	return buildNodeBlockIndexFor(path, workers, layout.nodes)
}

func buildNodeBlockIndexFor(path string, workers int, blocks []nodeBlock) (*nodeBlockIndex, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if workers < 1 {
		workers = 1
	}
	if workers > 32 {
		workers = 32
	}
	type blockJob struct {
		block   nodeBlock
		payload []byte
	}
	type blockResult struct {
		block nodeBlock
		err   error
	}
	jobs := make(chan blockJob, workers)
	results := make(chan blockResult, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				raw, err := decodeBlob(job.payload)
				if err == nil {
					job.block.minID, job.block.maxID, err = nodeRange(raw)
				}
				results <- blockResult{block: job.block, err: err}
			}
		}()
	}
	var decoded []blockResult
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		for result := range results {
			decoded = append(decoded, result)
		}
	}()

	var scanErr error
	for _, block := range blocks {
		payload := make([]byte, block.payloadSize)
		if _, err := file.ReadAt(payload, block.payloadOffset); err != nil {
			scanErr = err
			break
		}
		jobs <- blockJob{block: block, payload: payload}
	}
	close(jobs)
	wg.Wait()
	close(results)
	<-collectorDone
	if scanErr != nil {
		return nil, scanErr
	}
	for _, result := range decoded {
		if result.err != nil {
			return nil, result.err
		}
	}
	sort.Slice(decoded, func(i, j int) bool {
		return decoded[i].block.payloadOffset < decoded[j].block.payloadOffset
	})
	index := &nodeBlockIndex{blocks: make([]nodeBlock, 0, len(decoded))}
	var previousMax int64
	var havePrevious bool
	for _, result := range decoded {
		block := result.block
		if block.minID > block.maxID {
			continue
		}
		if block.minID < 0 {
			return nil, fmt.Errorf("node IDs %d..%d exceed supported range 0..%d", block.minID, block.maxID, int64(math.MaxInt64))
		}
		if havePrevious && block.minID < previousMax {
			return nil, fmt.Errorf("node block IDs are not monotonic: %d after %d", block.minID, previousMax)
		}
		index.blocks = append(index.blocks, block)
		previousMax = block.maxID
		havePrevious = true
	}
	return index, nil
}

func (index *nodeBlockIndex) decodeRange(path string, minID, maxID int64, onNode func(*Node)) error {
	return index.decodeSelectedRange(path, minID, maxID, nil, onNode)
}

type blockTagMatcher struct {
	keys       []string
	values     []string
	selections map[tagSelection]struct{}
}

func newBlockTagMatcher(table []string, selections tagSelections) *blockTagMatcher {
	if selections == nil {
		return nil
	}
	m := &blockTagMatcher{keys: make([]string, len(table)), values: table, selections: make(map[tagSelection]struct{}, len(selections))}
	selectedKeys := make(map[string]bool, len(selections))
	for _, pair := range selections {
		m.selections[pair] = struct{}{}
		selectedKeys[pair.key] = true
	}
	for i, s := range table {
		key := strings.ToLower(s)
		if selectedKeys[key] {
			m.keys[i] = key
		}
	}
	return m
}

func (m *blockTagMatcher) matches(keys, values []uint64) bool {
	return m.matchesStride(keys, values, 1)
}

func (m *blockTagMatcher) matchesStride(keys, values []uint64, stride int) bool {
	if m == nil {
		return true
	}
	n := min(len(keys), len(values))
	for i := ((n + stride - 1) / stride * stride) - stride; i >= 0; i -= stride {
		if keys[i] >= uint64(len(m.keys)) || values[i] >= uint64(len(m.values)) {
			continue
		}
		key := m.keys[keys[i]]
		if key == "" {
			continue
		}
		value := m.values[values[i]]
		_, exact := m.selections[tagSelection{key, value}]
		_, wildcard := m.selections[tagSelection{key, "*"}]
		if !exact && (!wildcard || value == "") {
			continue
		}
		// The decoder's lowercase-key map keeps the last valid value for a key.
		overridden := false
		for j := i + stride; j < n; j += stride {
			if keys[j] < uint64(len(m.keys)) && values[j] < uint64(len(m.values)) && m.keys[keys[j]] == key {
				overridden = true
				break
			}
		}
		if !overridden {
			return true
		}
	}
	return false
}

func (index *nodeBlockIndex) decodeSelectedRange(path string, minID, maxID int64, selection tagSelections, onNode func(*Node)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, block := range index.blocks {
		if block.maxID < minID || block.minID > maxID {
			continue
		}
		payload := make([]byte, block.payloadSize)
		if _, err := file.ReadAt(payload, block.payloadOffset); err != nil {
			return err
		}
		raw, err := decodeBlob(payload)
		if err != nil {
			return err
		}
		if _, _, err := parsePrimitiveBlockSelected(raw, nil, selection, func(id int64, lat, lon int32, tags map[string]string) {
			if id < minID || id > maxID {
				return
			}
			onNode(&Node{ID: id, Lat: float64(lat) * 1e-7, Lon: float64(lon) * 1e-7, Tags: tags})
		}); err != nil {
			return err
		}
	}
	return nil
}

func parseBlobHeader(data []byte) (string, int64, error) {
	var blockType string
	var dataSize int64
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return "", 0, protowire.ParseError(n)
		}
		data = data[n:]
		switch num {
		case 1:
			if typ != protowire.BytesType {
				return "", 0, fmt.Errorf("invalid PBF blob type field")
			}
			value, m := protowire.ConsumeString(data)
			if m < 0 {
				return "", 0, protowire.ParseError(m)
			}
			blockType = value
			data = data[m:]
		case 3:
			if typ != protowire.VarintType {
				return "", 0, fmt.Errorf("invalid PBF blob size field")
			}
			value, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return "", 0, protowire.ParseError(m)
			}
			dataSize = int64(value)
			data = data[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return "", 0, protowire.ParseError(m)
			}
			data = data[m:]
		}
	}
	return blockType, dataSize, nil
}

func decodeBlob(data []byte) ([]byte, error) {
	var raw, compressed []byte
	var rawSize uint64
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		if num == 2 && typ == protowire.VarintType {
			value, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			rawSize = value
			data = data[m:]
			continue
		}
		if typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			data = data[m:]
			continue
		}
		value, m := protowire.ConsumeBytes(data)
		if m < 0 {
			return nil, protowire.ParseError(m)
		}
		data = data[m:]
		switch num {
		case 1:
			raw = value
		case 3:
			compressed = value
		}
	}
	if len(raw) > 0 {
		return raw, nil
	}
	reader, err := zlib.NewReader(bytesReader(compressed))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	// Treat raw_size only as an allocation hint, and cap it for malformed input.
	if rawSize > 0 && rawSize <= 32<<20 && rawSize <= uint64(len(compressed))*128 {
		var output bytes.Buffer
		output.Grow(int(rawSize) + 512)
		if _, err := output.ReadFrom(reader); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
	return io.ReadAll(reader)
}

// bytesReader avoids importing bytes in the hot block-indexing path.
type byteReader struct{ data []byte }

func bytesReader(data []byte) io.Reader { return &byteReader{data: data} }
func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func nodeRange(data []byte) (int64, int64, error) {
	return parsePrimitiveBlock(data, nil, nil)
}

func parsePrimitiveBlock(data []byte, stringsTable []string, onNode func(int64, int32, int32, map[string]string)) (int64, int64, error) {
	return parsePrimitiveBlockSelected(data, stringsTable, nil, onNode)
}

func parsePrimitiveBlockSelected(data []byte, stringsTable []string, selection tagSelections, onNode func(int64, int32, int32, map[string]string)) (int64, int64, error) {
	var table []string
	granularity := int64(100)
	var latOffset, lonOffset int64
	if stringsTable != nil {
		table = stringsTable
	}
	var groups [][]byte
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, 0, protowire.ParseError(n)
		}
		data = data[n:]
		if typ == protowire.VarintType && num == 17 {
			value, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return 0, 0, protowire.ParseError(m)
			}
			granularity = int64(value)
			data = data[m:]
			continue
		}
		if typ == protowire.VarintType && (num == 19 || num == 20) {
			value, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return 0, 0, protowire.ParseError(m)
			}
			if num == 19 {
				latOffset = int64(value)
			} else {
				lonOffset = int64(value)
			}
			data = data[m:]
			continue
		}
		if typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return 0, 0, protowire.ParseError(m)
			}
			data = data[m:]
			continue
		}
		value, m := protowire.ConsumeBytes(data)
		if m < 0 {
			return 0, 0, protowire.ParseError(m)
		}
		data = data[m:]
		switch num {
		case 1:
			table = parseStringTable(value)
		case 2:
			groups = append(groups, value)
		}
	}
	var minID, maxID int64
	found := false
	matcher := newBlockTagMatcher(table, selection)
	for _, group := range groups {
		gmin, gmax, err := parsePrimitiveGroup(group, table, matcher, granularity, latOffset, lonOffset, onNode)
		if err != nil {
			return 0, 0, err
		}
		if gmin <= gmax && (!found || gmin < minID) {
			minID = gmin
		}
		if gmin <= gmax && (!found || gmax > maxID) {
			maxID = gmax
		}
		if gmin <= gmax {
			found = true
		}
	}
	if !found {
		return 1, 0, nil
	}
	return minID, maxID, nil
}

func parseStringTable(data []byte) []string {
	var table []string
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return table
		}
		data = data[n:]
		if num == 1 && typ == protowire.BytesType {
			value, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return table
			}
			table = append(table, string(value))
			data = data[m:]
		} else {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return table
			}
			data = data[m:]
		}
	}
	return table
}

func parsePrimitiveGroup(data []byte, table []string, matcher *blockTagMatcher, granularity, latOffset, lonOffset int64, onNode func(int64, int32, int32, map[string]string)) (int64, int64, error) {
	var minID, maxID int64
	found := false
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, 0, protowire.ParseError(n)
		}
		data = data[n:]
		if typ != protowire.BytesType || (num != 1 && num != 2) {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return 0, 0, protowire.ParseError(m)
			}
			data = data[m:]
			continue
		}
		value, m := protowire.ConsumeBytes(data)
		if m < 0 {
			return 0, 0, protowire.ParseError(m)
		}
		data = data[m:]
		var ids []int64
		if num == 1 {
			id, lat, lon, tags, err := parseNode(value, table, matcher)
			if err != nil {
				return 0, 0, err
			}
			ids = []int64{id}
			if onNode != nil {
				onNode(id, coordinateE7(latOffset, granularity, lat), coordinateE7(lonOffset, granularity, lon), tags)
			}
		} else {
			var err error
			ids, err = parseDense(value, table, matcher, granularity, latOffset, lonOffset, onNode)
			if err != nil {
				return 0, 0, err
			}
		}
		for _, id := range ids {
			if !found || id < minID {
				minID = id
			}
			if !found || id > maxID {
				maxID = id
			}
			found = true
		}
	}
	if !found {
		return 1, 0, nil
	}
	return minID, maxID, nil
}

func parseNode(data []byte, table []string, matcher *blockTagMatcher) (int64, int64, int64, map[string]string, error) {
	var id, lat, lon int64
	var keys, vals []uint64
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, 0, 0, nil, protowire.ParseError(n)
		}
		data = data[n:]
		if typ == protowire.VarintType && (num == 1 || num == 8 || num == 9) {
			value, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return 0, 0, 0, nil, protowire.ParseError(m)
			}
			data = data[m:]
			switch num {
			case 1:
				id = unzigzag(value)
			case 8:
				lat = unzigzag(value)
			case 9:
				lon = unzigzag(value)
			}
		} else if typ == protowire.BytesType && (num == 2 || num == 3) {
			value, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return 0, 0, 0, nil, protowire.ParseError(m)
			}
			data = data[m:]
			var err error
			if num == 2 {
				keys, err = packedVarints(value)
			} else {
				vals, err = packedVarints(value)
			}
			if err != nil {
				return 0, 0, 0, nil, err
			}
		} else {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return 0, 0, 0, nil, protowire.ParseError(m)
			}
			data = data[m:]
		}
	}
	if !matcher.matches(keys, vals) {
		return id, lat, lon, nil, nil
	}
	return id, lat, lon, tagsFromIndexes(keys, vals, table), nil
}

func parseDense(data []byte, table []string, matcher *blockTagMatcher, granularity, latOffset, lonOffset int64, onNode func(int64, int32, int32, map[string]string)) ([]int64, error) {
	var idValues, latValues, lonValues, keyValues []uint64
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		if typ == protowire.BytesType && (num == 1 || num == 8 || num == 9 || num == 10) {
			value, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			data = data[m:]
			// The range-only pass validates the remaining fields when nodes are decoded.
			if onNode == nil && num != 1 {
				continue
			}
			values, err := packedVarints(value)
			if err != nil {
				return nil, err
			}
			switch num {
			case 1:
				idValues = values
			case 8:
				latValues = values
			case 9:
				lonValues = values
			case 10:
				keyValues = values
			}
		} else {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			data = data[m:]
		}
	}
	ids := make([]int64, len(idValues))
	var id, lat, lon int64
	keyPos := 0
	for i := range idValues {
		var ok bool
		id, ok = addIDDelta(id, unzigzag(idValues[i]))
		if !ok {
			return nil, fmt.Errorf("dense node ID delta overflows int64")
		}
		if i < len(latValues) {
			lat += unzigzag(latValues[i])
		}
		if i < len(lonValues) {
			lon += unzigzag(lonValues[i])
		}
		ids[i] = id
		start := keyPos
		for keyPos < len(keyValues) && keyValues[keyPos] != 0 {
			if keyPos+1 >= len(keyValues) {
				return nil, fmt.Errorf("unterminated dense node tags")
			}
			keyPos += 2
		}
		end := keyPos
		if keyPos < len(keyValues) {
			keyPos++
		}
		if onNode != nil {
			matches := matcher.matchesStride(keyValues[start:end], keyValues[start+min(1, end-start):end], 2)
			var tags map[string]string
			if matches && end > start {
				tags = make(map[string]string, (end-start)/2)
				for j := start; j < end; j += 2 {
					key, value := keyValues[j], keyValues[j+1]
					if key < uint64(len(table)) && value < uint64(len(table)) {
						tags[strings.ToLower(table[key])] = table[value]
					}
				}
			}
			onNode(id, coordinateE7(latOffset, granularity, lat), coordinateE7(lonOffset, granularity, lon), tags)
		}
	}
	return ids, nil
}

func packedVarints(data []byte) ([]uint64, error) {
	var values []uint64
	for len(data) > 0 {
		value, n := protowire.ConsumeVarint(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		values = append(values, value)
		data = data[n:]
	}
	return values, nil
}

func tagsFromIndexes(keys, vals []uint64, table []string) map[string]string {
	if len(keys) == 0 || len(vals) == 0 {
		return nil
	}
	tags := make(map[string]string, len(keys))
	for i := 0; i < len(keys) && i < len(vals); i++ {
		if int(keys[i]) < len(table) && int(vals[i]) < len(table) {
			tags[strings.ToLower(table[keys[i]])] = table[vals[i]]
		}
	}
	return tags
}

func unzigzag(value uint64) int64 {
	return int64(value>>1) ^ -int64(value&1)
}

func coordinateE7(offset, granularity, value int64) int32 {
	return int32(math.Round(float64(offset+granularity*value) / 100))
}
