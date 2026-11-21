package osm2mysql

import (
	"slices"
	"strings"
	"unicode/utf8"
)

const csvShards = 4

// coord is a single resolved node position in E7 fixed-point.
type coord struct {
	latE7, lonE7 int32
}

// pointOnSurface returns a point guaranteed to lie inside (or on the boundary
// of) a simple closed polygon defined by verts (where verts[0]==verts[len-1]).
// Algorithm:
//  1. Compute the signed-area-weighted centroid (shoelace).
//  2. Cast a horizontal scan line through that centroid's latitude.
//  3. Collect every edge-scanline intersection (lon coords).
//  4. Pair them in sort order; each pair brackets an interior segment.
//  5. Return the midpoint of the longest such segment.
//
// Returns ok=false when the polygon is degenerate (zero signed area).
func pointOnSurface(verts []coord) (latE7, lonE7 int32, ok bool) {
	cLat, cLon, areaOK := shoelaceCentroid(verts)
	if !areaOK {
		return 0, 0, false
	}

	scan := float64(cLat)
	var xs []float64
	n := len(verts)
	for i := 0; i < n-1; i++ {
		y1 := float64(verts[i].latE7)
		y2 := float64(verts[i+1].latE7)
		if (y1 <= scan && y2 > scan) || (y2 <= scan && y1 > scan) {
			x1 := float64(verts[i].lonE7)
			x2 := float64(verts[i+1].lonE7)
			t := (scan - y1) / (y2 - y1)
			xs = append(xs, x1+t*(x2-x1))
		}
	}
	if len(xs) < 2 {
		return cLat, cLon, true
	}
	slices.Sort(xs)
	var bestStart, bestEnd float64
	var bestLen float64
	for i := 0; i+1 < len(xs); i += 2 {
		seg := xs[i+1] - xs[i]
		if seg > bestLen {
			bestLen = seg
			bestStart = xs[i]
			bestEnd = xs[i+1]
		}
	}
	return cLat, int32((bestStart + bestEnd) / 2), true
}

// shoelaceCentroid returns the signed-area-weighted centroid of a closed
// polygon. ok=false when signed area is zero (degenerate / collinear).
func shoelaceCentroid(verts []coord) (latE7, lonE7 int32, ok bool) {
	var sumA, sumCx, sumCy float64
	n := len(verts)
	if n == 0 {
		return 0, 0, false
	}
	x0, y0 := float64(verts[0].lonE7), float64(verts[0].latE7)
	for i := 0; i < n-1; i++ {
		x1 := float64(verts[i].lonE7) - x0
		y1 := float64(verts[i].latE7) - y0
		x2 := float64(verts[i+1].lonE7) - x0
		y2 := float64(verts[i+1].latE7) - y0
		cross := x1*y2 - x2*y1
		sumA += cross
		sumCx += (x1 + x2) * cross
		sumCy += (y1 + y2) * cross
	}
	if sumA == 0 {
		return 0, 0, false
	}
	cx := x0 + sumCx/(3*sumA)
	cy := y0 + sumCy/(3*sumA)
	return int32(cy), int32(cx), true
}

type csvRecord struct {
	poiLine []byte
}

type Parser struct {
	pbfFile         string
	tempDir         string
	log             *Logger
	threads         int
	sortMem         int64
	centroidWorkers int
	selections      tagSelections
	incompleteAreas int64
	invalidAreas    int64

	poiFiles []string
}

func NewParser(pbf, tempDir string, log *Logger, threads int, sortMem int64, centroidWorkers int) *Parser {
	if centroidWorkers < 1 {
		centroidWorkers = 1
	}
	return &Parser{
		pbfFile:         pbf,
		tempDir:         tempDir,
		log:             log,
		threads:         threads,
		sortMem:         sortMem,
		centroidWorkers: centroidWorkers,
	}
}

func (p *Parser) PoiFiles() []string { return p.poiFiles }

func truncateUTF8(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	count := 0
	for i := range s {
		count++
		if count > maxChars {
			return s[:i]
		}
	}
	return s
}

func sanitizeKey(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	// Keys arrive already lowercased by the PBF decoders.
	s = stripControlChars(s)
	s = truncateUTF8(s, 50)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return s
}

func sanitizeValue(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = stripControlChars(s)
	s = truncateUTF8(s, 200)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

func stripControlChars(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\t' || r == '\n' || r >= 0x20 {
			b.WriteRune(r)
		}
	}
	if b.Len() == len(s) {
		return s
	}
	return b.String()
}

// Parse runs the bounded selected-geometry pipeline. It returns the first
// I/O, PBF or geometry error encountered; partial scratch files are left for
// the caller to remove.
func (p *Parser) Parse() error {
	return p.parseRequiredNodes()
}
