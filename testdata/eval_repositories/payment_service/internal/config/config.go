package config

import (
	"os"
	"strconv"
)

type Config struct {
	Env     string
	Port    int
	DB_DSN  string
	Timeout int
}

func Load() *Config {
	port, _ := strconv.Atoi(os.Getenv("HTTP_PORT"))
	if port == 0 {
		port = 8080
	}
	return &Config{
		Env:     os.Getenv("ENV"),
		Port:    port,
		DB_DSN:  os.Getenv("DB_DSN"),
		Timeout: 30,
	}
}
