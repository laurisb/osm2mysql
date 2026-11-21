package osm2mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type Importer struct {
	log      *Logger
	dsn      string
	dsnBase  string
	dbName   string
	table    string
	lockConn *sql.Conn
	lockDB   *sql.DB
	lockName string
}

const poiTable = "poi"
const poiTableOwner = "osm2mysql:poi"

func validateTableName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("table name must contain 1 to 64 ASCII letters, digits or underscores")
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		default:
			return fmt.Errorf("invalid table name %q: only ASCII letters, digits and underscores are supported", name)
		}
	}
	return nil
}

func NewImporter(log *Logger, host string, port int, user, pass, dbName, table string) *Importer {
	dsnBase := fmt.Sprintf("%s:%s@tcp(%s:%d)/?charset=utf8mb4&parseTime=true", user, pass, host, port)
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&allowAllFiles=true", user, pass, host, port, dbName)

	return &Importer{
		log:     log,
		dsn:     dsn,
		dsnBase: dsnBase,
		dbName:  dbName,
		table:   table,
	}
}

func (imp *Importer) CreateDatabase() error {
	db, err := sql.Open("mysql", imp.dsnBase)
	if err != nil {
		return fmt.Errorf("open MySQL connection: %w", err)
	}
	defer db.Close()

	_, err = db.Exec(fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
		imp.dbName,
	))
	if err != nil {
		return fmt.Errorf("create database %q: %w", imp.dbName, err)
	}

	imp.log.Log(fmt.Sprintf("Database '%s' ready", imp.dbName))
	return nil
}

func (imp *Importer) CreateTables() error {
	imp.log.Log("Creating tables")

	db, err := sql.Open("mysql", imp.dsn)
	if err != nil {
		return fmt.Errorf("open database connection: %w", err)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return fmt.Errorf("connect to database %q: %w", imp.dbName, err)
	}
	lock := fmt.Sprintf("osm2mysql:%x", sha256.Sum256([]byte(strings.ToLower(imp.dbName))))[:64]
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 30)", lock).Scan(&acquired); err != nil {
		conn.Close()
		db.Close()
		return fmt.Errorf("acquire import lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		conn.Close()
		db.Close()
		return fmt.Errorf("acquire import lock: timed out waiting for another osm2mysql run on database %q", imp.dbName)
	}
	imp.lockConn, imp.lockDB, imp.lockName = conn, db, lock
	var comment string
	err = conn.QueryRowContext(ctx, "SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", imp.table).Scan(&comment)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect table %s: %w", imp.table, err)
	}
	if err == nil && comment != poiTableOwner {
		return fmt.Errorf("refusing to overwrite table %s: it is not owned by osm2mysql", imp.table)
	}
	stmts := []string{
		"DROP TABLE IF EXISTS `" + imp.table + "`",
		"CREATE TABLE `" + imp.table + "` (" + `
			osm_type ENUM('node', 'way') NOT NULL,
			osm_id BIGINT UNSIGNED NOT NULL,
			tag_name VARCHAR(50) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
			tag_value VARCHAR(200) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
			lat DECIMAL(8, 6) NOT NULL,
			lon DECIMAL(9, 6) NOT NULL,
			tags JSON NOT NULL
		) ENGINE=INNODB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='` + poiTableOwner + "'",
	}

	for _, s := range stmts {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("create table %s: %w", imp.table, err)
		}
	}
	return nil
}

func (imp *Importer) Close() error {
	if imp.lockConn == nil {
		return nil
	}
	var released sql.NullInt64
	err := imp.lockConn.QueryRowContext(context.Background(), "SELECT RELEASE_LOCK(?)", imp.lockName).Scan(&released)
	closeErr := imp.lockConn.Close()
	dbErr := imp.lockDB.Close()
	imp.lockConn = nil
	if err != nil {
		return err
	}
	if !released.Valid || released.Int64 != 1 {
		return fmt.Errorf("import lock was not held")
	}
	if closeErr != nil {
		return closeErr
	}
	return dbErr
}

func (imp *Importer) Import(poiFiles []string) error {
	imp.log.Log("Importing data")
	start := time.Now()

	// Launch one goroutine per file (each with its own MySQL connection)
	// for maximum parallel LOAD DATA throughput.
	type importJob struct {
		table string
		file  string
	}

	var jobs []importJob
	for _, f := range poiFiles {
		jobs = append(jobs, importJob{imp.table, f})
	}
	done := make(chan error, len(jobs))

	ctx := context.Background()

	for _, job := range jobs {
		go func(table, file string) {
			db, err := sql.Open("mysql", imp.dsn)
			if err != nil {
				done <- err
				return
			}
			defer db.Close()

			// Pin SETs + LOAD DATA + COMMIT to a single connection: SET autocommit=0
			// must apply to the same session that runs LOAD DATA, but *sql.DB is a pool.
			conn, err := db.Conn(ctx)
			if err != nil {
				done <- fmt.Errorf("acquire conn for %s (%s): %w", table, file, err)
				return
			}
			defer conn.Close()

			if _, err := conn.ExecContext(ctx, "SET autocommit = 0"); err != nil {
				done <- fmt.Errorf("SET autocommit for %s (%s): %w", table, file, err)
				return
			}
			if _, err := conn.ExecContext(ctx, "SET unique_checks = 0"); err != nil {
				done <- fmt.Errorf("SET unique_checks for %s (%s): %w", table, file, err)
				return
			}
			if _, err := conn.ExecContext(ctx, "SET foreign_key_checks = 0"); err != nil {
				done <- fmt.Errorf("SET foreign_key_checks for %s (%s): %w", table, file, err)
				return
			}

			absPath, _ := filepath.Abs(file)
			absPath = filepath.ToSlash(absPath)

			mysql.RegisterLocalFile(absPath)
			defer mysql.DeregisterLocalFile(absPath)

			_, err = conn.ExecContext(ctx, fmt.Sprintf(
				"LOAD DATA LOCAL INFILE '%s' INTO TABLE `%s` CHARACTER SET utf8mb4 FIELDS TERMINATED BY '\\t' ESCAPED BY '' LINES TERMINATED BY '\\n' (osm_type, osm_id, @tag_name, @tag_value, lat, lon, tags) SET tag_name = CONVERT(UNHEX(@tag_name) USING utf8mb4), tag_value = CONVERT(UNHEX(@tag_value) USING utf8mb4)",
				strings.ReplaceAll(absPath, "'", "\\'"),
				table,
			))

			if err != nil {
				done <- fmt.Errorf("LOAD DATA failed for %s (%s): %w", table, file, err)
				return
			}

			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				done <- fmt.Errorf("COMMIT for %s (%s): %w", table, file, err)
				return
			}
			os.Remove(file)
			done <- nil
		}(job.table, job.file)
	}

	var firstErr error
	for range jobs {
		if err := <-done; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return fmt.Errorf("import data: %w", firstErr)
	}

	imp.log.Log(fmt.Sprintf("Import completed in %s", formatDuration(time.Since(start))))
	return nil
}

func (imp *Importer) CreateIndexes() error {
	imp.log.Log("Creating indexes")
	start := time.Now()

	stmts := []string{
		"ALTER TABLE `" + imp.table + "` ADD PRIMARY KEY (`osm_type`, `osm_id`, `tag_name`, `tag_value`), ADD KEY `idx_lat_lon` (`lat`, `lon`)",
	}

	done := make(chan error, len(stmts))

	for _, s := range stmts {
		go func(stmt string) {
			db, err := sql.Open("mysql", imp.dsn)
			if err != nil {
				done <- err
				return
			}
			defer db.Close()

			_, err = db.Exec(stmt)
			done <- err
		}(s)
	}

	var firstErr error
	for range stmts {
		if err := <-done; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return fmt.Errorf("create indexes: %w", firstErr)
	}

	imp.log.Log(fmt.Sprintf("Indexing completed in %s", formatDuration(time.Since(start))))
	return nil
}
