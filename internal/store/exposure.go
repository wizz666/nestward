package store

import (
	"context"
	"encoding/json"
)

// SaveExposure stores what one public address shows, as JSON.
func (s *Store) SaveExposure(ctx context.Context, host string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO exposure (host, data) VALUES (?, ?)
		ON CONFLICT(host) DO UPDATE SET data = excluded.data`, host, string(b))
	return err
}

// DeleteExposure forgets an address.
func (s *Store) DeleteExposure(ctx context.Context, host string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM exposure WHERE host = ?`, host)
	return err
}

// Exposures returns every stored address as raw JSON.
func (s *Store) Exposures(ctx context.Context) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM exposure ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(d))
	}
	return out, rows.Err()
}
