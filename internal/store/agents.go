package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AgentCert is one certificate the core gave an agent.
type AgentCert struct {
	Serial    string    `json:"serial"`
	MachineID string    `json:"machine_id"`
	Hostname  string    `json:"hostname"`
	Created   time.Time `json:"created"`
	Revoked   bool      `json:"revoked"`
}

// AddAgentCert records a newly signed certificate. Earlier certificates
// of the same machine are revoked: a machine enrolled again gets one
// valid certificate only.
func (s *Store) AddAgentCert(ctx context.Context, c AgentCert) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE agent_certs SET revoked = 1 WHERE machine_id = ?`, c.MachineID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_certs (serial, machine_id, hostname, created) VALUES (?, ?, ?, ?)`,
		c.Serial, c.MachineID, c.Hostname, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// AgentCertValid reports whether a certificate is known, not revoked and
// belongs to machineID.
func (s *Store) AgentCertValid(ctx context.Context, serial, machineID string) (bool, error) {
	var mid string
	var revoked int
	err := s.db.QueryRowContext(ctx, `SELECT machine_id, revoked FROM agent_certs WHERE serial = ?`, serial).Scan(&mid, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return revoked == 0 && mid == machineID, nil
}

// AgentCerts lists the valid certificates, one per machine.
func (s *Store) AgentCerts(ctx context.Context) ([]AgentCert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT serial, machine_id, hostname, created FROM agent_certs WHERE revoked = 0 ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentCert{}
	for rows.Next() {
		var c AgentCert
		var created int64
		if err := rows.Scan(&c.Serial, &c.MachineID, &c.Hostname, &created); err != nil {
			return nil, err
		}
		c.Created = time.Unix(created, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// RemoveAgent revokes a machine's certificates and forgets the machine,
// its services and its checks.
func (s *Store) RemoveAgent(ctx context.Context, machineID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`UPDATE agent_certs SET revoked = 1 WHERE machine_id = ?`,
		`DELETE FROM machines WHERE id = ? AND source = 'agent'`,
		`DELETE FROM checks WHERE machine_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, machineID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddAgentCode stores a new add-machine code (as a hash) until expires.
// Expired codes are cleared at the same time.
func (s *Store) AddAgentCode(ctx context.Context, hash string, expires time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_codes WHERE expires <= ?`, time.Now().Unix()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_codes (hash, expires) VALUES (?, ?)`, hash, expires.Unix())
	return err
}

// UseAgentCode spends a code: true only once, and only before it expires.
func (s *Store) UseAgentCode(ctx context.Context, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_codes WHERE hash = ? AND expires > ?`, hash, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
