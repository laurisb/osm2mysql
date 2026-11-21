package osm2mysql

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const areaTagIndexSize = 40
const areaPointSize = 16

type areaSpool struct {
	file       *os.File
	mu         sync.Mutex
	offset     int64
	err        error
	tags       *ExtSorter
	tagAdder   *concurrentAdder
	points     *ExtSorter
	pointAdder *concurrentAdder
	selections tagSelections
}

func newAreaSpool(p *Parser) (*areaSpool, error) {
	file, err := os.Create(filepath.Join(p.tempDir, "area_tags.bin"))
	if err != nil {
		return nil, err
	}
	tags := newExtSorter(p.tempDir, "area_tag_index", areaTagIndexSize, p.sortMem/8, p.threads, keyWayID, lessWayID)
	points := newExtSorter(p.tempDir, "area_points", areaPointSize, p.sortMem/8, p.centroidWorkers, keyWayID, lessWayID)
	return &areaSpool{file: file, selections: p.selections, tags: tags, tagAdder: tags.concurrentAdder(p.threads),
		points: points, pointAdder: points.concurrentAdder(p.centroidWorkers)}, nil
}

// addWay spools one matching way's tags. It reports the first write failure;
// later calls return that same error immediately.
func (s *areaSpool) addWay(w *Way) error {
	tags, count := formatPOITagsJSON(w.Tags)
	payload := tags
	var records [][areaTagIndexSize]byte
	s.selections.forEachMatch(w.Tags, func(i int, pair tagSelection) {
		var rec [areaTagIndexSize]byte
		binary.LittleEndian.PutUint64(rec[:8], uint64(w.ID))
		binary.LittleEndian.PutUint32(rec[16:20], uint32(len(tags)))
		binary.LittleEndian.PutUint32(rec[20:24], uint32(count))
		binary.LittleEndian.PutUint32(rec[24:28], uint32(i))
		if s.selections[i].value == "*" {
			binary.LittleEndian.PutUint64(rec[28:36], uint64(len(payload)))
			binary.LittleEndian.PutUint32(rec[36:40], uint32(len(pair.value)))
			payload = append(payload, pair.value...)
		}
		records = append(records, rec)
	})
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	offset := s.offset
	n, err := s.file.Write(payload)
	s.offset += int64(n)
	if err != nil || n != len(payload) {
		if err == nil {
			err = io.ErrShortWrite
		}
		s.err = fmt.Errorf("write area tags: wrote %d of %d bytes: %w", n, len(payload), err)
		err = s.err
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	for _, rec := range records {
		binary.LittleEndian.PutUint64(rec[8:16], uint64(offset))
		binary.LittleEndian.PutUint64(rec[28:36], uint64(offset)+binary.LittleEndian.Uint64(rec[28:36]))
		s.tagAdder.addBatch(rec[:])
	}
	return nil
}

// Err reports the first tag-spool write failure.
func (s *areaSpool) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *areaSpool) Add(wayID int64, lat, lon int32) {
	var rec [areaPointSize]byte
	binary.LittleEndian.PutUint64(rec[:8], uint64(wayID))
	binary.LittleEndian.PutUint32(rec[8:12], uint32(lat))
	binary.LittleEndian.PutUint32(rec[12:16], uint32(lon))
	s.pointAdder.addBatch(rec[:])
}

func (s *areaSpool) emit(csvCh chan<- csvRecord) (int64, int64, error) {
	s.tagAdder.close()
	s.pointAdder.close()
	if err := s.Err(); err != nil {
		return 0, 0, err
	}
	tags, err := s.tags.merge()
	if err != nil {
		return 0, 0, err
	}
	defer tags.closeAll()
	points, err := s.points.merge()
	if err != nil {
		return 0, 0, err
	}
	defer points.closeAll()
	var areaCount, tagCount int64
	var current []byte
	for {
		point, ok, err := points.next()
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			break
		}
		id := binary.LittleEndian.Uint64(point[:8])
		for current == nil || binary.LittleEndian.Uint64(current[:8]) < id {
			rec, ok, err := tags.next()
			if err != nil {
				return 0, 0, err
			}
			if !ok {
				return 0, 0, fmt.Errorf("missing tags for area %d", id)
			}
			current = append(current[:0], rec...)
		}
		if binary.LittleEndian.Uint64(current[:8]) != id {
			return 0, 0, fmt.Errorf("missing tags for area %d", id)
		}
		lat := int32(binary.LittleEndian.Uint32(point[8:12]))
		lon := int32(binary.LittleEndian.Uint32(point[12:16]))
		for current != nil && binary.LittleEndian.Uint64(current[:8]) == id {
			offset := int64(binary.LittleEndian.Uint64(current[8:16]))
			length := binary.LittleEndian.Uint32(current[16:20])
			tagBuf := make([]byte, length)
			if _, err := s.file.ReadAt(tagBuf, offset); err != nil {
				return 0, 0, fmt.Errorf("read area %d tags: %w", id, err)
			}
			selectionIndex := binary.LittleEndian.Uint32(current[24:28])
			if uint64(selectionIndex) >= uint64(len(s.selections)) {
				return 0, 0, fmt.Errorf("invalid selection index for area %d", id)
			}
			pair := s.selections[selectionIndex]
			if pair.value == "*" {
				valueOffset := int64(binary.LittleEndian.Uint64(current[28:36]))
				valueLen := binary.LittleEndian.Uint32(current[36:40])
				value := make([]byte, valueLen)
				if _, err := s.file.ReadAt(value, valueOffset); err != nil {
					return 0, 0, fmt.Errorf("read area %d selection value: %w", id, err)
				}
				pair.value = string(value)
			}
			poi := formatPOILineJSON("way", int64(id), float64(lat)*1e-7, float64(lon)*1e-7, pair, tagBuf)
			csvCh <- csvRecord{poiLine: poi}
			areaCount++
			tagCount += int64(binary.LittleEndian.Uint32(current[20:24]))
			rec, ok, err := tags.next()
			if err != nil {
				return 0, 0, err
			}
			if !ok {
				current = nil
			} else {
				current = append(current[:0], rec...)
			}
		}
	}
	return areaCount, tagCount, nil
}
