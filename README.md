# osm2mysql: fast OpenStreetMap POI importer for MySQL

[![CI](https://github.com/laurisb/osm2mysql/actions/workflows/ci.yml/badge.svg)](https://github.com/laurisb/osm2mysql/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/laurisb/osm2mysql)](https://github.com/laurisb/osm2mysql/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/laurisb/osm2mysql)](go.mod)
[![License](https://img.shields.io/github/license/laurisb/osm2mysql)](LICENSE.md)

`osm2mysql` turns an OpenStreetMap data file into a MySQL table you can query, from a city extract to the full planet. Pick the features you want, such as eateries, viewpoints or charging stations, and get their names, coordinates and tags as rows.

OpenStreetMap is a free map of the world built from tagged objects. A point or area is described by `key=value` pairs such as `amenity=cafe` or `historic=castle`. `osm2mysql` reads an OSM PBF file, keeps the objects that match the pairs you pass with `-tag`, and loads them into MySQL.

It is a single static binary with no runtime dependencies, and it streams the data through disk instead of requiring it all to fit in memory. See [ARCHITECTURE.md](ARCHITECTURE.md) for how it works under the hood.

## What you get

- One MySQL table (default `poi`) with one row per matching element and selected pair.
- Matching nodes and closed ways. Open ways and relations are ignored.
- One representative point per closed way; invalid or incomplete areas are dropped and counted.
- All tags of the matched element in a JSON column, cleaned and length-limited.
- The target table is dropped and recreated on every run.

It is not a general-purpose OSM database, geocoder or router; it imports the pairs you select into one table.

## Output

For `germany.osm.pbf` with `-tag historic=castle -tag natural=peak`, one table named `poi`:

| Column | Type | Contains |
|---|---|---|
| `osm_type` | `ENUM('node','way')` | Element kind |
| `osm_id` | `BIGINT UNSIGNED` | OSM element ID |
| `tag_name` | `VARCHAR(50)` | Key of the selected pair that matched |
| `tag_value` | `VARCHAR(200)` | Value of that pair (the actual value when `*` was used) |
| `lat` | `DECIMAL(8,6)` | Latitude of the node or area point |
| `lon` | `DECIMAL(9,6)` | Longitude |
| `tags` | `JSON` | All retained tags of the element |

`tag_name` and `tag_value` are the filter columns: they record which `-tag`
pair selected the row. They are not the element's main tag or its `name` tag;
`tag_name` holds an OSM key such as `amenity`. An element matching several
selected pairs gets one row per pair, with the same coordinates and JSON tags
in each row. The primary key is `(osm_type, osm_id, tag_name, tag_value)`; group
by `(osm_type, osm_id)` when you want one map marker per element.

Tag keys are lowercased before matching; values are matched exactly and
case-sensitively, without splitting semicolon-separated values. Geometry
validation, ID limits and tag cleanup are covered in
[ARCHITECTURE.md](ARCHITECTURE.md).

Use a value of `*` to select any **non-empty** value for a key, for example
`-tag 'geological=*'` (quote it to prevent shell expansion). The output
`tag_value` contains the actual value, not `*`. Wildcard values are made valid
UTF-8 by removing invalid bytes and limited to 200 characters; otherwise their
original whitespace and case are preserved. `*` always means wildcard, so a
literal `*` cannot be selected separately. If a wildcard and an exact pair
match the same element and value, only one row is emitted.

After parsing, the log reports an `ERROR` for each selected pair that matched
no nodes or ways in the input. Matching open ways, ways tagged `area=no`, and
areas dropped for invalid or incomplete geometry count as matches even though
they do not produce POIs; relations do not count. These messages do not stop
the import or change its exit status.

Each run **drops and recreates the entire selected table** before parsing; supply
all the pairs you want in that run. Other tables are left intact. An existing
selected table without the importer's ownership comment is not overwritten. If
an import fails after table creation, the prior dataset has already been
removed and the new table may be incomplete.

## How it works

1. **Discover selected geometry:** spill node references for matching closed ways.
2. **Resolve geometry:** merge sorted references against ordered nodes; emit matching nodes and compute area points by concurrent way-ID ranges, then join them to spooled tags.
3. **Import:** bulk-load the resulting CSVs into MySQL and build indexes.

The sort-merge join is the key design choice: it avoids materializing a planet-wide coordinate index while keeping reference resolution bounded. See [ARCHITECTURE.md](ARCHITECTURE.md) for the pass-by-pass detail.

## Requirements

- **MySQL** (8+) with local-infile loading enabled (`local_infile=1`). The importer uses `LOAD DATA LOCAL INFILE`.
- **Disk** for scratch space (see [Disk space](#disk-space)).
- **OS**: Windows, macOS, or Linux (x64 or ARM64). No runtime dependencies; the binary is static.

OSM data must declare `Sort.Type_then_ID` in its PBF header. Sources include:

- [Planet OSM](https://planet.openstreetmap.org/): the whole world (~85 GB PBF).
- [Geofabrik](https://download.geofabrik.de/): continent, country and region extracts.

The same command imports any of these, including the full planet. A planet run
needs a long time and a large scratch directory; regional extracts are the
practical starting point.

### Downloading a PBF file

No PBF data ships with `osm2mysql`; download an extract for the area you need.
Geofabrik's Latvia extract is a good starting point (currently about 140 MB).

```sh
curl -L -o latvia.osm.pbf https://download.geofabrik.de/europe/latvia/latvia-latest.osm.pbf
```

## Installation

Download the binary for your platform from the [latest release](https://github.com/laurisb/osm2mysql/releases/latest) and unpack it. For example, on Linux x64:

```sh
curl -L https://github.com/laurisb/osm2mysql/releases/latest/download/osm2mysql_linux_amd64.tar.gz | tar xz
./osm2mysql -version
```

Or build/install with Go (1.25+):

```sh
go install github.com/laurisb/osm2mysql/cmd/osm2mysql@latest
```

## Quick start

1. Check that MySQL allows local data loading:

```sh
mysql -u root -e "SELECT @@local_infile"
```

It must return `1`. If it returns `0`, run `SET GLOBAL local_infile=1;` as root or enable it in the server configuration and restart MySQL.

2. Download an extract, as shown under [Requirements](#requirements).

3. Import every cafe in the file:

```sh
osm2mysql -pbf latvia.osm.pbf -tag amenity=cafe
```

4. Read a few rows:

```sql
SELECT osm_id, lat, lon, JSON_UNQUOTE(tags->'$.name') AS name
FROM poi
WHERE tag_name = 'amenity' AND tag_value = 'cafe'
LIMIT 5;
```

## Finding tags

Tags are documented on the [OpenStreetMap wiki](https://wiki.openstreetmap.org/wiki/Top-level_tag) and searchable at [taginfo](https://taginfo.openstreetmap.org/). Common starting points:

| Goal | `-tag` examples |
|---|---|
| Eateries | `amenity=cafe`, `amenity=restaurant`, `amenity=bar`, `amenity=biergarten`, `amenity=fast_food`, `amenity=ice_cream`, `amenity=pub` |
| Viewpoints | `tourism=viewpoint` |
| Pharmacies | `amenity=pharmacy` |
| Castles | `historic=castle` |
| EV chargers | `amenity=charging_station` |
| Anything with a wiki link | `wikipedia=*` |

## Usage

By default it connects to a local MySQL on `localhost:3306` as `root` with an empty password, using the `osm` database. Configure the connection when needed:

```sh
osm2mysql -db-host db.example.com -db-port 3306 -db-user me -db-name osm -pbf region.osm.pbf -tag tourism=museum
```

Choose a different output table with `-table-name`, for example
`-table-name map_poi`. Names must contain 1-64 ASCII letters, digits or
underscores. The default table is `poi`.

### Options

| Flag | Default | Purpose |
|---|---|---|
| `-pbf` | - | Path to the input PBF file (required) |
| `-tag` | - | Tag `name=value` to select; `*` matches non-empty values (required; repeatable) |
| `-db-host` | `localhost` | MySQL host |
| `-db-port` | `3306` | MySQL port |
| `-db-user` | `root` | MySQL user |
| `-db-pass` | (empty) | MySQL password |
| `-db-name` | `osm` | MySQL database |
| `-table-name` | `poi` | MySQL output table; recreated on each run |
| `-threads` | Go runtime parallelism | Decoder workers; `0` uses the Go runtime's `GOMAXPROCS` setting |
| `-sort-mem` | `0` (auto) | External-sort RAM budget in bytes; auto mode uses about 12.5% of system RAM (up to 8 GB), capped by PBF size; uses 2 GB if RAM detection fails |
| `-centroid-workers` | `0` (auto) | Concurrent way-ID ranges in Pass 3; auto mode scales with PBF size and is capped by CPU, RAM, and 8 workers |
| `-version` | `false` | Print version and exit |
| `-h` | - | Print usage and exit |

Exit codes: `0` success, `1` import failure, `2` invalid usage.

Any tag pair can select a POI, including address or metadata tags. Names and
values must be non-empty valid UTF-8 and at most 50 and 200 characters,
respectively. The first `=` separates the name and value; additional `=`
characters belong to the value. Quote arguments containing spaces or shell
meta characters. Repeated identical pairs are ignored after name lowercasing.
The selected pair is stored exactly as matched, while the JSON tags retain
the existing cleanup and length limits.

Increasing `-threads` may speed up decoding, but results depend on the input,
storage and CPU. Auto mode follows `GOMAXPROCS`, which can reflect the
environment's CPU limits or be set explicitly. Pass 2 uses at most one worker
per node-data block. More workers can increase memory and I/O pressure, so set
`-threads` lower if needed. `-sort-mem` controls sorting rather than decoder parallelism;
extra RAM is most useful when sorts spill to disk, not simply to fill unused
memory.

The parser always uses the required-node pipeline: it performs a discovery scan, indexes PBF node
blocks, joins node references concurrently over disjoint node-ID ranges, and computes centroids by concurrent way ranges.

## Disk space

`osm2mysql` writes scratch files to a `.osm2mysql/` folder next to the PBF, removed on a successful run. It warns if available disk space is below its conservative estimate based on PBF size and sort budget. Actual usage depends on the selected pairs.

The logged temporary directory size after parsing is a snapshot before CSV
loading, not peak scratch usage: intermediate files may already have been
removed.

## Querying

```sql
-- Cafés in a bounding box
SELECT p.osm_type, p.osm_id, p.lat, p.lon
FROM poi p
WHERE p.tag_name = 'amenity' AND p.tag_value = 'cafe'
  AND p.lat BETWEEN 50.88 AND 51.00
  AND p.lon BETWEEN 6.85 AND 7.05;
```

```sql
-- All tags for one POI
SELECT JSON_PRETTY(tags) FROM poi
WHERE osm_type = 'way' AND osm_id = 12345678
LIMIT 1;
```

```sql
-- Most common tag keys
SELECT k.name, COUNT(*) AS cnt
FROM poi p
JOIN JSON_TABLE(JSON_KEYS(p.tags), '$[*]' COLUMNS (name VARCHAR(50) PATH '$')) k
WHERE p.tag_name = 'amenity' AND p.tag_value = 'cafe'
GROUP BY k.name ORDER BY cnt DESC LIMIT 20;
```

The `tags` JSON column is not indexed by default. JSON extraction makes flexible tag storage and POI reads simple, but filtering by arbitrary tag keys can be slow. Add generated columns or functional indexes for tag paths that matter to your workload. The examples use plain `lat`/`lon` range queries, with no spatial columns or indexes, which keeps imports fast and the database small. If you later need high-performance spatial queries, add a `POINT` column and a spatial index:

```sql
ALTER TABLE poi ADD COLUMN geom POINT SRID 4326 NULL;
UPDATE poi SET geom = ST_SRID(POINT(lon, lat), 4326);
ALTER TABLE poi MODIFY COLUMN geom POINT SRID 4326 NOT NULL;
ALTER TABLE poi ADD SPATIAL INDEX idx_geom (geom);
```

## Performance

Times depend heavily on hardware (especially disk speed), database tuning, and file size.

For faster imports, tune MySQL: increase the InnoDB buffer pool and redo-log capacity, and consider disabling the doublewrite buffer and binary logging during the bulk load. Keep the PBF and the `.osm2mysql` scratch directory on a fast local disk, since sorting and merging spill to it.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build and test commands, the optional
MySQL integration tests, and lint expectations.

## License

[MIT](LICENSE.md)
