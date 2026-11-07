package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

const (
	cipherVersion = "v1"
	gcmTagSize    = 16
	gcmNonceSize  = 12
)

type TokenCipher struct {
	aead cipher.AEAD
}

func NewTokenCipher(rawKey string) (*TokenCipher, error) {
	key, err := config.ParseEncryptionKey(rawKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &TokenCipher{aead: aead}, nil
}

func (c *TokenCipher) Encrypt(plaintext string) (string, error) {
	iv := make([]byte, gcmNonceSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nil, iv, []byte(plaintext), nil)
	if len(sealed) < gcmTagSize {
		return "", errors.New("unexpected sealed payload length")
	}
	ciphertext := sealed[:len(sealed)-gcmTagSize]
	tag := sealed[len(sealed)-gcmTagSize:]

	return strings.Join([]string{
		cipherVersion,
		base64.RawURLEncoding.EncodeToString(iv),
		base64.RawURLEncoding.EncodeToString(tag),
		base64.RawURLEncoding.EncodeToString(ciphertext),
	}, ":"), nil
}

func (c *TokenCipher) Decrypt(payload string) (string, error) {
	parts := strings.Split(payload, ":")
	if len(parts) != 4 || parts[0] != cipherVersion || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", errors.New("Encrypted token has an unsupported or invalid format")
	}
	iv, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(iv) != gcmNonceSize {
		return "", errors.New("Encrypted token has an unsupported or invalid format")
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(tag) != gcmTagSize {
		return "", errors.New("Encrypted token has an unsupported or invalid format")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", errors.New("Encrypted token has an unsupported or invalid format")
	}

	combined := make([]byte, 0, len(ciphertext)+len(tag))
	combined = append(combined, ciphertext...)
	combined = append(combined, tag...)

	plaintext, err := c.aead.Open(nil, iv, combined, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt token: %w", err)
	}
	return string(plaintext), nil
}

type Store struct {
	db       *sql.DB
	cipher   *TokenCipher
	migrator *goose.Provider
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

	migrationsFS, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	migrator, err := goose.NewProvider(goose.DialectSQLite3, db, migrationsFS)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db, cipher: cipher, migrator: migrator}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (s *Store) Migrate(ctx context.Context, logger *config.Logger) error {
	results, err := s.migrator.Up(ctx)
	if err != nil {
		return err
	}
	if logger != nil {
		for _, res := range results {
			logger.Info("Database migration applied", map[string]any{
				"version": res.Source.Version,
				"path":    res.Source.Path,
			})
		}
	}
	return nil
}

func (s *Store) DBVersion(ctx context.Context) (int64, error) {
	return s.migrator.GetDBVersion(ctx)
}

func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM schema_migrations ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) Close() error {
	return s.db.Close()
}
