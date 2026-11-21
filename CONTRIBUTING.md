# osm2mysql: contributing

The module has two packages: `cmd/osm2mysql` (CLI entry point) and
`internal/osm2mysql` (importer).

## Prerequisites

- Go 1.25 or newer.
- MySQL 8+ with `local_infile=1`; optional, only for integration tests and
  manual imports.
- curl, to download a test extract.

## Build and test

Run these from the repository root. No Makefile or shell scripts are required;
every command works in PowerShell, CMD and POSIX shells.

| Command | Purpose |
|---|---|
| `go build ./...` | Compile all packages. |
| `go build -o osm2mysql.exe ./cmd/osm2mysql` | Build the CLI on Windows. |
| `go build -o osm2mysql ./cmd/osm2mysql` | Build the CLI on macOS/Linux. |
| `go run ./cmd/osm2mysql -version` | Build and run the version check. |
| `go test ./...` | Run all tests. No PBF, database or network needed. |
| `go vet ./...` | Run Go's static analysis checks. |
| `gofmt -l .` | List unformatted files; the output must be empty. |
| `go mod tidy` | Reconcile `go.mod` and `go.sum`. |

The parser tests build small PBF blocks in memory, so `go test ./...` works
offline.

Builds are pure Go (`CGO_ENABLED=0`), so cross-compiling only requires setting
`GOOS` and `GOARCH`. Release builds inject the version with
`-ldflags "-X main.version=1.2.3"`.

## Manual end-to-end run

Download a small extract (see the README) and import one tag pair:

```sh
curl -L -o latvia.osm.pbf https://download.geofabrik.de/europe/latvia/latvia-latest.osm.pbf
go run ./cmd/osm2mysql -pbf latvia.osm.pbf -tag tourism=viewpoint
```

MySQL must be running on `localhost:3306` and allow `LOAD DATA LOCAL INFILE`.
Do not commit downloaded `.osm.pbf` files.

## MySQL integration tests

`importer_integration_test.go` is skipped unless `OSM2MYSQL_TEST_MYSQL=1`. The
tests connect to `127.0.0.1:3306` as `root` with an empty password by default,
create a random `osm2mysql_test_*` database and drop only that database
afterwards. Override the connection with `OSM2MYSQL_TEST_HOST`,
`OSM2MYSQL_TEST_PORT`, `OSM2MYSQL_TEST_USER` and `OSM2MYSQL_TEST_PASS`.

PowerShell:

```powershell
$env:OSM2MYSQL_TEST_MYSQL = "1"
go test ./internal/osm2mysql
```

CMD:

```bat
set OSM2MYSQL_TEST_MYSQL=1
go test ./internal/osm2mysql
```

macOS/Linux:

```sh
OSM2MYSQL_TEST_MYSQL=1 go test ./internal/osm2mysql
```

## Linting

`gofmt -l .` must print nothing and `go vet ./...` must pass. `golangci-lint`
runs the same checks as CI: errcheck, govet, ineffassign, revive, staticcheck,
unused, gofmt and goimports.

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
golangci-lint run
```

## Pull requests

- Keep changes focused and update tests when behavior changes.
- Run `gofmt -l .`, `go vet ./...` and `go test ./...` before pushing.
- Contributions are accepted under the MIT license in [LICENSE.md](LICENSE.md).
