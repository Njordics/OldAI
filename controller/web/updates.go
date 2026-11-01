package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/store"
	"aicluster/controller/updates"
)

// Update-check settings live in the settings table: update_check ("on"/"off",
// on unless turned off), update_repo, update_branch, update_dismissed (the
// newest commit the admin chose to skip).

// GitHub owner and repository names; neither may start with a dot, so "."
// and ".." can't sneak into the API path.
var (
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,99}$`)
)

func validBranch(b string) bool {
	return branchRe.MatchString(b) && !strings.Contains(b, "..") && !strings.Contains(b, "//") && !strings.HasSuffix(b, "/")
}

// UpdateSettings reads the checker's settings from the store.
func UpdateSettings(st *store.Store) func(context.Context) updates.Settings {
	return func(ctx context.Context) updates.Settings {
		on, _ := st.Setting(ctx, "update_check", "on")
		repo, _ := st.Setting(ctx, "update_repo", updates.DefaultRepo)
		branch, _ := st.Setting(ctx, "update_branch", updates.DefaultBranch)
		return updates.Settings{Enabled: on != "off", Repo: repo, Branch: branch}
	}
}

func (s *Server) WithUpdates(u *updates.Checker) *Server {
	s.updates = u
	return s
}

func (s *Server) updateRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/updates", s.getUpdates)
	api.HandleFunc("POST /api/v1/updates/check", s.checkUpdates)
	api.HandleFunc("POST /api/v1/updates/dismiss", s.dismissUpdate)
}

type UpdateView struct {
	updates.Status
	// Dismissed is the newest commit the admin chose to skip; the pop-up
	// stays hidden until something newer appears.
	Dismissed string `json:"dismissed"`
}

func (s *Server) updateView(r *http.Request, st updates.Status) UpdateView {
	d, _ := s.core.Store.Setting(r.Context(), "update_dismissed", "")
	return UpdateView{Status: st, Dismissed: d}
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "updates_disabled", "update checks are not running")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.updateView(r, s.updates.Status()))
}

func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "updates_disabled", "update checks are not running")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.updateView(r, s.updates.Check(r.Context())))
}

type dismissRequest struct {
	Commit string `json:"commit"`
}

func (s *Server) dismissUpdate(w http.ResponseWriter, r *http.Request) {
	var req dismissRequest
	if err := httpx.ReadJSON(r, &req); err != nil || len(req.Commit) > 64 {
		httpx.WriteError(w, http.StatusBadRequest, proto.ErrBadRequest, "commit is required")
		return
	}
	if err := s.core.Store.SetSetting(r.Context(), "update_dismissed", req.Commit); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

// applyUpdateSettings handles the update fields of PUT /api/v1/settings.
func (s *Server) applyUpdateSettings(ctx context.Context, req Settings) (bool, string) {
	changed := false
	if req.UpdateCheck != nil {
		v := "off"
		if *req.UpdateCheck {
			v = "on"
		}
		_ = s.core.Store.SetSetting(ctx, "update_check", v)
		changed = true
	}
	if req.UpdateRepo != "" {
		if !repoRe.MatchString(req.UpdateRepo) {
			return false, "repository must look like owner/name"
		}
		_ = s.core.Store.SetSetting(ctx, "update_repo", req.UpdateRepo)
		changed = true
	}
	if req.UpdateBranch != "" {
		if !validBranch(req.UpdateBranch) {
			return false, "invalid branch name"
		}
		_ = s.core.Store.SetSetting(ctx, "update_branch", req.UpdateBranch)
		changed = true
	}
	if changed && s.updates != nil {
		s.updates.Wake()
	}
	return true, ""
}
