package osm2mysql

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestAreaSpoolSortsAndDropsUnresolved(t *testing.T) {
	p := NewParser("", t.TempDir(), NewLogger(), 4, 4096, 2)
	p.selections = tagSelections{{"type", "area"}}
	spool, err := newAreaSpool(p)
	if err != nil {
		t.Fatal(err)
	}

	defer spool.file.Close()
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < 18000; i += 4 {
				id := int64(18000 - i)
				if err := spool.addWay(&Way{ID: id, Tags: map[string]string{"name": fmt.Sprintf("area-%d", id), "type": "area"}}); err != nil {
					t.Error(err)
				}
				if id%2 == 0 {
					spool.Add(id, 0, 0)
				}
			}
		}(worker)
	}
	wg.Wait()
	ch := make(chan csvRecord, 9000)
	pois, tags, err := spool.emit(ch)
	close(ch)
	if err != nil || pois != 9000 || tags != 18000 {
		t.Fatalf("emit = %d POIs, %d tags, %v", pois, tags, err)
	}
	for rec := range ch {
		var id int
		fields := strings.Split(strings.TrimSpace(string(rec.poiLine)), "\t")
		if len(fields) != 7 {
			t.Fatalf("incorrect POI output: %q", rec.poiLine)
		}
		if _, err := fmt.Sscanf(fields[1], "%d", &id); err != nil || id%2 != 0 {
			t.Fatalf("incorrect POI ID in %q: %v", rec.poiLine, err)
		}
		var tags map[string]string
		if err := json.Unmarshal([]byte(fields[6]), &tags); err != nil || tags["name"] != fmt.Sprintf("area-%d", id) {
			t.Fatalf("incorrect JSON tags in %q: %#v, %v", rec.poiLine, tags, err)
		}
	}
}

func TestAreaSpoolWildcardValues(t *testing.T) {
	p := NewParser("", t.TempDir(), NewLogger(), 2, 4096, 1)
	p.selections = tagSelections{{"geological", "*"}, {"geological", "cave"}}
	spool, err := newAreaSpool(p)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.file.Close()
	values := []string{"cave", "  rock\t\n ", "*", "", string([]byte{0xff}) + strings.Repeat("é", 201), string([]byte{0xff})}
	for i, value := range values {
		id := int64(i + 1)
		if err := spool.addWay(&Way{ID: id, Tags: map[string]string{"geological": value}}); err != nil {
			t.Fatal(err)
		}
		if value != "" {
			spool.Add(id, 0, 0)
		}
	}
	ch := make(chan csvRecord, len(values))
	pois, _, err := spool.emit(ch)
	close(ch)
	if err != nil || pois != 5 {
		t.Fatalf("emit = %d POIs, %v", pois, err)
	}
	expected := map[string]string{
		"1": "cave", "2": "  rock\t\n ", "3": "*", "5": strings.Repeat("é", 200), "6": "",
	}
	for rec := range ch {
		fields := strings.Split(strings.TrimSuffix(string(rec.poiLine), "\n"), "\t")
		if len(fields) != 7 {
			t.Fatalf("invalid row %q", rec.poiLine)
		}
		value, err := hex.DecodeString(fields[3])
		if err != nil || expected[fields[1]] != string(value) {
			t.Fatalf("unexpected selection value in %q: %v", rec.poiLine, err)
		}
		delete(expected, fields[1])
	}
	if len(expected) != 0 {
		t.Fatalf("missing rows: %v", expected)
	}
}
