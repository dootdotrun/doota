// Package session implements stateless, signed session cookies.
//
// There is no session table. The signing key is generated on first boot and kept
// in app_config under session.secret (see Store.EnsureSessionSecret), so deleting
// that row invalidates every session — the logout-everywhere button. For a
// single-user tool that is sufficient: a session table would exist only to revoke
// sessions belonging to nobody else.
//
// The key is persisted rather than environmental because the environment version
// regenerated it whenever SESSION_SECRET was unset, which signed the operator out
// on every deploy.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CookieName is the session cookie name.
const CookieName = "doot_session"

// Manager issues and validates session cookies.
type Manager struct {
	secret []byte
	ttl    time.Duration
}

// NewManager returns a Manager signing with the given secret.
func NewManager(secret string, ttl time.Duration) *Manager {
	return &Manager{secret: []byte(secret), ttl: ttl}
}

// TTL is the session lifetime.
func (m *Manager) TTL() time.Duration { return m.ttl }

func (m *Manager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Issue writes a session cookie for the given user.
func (m *Manager) Issue(w http.ResponseWriter, r *http.Request, userID string) {
	payload := userID + "|" + strconv.FormatInt(time.Now().Unix(), 10)
	token := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + m.sign(payload)

	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(m.ttl),
		MaxAge:   int(m.ttl.Seconds()),
	})
}

// UserID validates the session cookie and returns the user id.
func (m *Manager) UserID(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return "", false
	}

	encoded, mac, ok := strings.Cut(c.Value, ".")
	if !ok {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	payload := string(raw)

	// Constant-time comparison: a timing-variable check here would leak the
	// signature a byte at a time.
	if !hmac.Equal([]byte(mac), []byte(m.sign(payload))) {
		return "", false
	}

	userID, issuedStr, ok := strings.Cut(payload, "|")
	if !ok || userID == "" {
		return "", false
	}
	issued, err := strconv.ParseInt(issuedStr, 10, 64)
	if err != nil {
		return "", false
	}
	if time.Since(time.Unix(issued, 0)) > m.ttl {
		return "", false
	}

	return userID, true
}

// Clear removes the session cookie.
func (m *Manager) Clear(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// isHTTPS reports whether the request arrived over TLS, accounting for Fly's
// proxy terminating TLS upstream.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}
