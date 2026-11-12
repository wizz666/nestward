package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Admin is the one account that may use Nestward's web interface.
type Admin struct {
	Username string
	PassHash string // argon2id
	TOTP     string // sealed TOTP secret
	TOTPLast int64  // last accepted time step, so a code cannot be used twice
	Created  time.Time
}

// ErrNoAdmin means no account has been created yet.
var ErrNoAdmin = errors.New("no admin account")

// Admin returns the account.
func (s *Store) Admin(ctx context.Context) (Admin, error) {
	var a Admin
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT username, pass_hash, totp, totp_last, created FROM admin WHERE id = 1`).
		Scan(&a.Username, &a.PassHash, &a.TOTP, &a.TOTPLast, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNoAdmin
	}
	a.Created = time.Unix(created, 0)
	return a, err
}

// CreateAdmin stores the account. It fails if one already exists.
func (s *Store) CreateAdmin(ctx context.Context, a Admin) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin (id, username, pass_hash, totp, totp_last, created) VALUES (1, ?, ?, ?, 0, ?)`,
		a.Username, a.PassHash, a.TOTP, time.Now().Unix())
	return err
}

// SetTOTPLast records the last used time step.
func (s *Store) SetTOTPLast(ctx context.Context, step int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE admin SET totp_last = ? WHERE id = 1`, step)
	return err
}

// DeleteAdmin removes the account and every session (recovery).
func (s *Store) DeleteAdmin(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin`)
	return err
}

// CreateSession stores a session by the hash of its token.
func (s *Store) CreateSession(ctx context.Context, idHash string, expires time.Time, ip string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id_hash, created, expires, ip) VALUES (?, ?, ?, ?)`,
		idHash, time.Now().Unix(), expires.Unix(), ip)
	return err
}

// SessionValid reports whether a session exists and has not expired.
func (s *Store) SessionValid(ctx context.Context, idHash string, now time.Time) (bool, error) {
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires FROM sessions WHERE id_hash = ?`, idHash).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return now.Unix() < exp, nil
}

// DeleteSession ends one session.
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// PurgeSessions removes expired sessions.
func (s *Store) PurgeSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires <= ?`, now.Unix())
	return err
}

// SavePublication stores one publication as JSON.
func (s *Store) SavePublication(ctx context.Context, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO publications (id, data) VALUES (?, ?)
		ON CONFLICT(id) DO UPDATE SET data = excluded.data`, id, string(b))
	return err
}

// Publications returns every publication as raw JSON.
func (s *Store) Publications(ctx context.Context) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM publications`)
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
