// Package store is the Controller's persistent state in SQLite. The core is
// its only writer. The schema is migrated forward on open.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Node states as the Controller sees them.
const (
	StateDiscovered = "discovered" // seen, not acted on yet
	StatePending    = "pending"    // waiting for admin approval or pairing code
	StateEnrolling  = "enrolling"  // handshake done or in flight, waiting for first heartbeat
	StateAdopted    = "adopted"    // heartbeating
	StateOffline    = "offline"    // adopted, heartbeats stopped
	StateReleased   = "released"   // released by admin; never auto-adopted again
	StateForeign    = "foreign"    // adopted by another cluster
)

type Node struct {
	NodeID          string          `json:"node_id"`
	Name            string          `json:"name"`
	HWFingerprint   string          `json:"hw_fingerprint"`
	State           string          `json:"state"`
	Phase           string          `json:"phase"`
	AgentState      string          `json:"agent_state"`
	ClusterID       string          `json:"cluster_id,omitempty"`
	Addr            string          `json:"addr"`
	Port            int             `json:"port"`
	FP              string          `json:"fp"`
	PairingRequired bool            `json:"pairing_required"`
	AgentVersion    string          `json:"agent_version"`
	HWSummary       string          `json:"hw_summary"`
	CertSerial      string          `json:"cert_serial,omitempty"`
	Returning       bool            `json:"returning"`
	Inventory       json.RawMessage `json:"inventory,omitempty"`
	Observed        json.RawMessage `json:"observed,omitempty"`
	Health          json.RawMessage `json:"health,omitempty"`
	Desired         json.RawMessage `json:"desired,omitempty"`
	DesiredGen      int64           `json:"desired_generation"`
	ObservedGen     int64           `json:"observed_generation"`
	BootID          string          `json:"boot_id,omitempty"`
	LastSeq         uint64          `json:"last_seq"`
	FirstSeen       time.Time       `json:"first_seen"`
	LastSeen        time.Time       `json:"last_seen"`
	LastHeartbeat   time.Time       `json:"last_heartbeat"`
	AdoptedAt       time.Time       `json:"adopted_at"`
	StateSince      time.Time       `json:"state_since"`
	LastError       string          `json:"last_error,omitempty"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex // serialises read-modify-write node updates
}

var ErrNotFound = errors.New("store: not found")

var migrations = []string{
	`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE nodes (
		node_id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		hw_fingerprint TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		phase TEXT NOT NULL DEFAULT '',
		agent_state TEXT NOT NULL DEFAULT '',
		cluster_id TEXT NOT NULL DEFAULT '',
		addr TEXT NOT NULL DEFAULT '',
		port INTEGER NOT NULL DEFAULT 0,
		fp TEXT NOT NULL DEFAULT '',
		pairing_required INTEGER NOT NULL DEFAULT 0,
		agent_version TEXT NOT NULL DEFAULT '',
		hw_summary TEXT NOT NULL DEFAULT '',
		cert_serial TEXT NOT NULL DEFAULT '',
		is_returning INTEGER NOT NULL DEFAULT 0,
		inventory TEXT, observed TEXT, health TEXT, desired TEXT,
		desired_gen INTEGER NOT NULL DEFAULT 0,
		observed_gen INTEGER NOT NULL DEFAULT 0,
		boot_id TEXT NOT NULL DEFAULT '',
		last_seq INTEGER NOT NULL DEFAULT 0,
		first_seen INTEGER NOT NULL DEFAULT 0,
		last_seen INTEGER NOT NULL DEFAULT 0,
		last_heartbeat INTEGER NOT NULL DEFAULT 0,
		adopted_at INTEGER NOT NULL DEFAULT 0,
		state_since INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX nodes_hw ON nodes(hw_fingerprint)`,
	`CREATE TABLE revoked (serial TEXT PRIMARY KEY, node_id TEXT NOT NULL, revoked_at INTEGER NOT NULL)`,
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; reads are cheap at cluster scale
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(v),0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(v) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Settings

func (s *Store) Setting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Nodes

const nodeCols = `node_id,name,hw_fingerprint,state,phase,agent_state,cluster_id,addr,port,fp,pairing_required,
	agent_version,hw_summary,cert_serial,is_returning,inventory,observed,health,desired,desired_gen,observed_gen,
	boot_id,last_seq,first_seen,last_seen,last_heartbeat,adopted_at,state_since,last_error`

type scanner interface{ Scan(...any) error }

func scanNode(r scanner) (*Node, error) {
	var n Node
	var inv, obs, health, desired sql.NullString
	var firstSeen, lastSeen, lastHB, adoptedAt, stateSince int64
	err := r.Scan(&n.NodeID, &n.Name, &n.HWFingerprint, &n.State, &n.Phase, &n.AgentState, &n.ClusterID, &n.Addr, &n.Port, &n.FP,
		&n.PairingRequired, &n.AgentVersion, &n.HWSummary, &n.CertSerial, &n.Returning, &inv, &obs, &health, &desired,
		&n.DesiredGen, &n.ObservedGen, &n.BootID, &n.LastSeq, &firstSeen, &lastSeen, &lastHB, &adoptedAt, &stateSince, &n.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.Inventory, n.Observed, n.Health, n.Desired = raw(inv), raw(obs), raw(health), raw(desired)
	n.FirstSeen, n.LastSeen, n.LastHeartbeat, n.AdoptedAt, n.StateSince =
		ms(firstSeen), ms(lastSeen), ms(lastHB), ms(adoptedAt), ms(stateSince)
	return &n, nil
}

func raw(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "" {
		return nil
	}
	return json.RawMessage(s.String)
}

func nullJSON(m json.RawMessage) any {
	if len(m) == 0 {
		return nil
	}
	return string(m)
}

func ms(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func toMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (s *Store) GetNode(ctx context.Context, id string) (*Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE node_id=?`, id))
}

func (s *Store) ListNodes(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY name, node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FindByFingerprint returns nodes (other than exceptID) with the same hardware.
func (s *Store) FindByFingerprint(ctx context.Context, fp, exceptID string) ([]*Node, error) {
	if fp == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE hw_fingerprint=? AND node_id<>?`, fp, exceptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// PutNode inserts or fully replaces a node row.
func (s *Store) PutNode(ctx context.Context, n *Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putNode(ctx, n)
}

func (s *Store) putNode(ctx context.Context, n *Node) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO nodes(`+nodeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.NodeID, n.Name, n.HWFingerprint, n.State, n.Phase, n.AgentState, n.ClusterID, n.Addr, n.Port, n.FP,
		n.PairingRequired, n.AgentVersion, n.HWSummary, n.CertSerial, n.Returning,
		nullJSON(n.Inventory), nullJSON(n.Observed), nullJSON(n.Health), nullJSON(n.Desired),
		n.DesiredGen, n.ObservedGen, n.BootID, n.LastSeq,
		toMS(n.FirstSeen), toMS(n.LastSeen), toMS(n.LastHeartbeat), toMS(n.AdoptedAt), toMS(n.StateSince), n.LastError)
	return err
}

// UpdateNode loads a node, lets fn mutate it, and writes it back in one
// transaction. fn returning an error aborts the update.
func (s *Store) UpdateNode(ctx context.Context, id string, fn func(n *Node) error) (*Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.GetNode(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := fn(n); err != nil {
		return nil, err
	}
	return n, s.putNode(ctx, n)
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE node_id=?`, id)
	return err
}

// Revocation

func (s *Store) Revoke(ctx context.Context, serial, nodeID string) error {
	if serial == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO revoked(serial,node_id,revoked_at) VALUES(?,?,?)`, serial, nodeID, time.Now().UnixMilli())
	return err
}

func (s *Store) IsRevoked(ctx context.Context, serial string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM revoked WHERE serial=?`, serial).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
