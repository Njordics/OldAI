package web

import (
	"archive/zip"
	"errors"
	"net/http"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/store"
)

func (s *Server) workspaceRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/workspace/files", s.wsList)
	api.HandleFunc("GET /api/v1/workspace/file", s.wsGet)
	api.HandleFunc("PUT /api/v1/workspace/file", s.wsPut)
	api.HandleFunc("POST /api/v1/workspace/rename", s.wsRename)
	api.HandleFunc("DELETE /api/v1/workspace/file", s.wsDelete)
	api.HandleFunc("GET /api/v1/workspace/zip", s.wsZip)
}

// File paths travel in ?path= so they may contain slashes.
func wsPath(w http.ResponseWriter, raw string) (string, bool) {
	p, err := store.CleanWorkspacePath(raw)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return "", false
	}
	return p, true
}

func (s *Server) wsList(w http.ResponseWriter, r *http.Request) {
	files, err := s.core.Store.ListWorkspace(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, files)
}

func (s *Server) wsGet(w http.ResponseWriter, r *http.Request) {
	p, ok := wsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	f, err := s.core.Store.GetWorkspaceFile(r.Context(), p)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such file")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, f)
}

type wsPutRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (s *Server) wsPut(w http.ResponseWriter, r *http.Request) {
	var req wsPutRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	p, ok := wsPath(w, req.Path)
	if !ok {
		return
	}
	f, err := s.core.Store.PutWorkspaceFile(r.Context(), p, req.Content)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, f)
}

type wsRenameRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) wsRename(w http.ResponseWriter, r *http.Request) {
	var req wsRenameRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	from, ok := wsPath(w, req.From)
	if !ok {
		return
	}
	to, ok := wsPath(w, req.To)
	if !ok {
		return
	}
	err := s.core.Store.RenameWorkspaceFile(r.Context(), from, to)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such file")
	case err != nil:
		httpx.WriteError(w, http.StatusConflict, proto.ErrWrongState, err.Error())
	default:
		httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
	}
}

func (s *Server) wsDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := wsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	if err := s.core.Store.DeleteWorkspaceFile(r.Context(), p); errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, proto.ErrNotFound, "no such file")
		return
	} else if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

func (s *Server) wsZip(w http.ResponseWriter, r *http.Request) {
	files, err := s.core.Store.WorkspaceContents(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="workspace-`+time.Now().Format("2006-01-02")+`.zip"`)
	zw := zip.NewWriter(w)
	for _, f := range files {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate, Modified: f.UpdatedAt})
		if err != nil {
			return
		}
		_, _ = fw.Write([]byte(f.Content))
	}
	_ = zw.Close()
}

// previewCSP isolates user code in the Code tab's preview: the frame is
// sandboxed without same-origin (no cookies, no dashboard access) and may
// not make network requests.
const previewCSP = "default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval'; style-src 'unsafe-inline'; " +
	"img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; form-action 'none'; " +
	"frame-ancestors 'self'; base-uri 'none'; sandbox allow-scripts allow-modals"

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	data, err := uiFiles.ReadFile("ui/preview.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", previewCSP)
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
