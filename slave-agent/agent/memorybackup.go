package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/proto"
)

// Memory backups (protocol §18). The Controller keeps copies of the
// cluster's shared memory on one node, which never reads them: models
// only ever see memory through the Controller. The node keeps the last few
// copies, and keeps them when it is released, so a reinstalled Controller
// can still restore them (with the user's say-so).

const (
	memBackupDir  = "memory-backup"
	memBackupKeep = 5
	memBackupMax  = 256 << 20
)

type memBackupInfo struct {
	File       string    `json:"file"`
	ClusterID  string    `json:"cluster_id"`
	Generation int64     `json:"generation"`
	Count      int       `json:"count"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	SavedAt    time.Time `json:"saved_at"`
}

func (a *Agent) memBackupPath(name string) string {
	return filepath.Join(a.cfg.DataDir, memBackupDir, name)
}

// handleMemBackupPut stores a copy the Controller sends.
func (a *Agent) handleMemBackupPut(w http.ResponseWriter, r *http.Request) {
	gen, err := strconv.ParseInt(r.Header.Get("X-AICluster-Generation"), 10, 64)
	count, _ := strconv.Atoi(r.Header.Get("X-AICluster-Count"))
	cluster, want := r.Header.Get("X-AICluster-Cluster"), strings.ToLower(r.Header.Get("X-AICluster-SHA256"))
	if err != nil || gen <= 0 || cluster == "" || len(want) != 64 {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "missing backup headers")
		return
	}
	if err := os.MkdirAll(a.memBackupPath(""), 0o700); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	tmp, err := os.CreateTemp(a.memBackupPath(""), ".part-*")
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r.Body, memBackupMax+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil || n > memBackupMax {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, fmt.Sprintf("backup not received: %v", err))
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "backup damaged in transit (checksum mismatch)")
		return
	}
	info := memBackupInfo{File: fmt.Sprintf("memory-%s-%d.json.gz", time.Now().UTC().Format("20060102T150405"), gen),
		ClusterID: cluster, Generation: gen, Count: count, SHA256: want, Size: n, SavedAt: time.Now().UTC()}
	if err := os.Rename(tmp.Name(), a.memBackupPath(info.File)); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(a.memBackupPath(info.File+".info"), b, 0o600); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	a.pruneMemBackups()
	a.log.Info("memory backup stored", "generation", gen, "memories", count, "bytes", n)
	httpx.WriteJSON(w, http.StatusOK, info)
}

// memBackups lists the stored copies, newest first.
func (a *Agent) memBackups() []memBackupInfo {
	files, _ := filepath.Glob(a.memBackupPath("*.info"))
	var out []memBackupInfo
	for _, f := range files {
		var i memBackupInfo
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &i) == nil {
			if _, err := os.Stat(a.memBackupPath(i.File)); err == nil {
				out = append(out, i)
			}
		}
	}
	sort.Slice(out, func(x, y int) bool { return out[x].SavedAt.After(out[y].SavedAt) })
	return out
}

func (a *Agent) pruneMemBackups() {
	for _, i := range a.memBackups()[min(memBackupKeep, len(a.memBackups())):] {
		_ = os.Remove(a.memBackupPath(i.File))
		_ = os.Remove(a.memBackupPath(i.File + ".info"))
	}
}

// handleMemBackupInfo describes the newest copy (404 when there is none).
func (a *Agent) handleMemBackupInfo(w http.ResponseWriter, r *http.Request) {
	all := a.memBackups()
	if len(all) == 0 {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no memory backup on this node")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, all[0])
}

// handleMemBackupGet sends the newest copy, its details in headers.
func (a *Agent) handleMemBackupGet(w http.ResponseWriter, r *http.Request) {
	all := a.memBackups()
	if len(all) == 0 {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no memory backup on this node")
		return
	}
	i := all[0]
	f, err := os.Open(a.memBackupPath(i.File))
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("X-AICluster-Cluster", i.ClusterID)
	w.Header().Set("X-AICluster-Generation", strconv.FormatInt(i.Generation, 10))
	w.Header().Set("X-AICluster-SHA256", i.SHA256)
	w.Header().Set("Content-Length", strconv.FormatInt(i.Size, 10))
	_, _ = io.Copy(w, f)
}
