package osm2mysql

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/encoding/protowire"
)

func requireSortedPBF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var size [4]byte
	if _, err := io.ReadFull(f, size[:]); err != nil {
		return fmt.Errorf("read PBF header size: %w", err)
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > 64<<10 {
		return fmt.Errorf("invalid PBF header size %d", n)
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(f, header); err != nil {
		return err
	}
	kind, dataSize, err := parseBlobHeader(header)
	if err != nil {
		return err
	}
	if kind != "OSMHeader" || dataSize <= 0 || dataSize > 32<<20 {
		return fmt.Errorf("expected valid OSMHeader blob")
	}
	payload := make([]byte, dataSize)
	if _, err := io.ReadFull(f, payload); err != nil {
		return err
	}
	data, err := decodeBlob(payload)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return protowire.ParseError(n)
		}
		data = data[n:]
		if num == 5 && typ == protowire.BytesType {
			value, m := protowire.ConsumeString(data)
			if m < 0 {
				return protowire.ParseError(m)
			}
			if value == "Sort.Type_then_ID" {
				return nil
			}
			data = data[m:]
		} else {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return protowire.ParseError(m)
			}
			data = data[m:]
		}
	}
	return fmt.Errorf("PBF header must declare Sort.Type_then_ID")
}
