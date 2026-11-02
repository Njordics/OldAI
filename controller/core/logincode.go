package core

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"

	"aicluster/common/pki"
)

// The login code is a short dashboard password shown on the Controller's
// console ("K7QM-4XPA"). It signs in to the dashboard only; the CLI and API
// keep the full admin token. Its 40 bits are safe only behind the login
// rate limits in web/session.go.

// loginAlphabet is Crockford's base32: no I, L, O or U, so nothing reads as
// another character on a screen.
const loginAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const loginCodeLen = 8

// NewLoginCode returns 8 random characters (40 bits).
func NewLoginCode() string {
	b := make([]byte, loginCodeLen)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = loginAlphabet[b[i]&31] // 256 is a multiple of 32: no bias
	}
	return string(b)
}

// FormatLoginCode splits a code for reading: "K7QM4XPA" -> "K7QM-4XPA".
func FormatLoginCode(code string) string {
	if len(code) != loginCodeLen {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// NormalizeLoginCode turns what someone typed into the stored form: any
// case, dashes and spaces ignored, look-alikes mapped (O->0, I/L->1, U->V).
func NormalizeLoginCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		case 'U':
			r = 'V'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (c *Core) loginCodePath() string { return filepath.Join(c.cfg.DataDir, "login.code") }

func (c *Core) loadLoginCode() error {
	if b, err := os.ReadFile(c.loginCodePath()); err == nil {
		if code := NormalizeLoginCode(string(b)); len(code) == loginCodeLen {
			c.setLoginCode(code)
			return nil
		}
	}
	_, err := c.RotateLoginCode()
	return err
}

// LoginCode is the current code, unformatted.
func (c *Core) LoginCode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginCode
}

func (c *Core) setLoginCode(code string) {
	c.mu.Lock()
	c.loginCode = code
	c.mu.Unlock()
}

// RotateLoginCode replaces the code; the console shows the new one within
// seconds. Existing dashboard sessions stay signed in.
func (c *Core) RotateLoginCode() (string, error) {
	code := NewLoginCode()
	// Written formatted, as the console shows it.
	if err := pki.WriteFileAtomic(c.loginCodePath(), []byte(FormatLoginCode(code)+"\n"), 0o600); err != nil {
		return "", err
	}
	c.setLoginCode(code)
	return code, nil
}
