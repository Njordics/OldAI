package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"time"
)

// Memory is one thing the cluster remembers for a memory space (§18).
type Memory struct {
	ID     string `json:"id"`
	Space  string `json:"space"`
	Kind   string `json:"kind"` // instruction | fact
	Text   string `json:"text"`
	Pinned bool   `json:"pinned"`
	Tier   string `json:"tier"`   // hot | cold
	Source string `json:"source"` // you | learned | restored
	// Embedding is the text's vector from EmbedModel; empty until one is made.
	Embedding  []float32 `json:"embedding,omitempty"`
	EmbedModel string    `json:"embed_model,omitempty"`
	Uses       int       `json:"uses"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

const (
	MemoryInstruction = "instruction"
	MemoryFact        = "fact"
	TierHot           = "hot"
	TierCold          = "cold"
	// settingMemoryGen counts every change to memory; backups carry it.
	settingMemoryGen = "memory_generation"
)

const memCols = `id,space,kind,text,pinned,tier,source,embedding,embed_model,uses,created_at,updated_at,last_used_at`

func scanMemory(r scanner) (*Memory, error) {
	var m Memory
	var pinned int
	var emb []byte
	var created, updated, used int64
	if err := r.Scan(&m.ID, &m.Space, &m.Kind, &m.Text, &pinned, &m.Tier, &m.Source, &emb, &m.EmbedModel, &m.Uses, &created, &updated, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.Pinned, m.Embedding = pinned != 0, decodeVec(emb)
	m.CreatedAt, m.UpdatedAt, m.LastUsedAt = ms(created), ms(updated), ms(used)
	return &m, nil
}

func encodeVec(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// ListMemories returns a space's memories ("" = every space), oldest first.
func (s *Store) ListMemories(ctx context.Context, space string) ([]*Memory, error) {
	q, args := `SELECT `+memCols+` FROM memories ORDER BY created_at, id`, []any{}
	if space != "" {
		q, args = `SELECT `+memCols+` FROM memories WHERE space=? ORDER BY created_at, id`, []any{space}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetMemory(ctx context.Context, id string) (*Memory, error) {
	return scanMemory(s.db.QueryRowContext(ctx, `SELECT `+memCols+` FROM memories WHERE id=?`, id))
}

func putMemory(ctx context.Context, x interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, m *Memory) error {
	pinned := 0
	if m.Pinned {
		pinned = 1
	}
	_, err := x.ExecContext(ctx, `INSERT INTO memories(`+memCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET space=excluded.space, kind=excluded.kind, text=excluded.text, pinned=excluded.pinned,
		tier=excluded.tier, source=excluded.source, embedding=excluded.embedding, embed_model=excluded.embed_model,
		uses=excluded.uses, created_at=excluded.created_at, updated_at=excluded.updated_at, last_used_at=excluded.last_used_at`,
		m.ID, m.Space, m.Kind, m.Text, pinned, m.Tier, m.Source, encodeVec(m.Embedding), m.EmbedModel, m.Uses,
		toMS(m.CreatedAt), toMS(m.UpdatedAt), toMS(m.LastUsedAt))
	return err
}

func bumpGen(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`, settingMemoryGen)
	return err
}

// PutMemory inserts or replaces a memory and counts the change.
func (s *Store) PutMemory(ctx context.Context, m *Memory) error {
	return s.memTx(ctx, func(tx *sql.Tx) error { return putMemory(ctx, tx, m) })
}

// DeleteMemories removes memories by ID and counts the change.
func (s *Store) DeleteMemories(ctx context.Context, ids ...string) error {
	return s.memTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteMemorySpace removes every memory of a space.
func (s *Store) DeleteMemorySpace(ctx context.Context, space string) error {
	return s.memTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE space=?`, space)
		return err
	})
}

// ReplaceMemories swaps every memory for ms and sets the generation (a
// restore from backup).
func (s *Store) ReplaceMemories(ctx context.Context, all []*Memory, gen int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories`); err != nil {
		return err
	}
	for _, m := range all {
		if err := putMemory(ctx, tx, m); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		settingMemoryGen, strconv.FormatInt(gen, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) memTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if err := bumpGen(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// TouchMemories records that memories were used in a reply: they count as
// hot again. It isn't a change worth a backup.
func (s *Store) TouchMemories(ctx context.Context, at time.Time, ids ...string) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET uses=uses+1, last_used_at=?, tier=? WHERE id=?`, toMS(at), TierHot, id); err != nil {
			return err
		}
	}
	return nil
}

// SetMemoryTier and SetMemoryEmbedding update derived fields without
// counting a change.
func (s *Store) SetMemoryTier(ctx context.Context, id, tier string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET tier=? WHERE id=?`, tier, id)
	return err
}

func (s *Store) SetMemoryEmbedding(ctx context.Context, id, model string, v []float32) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET embedding=?, embed_model=? WHERE id=?`, encodeVec(v), model, id)
	return err
}

// MemoryGeneration counts changes to memory since the cluster began.
func (s *Store) MemoryGeneration(ctx context.Context) (int64, error) {
	v, err := s.Setting(ctx, settingMemoryGen, "0")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}
