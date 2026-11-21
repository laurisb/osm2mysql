# osm2mysql: architecture

`osm2mysql` selects one or more OSM tag key/value pairs from a `Type_then_ID`
sorted PBF and writes matching node and area POIs to MySQL. The parser checks the header
for `Sort.Type_then_ID` before starting. Relations and open ways are ignored.
Closed ways are treated as areas unless tagged `area=no`.

Tag keys are lowercased; values are matched exactly, including case, except
that `*` selects any non-empty value. Output retains all tags, subject to
UTF-8 cleanup, control-character removal and the 50-character key and
200-character value limits. Wildcard rows retain the actual selected value
(with invalid UTF-8 removed and a 200-character limit).

## Pipeline

1. **Discover:** Probe sorted PBF block boundaries, index node ID ranges, and decode ways in parallel,
   select matching closed ways, spool their tags and selected pairs, and externally sort their
   node references and way spans. No relations are decoded. Referenced nodes
   need not match the selected tag; mixed-type boundary blocks are retained.
2. **Join:** Use node block counts to balance
   contiguous reference buckets across workers, decode disjoint node-ID ranges,
   and merge their coordinates with sorted references. Independently matching
   nodes are emitted as POIs with their own coordinates.
3. **Resolve:** Sort resolved vertices by way and sequence; reject incomplete,
   degenerate or self-intersecting rings, and calculate a point on the surface
   of each valid area. Antimeridian-crossing rings are unwrapped before
   validation and point selection. Counts of dropped incomplete and invalid
   areas are displayed after parsing.
4. **Emit:** Merge the sorted resolved area points with the selected way tag
   index and read their tags from the spool. Invalid areas produce no POI.
   One row per matching element and actual tag pair, with JSON tags, is written
   to CSV shards; overlapping exact and wildcard selections are deduplicated.
   Geometry is resolved once per area.
5. **Import:** Bulk-load CSVs and create indexes.

Compressed PBF blobs use the `klauspost/compress` zlib inflater. The declared
uncompressed size is only a capped allocation hint, not trusted as a limit.

IDs use the non-negative signed 64-bit range. Negative IDs, negative way
references and overflowing ID deltas are rejected. The node reference buckets are
sized from the input's indexed node ID range; references beyond that range
are placed in the final bucket and remain unresolved if absent. Coordinates
are E7 fixed-point during geometry processing. The join uses
bounded external sorts instead of a planet-wide coordinate map. Area points
and tag offsets use compact sorted records; each area's serialized JSON tags
and wildcard-selected values are stored in a separate spool until geometry is resolved. The startup
scratch estimate is based on input size and sort
memory, not a guarantee of peak space.

## Database

The output table defaults to `poi` and is configurable with `-table-name`.
It contains type, ID, selected tag name and value, latitude,
longitude, and JSON tags. The primary key includes the output pair, whose
binary collation preserves its stored identity, including case and trailing
spaces. The input filename is not stored.
Existing unowned output tables are rejected; ownership is recorded in the table
comment. A MySQL advisory lock serializes imports into a database. Each import
recreates the entire table before parsing. See [README.md](README.md) for flags
and queries.
