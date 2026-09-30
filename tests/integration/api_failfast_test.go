package integration

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/jobs"
)

func TestAPI_FailFastOnPortCollision(t *testing.T) {
	// Build the API binary before reserving a port so build time is not part of
	// the process startup observation.
	cmdBuild := exec.Command("go", "build", "-o", "../../bin/repolens-api", "../../cmd/api/main.go")
	if output, err := cmdBuild.CombinedOutput(); err != nil {
		t.Fatalf("failed to build API binary: %v\n%s", err, output)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve dynamic loopback port: %v", err)
	}
	defer listener.Close()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("read reserved port: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		t.Fatalf("invalid reserved port %q: %v", portText, err)
	}

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "api-failfast.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmdRun := exec.CommandContext(ctx, "../../bin/repolens-api")
	cmdRun.Env = append(os.Environ(),
		"HTTP_BIND_ADDR=127.0.0.1",
		"HTTP_PORT="+portText,
		"ENV=testing",
		"DB_DRIVER=sqlite",
		"DB_DSN="+dbPath,
		"SNAPSHOT_BASE_PATH="+filepath.Join(tempDir, "snapshots"),
		"PROVIDER_SECRET_PATH="+filepath.Join(tempDir, "provider.json"),
	)
	var output bytes.Buffer
	cmdRun.Stdout = &output
	cmdRun.Stderr = &output
	started := time.Now()
	err = cmdRun.Run()
	elapsed := time.Since(started)
	combinedOutput := output.String()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("API process reached the 5s context deadline after %s\n%s", elapsed, combinedOutput)
	}
	if err == nil {
		t.Fatalf("API unexpectedly stayed alive with occupied port %d", port)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("API exit = %v; want exit code 1\n%s", err, combinedOutput)
	}
	lowerOutput := strings.ToLower(combinedOutput)
	if !strings.Contains(lowerOutput, "address already in use") &&
		!(strings.Contains(lowerOutput, "bind") && strings.Contains(lowerOutput, "in use")) {
		t.Fatalf("API exited without evidence of the occupied listener being the cause\n%s", combinedOutput)
	}

	info, err := os.Stat(dbPath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("SQLite startup side effect missing: file=%v err=%v\n%s", info, err, combinedOutput)
	}
	sqliteDB, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open child SQLite database after exit: %v", err)
	}
	sqlDB, err := sqliteDB.DB()
	if err != nil {
		t.Fatalf("get child SQLite connection: %v", err)
	}
	defer sqlDB.Close()
	if !sqliteDB.Migrator().HasTable(&jobs.AnalysisJob{}) {
		t.Fatal("child SQLite database does not contain migrated analysis_jobs table")
	}
	t.Logf("observation: occupied port %d caused exit code 1 in %s; no context deadline; child SQLite migrations completed before listener failure", port, elapsed)
}
