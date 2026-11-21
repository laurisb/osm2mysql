package osm2mysql

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/encoding/protowire"
)

type blockLayout struct {
	nodes []nodeBlock
	ways  []nodeBlock
}

// discoverBlockLayout indexes blob offsets without reading payloads and probes
// type boundaries. A block containing multiple types belongs to both ranges.
func discoverBlockLayout(path string) (*blockLayout, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var blocks []nodeBlock
	for {
		var length [4]byte
		_, err := io.ReadFull(f, length[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		size := binary.BigEndian.Uint32(length[:])
		if size == 0 || size > 64<<10 {
			return nil, fmt.Errorf("invalid blob header size %d", size)
		}
		header := make([]byte, size)
		if _, err := io.ReadFull(f, header); err != nil {
			return nil, err
		}
		kind, dataSize, err := parseBlobHeader(header)
		if err != nil {
			return nil, err
		}
		offset, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if dataSize < 0 || dataSize > 1<<31 || dataSize > info.Size()-offset {
			return nil, fmt.Errorf("invalid PBF blob size %d", dataSize)
		}
		if kind == "OSMData" {
			blocks = append(blocks, nodeBlock{payloadOffset: offset, payloadSize: dataSize})
		}
		if _, err := f.Seek(dataSize, io.SeekCurrent); err != nil {
			return nil, err
		}
	}
	type bounds struct{ first, last int }
	cache := make(map[int]bounds)
	probe := func(i int) (bounds, error) {
		if b, ok := cache[i]; ok {
			return b, nil
		}
		data := make([]byte, blocks[i].payloadSize)
		if _, err := f.ReadAt(data, blocks[i].payloadOffset); err != nil {
			return bounds{}, err
		}
		raw, err := decodeBlob(data)
		if err != nil {
			return bounds{}, err
		}
		first, last, err := primitiveTypes(raw)
		if err != nil {
			return bounds{}, err
		}
		b := bounds{first, last}
		cache[i] = b
		return b, nil
	}
	search := func(predicate func(bounds) bool) (int, error) {
		lo, hi := 0, len(blocks)
		for lo < hi {
			mid := lo + (hi-lo)/2
			var b bounds
			for previous := mid; previous >= 0; previous-- {
				var err error
				b, err = probe(previous)
				if err != nil {
					return 0, err
				}
				if b.first != 0 {
					break
				}
			}
			if predicate(b) {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		return lo, nil
	}
	wayStart, err := search(func(b bounds) bool { return b.last >= 2 })
	if err != nil {
		return nil, err
	}
	nodeEnd, err := search(func(b bounds) bool { return b.first >= 2 })
	if err != nil {
		return nil, err
	}
	wayEnd, err := search(func(b bounds) bool { return b.first >= 3 })
	if err != nil {
		return nil, err
	}
	return &blockLayout{nodes: blocks[:nodeEnd], ways: blocks[wayStart:wayEnd]}, nil
}

func primitiveTypes(data []byte) (int, int, error) {
	first, last := 0, 0
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, 0, protowire.ParseError(n)
		}
		data = data[n:]
		if num == 2 && typ == protowire.BytesType {
			group, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return 0, 0, protowire.ParseError(m)
			}
			data = data[m:]
			for len(group) > 0 {
				field, wire, n := protowire.ConsumeTag(group)
				if n < 0 {
					return 0, 0, protowire.ParseError(n)
				}
				group = group[n:]
				kind := 0
				switch field {
				case 1, 2:
					kind = 1
				case 3:
					kind = 2
				case 4:
					kind = 3
				}
				if kind != 0 {
					if first == 0 || kind < first {
						first = kind
					}
					if kind > last {
						last = kind
					}
				}
				m := protowire.ConsumeFieldValue(field, wire, group)
				if m < 0 {
					return 0, 0, protowire.ParseError(m)
				}
				group = group[m:]
			}
		} else {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return 0, 0, protowire.ParseError(m)
			}
			data = data[m:]
		}
	}
	return first, last, nil
}
