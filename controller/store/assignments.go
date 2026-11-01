package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Assignment says "node N should hold model M at quantisation Q". The fit
// numbers are what the provisioner computed when it was placed; they let the
// next placement subtract what this one uses.
type Assignment struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	ModelID   string    `json:"model_id"`
	Quant     string    `json:"quant"`
	Runtime   string    `json:"runtime"`
	Ref       string    `json:"ref"`
	Size      int64     `json:"size"`
	Need      int64     `json:"need"`
	Placement string    `json:"placement"`
	VRAM      int64     `json:"vram"`
	RAM       int64     `json:"ram"`
	Residency string    `json:"residency"`
	CreatedAt time.Time `json:"created_at"`
	// Pooled models: every member of one pool shares GroupID; Role is
	// "leader" (holds the file, serves requests) or "worker" (computes its
	// Share of the layers). Plain assignments leave these empty.
	GroupID string  `json:"group_id,omitempty"`
	Role    string  `json:"role,omitempty"`
	Share   float64 `json:"share,omitempty"`
}

const (
	RoleLeader = "leader"
	RoleWorker = "worker"
)

const assignCols = `id,node_id,model_id,quant,runtime,ref,size,need,placement,vram,ram,residency,created_at,group_id,role,share`

func scanAssignment(r scanner) (*Assignment, error) {
	var a Assignment
	var created int64
	err := r.Scan(&a.ID, &a.NodeID, &a.ModelID, &a.Quant, &a.Runtime, &a.Ref, &a.Size, &a.Need, &a.Placement,
		&a.VRAM, &a.RAM, &a.Residency, &created, &a.GroupID, &a.Role, &a.Share)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	a.CreatedAt = ms(created)
	return &a, err
}

func (s *Store) ListAssignments(ctx context.Context, nodeID string) ([]*Assignment, error) {
	q := `SELECT ` + assignCols + ` FROM assignments`
	args := []any{}
	if nodeID != "" {
		q += ` WHERE node_id=?`
		args = append(args, nodeID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Assignment
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAssignment(ctx context.Context, id string) (*Assignment, error) {
	return scanAssignment(s.db.QueryRowContext(ctx, `SELECT `+assignCols+` FROM assignments WHERE id=?`, id))
}

func insertAssignment(ctx context.Context, x interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, a *Assignment) error {
	_, err := x.ExecContext(ctx, `INSERT INTO assignments(`+assignCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.NodeID, a.ModelID, a.Quant, a.Runtime, a.Ref, a.Size, a.Need, a.Placement, a.VRAM, a.RAM, a.Residency, toMS(a.CreatedAt),
		a.GroupID, a.Role, a.Share)
	return err
}

func (s *Store) PutAssignments(ctx context.Context, as []*Assignment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range as {
		if err := insertAssignment(ctx, tx, a); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SwapAssignment deletes oldID and inserts next in one transaction, so the
// node's desired state never contains both or neither.
func (s *Store) SwapAssignment(ctx context.Context, oldID string, next *Assignment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE id=?`, oldID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := insertAssignment(ctx, tx, next); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteAssignment(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM assignments WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteNodeAssignments(ctx context.Context, nodeID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM assignments WHERE node_id=?`, nodeID)
	return err
}

// MoveAssignments hands a reinstalled node's models to its new identity.
func (s *Store) MoveAssignments(ctx context.Context, fromNode, toNode string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE assignments SET node_id=? WHERE node_id=?`, toNode, fromNode)
	return err
}

// ListGroup returns the members of a pool, leader first.
func (s *Store) ListGroup(ctx context.Context, groupID string) ([]*Assignment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assignCols+` FROM assignments WHERE group_id=? ORDER BY role='leader' DESC, share DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Assignment
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteGroup removes a whole pool and returns the nodes it was on.
func (s *Store) DeleteGroup(ctx context.Context, groupID string) ([]string, error) {
	members, err := s.ListGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, ErrNotFound
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM assignments WHERE group_id=?`, groupID); err != nil {
		return nil, err
	}
	nodes := make([]string, 0, len(members))
	for _, m := range members {
		nodes = append(nodes, m.NodeID)
	}
	return nodes, nil
}

// GroupsOfNode lists the pools a node takes part in.
func (s *Store) GroupsOfNode(ctx context.Context, nodeID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT group_id FROM assignments WHERE node_id=? AND group_id<>''`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
