package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

const isoTimeFormat = "2006-01-02T15:04:05.000Z"

type Store struct {
	db     *sql.DB
	cipher *TokenCipher
}

func NewStore(databasePath, rawEncryptionKey string) (*Store, error) {
	cipher, err := NewTokenCipher(rawEncryptionKey)
	if err != nil {
		return nil, err
	}

	normalized := config.NormalizeSqlitePath(databasePath)
	dsn := normalized
	if normalized != ":memory:" {
		absPath, err := filepath.Abs(normalized)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return nil, err
		}
		dsn = absPath
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if normalized != ":memory:" {
		if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db, cipher: cipher}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (s *Store) Migrate(ctx context.Context, logger *config.Logger) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			id TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return err
	}

	entries, err := embeddedMigrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		migrationID := strings.TrimSuffix(entry.Name(), ".sql")
		var existing string
		err := s.db.QueryRowContext(ctx, "SELECT id FROM schema_migrations WHERE id = ?1", migrationID).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		sqlBytes, err := embeddedMigrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}

		if err := s.applyMigration(ctx, migrationID, string(sqlBytes)); err != nil {
			return err
		}

		if logger != nil {
			logger.Info("Database migration applied", map[string]any{
				"migrationId": migrationID,
			})
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, migrationID, sqlScript string) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = OFF;"); err != nil {
		return err
	}
	defer func() {
		_, _ = s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON;")
	}()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, sqlScript); err != nil {
		return fmt.Errorf("migration %s failed: %w", migrationID, err)
	}

	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check;")
	if err != nil {
		return err
	}
	hasFKViolation := rows.Next()
	_ = rows.Close()
	if hasFKViolation {
		return fmt.Errorf("migration %s violated foreign key constraints", migrationID)
	}

	now := time.Now().UTC().Format(isoTimeFormat)
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (id, applied_at) VALUES (?1, ?2)", migrationID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	return s.db.Close()
}

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func nullableString(val string) any {
	if val == "" {
		return nil
	}
	return val
}
