package osm2mysql

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
)

// requiredCSV starts the CSV writer goroutine. The returned channel is closed
// by the parser; the error channel then reports the first write/flush failure
// and is closed. The goroutine always drains the record channel, so a failed
// write cannot block the producers.
func (p *Parser) requiredCSV() (chan csvRecord, <-chan error) {
	poiWriters := make([]*bufio.Writer, csvShards)
	poiHandles := make([]*os.File, csvShards)
	var firstErr error
	for i := 0; i < csvShards; i++ {
		poiPath := fmt.Sprintf("%s/required.poi.%d.csv", p.tempDir, i)
		p.poiFiles = append(p.poiFiles, poiPath)
		pf, err := os.Create(poiPath)
		if err != nil {
			firstErr = fmt.Errorf("create POI CSV %s: %w", poiPath, err)
			break
		}
		poiWriters[i] = bufio.NewWriterSize(pf, 1<<20)
		poiHandles[i] = pf
	}
	ch := make(chan csvRecord, 32768)
	done := make(chan error, 1)
	go func() {
		err := firstErr
		poiShardIdx := 0
		for record := range ch {
			if err != nil || len(record.poiLine) == 0 {
				continue
			}
			idx := poiShardIdx % csvShards
			poiShardIdx++
			if _, werr := poiWriters[idx].Write(record.poiLine); werr != nil {
				err = fmt.Errorf("write POI CSV: %w", werr)
			}
		}
		for i := range poiWriters {
			if poiWriters[i] == nil {
				continue
			}
			if err == nil {
				if ferr := poiWriters[i].Flush(); ferr != nil {
					err = fmt.Errorf("flush POI CSV: %w", ferr)
				}
			}
			if cerr := poiHandles[i].Close(); cerr != nil && err == nil {
				err = fmt.Errorf("close POI CSV: %w", cerr)
			}
		}
		done <- err
		close(done)
	}()
	return ch, done
}

func formatPOITagsJSON(tags map[string]string) ([]byte, int) {
	normalized := make(map[string]string, len(tags))
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		normalized[sanitizeKey(key)] = sanitizeValue(tags[key])
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		panic(fmt.Sprintf("encode POI tags as JSON: %v", err))
	}
	return data, len(normalized)
}

func formatPOILine(kind string, id int64, lat, lon float64, selection tagSelection, tags map[string]string) ([]byte, int) {
	encoded, count := formatPOITagsJSON(tags)
	return formatPOILineJSON(kind, id, lat, lon, selection, encoded), count
}

func formatPOILineJSON(kind string, id int64, lat, lon float64, selection tagSelection, tags []byte) []byte {
	line := make([]byte, 0, 66+2*(len(selection.key)+len(selection.value))+len(tags))
	line = append(line, kind+"\t"...)
	line = strconv.AppendInt(line, id, 10)
	line = append(line, '\t')
	// Hex preserves arbitrary UTF-8 pair values across tab-delimited LOAD DATA.
	line = hex.AppendEncode(line, []byte(selection.key))
	line = append(line, '\t')
	line = hex.AppendEncode(line, []byte(selection.value))
	line = append(line, '\t')
	line = strconv.AppendFloat(line, lat, 'f', -1, 64)
	line = append(line, '\t')
	line = strconv.AppendFloat(line, lon, 'f', -1, 64)
	line = append(line, '\t')
	line = append(line, tags...)
	line = append(line, '\n')
	return line
}
