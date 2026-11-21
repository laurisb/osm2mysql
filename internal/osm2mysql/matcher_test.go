package osm2mysql

import (
	"reflect"
	"testing"
)

func TestSelectedDenseTagsMatchMapSemantics(t *testing.T) {
	table := []string{"", "HISTORIC", "historic", "castle", "other", "name", "Castle"}
	dense := appendBytesField(nil, 1, packedValues(zigzag(1), zigzag(1), zigzag(1), zigzag(1)))
	dense = appendBytesField(dense, 8, packedValues(0, 0, 0, 0))
	dense = appendBytesField(dense, 9, packedValues(0, 0, 0, 0))
	dense = appendBytesField(dense, 10, packedValues(
		1, 3, 5, 6, 0, 1, 3, 2, 4, 0, 1, 4, 2, 3, 0, 5, 6, 0))
	matcher := newBlockTagMatcher(table, tagSelections{{key: "historic", value: "castle"}})
	var actual []map[string]string
	_, err := parseDense(dense, table, matcher, 100, 0, 0, func(_ int64, _, _ int32, tags map[string]string) {
		actual = append(actual, tags)
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := []map[string]string{
		{"historic": "castle", "name": "Castle"}, nil, {"historic": "castle"}, nil,
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("got %#v, want %#v", actual, expected)
	}
}

func TestSelectedNodesAndWaysMatchFullDecode(t *testing.T) {
	table := []string{"", "HISTORIC", "historic", "castle", "other", "name", "Castle", "area", "no", "yes"}
	cases := []struct {
		name       string
		keys, vals []uint64
		match      bool
	}{
		{"matching", []uint64{1, 5}, []uint64{3, 6}, true},
		{"last matching", []uint64{1, 2}, []uint64{4, 3}, true},
		{"last different", []uint64{1, 2}, []uint64{3, 4}, false},
		{"case sensitive value", []uint64{1}, []uint64{6}, false},
		{"previously excluded value", []uint64{1}, []uint64{9}, false},
		{"unmatched", []uint64{5}, []uint64{6}, false},
	}

	matcher := newBlockTagMatcher(table, tagSelections{{key: "historic", value: "castle"}})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := appendVarintField(nil, 1, zigzag(42))
			node = appendVarintField(node, 8, zigzag(123))
			node = appendVarintField(node, 9, zigzag(-456))
			node = appendBytesField(node, 2, packedValues(tc.keys...))
			node = appendBytesField(node, 3, packedValues(tc.vals...))
			id, lat, lon, full, err := parseNode(node, table, nil)
			if err != nil {
				t.Fatal(err)
			}
			gotID, gotLat, gotLon, selected, err := parseNode(node, table, matcher)
			if err != nil {
				t.Fatal(err)
			}
			if id != gotID || lat != gotLat || lon != gotLon {
				t.Fatalf("unmatched node geometry changed: %d %d %d", gotID, gotLat, gotLon)
			}
			if tc.match {
				if !reflect.DeepEqual(selected, full) {
					t.Fatalf("selected node tags = %#v, want %#v", selected, full)
				}
			} else if selected != nil {
				t.Fatalf("nonmatching node tags = %#v", selected)
			}
			way := appendVarintField(nil, 1, 7)
			way = appendBytesField(way, 2, packedValues(tc.keys...))
			way = appendBytesField(way, 3, packedValues(tc.vals...))
			way = appendBytesField(way, 8, packedValues(zigzag(1), zigzag(1), zigzag(-1)))
			gotWay, err := parseSelectedWay(way, table, matcher, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.match {
				if gotWay == nil || !reflect.DeepEqual(gotWay.Tags, full) || !reflect.DeepEqual(gotWay.NodeIDs, []int64{1, 2, 1}) {
					t.Fatalf("selected way = %#v, full tags %#v", gotWay, full)
				}
			} else if gotWay != nil {
				t.Fatalf("nonmatching way = %#v", gotWay)
			}
		})
	}
	if matcher.matches([]uint64{1, 2}, []uint64{3, 100}) != true {
		t.Fatal("invalid later tag must not override a valid tag")
	}
	yesMatcher := newBlockTagMatcher(table, tagSelections{{key: "historic", value: "yes"}})
	if !yesMatcher.matches([]uint64{1}, []uint64{9}) {
		t.Fatal("exact selection must not apply the old value filter")
	}
}

func TestMultiPairMatcherLastKeyWins(t *testing.T) {
	table := []string{"", "HISTORIC", "historic", "castle", "other", "tourism", "viewpoint"}
	matcher := newBlockTagMatcher(table, tagSelections{{"historic", "castle"}, {"tourism", "viewpoint"}})
	for _, tc := range []struct {
		keys, values []uint64
		want         bool
	}{
		{[]uint64{1, 2}, []uint64{3, 4}, false},
		{[]uint64{1, 2, 5}, []uint64{3, 4, 6}, true},
		{[]uint64{5, 5, 1}, []uint64{6, 4, 3}, true},
		{[]uint64{5, 5, 1, 2}, []uint64{6, 4, 3, 4}, false},
	} {
		if got := matcher.matches(tc.keys, tc.values); got != tc.want {
			t.Fatalf("matches(%v, %v) = %v, want %v", tc.keys, tc.values, got, tc.want)
		}

		interleaved := make([]uint64, 0, len(tc.keys)*2)
		for i := range tc.keys {
			interleaved = append(interleaved, tc.keys[i], tc.values[i])
		}
		if got := matcher.matchesStride(interleaved, interleaved[1:], 2); got != tc.want {
			t.Fatalf("dense matches(%v) = %v, want %v", interleaved, got, tc.want)
		}
	}
}

func TestMultiPairDenseSelection(t *testing.T) {
	table := []string{"", "HISTORIC", "historic", "castle", "other", "tourism", "viewpoint"}
	dense := appendBytesField(nil, 1, packedValues(zigzag(1), zigzag(1), zigzag(1), zigzag(1)))
	dense = appendBytesField(dense, 8, packedValues(0, 0, 0, 0))
	dense = appendBytesField(dense, 9, packedValues(0, 0, 0, 0))
	dense = appendBytesField(dense, 10, packedValues(
		1, 3, 5, 6, 0,
		1, 3, 2, 4, 0,
		1, 3, 2, 4, 5, 6, 0,
		5, 6, 5, 4, 0))
	matcher := newBlockTagMatcher(table, tagSelections{{"historic", "castle"}, {"tourism", "viewpoint"}})
	var actual []map[string]string
	_, err := parseDense(dense, table, matcher, 100, 0, 0, func(_ int64, _, _ int32, tags map[string]string) {
		actual = append(actual, tags)
	})
	if err != nil {
		t.Fatal(err)
	}

	expected := []map[string]string{
		{"historic": "castle", "tourism": "viewpoint"}, nil,
		{"historic": "other", "tourism": "viewpoint"}, nil,
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("got %#v, want %#v", actual, expected)
	}
}

func TestWildcardMatcher(t *testing.T) {
	table := []string{"", "GEOLOGICAL", "geological", "cave", "peak", "*", "name"}
	matcher := newBlockTagMatcher(table, tagSelections{{"geological", "*"}})
	for _, tc := range []struct {
		keys, values []uint64
		want         bool
	}{
		{[]uint64{1}, []uint64{3}, true},
		{[]uint64{1}, []uint64{5}, true},
		{[]uint64{1}, []uint64{0}, false},
		{[]uint64{6}, []uint64{3}, false},
		{[]uint64{1, 2}, []uint64{3, 0}, false},
		{[]uint64{1, 2}, []uint64{0, 4}, true},
	} {
		if got := matcher.matches(tc.keys, tc.values); got != tc.want {
			t.Fatalf("matches(%v, %v) = %v, want %v", tc.keys, tc.values, got, tc.want)
		}
		interleaved := make([]uint64, 0, len(tc.keys)*2)
		for i := range tc.keys {
			interleaved = append(interleaved, tc.keys[i], tc.values[i])
		}
		if got := matcher.matchesStride(interleaved, interleaved[1:], 2); got != tc.want {
			t.Fatalf("dense matches(%v) = %v, want %v", interleaved, got, tc.want)
		}
	}
}
