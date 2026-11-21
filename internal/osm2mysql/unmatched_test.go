package osm2mysql

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnmatchedPairLogHelper(t *testing.T) {
	if os.Getenv("OSM2MYSQL_UNMATCHED_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	p := NewParser(os.Getenv("OSM2MYSQL_UNMATCHED_PBF"), t.TempDir(), NewLogger(), 2, 1<<20, 1)
	for _, input := range []string{
		"node=only", "shared=yes", "open=only", "excluded=yes", "incomplete=yes",
		"area=only", "relation=only", "missing=value", "node=ONLY", "missing=line\nbreak", "node=only",
		"open=*", "missing=*",
	} {
		if err := p.selections.Set(input); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
}

func TestUnmatchedPairLog(t *testing.T) {
	header := appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))
	header = appendBytesField(header, 5, []byte("Sort.Type_then_ID"))
	data := addTestBlob(nil, "OSMHeader", header)
	table := appendBytesField(nil, 1, nil)
	for _, value := range []string{"node", "only", "shared", "yes", "open", "excluded", "area", "no", "incomplete", "relation"} {
		table = appendBytesField(table, 1, []byte(value))
	}
	var nodeGroup []byte
	for i, xy := range [][2]int64{{0, 0}, {0, 100}, {100, 100}, {100, 0}} {
		node := appendVarintField(nil, 1, zigzag(int64(i+1)))
		node = appendVarintField(node, 8, zigzag(xy[0]))
		node = appendVarintField(node, 9, zigzag(xy[1]))
		if i == 0 {
			node = appendBytesField(node, 2, packedValues(1, 3))
			node = appendBytesField(node, 3, packedValues(2, 4))
		}
		nodeGroup = appendBytesField(nodeGroup, 1, node)
	}
	block := appendBytesField(nil, 1, table)
	data = addTestBlob(data, "OSMData", appendBytesField(block, 2, nodeGroup))

	var wayGroup []byte
	for _, way := range []struct {
		id         uint64
		keys, vals []uint64
		refs       []int64
	}{
		{10, []uint64{5}, []uint64{2}, []int64{1, 2}},
		{11, []uint64{6, 7}, []uint64{4, 8}, []int64{1, 2, 3, 1}},
		{12, []uint64{9}, []uint64{4}, []int64{1, 2, 99, 1}},
		{13, []uint64{7, 3}, []uint64{2, 4}, []int64{1, 2, 3, 4, 1}},
	} {
		raw := appendVarintField(nil, 1, way.id)
		raw = appendBytesField(raw, 2, packedValues(way.keys...))
		raw = appendBytesField(raw, 3, packedValues(way.vals...))
		var refs []uint64
		var previous int64
		for _, ref := range way.refs {
			refs = append(refs, zigzag(ref-previous))
			previous = ref
		}
		raw = appendBytesField(raw, 8, packedValues(refs...))
		wayGroup = appendBytesField(wayGroup, 3, raw)
	}
	block = appendBytesField(nil, 1, table)
	data = addTestBlob(data, "OSMData", appendBytesField(block, 2, wayGroup))
	relation := appendVarintField(nil, 1, 20)
	relation = appendBytesField(relation, 2, packedValues(10))
	relation = appendBytesField(relation, 3, packedValues(2))
	block = appendBytesField(nil, 1, table)
	data = addTestBlob(data, "OSMData", appendBytesField(block, 2, appendBytesField(nil, 4, relation)))

	path := filepath.Join(t.TempDir(), "selection.osm.pbf")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestUnmatchedPairLogHelper$")
	cmd.Env = append(os.Environ(), "OSM2MYSQL_UNMATCHED_HELPER=1", "OSM2MYSQL_UNMATCHED_PBF="+path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("parser failed: %v\n%s", err, output)
	}
	log := string(output)
	for _, pair := range []string{`"relation"="only"`, `"missing"="value"`, `"node"="ONLY"`, `"missing"="line\nbreak"`, `"missing"="*"`} {
		if !strings.Contains(log, "ERROR: tag pair "+pair+" matched no nodes or ways") {
			t.Errorf("missing error for %s:\n%s", pair, log)
		}
	}
	if count := strings.Count(log, "ERROR: tag pair "); count != 5 {
		t.Errorf("got %d errors, want 5:\n%s", count, log)
	}
	if !strings.Contains(log, "POIs: nodes 2, areas 2") ||
		!strings.Contains(log, "Dropped areas: incomplete 1") {
		t.Errorf("unexpected parsing outcome:\n%s", log)
	}
}
