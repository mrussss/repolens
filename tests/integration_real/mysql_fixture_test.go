package integration_real

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	realMySQLAdminUser     = "root"
	realMySQLAdminPassword = "testpass"
	realMySQLBootstrapDB   = "repolens_fixture_bootstrap"
	realMySQLAdminTimeout  = 10 * time.Second
	realMySQLStopTimeout   = 15 * time.Second
)

type sharedMySQLHarness struct {
	startOnce sync.Once

	container *tcmysql.MySQLContainer
	initErr   error
	host      string
	port      string

	databaseCounter atomic.Uint64
	containerStarts atomic.Uint64
	databaseCount   atomic.Uint64

	namesMu       sync.Mutex
	databaseNames map[string]struct{}

	stopOnce sync.Once
	stopErr  error
}

var realMySQLHarness sharedMySQLHarness

func TestMain(m *testing.M) {
	code := m.Run()

	ctx, cancel := context.WithTimeout(context.Background(), realMySQLStopTimeout)
	stopErr := realMySQLHarness.shutdown(ctx)
	cancel()
	if stopErr != nil {
		fmt.Fprintf(os.Stderr, "real MySQL shared container termination failed: %v\n", stopErr)
		if code == 0 {
			code = 1
		}
	}

	starts := realMySQLHarness.containerStarts.Load()
	databases := realMySQLHarness.databaseCount.Load()
	realMySQLHarness.namesMu.Lock()
	distinctNames := uint64(len(realMySQLHarness.databaseNames))
	realMySQLHarness.namesMu.Unlock()
	fmt.Fprintf(os.Stderr, "real MySQL harness: container_starts=%d isolated_databases=%d distinct_database_names=%d\n", starts, databases, distinctNames)
	if starts > 1 || databases != distinctNames {
		if code == 0 {
			code = 1
		}
	}

	os.Exit(code)
}

func (h *sharedMySQLHarness) ensureStarted() error {
	h.startOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		container, err := tcmysql.RunContainer(ctx,
			tc.WithImage("mysql:8.0"),
			tcmysql.WithDatabase(realMySQLBootstrapDB),
			tcmysql.WithUsername(realMySQLAdminUser),
			tcmysql.WithPassword(realMySQLAdminPassword),
		)
		if err != nil {
			h.initErr = err
			return
		}
		h.container = container
		h.containerStarts.Add(1)

		h.host, err = container.Host(ctx)
		if err != nil {
			h.initErr = fmt.Errorf("get shared MySQL host: %w", err)
			h.terminatePartialStart()
			return
		}
		mappedPort, err := container.MappedPort(ctx, "3306/tcp")
		if err != nil {
			h.initErr = fmt.Errorf("get shared MySQL mapped port: %w", err)
			h.terminatePartialStart()
			return
		}
		h.port = mappedPort.Port()
	})
	return h.initErr
}

func (h *sharedMySQLHarness) terminatePartialStart() {
	ctx, cancel := context.WithTimeout(context.Background(), realMySQLStopTimeout)
	defer cancel()
	if err := h.container.Terminate(ctx); err != nil {
		h.initErr = fmt.Errorf("%w (also failed to terminate partial container: %v)", h.initErr, err)
	}
	h.container = nil
}

func (h *sharedMySQLHarness) shutdown(ctx context.Context) error {
	h.stopOnce.Do(func() {
		if h.container != nil {
			h.stopErr = h.container.Terminate(ctx)
			h.container = nil
		}
	})
	return h.stopErr
}

func (h *sharedMySQLHarness) adminDSN() string {
	address := net.JoinHostPort(h.host, h.port)
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?charset=utf8mb4&parseTime=True&loc=Local&timeout=5s&readTimeout=5s&writeTimeout=5s",
		realMySQLAdminUser, realMySQLAdminPassword, address, realMySQLBootstrapDB)
}

func (h *sharedMySQLHarness) testDSN(databaseName string) string {
	address := net.JoinHostPort(h.host, h.port)
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?charset=utf8mb4&parseTime=True&loc=Local&timeout=5s&readTimeout=10s&writeTimeout=10s",
		realMySQLAdminUser, realMySQLAdminPassword, address, databaseName)
}

func (h *sharedMySQLHarness) openAdminDB() (*sql.DB, error) {
	db, err := sql.Open("mysql", h.adminDSN())
	if err != nil {
		return nil, fmt.Errorf("open MySQL admin connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func (h *sharedMySQLHarness) createDatabase(t *testing.T) (string, func()) {
	t.Helper()
	if err := h.ensureStarted(); err != nil {
		if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "1" {
			t.Fatalf("FAILED: real MySQL testcontainers required by release gate but failed to start: %v", err)
		}
		t.Skipf("Skipping real MySQL testcontainers test (Docker not available: %v)", err)
		return "", func() {}
	}

	databaseName := fmt.Sprintf("repolens_test_%06d", h.databaseCounter.Add(1))
	adminDB, err := h.openAdminDB()
	if err != nil {
		t.Fatalf("open MySQL admin connection for database %s: %v", databaseName, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), realMySQLAdminTimeout)
	if err := adminDB.PingContext(ctx); err != nil {
		cancel()
		_ = adminDB.Close()
		t.Fatalf("ping MySQL admin connection for database %s: %v", databaseName, err)
	}
	_, err = adminDB.ExecContext(ctx, "CREATE DATABASE `"+databaseName+"`")
	cancel()
	_ = adminDB.Close()
	if err != nil {
		t.Fatalf("create isolated MySQL database %s: %v", databaseName, err)
	}

	h.namesMu.Lock()
	if h.databaseNames == nil {
		h.databaseNames = make(map[string]struct{})
	}
	_, duplicate := h.databaseNames[databaseName]
	h.databaseNames[databaseName] = struct{}{}
	h.namesMu.Unlock()
	h.databaseCount.Add(1)
	if duplicate {
		t.Fatalf("shared MySQL harness reused isolated database name %s", databaseName)
	}

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			dropCtx, dropCancel := context.WithTimeout(context.Background(), realMySQLAdminTimeout)
			defer dropCancel()

			dropAdminDB, err := h.openAdminDB()
			if err != nil {
				t.Errorf("open MySQL admin connection to drop database %s: %v", databaseName, err)
				return
			}
			defer func() { _ = dropAdminDB.Close() }()
			if err := dropAdminDB.PingContext(dropCtx); err != nil {
				t.Errorf("ping MySQL admin connection to drop database %s: %v", databaseName, err)
				return
			}
			if _, err := dropAdminDB.ExecContext(dropCtx, "DROP DATABASE IF EXISTS `"+databaseName+"`"); err != nil {
				t.Errorf("drop isolated MySQL database %s: %v", databaseName, err)
			}
		})
	}
	return databaseName, cleanup
}

func setupRealMySQLDatabase(t *testing.T) (*gorm.DB, *sql.DB, func()) {
	t.Helper()
	databaseName, dropDatabase := realMySQLHarness.createDatabase(t)
	var sqlDB *sql.DB
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if sqlDB != nil {
				if err := sqlDB.Close(); err != nil {
					t.Errorf("close MySQL connection for database %s: %v", databaseName, err)
				}
			}
			dropDatabase()
		})
	}
	t.Cleanup(cleanup)

	db, err := gorm.Open(gormmysql.Open(realMySQLHarness.testDSN(databaseName)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if db != nil {
		sqlDB, _ = db.DB()
	}
	if err != nil {
		t.Fatalf("open isolated real MySQL database %s: %v", databaseName, err)
	}
	if sqlDB == nil {
		t.Fatalf("get SQL connection for isolated real MySQL database %s: unavailable", databaseName)
	}
	return db, sqlDB, cleanup
}
