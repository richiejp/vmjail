package orchestrator

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const dashboardTestHost = "example.com"

func TestDashboardAuthLoginAndSession(t *testing.T) {
	auth := testDashboardAuth(t, "correct horse")
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := auth.wrap(next)

	locked := httptest.NewRecorder()
	handler.ServeHTTP(locked, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if locked.Code != http.StatusSeeOther || locked.Header().Get("Location") != "/login" {
		t.Fatalf("locked response = %d %q", locked.Code, locked.Header().Get("Location"))
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if health.Code != http.StatusNoContent {
		t.Fatalf("health response = %d", health.Code)
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", http.NoBody))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Unlock orchestrator") {
		t.Fatalf("login page = %d %q", page.Code, page.Body.String())
	}

	response := dashboardLoginRequest(handler, "correct horse")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login response = %d: %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %#v", cookies)
	}
	unlockedRequest := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	unlockedRequest.AddCookie(cookies[0])
	unlocked := httptest.NewRecorder()
	handler.ServeHTTP(unlocked, unlockedRequest)
	if unlocked.Code != http.StatusNoContent {
		t.Fatalf("unlocked response = %d", unlocked.Code)
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/logout", http.NoBody)
	logoutRequest.Header.Set("Origin", "https://"+dashboardTestHost)
	logoutRequest.Host = dashboardTestHost
	logoutRequest.AddCookie(cookies[0])
	logout := httptest.NewRecorder()
	handler.ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/login" {
		t.Fatalf("logout response = %d %q", logout.Code, logout.Header().Get("Location"))
	}
	if auth.validSession(cookies[0].Value) {
		t.Fatal("logout did not revoke session")
	}
}

func TestDashboardAuthRejectsAndThrottles(t *testing.T) {
	auth := testDashboardAuth(t, "secret")
	handler := auth.wrap(http.NotFoundHandler())
	for range dashboardAttemptLimit {
		if got := dashboardLoginRequest(handler, "wrong").Code; got != http.StatusUnauthorized {
			t.Fatalf("wrong-password response = %d", got)
		}
	}
	if got := dashboardLoginRequest(handler, "secret").Code; got != http.StatusTooManyRequests {
		t.Fatalf("limited response = %d", got)
	}
	auth.now = func() time.Time { return time.Unix(1, 0).Add(dashboardAttemptWindow) }
	if got := dashboardLoginRequest(handler, "secret").Code; got != http.StatusSeeOther {
		t.Fatalf("response after window = %d", got)
	}
}

func TestDashboardAuthExpiryInvalidTokensAndFailures(t *testing.T) {
	auth := testDashboardAuth(t, "secret")
	now := time.Unix(100, 0).UTC()
	auth.now = func() time.Time { return now }
	handler := auth.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	login := dashboardLoginRequest(handler, "secret")
	cookie := login.Result().Cookies()[0]

	now = now.Add(dashboardSessionTTL)
	expired := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	expired.AddCookie(cookie)
	expiredResponse := httptest.NewRecorder()
	handler.ServeHTTP(expiredResponse, expired)
	if expiredResponse.Code != http.StatusSeeOther {
		t.Fatalf("expired response = %d", expiredResponse.Code)
	}
	if auth.validSession("not-base64!") || auth.validSession("c2hvcnQ") {
		t.Fatal("malformed tokens must not validate")
	}

	auth.rand = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	if got := dashboardLoginRequest(handler, "secret").Code; got != http.StatusInternalServerError {
		t.Fatalf("entropy failure response = %d", got)
	}
	badForm := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password="+strings.Repeat("x", dashboardLoginBytes)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://"+dashboardTestHost)
	request.Host = dashboardTestHost
	handler.ServeHTTP(badForm, request)
	if badForm.Code != http.StatusBadRequest {
		t.Fatalf("oversized form response = %d", badForm.Code)
	}
}

func TestDashboardAuthRequiresSameOriginAndAudits(t *testing.T) {
	auth := testDashboardAuth(t, "secret")
	var events []string
	auth.audit = func(event, _ string) { events = append(events, event) }
	handler := auth.wrap(http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=secret"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.Host = "dashboard.example"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || len(events) != 1 || events[0] != "login_origin_rejected" {
		t.Fatalf("cross-origin response/events = %d %#v", response.Code, events)
	}
	if dashboardSameOrigin(httptest.NewRequest(http.MethodPost, "/", http.NoBody)) {
		t.Fatal("missing origin must fail closed")
	}

	login := dashboardLoginRequest(handler, "secret")
	cookie := login.Result().Cookies()[0]
	crossOriginLogout := httptest.NewRequest(http.MethodPost, "/logout", http.NoBody)
	crossOriginLogout.Header.Set("Origin", "https://attacker.example")
	crossOriginLogout.Host = dashboardTestHost
	crossOriginLogout.AddCookie(cookie)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, crossOriginLogout)
	if logoutResponse.Code != http.StatusForbidden || events[len(events)-1] != "logout_origin_rejected" {
		t.Fatalf("cross-origin logout = %d %#v", logoutResponse.Code, events)
	}
}

func TestDashboardAuthBoundsStateAndParsesClients(t *testing.T) {
	auth := testDashboardAuth(t, "secret")
	now := time.Unix(10, 0).UTC()
	for i := range dashboardMaxSessions + 1 {
		var digest [32]byte
		digest[0], digest[1] = byte(i), byte(i>>8)
		auth.recordSuccess("client", digest, now)
	}
	if len(auth.sessions) != dashboardMaxSessions {
		t.Fatalf("sessions = %d", len(auth.sessions))
	}
	for i := range dashboardMaxClients {
		auth.attempts[string(rune(i))] = dashboardAttempt{failed: 1, start: now}
	}
	if !auth.rateLimited("new-client", now) {
		t.Fatal("full client table must fail closed")
	}
	auth.recordFailure("another-client", now)
	if len(auth.attempts) != dashboardMaxClients {
		t.Fatalf("attempt table grew to %d", len(auth.attempts))
	}
	auth.deleteSession("invalid!")
	auth.deleteSession("c2hvcnQ")
	withoutCookie := httptest.NewRecorder()
	auth.logout(withoutCookie, sameOriginRequest(http.MethodPost, "/logout"))
	if withoutCookie.Code != http.StatusSeeOther {
		t.Fatalf("direct logout without cookie = %d", withoutCookie.Code)
	}
	if got := dashboardClient("192.0.2.1:1234"); got != "192.0.2.1" {
		t.Fatalf("parsed client = %q", got)
	}
	if got := dashboardClient("local"); got != "local" {
		t.Fatalf("unparsed client = %q", got)
	}
}

func TestDashboardAuthDefaultAudit(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	newDashboardAuth(hash).audit("test", "client")
}

func testDashboardAuth(t *testing.T, password string) *dashboardAuth {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	auth := newDashboardAuth(hash)
	auth.now = func() time.Time { return time.Unix(1, 0).UTC() }
	auth.rand = func(dst []byte) (int, error) {
		for i := range dst {
			dst[i] = byte(i)
		}
		return len(dst), nil
	}
	auth.audit = func(string, string) {}
	return auth
}

func dashboardLoginRequest(handler http.Handler, password string) *httptest.ResponseRecorder {
	form := url.Values{"password": {password}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://"+dashboardTestHost)
	request.Host = dashboardTestHost
	request.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func sameOriginRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, http.NoBody)
	request.Header.Set("Origin", "https://"+dashboardTestHost)
	request.Host = dashboardTestHost
	return request
}
