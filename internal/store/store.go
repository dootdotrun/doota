// Package store is the only durable state in doot-ai.
//
// The Fly machine is disposable: no volume, no local database, nothing on disk
// whose loss would be noticed. Everything that matters is in Postgres.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
)

// Store wraps the database handle.
type Store struct {
	DB *sql.DB
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Modest pool. One machine, one runner goroutine, and a handful of concurrent
	// HTTP requests. Neon's connection limits are the binding constraint, not
	// our concurrency.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Store{DB: db}, nil
}

// Ping verifies the database is reachable. Used by /healthz: a process that
// cannot reach its only datastore is not healthy and should be restarted.
func (s *Store) Ping(ctx context.Context) error {
	return s.DB.PingContext(ctx)
}

// Close releases the pool.
func (s *Store) Close() error {
	return s.DB.Close()
}
