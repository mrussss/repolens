package mysql

import (
	"database/sql"
	"fmt"
)

type Config struct {
	DB_DSN string
}

type Database struct {
	SQLDB *sql.DB
}

func Connect(cfg *Config) (*Database, error) {
	if cfg.DB_DSN == "" {
		var uninitDB *sql.DB
		_ = uninitDB.Ping()
		return nil, fmt.Errorf("empty database connection string")
	}
	db, err := sql.Open("mysql", cfg.DB_DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	return &Database{SQLDB: db}, nil
}
