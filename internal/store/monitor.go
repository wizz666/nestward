package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Setting returns a stored setting and whether it exists.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetSetting stores a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteSetting removes a setting.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

// Settings returns every setting (values as stored, secrets still sealed).
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveCheck stores one check as JSON under its id. The monitor package
// owns the check type, so the store only keeps the bytes.
func (s *Store) SaveCheck(ctx context.Context, id, machineID string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO checks (id, machine_id, data) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET machine_id = excluded.machine_id, data = excluded.data`, id, machineID, string(b))
	return err
}

// DeleteCheck removes a check.
func (s *Store) DeleteCheck(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM checks WHERE id = ?`, id)
	return err
}

// Checks returns every stored check as raw JSON.
func (s *Store) Checks(ctx context.Context) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM checks ORDER BY id`)
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

// maxEvents is how many events are kept.
const maxEvents = 2000

// AddEvent stores one event and drops the oldest beyond maxEvents.
func (s *Store) AddEvent(ctx context.Context, at time.Time, machineID string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO events (at, machine_id, data) VALUES (?, ?, ?)`,
		at.Unix(), machineID, string(b)); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM events WHERE id <= (SELECT MAX(id) FROM events) - ?`, maxEvents)
	return err
}

// Events returns the newest events first as raw JSON.
func (s *Store) Events(ctx context.Context, limit int) ([]json.RawMessage, error) {
	if limit <= 0 || limit > maxEvents {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM events ORDER BY id DESC LIMIT ?`, limit)
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
