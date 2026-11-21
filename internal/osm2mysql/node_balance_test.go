package osm2mysql

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBalanceNodeBuckets(t *testing.T) {
	idx := &nodeBlockIndex{}
	for bucket := 0; bucket < 8; bucket++ {
		for i := 0; i < 10; i++ {
			idx.blocks = append(idx.blocks, nodeBlock{minID: int64(bucket * 100), maxID: int64(bucket*100 + 99)})
		}
	}
	ranges := balanceNodeBuckets(idx, 100, 16, 4)
	start := 0
	for _, r := range ranges {
		if r.start != start || r.end <= r.start || r.end > 16 {
			t.Fatalf("invalid bucket ranges: %v", ranges)
		}
		start = r.end
	}
	if start != 16 || ranges[0].end >= 4 {
		t.Fatalf("unbalanced partition: %v", ranges)
	}
	empty := balanceNodeBuckets(&nodeBlockIndex{}, 100, 16, 4)
	start = 0
	for _, r := range empty {
		if r.start != start || r.end <= r.start {
			t.Fatalf("invalid empty-index partition: %v", empty)
		}
		start = r.end
	}
	if start != 16 {
		t.Fatalf("empty-index partition omitted buckets: %v", empty)
	}
}

func TestJoinAcrossNodeBucketBoundaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boundaries.osm.pbf")
	header := appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))
	header = appendBytesField(header, 5, []byte("Sort.Type_then_ID"))
	data := addTestBlob(nil, "OSMHeader", header)
	table := appendBytesField(nil, 1, nil)
	table = appendBytesField(table, 1, []byte("historic"))
	table = appendBytesField(table, 1, []byte("castle"))
	width := int64((uint64(math.MaxInt64) + numWnrefBuckets) / numWnrefBuckets)
	ids := []int64{1, width - 1, width, math.MaxInt64}
	positions := [][2]int64{{0, 0}, {0, 100}, {100, 100}, {100, 0}}
	for i, id := range ids {
		node := appendVarintField(nil, 1, zigzag(id))
		node = appendVarintField(node, 8, zigzag(positions[i][0]))
		node = appendVarintField(node, 9, zigzag(positions[i][1]))
		block := appendBytesField(nil, 1, table)
		block = appendBytesField(block, 2, appendBytesField(nil, 1, node))
		data = addTestBlob(data, "OSMData", block)
	}

	way := appendVarintField(nil, 1, math.MaxInt64)
	way = appendBytesField(way, 2, packedValues(1))
	way = appendBytesField(way, 3, packedValues(2))
	way = appendBytesField(way, 8, packedValues(zigzag(ids[0]), zigzag(ids[1]-ids[0]), zigzag(ids[2]-ids[1]), zigzag(ids[3]-ids[2]), zigzag(ids[0]-ids[3])))
	block := appendBytesField(nil, 1, table)
	block = appendBytesField(block, 2, appendBytesField(nil, 3, way))
	data = addTestBlob(data, "OSMData", block)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewParser(path, dir, NewLogger(), 4, 1<<20, 2)
	p.selections = tagSelections{{"historic", "castle"}}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
	var output string
	for _, file := range p.PoiFiles() {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		output += string(raw)
	}
	if output != "way\t9223372036854775807\t686973746f726963\t636173746c65\t0.0000049999999999999996\t0.0000049999999999999996\t{\"historic\":\"castle\"}\n" || p.incompleteAreas != 0 || p.invalidAreas != 0 {
		t.Fatalf("unexpected output %q, incomplete %d, invalid %d", strings.TrimSpace(output), p.incompleteAreas, p.invalidAreas)
	}
}

func TestWayPartitionWideIDs(t *testing.T) {
	for _, maximum := range []uint64{1, 1<<32 - 1, 1 << 32, math.MaxInt64} {
		for _, workers := range []int{1, 2, 3, 8, 24} {
			for _, id := range []uint64{0, maximum / 2, maximum - 1, maximum} {
				product := new(big.Int).Mul(new(big.Int).SetUint64(id), big.NewInt(int64(workers)))
				want := int(product.Div(product, new(big.Int).SetUint64(maximum+1)).Int64())
				if got := wayPartition(id, maximum, workers); got != want {
					t.Fatalf("wayPartition(%d, %d, %d) = %d, want %d", id, maximum, workers, got, want)
				}
			}
		}
	}
}

func TestWideIDsPipeline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wide-ids.osm.pbf")
	header := appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))
	header = appendBytesField(header, 5, []byte("Sort.Type_then_ID"))
	data := addTestBlob(nil, "OSMHeader", header)
	table := appendBytesField(nil, 1, nil)
	table = appendBytesField(table, 1, []byte("historic"))
	table = appendBytesField(table, 1, []byte("castle"))
	ids := []int64{0, 1, 1<<34 - 1, 1 << 34, math.MaxInt64 - 1, math.MaxInt64}
	positions := [][2]int64{{0, 0}, {0, 100}, {100, 100}, {100, 0}, {200, 200}, {300, 300}}
	var nodeGroup []byte
	for i, id := range ids {
		node := appendVarintField(nil, 1, zigzag(id))
		node = appendVarintField(node, 8, zigzag(positions[i][0]))
		node = appendVarintField(node, 9, zigzag(positions[i][1]))
		if id == 0 || id == 1<<34-1 || id == 1<<34 || id == math.MaxInt64 {
			node = appendBytesField(node, 2, packedValues(1))
			node = appendBytesField(node, 3, packedValues(2))
		}
		nodeGroup = appendBytesField(nodeGroup, 1, node)
	}
	block := appendBytesField(nil, 1, table)
	data = addTestBlob(data, "OSMData", appendBytesField(block, 2, nodeGroup))
	var wayGroup []byte
	for _, id := range []uint64{1, 1<<32 + 1, 2<<32 + 1, math.MaxInt64} {
		way := appendVarintField(nil, 1, id)
		way = appendBytesField(way, 2, packedValues(1))
		way = appendBytesField(way, 3, packedValues(2))
		way = appendBytesField(way, 8, packedValues(zigzag(ids[0]), zigzag(ids[1]-ids[0]), zigzag(ids[2]-ids[1]), zigzag(ids[3]-ids[2]), zigzag(ids[0]-ids[3])))
		wayGroup = appendBytesField(wayGroup, 3, way)
	}
	block = appendBytesField(nil, 1, table)
	data = addTestBlob(data, "OSMData", appendBytesField(block, 2, wayGroup))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewParser(path, dir, NewLogger(), 4, 1<<20, 3)
	p.selections = tagSelections{{"historic", "castle"}}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
	rows := make(map[string]bool)
	for _, file := range p.PoiFiles() {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) != 7 {
				t.Fatalf("invalid row: %s", line)
			}
			identity := fields[0] + ":" + fields[1]
			if rows[identity] {
				t.Fatalf("duplicate row: %s", identity)
			}
			if fields[0] == "way" && (fields[4] != "0.0000049999999999999996" || fields[5] != "0.0000049999999999999996") {
				t.Fatalf("wrong area point: %s", line)
			}
			rows[identity] = true
		}
	}
	for _, id := range []uint64{1, 1<<32 + 1, 2<<32 + 1, math.MaxInt64} {
		if !rows[fmt.Sprintf("way:%d", id)] {
			t.Fatalf("missing way %d: %v", id, rows)
		}
	}
	if !rows["node:0"] || !rows["node:17179869183"] || !rows["node:17179869184"] || !rows["node:9223372036854775807"] ||
		len(rows) != 8 || p.incompleteAreas != 0 || p.invalidAreas != 0 {
		t.Fatalf("unexpected rows %v; incomplete=%d invalid=%d", rows, p.incompleteAreas, p.invalidAreas)
	}
	if os.Getenv("OSM2MYSQL_TEST_MYSQL") == "1" {
		dbName, admin := newTestDatabase(t)
		imp := mysqlTestImporter(t, dbName, poiTable)
		defer func() {
			if err := imp.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := imp.CreateTables(); err != nil {
			t.Fatal(err)
		}
		if err := imp.Import(p.PoiFiles()); err != nil {
			t.Fatal(err)
		}
		if err := imp.CreateIndexes(); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`poi` WHERE osm_id > 4294967295").Scan(&count); err != nil || count != 6 {
			t.Fatalf("wide IDs in MySQL: count=%d, error=%v", count, err)
		}
		if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`poi` WHERE osm_id = 9223372036854775807").Scan(&count); err != nil || count != 2 {
			t.Fatalf("maximum IDs in MySQL: count=%d, error=%v", count, err)
		}
	}
}
