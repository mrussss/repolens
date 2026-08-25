package mysql

import (
	"database/sql"
	"fmt"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"repolens/internal/platform/config"
)

type DB struct {
	GormDB *gorm.DB
	SqlDB  *sql.DB
}

func Connect(cfg *config.Config) (*DB, error) {
	var dialector gorm.Dialector

	if cfg.DBDriver == "mysql" {
		dialector = mysql.Open(cfg.DSN)
	} else {
		dialector = sqlite.Open(cfg.DSN)
	}

	logLevel := gormlogger.Warn
	if cfg.Env == "development" {
		logLevel = gormlogger.Info
	}

	gdb, err := gorm.Open(dialector, &gorm.Config{
		Logger: gormlogger.Default.LogMode(logLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	sdb, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get sql.DB: %w", err)
	}

	sdb.SetMaxIdleConns(10)
	sdb.SetMaxOpenConns(100)
	sdb.SetConnMaxLifetime(time.Hour)

	return &DB{
		GormDB: gdb,
		SqlDB:  sdb,
	}, nil
}

func (db *DB) Close() error {
	if db.SqlDB != nil {
		return db.SqlDB.Close()
	}
	return nil
}
