package osm2mysql

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestMySQLUnifiedTable(t *testing.T) {
	if os.Getenv("OSM2MYSQL_TEST_MYSQL") != "1" {
		t.Skip("set OSM2MYSQL_TEST_MYSQL=1 to test against a local MySQL")
	}
	dbName, _ := newTestDatabase(t)
	db, err := mysqlTestConn(t, dbName)
	if err != nil {
		t.Fatal(err)
	}

	defer db.Close()
	if _, err := db.Exec("CREATE TABLE unrelated_data (id INT) COMMENT='unowned'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO unrelated_data VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE poi (id INT)"); err != nil {
		t.Fatal(err)
	}
	collision := mysqlTestImporter(t, dbName, poiTable)
	if err := collision.CreateTables(); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("expected ownership collision, got %v", err)
	}
	if err := collision.Close(); err != nil {
		t.Error(err)
	}
	if _, err := db.Exec("DROP TABLE poi"); err != nil {
		t.Fatal(err)
	}
	create := func(table string, pairs []tagSelection, id int) {
		t.Helper()
		imp := mysqlTestImporter(t, dbName, table)
		defer func() {
			if err := imp.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := imp.CreateTables(); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		poi := filepath.Join(dir, "poi.csv")
		var data []byte
		for _, pair := range pairs {
			line, _ := formatPOILine("node", int64(id), 1.5, 2.5, pair, map[string]string{
				pair.key: pair.value,
				"name":   "  Q\t\" \\\n Café  ",
			})
			data = append(data, line...)
		}
		if err := os.WriteFile(poi, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := imp.Import([]string{poi}); err != nil {
			t.Fatal(err)
		}
		if err := imp.CreateIndexes(); err != nil {
			t.Fatal(err)
		}
	}
	create(poiTable, []tagSelection{{"historic", "castle"}, {"tourism", "viewpoint"}}, 1)
	create("my_poi", []tagSelection{{"tourism", "viewpoint"}}, 2)
	create(poiTable, []tagSelection{{"historic", "castle"}, {"historic", "Castle"}, {"historic", "castle "}, {"note", "Café\t=ok\n"}}, 3)
	var count int
	var value string
	if err := db.QueryRow("SELECT COUNT(*) FROM poi WHERE osm_id=3").Scan(&count); err != nil || count != 4 {
		t.Fatalf("recreated table rows = %d, error = %v; want 4", count, err)
	}
	if err := db.QueryRow("SELECT tag_value FROM poi WHERE tag_name='note'").Scan(&value); err != nil || value != "Café\t=ok\n" {
		t.Fatalf("exact tag value = %q, error = %v", value, err)
	}
	if err := db.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(tags, '$.name')) FROM poi LIMIT 1").Scan(&value); err != nil || value != "Q \" \\  Café" {
		t.Fatalf("escaped JSON tag = %q, error = %v", value, err)
	}
	if _, err := db.Exec("INSERT INTO poi SELECT * FROM poi LIMIT 1"); err == nil {
		t.Fatal("duplicate element and pair should violate the primary key")
	}
	if err := db.QueryRow("SELECT id FROM unrelated_data").Scan(&count); err != nil || count != 7 {
		t.Fatalf("unrelated table was changed: id=%d, error=%v", count, err)
	}
	if err := db.QueryRow("SELECT osm_id FROM my_poi").Scan(&count); err != nil || count != 2 {
		t.Fatalf("other imported table was changed: id=%d, error=%v", count, err)
	}
	if _, err := db.Exec("CREATE TABLE protected_table (id INT)"); err != nil {
		t.Fatal(err)
	}
	protected := mysqlTestImporter(t, dbName, "protected_table")
	if err := protected.CreateTables(); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("expected custom table ownership collision, got %v", err)
	}
	if err := protected.Close(); err != nil {
		t.Error(err)
	}
}

func newTestDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("osm2mysql_test_%x", suffix)
	admin, err := mysqlTestConn(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "`"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + dbName + "`"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		admin.Close()
	})
	return dbName, admin
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func mysqlTestAddr(t *testing.T) (host string, port int) {
	t.Helper()
	host = envOrDefault("OSM2MYSQL_TEST_HOST", "127.0.0.1")
	portText := envOrDefault("OSM2MYSQL_TEST_PORT", "3306")
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("OSM2MYSQL_TEST_PORT=%q: %v", portText, err)
	}
	return host, port
}

func mysqlTestDSN(t *testing.T, dbName string) string {
	t.Helper()
	host, port := mysqlTestAddr(t)
	cfg := mysql.NewConfig()
	cfg.User = envOrDefault("OSM2MYSQL_TEST_USER", "root")
	cfg.Passwd = os.Getenv("OSM2MYSQL_TEST_PASS")
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	cfg.DBName = dbName
	cfg.AllowAllFiles = true
	return cfg.FormatDSN()
}

func mysqlTestConn(t *testing.T, dbName string) (*sql.DB, error) {
	t.Helper()
	return sql.Open("mysql", mysqlTestDSN(t, dbName))
}

func mysqlTestImporter(t *testing.T, dbName, table string) *Importer {
	t.Helper()
	host, port := mysqlTestAddr(t)
	user := envOrDefault("OSM2MYSQL_TEST_USER", "root")
	pass := os.Getenv("OSM2MYSQL_TEST_PASS")
	return NewImporter(NewLogger(), host, port, user, pass, dbName, table)
}
