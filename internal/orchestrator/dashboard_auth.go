package orchestrator

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	dashboardSessionCookie = "daft_dashboard_session"
	dashboardSessionTTL    = 12 * time.Hour
	dashboardAttemptWindow = 5 * time.Minute
	dashboardAttemptLimit  = 5
	dashboardMaxSessions   = 128
	dashboardMaxClients    = 4096
	dashboardLoginBytes    = 4096
)

type dashboardSession struct {
	expires time.Time
}

type dashboardAttempt struct {
	failed int
	start  time.Time
}

type dashboardAuth struct {
	passwordHash []byte
	mu           sync.Mutex
	sessions     map[[sha256.Size]byte]dashboardSession
	attempts     map[string]dashboardAttempt
	now          func() time.Time
	rand         func([]byte) (int, error)
	loginPage    *template.Template
}

func newDashboardAuth(passwordHash []byte) *dashboardAuth {
	return &dashboardAuth{
		passwordHash: append([]byte(nil), passwordHash...),
		sessions:     make(map[[sha256.Size]byte]dashboardSession),
		attempts:     make(map[string]dashboardAttempt),
		now:          time.Now,
		rand:         rand.Read,
		loginPage:    template.Must(template.New("login").Parse(dashboardLoginHTML)),
	}
}

func (a *dashboardAuth) wrap(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", a.serveLogin)
	mux.HandleFunc("POST /login", a.login)
	mux.Handle("GET /healthz", next)
	mux.Handle("/", a.requireSession(next))
	return mux
}

func (a *dashboardAuth) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(dashboardSessionCookie)
		if err == nil && a.validSession(cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (a *dashboardAuth) validSession(token string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return false
	}
	digest := sha256.Sum256(raw)
	now := a.now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanupLocked(now)
	_, ok := a.sessions[digest]
	return ok
}

func (a *dashboardAuth) serveLogin(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = a.loginPage.Execute(w, nil)
}

func (a *dashboardAuth) login(w http.ResponseWriter, r *http.Request) {
	client := dashboardClient(r.RemoteAddr)
	now := a.now().UTC()
	if a.rateLimited(client, now) {
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, dashboardLoginBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid login", http.StatusBadRequest)
		return
	}
	if bcrypt.CompareHashAndPassword(a.passwordHash, []byte(r.FormValue("password"))) != nil {
		a.recordFailure(client, now)
		http.Error(w, "invalid login", http.StatusUnauthorized)
		return
	}
	token := make([]byte, 32)
	if _, err := a.rand(token); err != nil {
		http.Error(w, "create session", http.StatusInternalServerError)
		return
	}
	a.recordSuccess(client, sha256.Sum256(token), now)
	http.SetCookie(w, &http.Cookie{
		Name: dashboardSessionCookie, Value: base64.RawURLEncoding.EncodeToString(token),
		Path: "/", MaxAge: int(dashboardSessionTTL.Seconds()), Expires: now.Add(dashboardSessionTTL),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *dashboardAuth) rateLimited(client string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanupLocked(now)
	attempt, ok := a.attempts[client]
	if !ok {
		return len(a.attempts) >= dashboardMaxClients
	}
	return attempt.failed >= dashboardAttemptLimit
}

func (a *dashboardAuth) recordFailure(client string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanupLocked(now)
	attempt, ok := a.attempts[client]
	if !ok {
		if len(a.attempts) >= dashboardMaxClients {
			return
		}
		attempt.start = now
	}
	attempt.failed++
	a.attempts[client] = attempt
}

func (a *dashboardAuth) recordSuccess(client string, digest [sha256.Size]byte, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanupLocked(now)
	delete(a.attempts, client)
	if len(a.sessions) >= dashboardMaxSessions {
		for key := range a.sessions {
			delete(a.sessions, key)
			break
		}
	}
	a.sessions[digest] = dashboardSession{expires: now.Add(dashboardSessionTTL)}
}

func (a *dashboardAuth) cleanupLocked(now time.Time) {
	for key, session := range a.sessions {
		if !now.Before(session.expires) {
			delete(a.sessions, key)
		}
	}
	for client, attempt := range a.attempts {
		if !now.Before(attempt.start.Add(dashboardAttemptWindow)) {
			delete(a.attempts, client)
		}
	}
}

func dashboardClient(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

const dashboardLoginHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Unlock darkaifactory</title><style>body{font:16px system-ui;max-width:24rem;margin:12vh auto;padding:1rem;background:#111827;color:#e5e7eb}form{display:grid;gap:1rem;padding:1.5rem;background:#1f2937;border-radius:.75rem}input,button{font:inherit;padding:.75rem}button{cursor:pointer}</style></head><body><main><h1>Unlock orchestrator</h1><form method="post" action="/login"><label>Password <input name="password" type="password" required autofocus autocomplete="current-password"></label><button type="submit">Unlock</button></form></main></body></html>`
