package osm2mysql

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRequireSortedPBF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unsorted.pbf")
	if err := os.WriteFile(path, addTestBlob(nil, "OSMHeader", appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := requireSortedPBF(path); err == nil || !strings.Contains(err.Error(), "Sort.Type_then_ID") {
		t.Fatalf("unsorted header error = %v", err)
	}
}

func TestPairMatching(t *testing.T) {
	s := tagSelections{{"amenity", "Cafe"}, {"shop", "books"}}
	if !s.matches(map[string]string{"amenity": "Cafe"}) ||
		!s.matches(map[string]string{"shop": "books"}) ||
		s.matches(map[string]string{"amenity": "cafe"}) ||
		s.matches(map[string]string{"amenity": "Cafe;bar"}) ||
		s.matches(map[string]string{"shop": "Cafe"}) {
		t.Fatal("tag pair matching is not exact")
	}
}

func TestWildcardSelections(t *testing.T) {
	for _, selections := range []tagSelections{
		{{"geological", "*"}, {"geological", "cave"}, {"name", "*"}},
		{{"geological", "cave"}, {"geological", "*"}, {"name", "*"}},
	} {
		var got []tagSelection
		selections.forEachMatch(map[string]string{"geological": "cave", "name": ""}, func(_ int, pair tagSelection) {
			got = append(got, pair)
		})
		if !reflect.DeepEqual(got, []tagSelection{{"geological", "cave"}}) {
			t.Fatalf("overlapping selections returned %v", got)
		}
		if selections.matches(map[string]string{"name": ""}) {
			t.Fatal("wildcard matched empty value")
		}
		var cleaned tagSelection
		selections.forEachMatch(map[string]string{"name": string([]byte{0xff}) + strings.Repeat("é", 201)}, func(_ int, pair tagSelection) {
			cleaned = pair
		})
		if cleaned.value != strings.Repeat("é", 200) {
			t.Fatalf("wildcard value not normalized: %q", cleaned.value)
		}
	}
}

func TestTagFlag(t *testing.T) {
	var selections tagSelections
	for _, input := range []string{"HISTORIC=castle", "historic=castle", "name=a=b", "name=Café\t"} {
		if err := selections.Set(input); err != nil {
			t.Fatalf("Set(%q): %v", input, err)
		}
	}
	want := tagSelections{{"historic", "castle"}, {"name", "a=b"}, {"name", "Café\t"}}
	if !reflect.DeepEqual(selections, want) {
		t.Fatalf("selections = %#v, want %#v", selections, want)
	}
	for _, input := range []string{"", "name", "=x", "name=", string([]byte{0xff}) + "=x", "name=" + string([]byte{0xff}), strings.Repeat("x", 51) + "=v", "x=" + strings.Repeat("é", 201)} {
		if err := selections.Set(input); err == nil {
			t.Errorf("Set(%q) should fail", input)
		}
	}
	if err := selections.Set(strings.Repeat("é", 50) + "=" + strings.Repeat("é", 200)); err != nil {
		t.Fatalf("valid UTF-8 length boundary rejected: %v", err)
	}
}

func TestCoordinateFormattingPreservesDatabaseRounding(t *testing.T) {
	e7 := int32(5)
	value := float64(e7) * 1e-7
	current := strconv.FormatFloat(value, 'f', -1, 64)
	shortened := strconv.FormatFloat(value, 'f', 6, 64)
	if current != "0.0000005" || shortened != "0.000000" {
		t.Fatalf("coordinate formatting changed: full %q, shortened %q", current, shortened)
	}
}

func TestFormatPOITagsJSON(t *testing.T) {
	tags := map[string]string{
		"name":                    "  Cafe\t\"Central\"\n\\  ",
		"source":                  "survey",
		"name:de":                 "Café",
		"amenity":                 "Law Firm",
		"note":                    "-",
		"empty":                   "",
		string([]byte{0xff, 'k'}): string([]byte{0xff, 'v'}),
	}
	line, count := formatPOILine("node", 7, 1, 2, tagSelection{"amenity", "Law Firm"}, tags)
	fields := strings.Split(strings.TrimSuffix(string(line), "\n"), "\t")
	if count != 7 || len(fields) != 7 || strings.ContainsAny(fields[6], "\t\r\n") || !json.Valid([]byte(fields[6])) {
		t.Fatalf("invalid JSON POI row: count=%d, line=%q", count, line)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(fields[6]), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["name"] != `Cafe "Central" \` || decoded["k"] != "v" ||
		decoded["source"] != "survey" || decoded["name:de"] != "Café" ||
		decoded["amenity"] != "Law Firm" || decoded["note"] != "-" {
		t.Fatalf("unexpected normalized JSON tags: %#v", decoded)
	}
	if value, ok := decoded["empty"]; !ok || value != "" {
		t.Fatalf("empty tag was not retained: %#v", decoded)
	}
	again, _ := formatPOITagsJSON(map[string]string{
		string([]byte{0xff, 'k'}): string([]byte{0xff, 'v'}),
		"name":                    "  Cafe\t\"Central\"\n\\  ",
		"source":                  "survey",
		"name:de":                 "Café",
		"amenity":                 "Law Firm",
		"note":                    "-",
		"empty":                   "",
	})
	if string(again) != fields[6] {
		t.Fatalf("JSON encoding is nondeterministic: %s != %s", again, fields[6])
	}
}

func TestValidArea(t *testing.T) {
	square := []coord{{0, 0}, {0, 10}, {10, 10}, {10, 0}, {0, 0}}
	bow := []coord{{0, 0}, {10, 10}, {0, 10}, {10, 0}, {0, 0}}
	if !validArea(square) || validArea(bow) || validArea([]coord{{0, 0}, {0, 1}, {0, 2}, {0, 0}}) {
		t.Fatal("area validity mismatch")
	}
	if !validArea([]coord{{500000000, 100000000}, {500000000, 100000001}, {500000001, 100000001}, {500000001, 100000000}, {500000000, 100000000}}) {
		t.Fatal("small area at a large coordinate was rejected")
	}
}

func TestAreaPointAntimeridian(t *testing.T) {
	tests := []struct {
		name string
		ring []coord
		ok   bool
	}{
		{"east to west", []coord{{0, 1790000000}, {10000000, 1790000000}, {10000000, -1790000000}, {0, -1790000000}, {0, 1790000000}}, true},
		{"west to east", []coord{{0, -1790000000}, {10000000, -1790000000}, {10000000, 1790000000}, {0, 1790000000}, {0, -1790000000}}, true},
		{"self intersecting", []coord{{0, 1790000000}, {10000000, -1790000000}, {0, -1790000000}, {10000000, 1790000000}, {0, 1790000000}}, false},
		{"unclosed", []coord{{0, 1790000000}, {10000000, 1790000000}, {10000000, -1790000000}, {0, -1790000000}}, false},
		{"world winding", []coord{{0, 0}, {10000000, 1200000000}, {0, -1200000000}, {0, 0}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lat, lon, ok := areaPoint(tt.ring)
			if ok != tt.ok {
				t.Fatalf("areaPoint = %d, %d, %v", lat, lon, ok)
			}
			if ok && (lat != 5000000 || (lon < 1790000000 && lon > -1790000000)) {
				t.Fatalf("point is not on the antimeridian area: %d, %d", lat, lon)
			}
		})
	}
	normal := []coord{{0, 0}, {0, 10}, {10, 10}, {10, 0}, {0, 0}}
	lat, lon, ok := areaPoint(normal)
	wantLat, wantLon, wantOK := pointOnSurface(normal)
	if !ok || !wantOK || lat != wantLat || lon != wantLon {
		t.Fatalf("non-crossing area changed: %d, %d, %v", lat, lon, ok)
	}
}

func TestAntimeridianPipeline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "antimeridian.osm.pbf")
	header := appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))
	header = appendBytesField(header, 5, []byte("Sort.Type_then_ID"))
	data := addTestBlob(nil, "OSMHeader", header)
	table := appendBytesField(nil, 1, nil)
	table = appendBytesField(table, 1, []byte("historic"))
	table = appendBytesField(table, 1, []byte("castle"))
	var nodeGroup []byte
	for i, xy := range [][2]int64{{0, 1790000000}, {10000000, 1790000000}, {10000000, -1790000000}, {0, -1790000000}} {
		node := appendVarintField(nil, 1, zigzag(int64(i+1)))
		node = appendVarintField(node, 8, zigzag(xy[0]))
		node = appendVarintField(node, 9, zigzag(xy[1]))
		nodeGroup = appendBytesField(nodeGroup, 1, node)
	}
	block := appendBytesField(nil, 1, table)
	block = appendBytesField(block, 2, nodeGroup)
	data = addTestBlob(data, "OSMData", block)
	way := appendVarintField(nil, 1, 10)
	way = appendBytesField(way, 2, packedValues(1))
	way = appendBytesField(way, 3, packedValues(2))
	way = appendBytesField(way, 8, packedValues(zigzag(1), zigzag(1), zigzag(1), zigzag(1), zigzag(-3)))
	block = appendBytesField(nil, 1, table)
	block = appendBytesField(block, 2, appendBytesField(nil, 3, way))
	data = addTestBlob(data, "OSMData", block)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewParser(path, dir, NewLogger(), 2, 1<<20, 1)
	p.selections = tagSelections{{"historic", "castle"}}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, file := range p.PoiFiles() {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 0 {
			lines = append(lines, strings.Split(strings.TrimSpace(string(raw)), "\n")...)
		}
	}
	if len(lines) != 1 || p.invalidAreas != 0 || p.incompleteAreas != 0 {
		t.Fatalf("POIs = %v, invalid = %d, incomplete = %d", lines, p.invalidAreas, p.incompleteAreas)
	}
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 7 || fields[0] != "way" || fields[1] != "10" {
		t.Fatalf("unexpected POI: %q", lines[0])
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(fields[6]), &tags); err != nil || tags["historic"] != "castle" {
		t.Fatalf("unexpected JSON tags in %q: %#v, %v", lines[0], tags, err)
	}
	lat, errLat := strconv.ParseFloat(fields[4], 64)
	lon, errLon := strconv.ParseFloat(fields[5], 64)
	if errLat != nil || errLon != nil || lat != 0.5 || math.Abs(lon) < 179 || math.Abs(lon) > 180 {
		t.Fatalf("unexpected antimeridian point: %q", lines[0])
	}
}

func TestSingleTagPipeline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.osm.pbf")
	var data []byte
	header := appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))
	header = appendBytesField(header, 5, []byte("Sort.Type_then_ID"))
	data = addTestBlob(data, "OSMHeader", header)
	table := appendBytesField(nil, 1, nil)
	for _, s := range []string{"HISTORIC", "castle", "name", "Example", "area", "no", "source", "survey", "type", "route"} {
		table = appendBytesField(table, 1, []byte(s))
	}
	var nodeGroup []byte
	for i, xy := range [][2]int64{{0, 0}, {0, 100}, {100, 100}, {100, 0}, {200, 200}, {100, 75}, {25, 75}, {25, 25}, {100, 25}, {100, 0}} {
		node := appendVarintField(nil, 1, zigzag(int64(i+1)))
		node = appendVarintField(node, 8, zigzag(xy[0]))
		node = appendVarintField(node, 9, zigzag(xy[1]))
		if i == 4 {
			node = appendBytesField(node, 2, packedValues(1, 3, 7))
			node = appendBytesField(node, 3, packedValues(2, 4, 8))
		}
		nodeGroup = appendBytesField(nodeGroup, 1, node)
	}
	primitive := appendBytesField(nil, 1, table)
	primitive = appendBytesField(primitive, 2, nodeGroup)
	data = addTestBlob(data, "OSMData", primitive)
	var wayGroup []byte
	for _, w := range []struct {
		id     int64
		refs   []int64
		areaNo bool
	}{
		{10, []int64{1, 2, 3, 4, 1}, false},
		{11, []int64{1, 2, 3}, false},
		{12, []int64{1, 2, 3, 1}, true},
		{13, []int64{1, 2, 99, 1}, false},
		{14, []int64{1, 2, 1}, false},
		{15, []int64{1, 3, 2, 4, 1}, false},
		{16, []int64{1, 2, 3, 6, 7, 8, 9, 10, 1}, false},
	} {
		way := appendVarintField(nil, 1, uint64(w.id))
		keys, vals := []uint64{1}, []uint64{2}
		if w.id == 10 || w.id == 13 || w.id == 14 || w.id == 15 {
			keys, vals = append(keys, 7), append(vals, 8)
		}
		if w.areaNo {
			keys, vals = append(keys, 5), append(vals, 6)
		}
		way = appendBytesField(way, 2, packedValues(keys...))
		way = appendBytesField(way, 3, packedValues(vals...))
		var refs []uint64
		var prev int64
		for _, ref := range w.refs {
			refs = append(refs, zigzag(ref-prev))
			prev = ref
		}
		way = appendBytesField(way, 8, packedValues(refs...))
		wayGroup = appendBytesField(wayGroup, 3, way)
	}
	primitive = appendBytesField(nil, 1, table)
	primitive = appendBytesField(primitive, 2, wayGroup)
	data = addTestBlob(data, "OSMData", primitive)
	relation := appendVarintField(nil, 1, 20)
	relation = appendBytesField(relation, 2, packedValues(1))
	relation = appendBytesField(relation, 3, packedValues(2))
	relation = appendBytesField(relation, 8, packedValues(0))
	relation = appendBytesField(relation, 9, packedValues(zigzag(10)))
	relation = appendBytesField(relation, 10, packedValues(1))
	group := appendBytesField(nil, 4, relation)
	route := appendVarintField(nil, 1, 21)
	route = appendBytesField(route, 2, packedValues(1, 9))
	route = appendBytesField(route, 3, packedValues(2, 10))
	group = appendBytesField(group, 4, route)
	primitive = appendBytesField(nil, 1, table)
	primitive = appendBytesField(primitive, 2, group)
	data = addTestBlob(data, "OSMData", primitive)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := requireSortedPBF(path); err != nil {
		t.Fatal(err)
	}
	p := NewParser(path, dir, NewLogger(), 2, 1<<20, 1)
	p.selections = tagSelections{{"historic", "castle"}}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
	if p.incompleteAreas != 1 || p.invalidAreas != 2 {
		t.Fatalf("dropped areas = incomplete %d, invalid %d", p.incompleteAreas, p.invalidAreas)
	}
	var pois string
	for _, file := range p.PoiFiles() {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		pois += string(b)
	}
	if strings.Count(pois, "\n") != 3 || !strings.Contains(pois, "node\t5\t") || !strings.Contains(pois, "way\t10\t") || !strings.Contains(pois, "way\t16\t") {
		t.Fatalf("unexpected POIs: %s", pois)
	}
	for _, line := range strings.Split(strings.TrimSpace(pois), "\n") {
		fields := strings.Split(line, "\t")
		if fields[1] == "16" {
			lat, errLat := strconv.ParseFloat(fields[4], 64)
			lon, errLon := strconv.ParseFloat(fields[5], 64)
			if errLat != nil || errLon != nil || lat < 0 || lat > 0.00001 || lon < 0 || lon > 0.00001 ||
				(lat > 0.0000025 && lon > 0.0000025 && lon < 0.0000075) {
				t.Fatalf("point outside non-convex area: %q", line)
			}
			continue
		}
		expected := 0.000005
		if fields[0] == "node" {
			expected = 0.00002
		}
		if len(fields) != 7 {
			t.Fatalf("expected JSON tags field in %q", line)
		}
		var gotTags map[string]string
		if err := json.Unmarshal([]byte(fields[6]), &gotTags); err != nil {
			t.Fatalf("invalid JSON tags in %q: %v", line, err)
		}
		for _, raw := range fields[4:6] {
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.Abs(value-expected) > 1e-10 {
				t.Fatalf("unexpected coordinate in %q: %v", line, err)
			}
		}
	}
	tagCount := 0
	for _, line := range strings.Split(strings.TrimSpace(pois), "\n") {
		fields := strings.Split(line, "\t")
		var gotTags map[string]string
		if err := json.Unmarshal([]byte(fields[6]), &gotTags); err != nil {
			t.Fatal(err)
		}
		tagCount += len(gotTags)
		if fields[0] == "node" && fields[1] == "5" &&
			!reflect.DeepEqual(gotTags, map[string]string{"historic": "castle", "name": "Example", "source": "survey"}) {
			t.Fatalf("node JSON tags = %#v", gotTags)
		}
		if fields[0] == "way" && fields[1] == "10" && gotTags["historic"] != "castle" {
			t.Fatalf("area JSON tags = %#v", gotTags)
		}
	}
	if tagCount != 6 {
		t.Fatalf("JSON tag count = %d, want 6", tagCount)
	}
	nodeOnly := NewParser(path, t.TempDir(), NewLogger(), 2, 1<<20, 1)
	nodeOnly.selections = tagSelections{{"source", "survey"}}
	if err := nodeOnly.Parse(); err != nil {
		t.Fatal(err)
	}
	var nodeOnlyPOIs string
	for _, file := range nodeOnly.PoiFiles() {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		nodeOnlyPOIs += string(b)
	}
	if strings.Count(nodeOnlyPOIs, "\n") != 2 || !strings.Contains(nodeOnlyPOIs, "node\t5\t") || !strings.Contains(nodeOnlyPOIs, "way\t10\t") {
		t.Fatalf("node-only output: %s", nodeOnlyPOIs)
	}
	multi := NewParser(path, t.TempDir(), NewLogger(), 2, 1<<20, 1)
	multi.selections = tagSelections{{"historic", "castle"}, {"source", "survey"}}
	if err := multi.Parse(); err != nil {
		t.Fatal(err)
	}
	rows := make(map[string]bool)
	for _, file := range multi.PoiFiles() {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) != 7 {
				t.Fatalf("unexpected row %q", line)
			}
			key, errKey := hex.DecodeString(fields[2])
			value, errValue := hex.DecodeString(fields[3])
			if errKey != nil || errValue != nil {
				t.Fatalf("invalid pair encoding: %q", line)
			}
			identity := fields[0] + ":" + fields[1] + ":" + string(key) + "=" + string(value)
			if rows[identity] {
				t.Fatalf("duplicate row %s", identity)
			}
			rows[identity] = true
		}
	}
	wantRows := map[string]bool{
		"node:5:historic=castle": true, "node:5:source=survey": true,
		"way:10:historic=castle": true, "way:10:source=survey": true,
		"way:16:historic=castle": true,
	}
	if !reflect.DeepEqual(rows, wantRows) || multi.incompleteAreas != 1 || multi.invalidAreas != 2 {
		t.Fatalf("multi-pair rows=%v, incomplete=%d, invalid=%d", rows, multi.incompleteAreas, multi.invalidAreas)
	}
	for _, selections := range []tagSelections{
		{{"historic", "*"}, {"historic", "castle"}, {"source", "*"}},
		{{"historic", "castle"}, {"historic", "*"}, {"source", "*"}},
	} {
		wildcard := NewParser(path, t.TempDir(), NewLogger(), 2, 1<<20, 1)
		wildcard.selections = selections
		if err := wildcard.Parse(); err != nil {
			t.Fatal(err)
		}
		got := make(map[string]bool)
		for _, file := range wildcard.PoiFiles() {
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
					t.Fatalf("invalid wildcard row: %q", line)
				}
				key, errKey := hex.DecodeString(fields[2])
				value, errValue := hex.DecodeString(fields[3])
				if errKey != nil || errValue != nil {
					t.Fatalf("invalid wildcard row: %q", line)
				}
				identity := fields[0] + ":" + fields[1] + ":" + string(key) + "=" + string(value)
				if got[identity] {
					t.Fatalf("duplicate wildcard row: %s", identity)
				}
				got[identity] = true
			}
		}
		if !reflect.DeepEqual(got, wantRows) {
			t.Fatalf("wildcard rows=%v, want %v", got, wantRows)
		}
	}
	if os.Getenv("OSM2MYSQL_TEST_MYSQL") == "1" {
		testCLIImport(t, path)
	}
}

func TestCLIHelper(t *testing.T) {
	if os.Getenv("OSM2MYSQL_CLI_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	flag.CommandLine = flag.NewFlagSet("osm2mysql", flag.ExitOnError)
	os.Args = []string{
		"osm2mysql", "-pbf", os.Getenv("OSM2MYSQL_TEST_PBF"),
		"-tag", "historic=castle", "-tag", "source=survey", "-tag", "HISTORIC=castle",
		"-db-host", "127.0.0.1", "-db-name", os.Getenv("OSM2MYSQL_TEST_DB"),
		"-sort-mem", "1048576", "-centroid-workers", "1",
	}
	if table := os.Getenv("OSM2MYSQL_TEST_TABLE"); table != "" {
		os.Args = append(os.Args, "-table-name", table)
	}
	if extra := os.Getenv("OSM2MYSQL_TEST_EXTRA_TAG"); extra != "" {
		os.Args = append(os.Args, "-tag", extra)
	}
	switch threads := os.Getenv("OSM2MYSQL_TEST_THREADS"); threads {
	case "":
		os.Args = append(os.Args, "-threads", "2")
	case "default":
	default:
		os.Args = append(os.Args, "-threads", threads)
	}
	Run("test")
}

func testCLIImport(t *testing.T, path string) {
	t.Helper()
	dbName, admin := newTestDatabase(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCLIHelper$")
	cmd.Env = append(os.Environ(),
		"OSM2MYSQL_CLI_HELPER=1", "OSM2MYSQL_TEST_PBF="+path, "OSM2MYSQL_TEST_DB="+dbName,
		"GOMAXPROCS=3", "OSM2MYSQL_TEST_THREADS=0")
	if output, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(output), "ERROR: tag pair") || !strings.Contains(string(output), "Using 3 decoder threads") {
		t.Fatalf("CLI failed or reported a matched pair as missing: %v\n%s", err, output)
	} else {
		log := string(output)
		sizePos := strings.Index(log, "Temporary directory size after parsing (not peak): ")
		importPos := strings.Index(log, "Importing data")
		if sizePos < 0 || importPos < 0 || sizePos >= importPos || !strings.Contains(log[sizePos:importPos], " B") || strings.Contains(log[sizePos:importPos], " bytes") {
			t.Fatalf("expected readable scratch snapshot before import:\n%s", log)
		}
		for _, expected := range []string{"Parsing in 4 passes", "Pass 1: discover", "Pass 2: decode", "Pass 3: resolve", "Pass 4: emit", "Pass 1: complete", "Pass 2: complete", "Pass 3: complete", "Pass 4: complete"} {
			if !strings.Contains(log, expected) {
				t.Fatalf("missing %q in log:\n%s", expected, log)
			}
			for _, expected := range []string{"Pass 1: complete in ", " refs joined in ", " centroids in ", " way POIs in ", "Import completed in ", "Indexing completed in ", "Finished in "} {
				if !strings.Contains(log, expected) {
					t.Fatalf("missing duration wording %q in log:\n%s", expected, log)
				}
			}
		}
		if strings.Contains(log, "/4") || strings.Contains(log, " memory: heap ") {
			t.Fatalf("unexpected pass fraction or memory diagnostics:\n%s", log)
		}
	}
	var pois int
	if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`poi`").Scan(&pois); err != nil {
		t.Fatal(err)
	}
	if pois != 5 {
		t.Fatalf("CLI import: got %d POIs, want 5", pois)
	}
	cmd = exec.Command(exe, "-test.run=^TestCLIHelper$")
	cmd.Env = append(os.Environ(),
		"OSM2MYSQL_CLI_HELPER=1", "OSM2MYSQL_TEST_PBF="+path, "OSM2MYSQL_TEST_DB="+dbName,
		"GOMAXPROCS=3", "OSM2MYSQL_TEST_THREADS=default")
	if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), "Using 3 decoder threads") {
		t.Fatalf("CLI default worker count failed: %v\n%s", err, output)
	}
	var tags int
	if err := admin.QueryRow("SELECT SUM(JSON_LENGTH(tags)) FROM `" + dbName + "`.`poi`").Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if tags != 11 {
		t.Fatalf("CLI import: got %d JSON tags, want 11", tags)
	}
	var historic, source int
	if err := admin.QueryRow("SELECT SUM(tag_name='historic' AND tag_value='castle'), SUM(tag_name='source' AND tag_value='survey') FROM `"+dbName+"`.`poi`").Scan(&historic, &source); err != nil || historic != 3 || source != 2 {
		t.Fatalf("CLI pair counts: historic=%d source=%d error=%v", historic, source, err)
	}
	cmd = exec.Command(exe, "-test.run=^TestCLIHelper$")
	cmd.Env = append(os.Environ(), "OSM2MYSQL_CLI_HELPER=1", "OSM2MYSQL_TEST_PBF="+path, "OSM2MYSQL_TEST_DB="+dbName)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CLI reimport failed: %v\n%s", err, output)
	}
	if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`poi`").Scan(&pois); err != nil || pois != 5 {
		t.Fatalf("CLI reimport count=%d error=%v", pois, err)
	}
	cmd = exec.Command(exe, "-test.run=^TestCLIHelper$")
	cmd.Env = append(os.Environ(), "OSM2MYSQL_CLI_HELPER=1", "OSM2MYSQL_TEST_PBF="+path, "OSM2MYSQL_TEST_DB="+dbName, "OSM2MYSQL_TEST_TABLE=custom_poi", "OSM2MYSQL_TEST_EXTRA_TAG=historic=Castle")
	if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), `ERROR: tag pair "historic"="Castle" matched no nodes or ways`) {
		t.Fatalf("CLI custom table import failed or missing unmatched error: %v\n%s", err, output)
	}
	if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`custom_poi`").Scan(&pois); err != nil || pois != 5 {
		t.Fatalf("custom table count=%d error=%v", pois, err)
	}
	cmd = exec.Command(exe, "-test.run=^TestCLIHelper$")
	cmd.Env = append(os.Environ(), "OSM2MYSQL_CLI_HELPER=1", "OSM2MYSQL_TEST_PBF="+path, "OSM2MYSQL_TEST_DB="+dbName, "OSM2MYSQL_TEST_TABLE=custom_poi", "OSM2MYSQL_TEST_EXTRA_TAG=historic=*")
	if output, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(output), "ERROR: tag pair") {
		t.Fatalf("CLI wildcard import failed: %v\n%s", err, output)
	}
	if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`custom_poi` WHERE tag_name='historic' AND tag_value='castle'").Scan(&pois); err != nil || pois != 3 {
		t.Fatalf("CLI wildcard rows=%d error=%v", pois, err)
	}
	var indexes int
	if err := admin.QueryRow("SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME='custom_poi' AND INDEX_NAME IN ('PRIMARY', 'idx_lat_lon')", dbName).Scan(&indexes); err != nil || indexes != 2 {
		t.Fatalf("custom table indexes=%d error=%v", indexes, err)
	}
	if _, err := admin.Exec("INSERT INTO `" + dbName + "`.`custom_poi` (osm_type, osm_id, tag_name, tag_value, lat, lon, tags) VALUES ('node', 999999, 'test', 'marker', 0, 0, '{}')"); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(exe, "-test.run=^TestCLIHelper$")
	cmd.Env = append(os.Environ(), "OSM2MYSQL_CLI_HELPER=1", "OSM2MYSQL_TEST_PBF="+path, "OSM2MYSQL_TEST_DB="+dbName, "OSM2MYSQL_TEST_TABLE=custom_poi")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CLI custom table reimport failed: %v\n%s", err, output)
	}
	if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`custom_poi`").Scan(&pois); err != nil || pois != 5 {
		t.Fatalf("custom table reimport count=%d error=%v", pois, err)
	}
	if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`poi`").Scan(&pois); err != nil || pois != 5 {
		t.Fatalf("default table changed: count=%d error=%v", pois, err)
	}
	cmd = exec.Command(exe, "-test.run=^TestCLIHelper$")
	cmd.Env = append(os.Environ(), "OSM2MYSQL_CLI_HELPER=1", "OSM2MYSQL_TEST_PBF="+path, "OSM2MYSQL_TEST_DB="+dbName, "OSM2MYSQL_TEST_TABLE=invalid-name")
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "invalid table name") {
		t.Fatalf("invalid table name accepted: %v\n%s", err, output)
	}
	for _, table := range []string{"poi", "custom_poi"} {
		if err := admin.QueryRow("SELECT COUNT(*) FROM `" + dbName + "`.`" + table + "`").Scan(&pois); err != nil || pois != 5 {
			t.Fatalf("table %s changed after invalid name: count=%d error=%v", table, pois, err)
		}
	}
}

func addTestBlob(data []byte, kind string, payload []byte) []byte {
	blob := appendBytesField(nil, 1, payload)
	header := appendBytesField(nil, 1, []byte(kind))
	header = appendVarintField(header, 3, uint64(len(blob)))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(header)))
	data = append(data, length[:]...)
	data = append(data, header...)
	return append(data, blob...)
}
