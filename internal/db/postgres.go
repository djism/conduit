package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"

	"github.com/djism/conduit/config"
	_ "github.com/lib/pq" // PostgreSQL driver — blank import registers it
)

// migrations holds all SQL files embedded at compile time.
// This means the binary carries its own migrations —
// no need to ship a separate migrations directory.
//
//go:embed migrations/*.sql
var migrations embed.FS

// DB wraps sql.DB to add Conduit-specific methods.
type DB struct {
	*sql.DB
}

// New opens a connection pool to PostgreSQL and runs all pending migrations.
// Returns an error if the connection fails or any migration fails.
//
// Connection pool settings:
//   - MaxOpenConns: 25 — max simultaneous connections to PostgreSQL
//   - MaxIdleConns: 10 — keep 10 connections warm for reuse
//
// These numbers are tuned for a moderate-traffic service. For very high
// traffic you'd increase MaxOpenConns, but PostgreSQL itself struggles
// above ~100 connections without a pooler like PgBouncer.
func New(cfg *config.Config) (*DB, error) {
	sqlDB, err := sql.Open("postgres", cfg.PostgresDSN())
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}

	// sql.Open is lazy — it doesn't actually connect until a query is made.
	// Ping forces an immediate connection attempt so we fail fast at startup.
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	// Connection pool tuning.
	// MaxOpenConns caps the total number of connections.
	// MaxIdleConns keeps a warm pool ready to serve requests.
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(10)

	db := &DB{sqlDB}

	// Run migrations before accepting any traffic.
	if err := db.runMigrations(); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	log.Println("✓ PostgreSQL connected and migrations applied")
	return db, nil
}

// runMigrations reads all SQL files from the embedded migrations directory,
// sorts them by filename (so 001_ runs before 002_), and executes each one.
//
// This is a simple migration runner — it re-runs migrations on every startup
// but all migrations use CREATE TABLE IF NOT EXISTS so they're idempotent.
// For a production system you'd add a migrations table to track what's run.
func (db *DB) runMigrations() error {
	// Read all .sql files from the embedded filesystem.
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("reading migrations directory: %w", err)
	}

	// Sort by filename — ensures 001_ runs before 002_ before 003_.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		content, err := fs.ReadFile(migrations, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", entry.Name(), err)
		}

		if _, err := db.Exec(string(content)); err != nil {
			return fmt.Errorf("executing migration %s: %w", entry.Name(), err)
		}

		log.Printf("✓ Migration applied: %s", entry.Name())
	}

	return nil
}

// HealthCheck verifies the database connection is still alive.
// Called by the /health HTTP endpoint so load balancers can
// detect and route around a dead database connection.
func (db *DB) HealthCheck() error {
	if err := db.Ping(); err != nil {
		return fmt.Errorf("postgres health check failed: %w", err)
	}
	return nil
}
