package osm2mysql

import (
	"math"
	"strings"
	"testing"
)

func TestIDDeltaBounds(t *testing.T) {
	for _, tc := range []struct {
		id, delta, want int64
		ok              bool
	}{
		{0, math.MaxInt64, math.MaxInt64, true},
		{math.MaxInt64, 1, 0, false},
		{0, math.MinInt64, math.MinInt64, true},
		{math.MinInt64, -1, 0, false},
		{math.MaxInt64, -math.MaxInt64, 0, true},
	} {
		if got, ok := addIDDelta(tc.id, tc.delta); got != tc.want || ok != tc.ok {
			t.Errorf("addIDDelta(%d, %d) = %d, %v; want %d, %v", tc.id, tc.delta, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDenseNodeIDOverflow(t *testing.T) {
	dense := appendBytesField(nil, 1, packedValues(zigzag(math.MaxInt64), zigzag(1)))
	block := appendBytesField(nil, 2, appendBytesField(nil, 2, dense))
	if _, _, err := nodeRange(block); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("nodeRange overflow error = %v", err)
	}
}

func TestWayReferenceInvalidIDs(t *testing.T) {
	table := []string{"", "historic", "castle"}
	matcher := newBlockTagMatcher(table, tagSelections{{"historic", "castle"}})
	for _, refs := range [][]uint64{
		{zigzag(-1)},
		{zigzag(math.MaxInt64), zigzag(1)},
	} {
		way := appendVarintField(nil, 1, 1)
		way = appendBytesField(way, 2, packedValues(1))
		way = appendBytesField(way, 3, packedValues(2))
		way = appendBytesField(way, 8, packedValues(refs...))
		if _, err := parseSelectedWay(way, table, matcher, nil); err == nil || !strings.Contains(err.Error(), "invalid node reference ID") {
			t.Fatalf("refs %v: error = %v", refs, err)
		}
	}
}
