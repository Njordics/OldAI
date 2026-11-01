package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/proto"
)

//go:embed ui
var uiFiles embed.FS

const (
	sessionCookie = "aic_session"
	sessionTTL    = 12 * time.Hour
	// csrfHeader must accompany every mutating request authenticated by
	// cookie. Browsers can't add it cross-site without a CORS preflight,
	// which this server never approves.
	csrfHeader = "X-Requested-With"
	csrfValue  = "aicluster"
)

// sessionValue is "<expiry unix>.<hex HMAC(admin token, expiry)>". It proves
// the holder knew the admin token, expires on its own, and stops working when
// the token is rotated.
func (s *Server) sessionValue(exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return e + "." + s.sessionMAC(e)
}

func (s *Server) sessionMAC(exp string) string {
	m := hmac.New(sha256.New, []byte(s.core.AdminToken))
	m.Write([]byte("dashboard-session:" + exp))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) validSession(v string) bool {
	exp, mac, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > n {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(s.sessionMAC(exp)))
}

func (s *Server) tokenOK(tok string) bool {
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.core.AdminToken)) == 1
}

// auth accepts either the CLI's bearer token or a dashboard session cookie.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && s.tokenOK(tok) {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(sessionCookie); err == nil && s.validSession(c.Value) {
			if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != csrfValue {
				httpx.WriteError(w, http.StatusForbidden, proto.ErrForbidden, "missing "+csrfHeader+" header")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		httpx.WriteError(w, http.StatusUnauthorized, proto.ErrForbidden, "sign in with the admin token")
	})
}

type loginRequest struct {
	Token string `json:"token"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.ReadJSON(r, &req); err != nil || !s.tokenOK(strings.TrimSpace(req.Token)) {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		s.log.Warn("dashboard sign-in failed", "remote", r.RemoteAddr)
		httpx.WriteError(w, http.StatusUnauthorized, proto.ErrForbidden, "wrong admin token")
		return
	}
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.sessionValue(exp), Path: "/", Expires: exp,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}
