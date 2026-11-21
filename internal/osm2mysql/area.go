package osm2mysql

// areaPoint unwraps antimeridian-crossing rings before planar validation and
// point selection, then returns the longitude to the usual -180..180 range.
func areaPoint(verts []coord) (int32, int32, bool) {
	const halfWorld int64 = 1800000000
	const world = 2 * halfWorld
	crosses := false
	for i := 1; i < len(verts); i++ {
		delta := int64(verts[i].lonE7) - int64(verts[i-1].lonE7)
		if delta > halfWorld || delta < -halfWorld {
			crosses = true
			break
		}
	}
	if !crosses {
		if !validArea(verts) {
			return 0, 0, false
		}
		return pointOnSurface(verts)
	}
	lons := make([]int64, len(verts))
	lons[0] = int64(verts[0].lonE7)
	minLon, maxLon := lons[0], lons[0]
	for i := 1; i < len(verts); i++ {
		delta := int64(verts[i].lonE7) - int64(verts[i-1].lonE7)
		if delta > halfWorld {
			delta -= world
		} else if delta < -halfWorld {
			delta += world
		}
		lons[i] = lons[i-1] + delta
		minLon, maxLon = min(minLon, lons[i]), max(maxLon, lons[i])
	}
	if lons[0] != lons[len(lons)-1] || maxLon-minLon > (1<<32)-1 {
		return 0, 0, false
	}
	offset := minLon + (maxLon-minLon+1)/2
	unwrapped := make([]coord, len(verts))
	for i := range verts {
		unwrapped[i] = coord{latE7: verts[i].latE7, lonE7: int32(lons[i] - offset)}
	}
	if !validArea(unwrapped) {
		return 0, 0, false
	}
	lat, lon, ok := pointOnSurface(unwrapped)
	if !ok {
		return 0, 0, false
	}
	wrapped := (int64(lon) + offset) % world
	if wrapped > halfWorld {
		wrapped -= world
	} else if wrapped < -halfWorld {
		wrapped += world
	}
	return lat, int32(wrapped), true
}

// validArea rejects degenerate rings and intersecting non-adjacent edges.
func validArea(v []coord) bool {
	if len(v) < 4 || v[0] != v[len(v)-1] {
		return false
	}
	if _, _, ok := shoelaceCentroid(v); !ok {
		return false
	}
	edges := len(v) - 1
	for i := 0; i < edges; i++ {
		if v[i] == v[i+1] {
			return false
		}
		for j := i + 1; j < edges; j++ {
			if j == i+1 || (i == 0 && j == edges-1) {
				continue
			}
			if segmentsIntersect(v[i], v[i+1], v[j], v[j+1]) {
				return false
			}
		}
	}
	return true
}

func segmentsIntersect(a, b, c, d coord) bool {
	cross := func(a, b, c coord) float64 {
		return (float64(b.lonE7)-float64(a.lonE7))*(float64(c.latE7)-float64(a.latE7)) -
			(float64(b.latE7)-float64(a.latE7))*(float64(c.lonE7)-float64(a.lonE7))
	}
	on := func(a, b, c coord) bool {
		return c.lonE7 >= min(a.lonE7, b.lonE7) && c.lonE7 <= max(a.lonE7, b.lonE7) &&
			c.latE7 >= min(a.latE7, b.latE7) && c.latE7 <= max(a.latE7, b.latE7)
	}
	x, y, z, w := cross(a, b, c), cross(a, b, d), cross(c, d, a), cross(c, d, b)
	return (x == 0 && on(a, b, c)) || (y == 0 && on(a, b, d)) ||
		(z == 0 && on(c, d, a)) || (w == 0 && on(c, d, b)) ||
		((x < 0 && y > 0 || x > 0 && y < 0) && (z < 0 && w > 0 || z > 0 && w < 0))
}
