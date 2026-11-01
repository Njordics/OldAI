package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// The dashboard's Code tab keeps its files here, so they follow the admin
// to any browser and are covered by Controller backups.

const (
	MaxWorkspaceFile  = 1 << 20 // bytes per file
	MaxWorkspaceFiles = 500
)

var ErrWorkspaceFull = errors.New("workspace is full")

type WorkspaceFile struct {
	Path      string    `json:"path"`
	Content   string    `json:"content,omitempty"`
	Size      int       `json:"size"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CleanWorkspacePath accepts relative paths like "src/app.js" and rejects
// anything that could escape or confuse ("..", absolute, control chars).
func CleanWorkspacePath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	if p == "" || len(p) > 200 || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("invalid file name")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`:*?"<>|`, r) {
			return "", fmt.Errorf("invalid character in file name")
		}
	}
	clean := path.Clean(p)
	if clean != p || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("invalid file name")
	}
	return clean, nil
}

func (s *Store) ListWorkspace(ctx context.Context) ([]WorkspaceFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, length(content), updated_at FROM workspace_files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkspaceFile{}
	for rows.Next() {
		var f WorkspaceFile
		var at int64
		if err := rows.Scan(&f.Path, &f.Size, &at); err != nil {
			return nil, err
		}
		f.UpdatedAt = ms(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) GetWorkspaceFile(ctx context.Context, p string) (*WorkspaceFile, error) {
	var f WorkspaceFile
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT path, content, updated_at FROM workspace_files WHERE path=?`, p).Scan(&f.Path, &f.Content, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	f.Size, f.UpdatedAt = len(f.Content), ms(at)
	return &f, err
}

func (s *Store) PutWorkspaceFile(ctx context.Context, p, content string) (*WorkspaceFile, error) {
	if len(content) > MaxWorkspaceFile {
		return nil, fmt.Errorf("file is larger than %d KB", MaxWorkspaceFile>>10)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM workspace_files WHERE path<>?`, p).Scan(&n); err != nil {
		return nil, err
	}
	if n >= MaxWorkspaceFiles {
		return nil, ErrWorkspaceFull
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspace_files(path, content, updated_at) VALUES(?,?,?)
		ON CONFLICT(path) DO UPDATE SET content=excluded.content, updated_at=excluded.updated_at`, p, content, toMS(now))
	if err != nil {
		return nil, err
	}
	return &WorkspaceFile{Path: p, Size: len(content), UpdatedAt: now}, nil
}

func (s *Store) RenameWorkspaceFile(ctx context.Context, from, to string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE workspace_files SET path=?, updated_at=? WHERE path=?`, to, toMS(time.Now()), from)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%s already exists", to)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteWorkspaceFile(ctx context.Context, p string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspace_files WHERE path=?`, p)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// WorkspaceContents returns every file with its content (zip download).
func (s *Store) WorkspaceContents(ctx context.Context) ([]WorkspaceFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, content, updated_at FROM workspace_files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceFile
	for rows.Next() {
		var f WorkspaceFile
		var at int64
		if err := rows.Scan(&f.Path, &f.Content, &at); err != nil {
			return nil, err
		}
		f.Size, f.UpdatedAt = len(f.Content), ms(at)
		out = append(out, f)
	}
	return out, rows.Err()
}
