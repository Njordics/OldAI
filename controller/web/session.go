package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/core"
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
	// Token is the login code shown on the Controller's console
	// ("K7QM-4XPA"), or the full admin token.
	Token string `json:"token"`
}

// codeOK checks a typed login code against the current one.
func (s *Server) codeOK(typed string) bool {
	want := s.core.LoginCode()
	return want != "" && subtle.ConstantTimeCompare([]byte(core.NormalizeLoginCode(typed)), []byte(want)) == 1
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	_ = httpx.ReadJSON(r, &req)
	typed := strings.TrimSpace(req.Token)
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	isCode := len(typed) < 32 // anything shorter can't be the full token
	if wait := s.limiter.allow(addr, isCode); wait > 0 {
		s.log.Warn("dashboard sign-in throttled", "remote", addr, "retry_in", wait.Round(time.Second))
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		httpx.WriteError(w, http.StatusTooManyRequests, proto.ErrForbidden,
			"too many wrong attempts; try again in "+humanWait(wait))
		return
	}
	if !(isCode && s.codeOK(typed)) && !(!isCode && s.tokenOK(typed)) {
		s.limiter.failed(addr, isCode)
		time.Sleep(500 * time.Millisecond) // slow down guessing
		s.log.Warn("dashboard sign-in failed", "remote", addr)
		httpx.WriteError(w, http.StatusUnauthorized, proto.ErrForbidden, "wrong login code")
		return
	}
	s.limiter.succeeded(addr)
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.sessionValue(exp), Path: "/", Expires: exp,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}

func humanWait(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())+1) + " seconds"
	}
	return strconv.Itoa(int(d.Minutes())+1) + " minutes"
}

// rotateLoginCode issues a new login code; the console shows it within
// seconds. Signed-in dashboards stay signed in.
func (s *Server) rotateLoginCode(w http.ResponseWriter, r *http.Request) {
	code, err := s.core.RotateLoginCode()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("login code rotated", "remote", r.RemoteAddr)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"login_code": core.FormatLoginCode(code)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	httpx.WriteJSON(w, http.StatusOK, proto.OK{V: proto.Version, OK: true})
}
