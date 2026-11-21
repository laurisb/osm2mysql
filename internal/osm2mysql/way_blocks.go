package osm2mysql

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/encoding/protowire"
)

// decodeSelectedWays skips all node and relation primitives, including
// non-dense nodes unsupported by the upstream decoder.
type wayScanStats struct {
	open    atomic.Int64
	closed  atomic.Int64
	matches *selectionMatches
}

func decodeSelectedWays(path string, workers int, blocks []nodeBlock, selection tagSelections, stats *wayScanStats, onWay func(*Way)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan []byte, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for payload := range jobs {
				raw, err := decodeBlob(payload)
				if err == nil {
					err = decodeWayBlock(raw, selection, stats, onWay)
				}
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, block := range blocks {
		payload := make([]byte, block.payloadSize)
		if _, readErr := f.ReadAt(payload, block.payloadOffset); readErr != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = readErr
			}
			mu.Unlock()
			break
		}
		jobs <- payload
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

func decodeWayBlock(data []byte, selection tagSelections, stats *wayScanStats, onWay func(*Way)) error {
	var table []string
	var groups [][]byte
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return protowire.ParseError(n)
		}
		data = data[n:]
		if typ == protowire.BytesType && (num == 1 || num == 2) {
			v, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return protowire.ParseError(m)
			}
			data = data[m:]
			if num == 1 {
				table = parseStringTable(v)
			} else {
				groups = append(groups, v)
			}
		} else {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return protowire.ParseError(m)
			}
			data = data[m:]
		}
	}
	matcher := newBlockTagMatcher(table, selection)
	for _, group := range groups {
		for len(group) > 0 {
			num, typ, n := protowire.ConsumeTag(group)
			if n < 0 {
				return protowire.ParseError(n)
			}
			group = group[n:]
			if num == 3 && typ == protowire.BytesType {
				v, m := protowire.ConsumeBytes(group)
				if m < 0 {
					return protowire.ParseError(m)
				}
				group = group[m:]
				way, err := parseSelectedWay(v, table, matcher, stats)
				if err != nil {
					return err
				}
				if way != nil {
					onWay(way)
				}
			} else {
				m := protowire.ConsumeFieldValue(num, typ, group)
				if m < 0 {
					return protowire.ParseError(m)
				}
				group = group[m:]
			}
		}
	}
	return nil
}

func parseSelectedWay(data []byte, table []string, matcher *blockTagMatcher, stats *wayScanStats) (*Way, error) {
	var id int64
	var keys, values, refs []uint64
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		if num == 1 && typ == protowire.VarintType {
			value, m := protowire.ConsumeVarint(data)
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			id, data = int64(value), data[m:]
		} else if (num == 2 || num == 3 || num == 8) && (typ == protowire.BytesType || typ == protowire.VarintType) {
			var valuesInField []uint64
			var m int
			if typ == protowire.BytesType {
				var raw []byte
				raw, m = protowire.ConsumeBytes(data)
				if m < 0 {
					return nil, protowire.ParseError(m)
				}
				var err error
				valuesInField, err = packedVarints(raw)
				if err != nil {
					return nil, err
				}
			} else {
				var value uint64
				value, m = protowire.ConsumeVarint(data)
				if m < 0 {
					return nil, protowire.ParseError(m)
				}
				valuesInField = []uint64{value}
			}
			data = data[m:]
			switch num {
			case 2:
				keys = append(keys, valuesInField...)
			case 3:
				values = append(values, valuesInField...)
			case 8:
				refs = append(refs, valuesInField...)
			}
		} else {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			data = data[m:]
		}
	}
	if len(keys) != len(values) {
		return nil, fmt.Errorf("way %d has mismatched tag keys and values", id)
	}
	for i := range keys {
		if keys[i] >= uint64(len(table)) || values[i] >= uint64(len(table)) {
			return nil, fmt.Errorf("way %d has invalid tag index", id)
		}
	}
	if !matcher.matches(keys, values) {
		return nil, nil
	}
	tags := tagsFromIndexes(keys, values, table)
	if stats != nil && stats.matches != nil {
		stats.matches.record(tags)
	}
	nodeIDs := make([]int64, len(refs))
	var current int64
	for i, ref := range refs {
		var ok bool
		current, ok = addIDDelta(current, unzigzag(ref))
		if !ok || current < 0 {
			return nil, fmt.Errorf("way %d has invalid node reference ID", id)
		}
		nodeIDs[i] = current
	}
	if len(nodeIDs) < 2 || nodeIDs[0] != nodeIDs[len(nodeIDs)-1] {
		if stats != nil {
			stats.open.Add(1)
		}
		return nil, nil
	}
	if stats != nil {
		stats.closed.Add(1)
	}
	if tags["area"] == "no" {
		return nil, nil
	}
	return &Way{ID: id, NodeIDs: nodeIDs, Tags: tags}, nil
}
