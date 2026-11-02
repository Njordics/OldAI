package web

import (
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/controller/gateway"
	"aicluster/controller/memory"
	"aicluster/controller/store"
)

// Shared memory (protocol §18): what the cluster remembers for each memory
// space, its backup, and the notice shown after a restore.

// WithMemory enables the Memory page.
func (s *Server) WithMemory(m *memory.Service) *Server {
	s.mem = m
	return s
}

func (s *Server) memoryRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/memory", s.getMemory)
	api.HandleFunc("POST /api/v1/memory", s.addMemory)
	api.HandleFunc("PUT /api/v1/memory/settings", s.putMemorySettings)
	api.HandleFunc("PUT /api/v1/memory/{id}", s.updateMemory)
	api.HandleFunc("DELETE /api/v1/memory/{id}", s.deleteMemory)
	api.HandleFunc("POST /api/v1/memory/forget-space", s.forgetSpace)
	api.HandleFunc("POST /api/v1/memory/backup", s.backupMemory)
	api.HandleFunc("POST /api/v1/memory/restore", s.restoreMemory)
	api.HandleFunc("GET /api/v1/memory/notice", s.getMemoryNotice)
	api.HandleFunc("POST /api/v1/memory/notice/dismiss", s.dismissMemoryNotice)
	api.HandleFunc("PUT /api/v1/keys/{id}/memory", s.setKeyMemory)
}

// MemoryView is GET /api/v1/memory?space=.
type MemoryView struct {
	Enabled  bool                   `json:"enabled"`
	Space    string                 `json:"space"`
	Spaces   []SpaceInfo            `json:"spaces"`
	Memories []*store.Memory        `json:"memories"`
	Model    string                 `json:"model"` // reads messages for facts; "" = automatic
	Backup   core.MemoryBackupState `json:"backup"`
	Notice   *core.MemoryNotice     `json:"notice,omitempty"`
	Nodes    []NodeChoice           `json:"nodes"` // for the backup node choice
}

// SpaceInfo is one memory space and who uses it.
type SpaceInfo struct {
	Name     string   `json:"name"`
	Memories int      `json:"memories"`
	Keys     []string `json:"keys"` // API key names; "dashboard" for the dashboard's chat
}

type NodeChoice struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	State  string `json:"state"`
}

var spaceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

func (s *Server) memRequired(w http.ResponseWriter) bool {
	if s.mem == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "memory_disabled", "memory is not running")
		return false
	}
	return true
}

func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memRequired(w) {
		return
	}
	ctx := r.Context()
	space := r.URL.Query().Get("space")
	if space == "" {
		space = "default"
	}
	all, err := s.core.Store.ListMemories(ctx, "")
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	spaces := map[string]*SpaceInfo{"default": {Name: "default", Keys: []string{"dashboard"}}}
	v := MemoryView{Enabled: s.core.MemoryEnabled(ctx), Space: space, Memories: []*store.Memory{},
		Backup: s.core.MemoryBackup(ctx), Notice: s.core.MemoryNoticeNow(ctx), Nodes: []NodeChoice{}}
	v.Model, _ = s.core.Store.Setting(ctx, gateway.SettingMemoryModel, "")
	for _, m := range all {
		if spaces[m.Space] == nil {
			spaces[m.Space] = &SpaceInfo{Name: m.Space}
		}
		spaces[m.Space].Memories++
		if m.Space == space {
			m.Embedding = nil // not for the browser
			v.Memories = append(v.Memories, m)
		}
	}
	if keys, err := s.core.Store.ListAPIKeys(ctx); err == nil {
		for _, k := range keys {
			if k.Memory == "" {
				continue
			}
			if spaces[k.Memory] == nil {
				spaces[k.Memory] = &SpaceInfo{Name: k.Memory}
			}
			spaces[k.Memory].Keys = append(spaces[k.Memory].Keys, k.Name)
		}
	}
	for _, sp := range spaces {
		if sp.Keys == nil {
			sp.Keys = []string{}
		}
		v.Spaces = append(v.Spaces, *sp)
	}
	sort.Slice(v.Spaces, func(i, j int) bool { return v.Spaces[i].Name < v.Spaces[j].Name })
	if nodes, err := s.core.Store.ListNodes(ctx); err == nil {
		for _, n := range nodes {
			if n.State == store.StateAdopted || n.State == store.StateOffline {
				v.Nodes = append(v.Nodes, NodeChoice{NodeID: n.NodeID, Name: n.Name, State: n.State})
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// MemoryInput adds or changes a memory.
type MemoryInput struct {
	Space  string `json:"space"`
	Kind   string `json:"kind"` // instruction | fact
	Text   string `json:"text"`
	Pinned bool   `json:"pinned"`
}

func (s *Server) addMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memRequired(w) {
		return
	}
	var in MemoryInput
	if err := httpx.ReadJSON(r, &in); err != nil || !spaceRe.MatchString(in.Space) {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "a memory needs a space (lowercase letters, digits, . _ -) and text")
		return
	}
	m, err := s.mem.Add(r.Context(), in.Space, in.Kind, in.Text, "you", in.Pinned)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	m.Embedding = nil
	httpx.WriteJSON(w, http.StatusCreated, m)
}

func (s *Server) updateMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memRequired(w) {
		return
	}
	var in MemoryInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	m, err := s.core.Store.GetMemory(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such memory")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	if strings.TrimSpace(in.Text) != "" {
		m.Text = in.Text
	}
	if in.Kind == store.MemoryInstruction || in.Kind == store.MemoryFact {
		m.Kind = in.Kind
	}
	m.Pinned, m.Source = in.Pinned, "you"
	if err := s.mem.Update(r.Context(), m); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	m.Embedding = nil
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	if err := s.core.Store.DeleteMemories(r.Context(), r.PathValue("id")); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

func (s *Server) forgetSpace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Space string `json:"space"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil || in.Space == "" {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "space is required")
		return
	}
	if err := s.core.Store.DeleteMemorySpace(r.Context(), in.Space); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("memory space forgotten", "space", in.Space)
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

// MemorySettings is PUT /api/v1/memory/settings; absent fields are kept.
type MemorySettings struct {
	Enabled    *bool   `json:"enabled,omitempty"`
	BackupNode *string `json:"backup_node,omitempty"` // "" = automatic
	Model      *string `json:"model,omitempty"`       // "" = automatic
}

func (s *Server) putMemorySettings(w http.ResponseWriter, r *http.Request) {
	var in MemorySettings
	if err := httpx.ReadJSON(r, &in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	if in.Enabled != nil {
		_ = s.core.SetMemoryEnabled(ctx, *in.Enabled)
	}
	if in.BackupNode != nil {
		if err := s.core.SetMemoryBackupNode(ctx, *in.BackupNode); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "no such node")
			return
		}
	}
	if in.Model != nil {
		_ = s.core.Store.SetSetting(ctx, gateway.SettingMemoryModel, strings.TrimSpace(*in.Model))
	}
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

func (s *Server) backupMemory(w http.ResponseWriter, r *http.Request) {
	if err := s.core.BackupMemoryNow(r.Context()); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "backup_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.core.MemoryBackup(r.Context()))
}

func (s *Server) restoreMemory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NodeID string `json:"node_id"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil || in.NodeID == "" {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "node_id is required")
		return
	}
	n, err := s.core.RestoreMemoryFrom(r.Context(), in.NodeID)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "restore_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

// getMemoryNotice is the cheap poll behind the dashboard's banner.
func (s *Server) getMemoryNotice(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"notice": s.core.MemoryNoticeNow(r.Context())})
}

func (s *Server) dismissMemoryNotice(w http.ResponseWriter, r *http.Request) {
	s.core.DismissMemoryNotice(r.Context())
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

func (s *Server) setKeyMemory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Space string `json:"space"` // "" = no memory
	}
	if err := httpx.ReadJSON(r, &in); err != nil || (in.Space != "" && !spaceRe.MatchString(in.Space)) {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "space must be lowercase letters, digits, . _ - (or empty for none)")
		return
	}
	err := s.core.Store.SetAPIKeyMemory(r.Context(), r.PathValue("id"), in.Space)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such key")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}
