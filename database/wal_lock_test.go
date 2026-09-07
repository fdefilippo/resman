package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDocumentedLiveSQLiteInspectionIsReadOnly(t *testing.T) {
	data, err := os.ReadFile("../docs/METRICS-DATABASE.md")
	if err != nil {
		t.Fatal(err)
	}
	commands := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "sqlite3 ") {
			commands++
			if !strings.HasPrefix(line, "sqlite3 -readonly ") {
				t.Errorf("live inspection example opens a writable connection: %s", line)
			}
		}
	}
	if commands == 0 {
		t.Fatal("no direct inspection commands were verified")
	}
}

// TestSQLiteExternalClientHelper uses a separate process so SQLite's internal
// same-process connection tracking cannot conceal lost POSIX locks.
func TestSQLiteExternalClientHelper(t *testing.T) {
	path := os.Getenv("RESMAN_SQLITE_EXTERNAL_CLIENT_DB")
	if path == "" {
		return
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_busy_timeout=5000"}
	db, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM system_metrics").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fmt.Println(count)
	os.Exit(0)
}

func externalSQLiteCount(t *testing.T, path string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if client := os.Getenv("RESMAN_TEST_SQLITE3_CLIENT"); client != "" {
		cmd = exec.CommandContext(ctx, client, path, "SELECT COUNT(*) FROM system_metrics;")
	} else {
		cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteExternalClientHelper$")
		cmd.Env = append(os.Environ(), "RESMAN_SQLITE_EXTERNAL_CLIENT_DB="+path)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("external read-write SQLite client: %v: %s", err, output)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatalf("invalid external row count: %v: %s", err, output)
	}
	return count
}

func TestLiveSQLiteWALSurvivesExternalReadWriteInspection(t *testing.T) {
	path := privateTestDatabasePath(t, "metrics.db")
	manager, err := NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	var previousWAL, previousSHM os.FileInfo
	for cycle := 1; cycle <= 3; cycle++ {
		if err := manager.writeSystemMetricsForTest(&SystemMetricsRecord{Timestamp: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		wal, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		shm, err := os.Stat(path + "-shm")
		if err != nil {
			t.Fatal(err)
		}
		if previousWAL != nil && (!os.SameFile(wal, previousWAL) || !os.SameFile(shm, previousSHM)) {
			t.Fatal("live sidecars changed identity between writes")
		}
		if got := externalSQLiteCount(t, path); got != cycle {
			t.Fatalf("external history frozen: got %d rows, want %d before daemon close", got, cycle)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(path + suffix); err != nil {
				t.Fatalf("external reader removed live %s: %v", suffix, err)
			}
		}
		previousWAL, previousSHM = wal, shm
	}
}
