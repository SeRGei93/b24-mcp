package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) GetSchemaCache(ctx context.Context, connectionID, key string, maxAgeSeconds int, dest any) (bool, error) {
	minFetchedAt := time.Now().UTC().Add(-time.Duration(maxAgeSeconds) * time.Second).Format(isoTimeFormat)
	var payload string
	err := s.db.QueryRowContext(ctx, `
		SELECT payload FROM schema_cache
		WHERE connection_id = ?1 AND cache_key = ?2
		  AND fetched_at > ?3
	`, connectionID, key, minFetchedAt).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(payload), dest); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) SetSchemaCache(ctx context.Context, connectionID, key string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(isoTimeFormat)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO schema_cache (connection_id, cache_key, payload, fetched_at)
		VALUES (?1,?2,?3,?4)
		ON CONFLICT (connection_id, cache_key) DO UPDATE SET
			payload = excluded.payload,
			fetched_at = excluded.fetched_at
	`, connectionID, key, string(encoded), now)
	return err
}

func (s *Store) ClearSchemaCache(ctx context.Context, connectionID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM schema_cache WHERE connection_id = ?1", connectionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
