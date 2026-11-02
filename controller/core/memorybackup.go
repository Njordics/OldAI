package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"aicluster/common/proto"
	"aicluster/controller/store"
)

// Memory backups (protocol §18). The shared memory lives in the Controller's
// database. A copy goes to one Slave, the backup node, whenever memory
// changes and at least hourly; models never read it. If the Controller
// finds its memory missing or older than the backup (a database restored
// from an older copy, a bug, a failed disk), it restores the backup and
// leaves a notice for the dashboard. A backup from another cluster, as after
// reinstalling the Controller, is only offered, never restored by itself.

const (
	settingMemory       = "memory"             // on (default) | off
	settingMemBackup    = "memory_backup_node" // node ID; "" = automatic
	settingMemBackupRun = "memory_backup_state"
	settingMemNotice    = "memory_notice"
	memBackupRefresh    = time.Hour
	memCheckEarly       = 10 * time.Minute // after start, look for losses every loop
	memCheckLater       = 10 * time.Minute // then this often
)

// MemoryEnabled is the Settings switch for shared memory.
func (c *Core) MemoryEnabled(ctx context.Context) bool {
	v, _ := c.Store.Setting(ctx, settingMemory, "on")
	return v != "off"
}

func (c *Core) SetMemoryEnabled(ctx context.Context, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	return c.Store.SetSetting(ctx, settingMemory, v)
}

// MemoryBackupState is what the dashboard shows about backups.
type MemoryBackupState struct {
	Configured string    `json:"configured"` // node ID chosen in Settings; "" = automatic
	NodeID     string    `json:"node_id,omitempty"`
	NodeName   string    `json:"node_name,omitempty"`
	Generation int64     `json:"generation"`
	Count      int       `json:"count"`
	At         time.Time `json:"at,omitempty"`
	Error      string    `json:"error,omitempty"`
	Local      int64     `json:"local_generation"`
}

// MemoryNotice tells the user that memory was restored, or that a backup
// from another cluster was found.
type MemoryNotice struct {
	Kind       string    `json:"kind"` // restored | found
	NodeID     string    `json:"node_id"`
	NodeName   string    `json:"node_name"`
	ClusterID  string    `json:"cluster_id"`
	Generation int64     `json:"generation"`
	Count      int       `json:"count"`
	SavedAt    time.Time `json:"saved_at"`
	Before     int       `json:"before"` // memories the Controller had
	Reason     string    `json:"reason,omitempty"`
	At         time.Time `json:"at"`
	Dismissed  bool      `json:"dismissed,omitempty"`
}

type memBackups struct {
	mu      sync.Mutex // one backup or restore at a time
	started time.Time
	checked time.Time
}

type backupFile struct {
	V          int             `json:"v"`
	ClusterID  string          `json:"cluster_id"`
	Generation int64           `json:"generation"`
	SavedAt    time.Time       `json:"saved_at"`
	Memories   []*store.Memory `json:"memories"`
}

type backupInfo struct {
	ClusterID  string    `json:"cluster_id"`
	Generation int64     `json:"generation"`
	Count      int       `json:"count"`
	SHA256     string    `json:"sha256"`
	SavedAt    time.Time `json:"saved_at"`
}

func (c *Core) readJSONSetting(ctx context.Context, key string, v any) bool {
	raw, err := c.Store.Setting(ctx, key, "")
	return err == nil && raw != "" && json.Unmarshal([]byte(raw), v) == nil
}

func (c *Core) writeJSONSetting(ctx context.Context, key string, v any) {
	b, _ := json.Marshal(v)
	if err := c.Store.SetSetting(ctx, key, string(b)); err != nil {
		c.log.Error("save setting", "key", key, "err", err)
	}
}

// MemoryBackup describes the latest backup.
func (c *Core) MemoryBackup(ctx context.Context) MemoryBackupState {
	var st MemoryBackupState
	c.readJSONSetting(ctx, settingMemBackupRun, &st)
	st.Configured, _ = c.Store.Setting(ctx, settingMemBackup, "")
	st.Local, _ = c.Store.MemoryGeneration(ctx)
	return st
}

// SetMemoryBackupNode picks the backup node ("" = automatic).
func (c *Core) SetMemoryBackupNode(ctx context.Context, nodeID string) error {
	if nodeID != "" {
		if _, err := c.Store.GetNode(ctx, nodeID); err != nil {
			return err
		}
	}
	return c.Store.SetSetting(ctx, settingMemBackup, nodeID)
}

// MemoryNoticeNow is the notice to show, if any.
func (c *Core) MemoryNoticeNow(ctx context.Context) *MemoryNotice {
	var n MemoryNotice
	if !c.readJSONSetting(ctx, settingMemNotice, &n) || n.Dismissed {
		return nil
	}
	return &n
}

func (c *Core) DismissMemoryNotice(ctx context.Context) {
	var n MemoryNotice
	if c.readJSONSetting(ctx, settingMemNotice, &n) {
		n.Dismissed = true
		c.writeJSONSetting(ctx, settingMemNotice, n)
	}
}

// backupNodes lists online nodes that keep memory backups.
func (c *Core) backupNodes(ctx context.Context) []*store.Node {
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return nil
	}
	fresh := c.cfg.HeartbeatInterval * proto.MissedHeartbeatsOffline
	var out []*store.Node
	for _, n := range nodes {
		var obs proto.ObservedState
		if n.State != store.StateAdopted || time.Since(n.LastHeartbeat) > fresh ||
			json.Unmarshal(n.Observed, &obs) != nil || obs.Versions[proto.VersionMemoryBackup] != "yes" {
			continue
		}
		out = append(out, n)
	}
	return out
}

// backupTarget is the configured node if it is online, else the online
// node with the most free disk.
func (c *Core) backupTarget(ctx context.Context) *store.Node {
	nodes := c.backupNodes(ctx)
	want, _ := c.Store.Setting(ctx, settingMemBackup, "")
	var best *store.Node
	var bestFree uint64
	for _, n := range nodes {
		if n.NodeID == want {
			return n
		}
		var h proto.Health
		_ = json.Unmarshal(n.Health, &h)
		if best == nil || h.DiskFree > bestFree {
			best, bestFree = n, h.DiskFree
		}
	}
	if want != "" {
		return nil // the chosen node is offline: wait for it
	}
	return best
}

func (c *Core) memoryBackupLoop(ctx context.Context) {
	c.membk.started = time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.MemoryBackupEvery):
		}
		c.membk.mu.Lock()
		if time.Since(c.membk.started) < memCheckEarly || time.Since(c.membk.checked) > memCheckLater {
			c.checkMemoryLoss(ctx)
			c.membk.checked = time.Now()
		}
		c.backupMemory(ctx, false)
		c.membk.mu.Unlock()
	}
}

// BackupMemoryNow sends a copy to the backup node at once.
func (c *Core) BackupMemoryNow(ctx context.Context) error {
	c.membk.mu.Lock()
	defer c.membk.mu.Unlock()
	return c.backupMemory(ctx, true)
}

func (c *Core) backupMemory(ctx context.Context, force bool) error {
	gen, err := c.Store.MemoryGeneration(ctx)
	if err != nil || gen == 0 {
		// A cluster that never had memory sends nothing, so it can't
		// overwrite a backup it may still need to restore.
		return err
	}
	var st MemoryBackupState
	c.readJSONSetting(ctx, settingMemBackupRun, &st)
	target := c.backupTarget(ctx)
	if target == nil {
		if st.Error == "" || force {
			st.Error = "no node is online to keep the backup"
			c.writeJSONSetting(ctx, settingMemBackupRun, st)
		}
		return errors.New(st.Error)
	}
	if !force && st.Error == "" && st.NodeID == target.NodeID && st.Generation == gen && time.Since(st.At) < memBackupRefresh {
		return nil
	}
	all, err := c.Store.ListMemories(ctx, "")
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_ = json.NewEncoder(zw).Encode(backupFile{V: 1, ClusterID: c.ClusterID, Generation: gen, SavedAt: time.Now().UTC(), Memories: all})
	_ = zw.Close()
	sum := sha256.Sum256(buf.Bytes())
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, nodeBase(target)+"/v1/memory-backup", bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-AICluster-Cluster", c.ClusterID)
	req.Header.Set("X-AICluster-Generation", strconv.FormatInt(gen, 10))
	req.Header.Set("X-AICluster-Count", strconv.Itoa(len(all)))
	req.Header.Set("X-AICluster-SHA256", hex.EncodeToString(sum[:]))
	st = MemoryBackupState{NodeID: target.NodeID, NodeName: target.Name, Generation: st.Generation, Count: st.Count, At: st.At}
	resp, err := c.NodeClient(target.NodeID, time.Minute).Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("the node answered %s", resp.Status)
		}
	}
	if err != nil {
		st.Error = "backup to " + target.Name + " failed: " + err.Error()
		c.writeJSONSetting(ctx, settingMemBackupRun, st)
		c.log.Warn("memory backup failed", "node", target.Name, "err", err)
		return err
	}
	st.Generation, st.Count, st.At = gen, len(all), time.Now().UTC()
	c.writeJSONSetting(ctx, settingMemBackupRun, st)
	c.log.Info("memory backed up", "node", target.Name, "generation", gen, "memories", len(all))
	return nil
}

// checkMemoryLoss compares the Controller's memory with the newest backup
// on any node and restores it when the Controller's is missing or older.
func (c *Core) checkMemoryLoss(ctx context.Context) {
	localGen, err := c.Store.MemoryGeneration(ctx)
	if err != nil {
		return
	}
	local, err := c.Store.ListMemories(ctx, "")
	if err != nil {
		return
	}
	var best *store.Node
	var bestInfo backupInfo
	for _, n := range c.backupNodes(ctx) {
		var info backupInfo
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := httpGetJSON(cctx, c.NodeClient(n.NodeID, 10*time.Second), nodeBase(n)+"/v1/memory-backup/info", &info)
		cancel()
		if err != nil || info.Count == 0 {
			continue
		}
		// Ours first, then the newest.
		better := best == nil ||
			(info.ClusterID == c.ClusterID) != (bestInfo.ClusterID == c.ClusterID) && info.ClusterID == c.ClusterID ||
			(info.ClusterID == c.ClusterID) == (bestInfo.ClusterID == c.ClusterID) && info.SavedAt.After(bestInfo.SavedAt)
		if better {
			best, bestInfo = n, info
		}
	}
	if best == nil {
		return
	}
	if bestInfo.ClusterID != c.ClusterID {
		if len(local) > 0 {
			return
		}
		var old MemoryNotice
		c.readJSONSetting(ctx, settingMemNotice, &old)
		if old.Kind == "found" && old.NodeID == best.NodeID && old.Generation == bestInfo.Generation {
			return // already offered
		}
		c.writeJSONSetting(ctx, settingMemNotice, MemoryNotice{Kind: "found", NodeID: best.NodeID, NodeName: best.Name,
			ClusterID: bestInfo.ClusterID, Generation: bestInfo.Generation, Count: bestInfo.Count, SavedAt: bestInfo.SavedAt, At: time.Now().UTC()})
		c.log.Warn("found a memory backup from another cluster; the dashboard offers to restore it", "node", best.Name, "memories", bestInfo.Count)
		return
	}
	var reason string
	switch {
	case bestInfo.Generation > localGen:
		reason = fmt.Sprintf("the Controller's memory was older than the backup (change %d, backup at change %d)", localGen, bestInfo.Generation)
	case len(local) == 0 && bestInfo.Generation >= localGen:
		reason = "the Controller's memory was empty"
	default:
		return
	}
	if err := c.restoreMemory(ctx, best, false); err != nil {
		c.log.Error("memory restore failed", "node", best.Name, "err", err)
		return
	}
	c.writeJSONSetting(ctx, settingMemNotice, MemoryNotice{Kind: "restored", NodeID: best.NodeID, NodeName: best.Name,
		ClusterID: bestInfo.ClusterID, Generation: bestInfo.Generation, Count: bestInfo.Count, SavedAt: bestInfo.SavedAt,
		Before: len(local), Reason: reason, At: time.Now().UTC()})
	c.log.Warn("memory restored from backup", "node", best.Name, "memories", bestInfo.Count, "had", len(local), "why", reason)
}

// RestoreMemoryFrom restores the newest backup on a node, even one from
// another cluster (the user chose it).
func (c *Core) RestoreMemoryFrom(ctx context.Context, nodeID string) (*MemoryNotice, error) {
	n, err := c.Store.GetNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	c.membk.mu.Lock()
	defer c.membk.mu.Unlock()
	before, _ := c.Store.ListMemories(ctx, "")
	f, err := c.fetchBackup(ctx, n)
	if err != nil {
		return nil, err
	}
	if err := c.applyBackup(ctx, f, true); err != nil {
		return nil, err
	}
	notice := MemoryNotice{Kind: "restored", NodeID: n.NodeID, NodeName: n.Name, ClusterID: f.ClusterID, Generation: f.Generation,
		Count: len(f.Memories), SavedAt: f.SavedAt, Before: len(before), Reason: "you restored it", At: time.Now().UTC()}
	c.writeJSONSetting(ctx, settingMemNotice, notice)
	c.log.Warn("memory restored from backup at the user's request", "node", n.Name, "memories", len(f.Memories))
	return &notice, nil
}

func (c *Core) restoreMemory(ctx context.Context, n *store.Node, foreign bool) error {
	f, err := c.fetchBackup(ctx, n)
	if err != nil {
		return err
	}
	return c.applyBackup(ctx, f, foreign)
}

func (c *Core) fetchBackup(ctx context.Context, n *store.Node) (*backupFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeBase(n)+"/v1/memory-backup", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.NodeClient(n.NodeID, time.Minute).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", n.Name, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != resp.Header.Get("X-AICluster-SHA256") {
		return nil, errors.New("the backup is damaged (checksum mismatch)")
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var f backupFile
	if err := json.NewDecoder(zr).Decode(&f); err != nil {
		return nil, fmt.Errorf("the backup can't be read: %w", err)
	}
	return &f, nil
}

// applyBackup replaces the Controller's memory with f. A backup from this
// cluster keeps its change count; one from another cluster continues from
// whichever count is higher, so the next backup is newer than both.
func (c *Core) applyBackup(ctx context.Context, f *backupFile, foreign bool) error {
	gen := f.Generation
	if foreign || f.ClusterID != c.ClusterID {
		local, _ := c.Store.MemoryGeneration(ctx)
		gen = max(local, f.Generation) + 1
	}
	for _, m := range f.Memories {
		if m.Source == "" {
			m.Source = "restored"
		}
	}
	sort.SliceStable(f.Memories, func(i, j int) bool { return f.Memories[i].CreatedAt.Before(f.Memories[j].CreatedAt) })
	return c.Store.ReplaceMemories(ctx, f.Memories, gen)
}

func httpGetJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
