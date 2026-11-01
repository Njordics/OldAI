package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// API keys for the OpenAI-compatible gateway. A key is "aic_" + 40 base32
// characters (200 random bits). Only its SHA-256 is stored: a slow password
// hash adds nothing for a secret this long, and the gateway checks a key on
// every request. The first 8 characters after "aic_" are kept in clear to
// find the row and to show in the UI.
type APIKey struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	Models     []string  `json:"models"` // empty = all models
	RPM        int       `json:"rpm"`    // requests per minute, 0 = unlimited
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

const keyPrefixLen = 8

var keyEncoding = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)

func hashKey(k string) string {
	s := sha256.Sum256([]byte(k))
	return hex.EncodeToString(s[:])
}

// CreateAPIKey returns the stored record and the secret, which is shown once.
func (s *Store) CreateAPIKey(ctx context.Context, name string, models []string, rpm int) (*APIKey, string, error) {
	raw := make([]byte, 25)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	body := keyEncoding.EncodeToString(raw) // 40 chars
	secret := "aic_" + body
	k := &APIKey{ID: ulid.Make().String(), Name: name, Prefix: body[:keyPrefixLen], Models: models, RPM: rpm, CreatedAt: time.Now().UTC()}
	if k.Models == nil {
		k.Models = []string{}
	}
	mj, _ := json.Marshal(k.Models)
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(id,name,prefix,hash,models,rpm,created_at,last_used_at) VALUES (?,?,?,?,?,?,?,0)`,
		k.ID, k.Name, k.Prefix, hashKey(secret), string(mj), k.RPM, toMS(k.CreatedAt))
	if err != nil {
		return nil, "", err
	}
	return k, secret, nil
}

func scanKey(r scanner) (*APIKey, string, error) {
	var k APIKey
	var hash, models string
	var created, used int64
	if err := r.Scan(&k.ID, &k.Name, &k.Prefix, &hash, &models, &k.RPM, &created, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	_ = json.Unmarshal([]byte(models), &k.Models)
	if k.Models == nil {
		k.Models = []string{}
	}
	k.CreatedAt, k.LastUsedAt = ms(created), ms(used)
	return &k, hash, nil
}

const keyCols = `id,name,prefix,hash,models,rpm,created_at,last_used_at`

func (s *Store) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIKey{}
	for rows.Next() {
		k, _, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// VerifyAPIKey returns the key record for a presented secret.
func (s *Store) VerifyAPIKey(ctx context.Context, secret string) (*APIKey, error) {
	body, ok := strings.CutPrefix(secret, "aic_")
	if !ok || len(body) < keyPrefixLen {
		return nil, ErrNotFound
	}
	k, hash, err := scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE prefix=?`, body[:keyPrefixLen]))
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashKey(secret))) != 1 {
		return nil, ErrNotFound
	}
	return k, nil
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchAPIKey(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at=? WHERE id=?`, toMS(at), id)
	return err
}
