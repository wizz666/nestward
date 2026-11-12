// Package store keeps Nestward's state in one SQLite file. It uses a
// pure-Go driver, so the program needs no C libraries.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/hardware"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store is the database.
type Store struct {
	db *sql.DB
}

// Machine is one known machine with its latest hardware report.
type Machine struct {
	ID       string          `json:"id"`
	Hostname string          `json:"hostname"`
	Source   string          `json:"source"` // "local" or "agent"
	Address  string          `json:"address,omitempty"`
	LastSeen time.Time       `json:"last_seen"`
	Hardware hardware.Report `json:"hardware"`
	// Discovery is nil when the machine has not reported services.
	Discovery *discovery.Result `json:"discovery,omitempty"`
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS machines (
		id        TEXT PRIMARY KEY,
		hostname  TEXT NOT NULL,
		source    TEXT NOT NULL,
		last_seen INTEGER NOT NULL,
		hardware  TEXT NOT NULL
	)`,
	`ALTER TABLE machines ADD COLUMN address TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE machines ADD COLUMN discovery TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE checks (
		id         TEXT PRIMARY KEY,
		machine_id TEXT NOT NULL,
		data       TEXT NOT NULL
	)`,
	`CREATE TABLE events (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		at         INTEGER NOT NULL,
		machine_id TEXT NOT NULL,
		data       TEXT NOT NULL
	)`,
	`CREATE TABLE admin (
		id        INTEGER PRIMARY KEY CHECK (id = 1),
		username  TEXT NOT NULL,
		pass_hash TEXT NOT NULL,
		totp      TEXT NOT NULL,
		totp_last INTEGER NOT NULL DEFAULT 0,
		created   INTEGER NOT NULL
	)`,
	`CREATE TABLE sessions (
		id_hash   TEXT PRIMARY KEY,
		created   INTEGER NOT NULL,
		expires   INTEGER NOT NULL,
		ip        TEXT NOT NULL
	)`,
	`CREATE TABLE publications (
		id   TEXT PRIMARY KEY,
		data TEXT NOT NULL
	)`,
	`CREATE TABLE agent_certs (
		serial     TEXT PRIMARY KEY,
		machine_id TEXT NOT NULL,
		hostname   TEXT NOT NULL,
		created    INTEGER NOT NULL,
		revoked    INTEGER NOT NULL DEFAULT 0
	)`,
	// Add-machine codes live in the database so that both the web page and
	// `nestward agent-code` on the core's own machine can make them. Only a
	// hash of each code is kept.
	`CREATE TABLE agent_codes (
		hash    TEXT PRIMARY KEY,
		expires INTEGER NOT NULL
	)`,
	// What each public address shows a stranger (the exposure check).
	`CREATE TABLE exposure (
		host TEXT PRIMARY KEY,
		data TEXT NOT NULL
	)`,
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite writes one at a time; keep it simple
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRow(`SELECT v FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.Exec(`INSERT INTO schema_version (v) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := s.db.Exec(`UPDATE schema_version SET v = ?`, i+1); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SaveMachine inserts or updates a machine's latest report. address is
// where the machine was seen from (empty for the local machine); disc
// may be nil when the machine sent no services.
func (s *Store) SaveMachine(ctx context.Context, source, address string, hw hardware.Report, disc *discovery.Result) error {
	if hw.MachineID == "" {
		return errors.New("machine id is empty")
	}
	b, err := json.Marshal(hw)
	if err != nil {
		return err
	}
	d := ""
	if disc != nil {
		db, err := json.Marshal(disc)
		if err != nil {
			return err
		}
		d = string(db)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO machines (id, hostname, source, address, last_seen, hardware, discovery) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET hostname = excluded.hostname, source = excluded.source, address = excluded.address,
			last_seen = excluded.last_seen, hardware = excluded.hardware, discovery = excluded.discovery`,
		hw.MachineID, hw.Hostname, source, address, time.Now().Unix(), string(b), d)
	return err
}

// Machines returns every known machine, local first, then by name.
func (s *Store) Machines(ctx context.Context) ([]Machine, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, hostname, source, address, last_seen, hardware, discovery FROM machines
		ORDER BY CASE source WHEN 'local' THEN 0 ELSE 1 END, hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		var m Machine
		var seen int64
		var raw, disc string
		if err := rows.Scan(&m.ID, &m.Hostname, &m.Source, &m.Address, &seen, &raw, &disc); err != nil {
			return nil, err
		}
		if disc != "" {
			m.Discovery = &discovery.Result{}
			if err := json.Unmarshal([]byte(disc), m.Discovery); err != nil {
				return nil, fmt.Errorf("machine %s discovery: %w", m.ID, err)
			}
		}
		m.LastSeen = time.Unix(seen, 0).UTC()
		if err := json.Unmarshal([]byte(raw), &m.Hardware); err != nil {
			return nil, fmt.Errorf("machine %s: %w", m.ID, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
