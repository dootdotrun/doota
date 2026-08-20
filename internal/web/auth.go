package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dootdotrun/doot-ai/internal/store"
)

type ctxKey int

const userCtxKey ctxKey = 0

// Login throttling. /login is the only endpoint an outsider can reach, so it is
// the only one that needs this.
const (
	maxLoginFailures = 5
	loginLockout     = 5 * time.Minute
	loginFailDelay   = 400 * time.Millisecond
)

// loginLimiter locks a client out after repeated failures.
//
// Kept rather than cut, unlike most of what the audit flagged here. This app sits on
// a public URL with one account whose default password is doot/doot, so a lockout is
// the cheapest thing standing between that and a dictionary. What went is the
// bookkeeping: a struct per client tracking a failure count and a lockout deadline
// separately, with a reset path and an expiry-forgetting branch. One deadline per
// client says the same thing.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]int
	locked   map[string]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string]int{}, locked: map[string]time.Time{}}
}

// lockedFor reports the remaining lockout, if any.
func (l *loginLimiter) lockedFor(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if remaining := time.Until(l.locked[key]); remaining > 0 {
		return remaining
	}
	return 0
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[key]++
	if l.failures[key] >= maxLoginFailures {
		l.locked[key] = time.Now().Add(loginLockout)
		l.failures[key] = 0
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
	delete(l.locked, key)
}

// requireAuth gates every screen and action.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.sess.UserID(r)
		if !ok {
			s.redirectToLogin(w, r)
			return
		}

		user, err := s.store.UserByID(r.Context(), id)
		if err != nil {
			// Valid signature but no such user: the account was deleted or the
			// database was replaced. Drop the cookie rather than loop.
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Error("load session user", "error", err)
			}
			s.sess.Clear(w, r)
			s.redirectToLogin(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	// htmx swallows a 302 into the fragment; this header makes it navigate.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// userFrom returns the authenticated user. Only valid behind requireAuth.
func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userCtxKey).(*store.User)
	return u
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sess.UserID(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", page{Title: "Sign in"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, "login", page{Title: "Sign in", Error: "Malformed form submission."})
		return
	}

	key := clientKey(r)
	if remaining := s.limiter.lockedFor(key); remaining > 0 {
		s.log.Warn("login locked out", "remaining_s", int(remaining.Seconds()))
		s.render(w, r, "login", page{
			Title: "Sign in",
			Error: "Too many failed attempts. Try again in " + strconv.Itoa(int(remaining.Minutes())+1) + " minutes.",
		})
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	user, err := s.store.UserByUsername(r.Context(), username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("login lookup", "error", err)
		s.render(w, r, "login", page{Title: "Sign in", Error: "Something went wrong. Try again."})
		return
	}

	// Same response and same delay whether the user exists or the password is
	// wrong, so this cannot be used to enumerate usernames.
	if err != nil || !user.CheckPassword(password) {
		time.Sleep(loginFailDelay)
		s.limiter.fail(key)
		s.log.Warn("failed login", "username", username)
		s.render(w, r, "login", page{Title: "Sign in", Error: "Incorrect username or password."})
		return
	}

	s.limiter.reset(key)
	s.sess.Issue(w, r, user.ID)
	s.log.Info("login", "username", user.Username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sess.Clear(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func clientKey(r *http.Request) string {
	if ip := r.Header.Get("Fly-Client-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}
