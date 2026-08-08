package orchestrator

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/bcrypt"

	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

func TestDashboardWebAuthnStorePersistsAndUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "webauthn.json")
	store, err := openDashboardWebAuthnStore(path)
	if err != nil {
		t.Fatalf("open new store: %v", err)
	}
	firstID := bytes.Clone(store.user().id)
	credential := webauthn.Credential{ID: []byte("key-one"), PublicKey: []byte("public-key")}
	if err := store.addCredential(credential); err != nil {
		t.Fatalf("add credential: %v", err)
	}
	if err := store.addCredential(credential); err == nil {
		t.Fatal("expected duplicate credential error")
	}
	credential.Authenticator.SignCount = 7
	if err := store.updateCredential(credential); err != nil {
		t.Fatalf("update credential: %v", err)
	}
	if err := store.updateCredential(webauthn.Credential{ID: []byte("missing")}); err == nil {
		t.Fatal("expected missing credential error")
	}

	reopened, err := openDashboardWebAuthnStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	user := reopened.user()
	if !bytes.Equal(user.id, firstID) || len(user.credentials) != 1 || user.credentials[0].Authenticator.SignCount != 7 {
		t.Fatalf("reopened state = %#v", user)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}
}

func TestDashboardWebAuthnStoreRejectsInvalidState(t *testing.T) {
	dir := t.TempDir()
	if _, err := openDashboardWebAuthnStore("relative.json"); err == nil {
		t.Fatal("expected relative-path error")
	}
	if _, err := openDashboardWebAuthnStore(dir); err == nil {
		t.Fatal("expected directory read error")
	}
	tests := []struct {
		name string
		data []byte
	}{
		{"invalid-json", []byte("{")},
		{"invalid-user", []byte(`{"user_id":"c2hvcnQ=","credentials":[]}`)},
		{"too-many-credentials", mustDashboardWebAuthnState(t, dashboardWebAuthnState{UserID: make([]byte, dashboardWebAuthnUserIDBytes), Credentials: make([]webauthn.Credential, dashboardWebAuthnMaxKeys+1)})},
		{"too-large", bytes.Repeat([]byte("x"), dashboardWebAuthnMaxFile+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			if _, err := openDashboardWebAuthnStore(path); err == nil {
				t.Fatal("expected invalid-state error")
			}
		})
	}
}

func TestDashboardWebAuthnCredentialLimitAndFailedWriteRollback(t *testing.T) {
	store, err := openDashboardWebAuthnStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.state.Credentials = make([]webauthn.Credential, dashboardWebAuthnMaxKeys)
	if err := store.addCredential(webauthn.Credential{ID: []byte("extra")}); err == nil {
		t.Fatal("expected credential-limit error")
	}
	store.state.Credentials = nil
	store.path = filepath.Join(t.TempDir(), "missing", "state.json")
	blocker := filepath.Dir(store.path)
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if err := store.addCredential(webauthn.Credential{ID: []byte("key")}); err == nil {
		t.Fatal("expected persistence error")
	}
	if store.credentialCount() != 0 {
		t.Fatal("failed persistence changed in-memory state")
	}
	store.state.Credentials = []webauthn.Credential{{ID: []byte("existing")}}
	if err := store.updateCredential(webauthn.Credential{ID: []byte("existing"), PublicKey: []byte("changed")}); err == nil {
		t.Fatal("expected update persistence error")
	}
	if len(store.state.Credentials[0].PublicKey) != 0 {
		t.Fatal("failed update changed in-memory state")
	}
}

func TestDashboardWebAuthnStoreRejectsOversizeAndFilesystemFailures(t *testing.T) {
	store, err := openDashboardWebAuthnStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	oversized := dashboardWebAuthnState{
		UserID:      make([]byte, dashboardWebAuthnUserIDBytes),
		Credentials: []webauthn.Credential{{ID: bytes.Repeat([]byte("x"), dashboardWebAuthnMaxFile)}},
	}
	if err := store.saveStateLocked(oversized); err == nil {
		t.Fatal("expected oversized-state error")
	}
	store.path = "/proc/dashboard-webauthn-state.json"
	if err := store.saveStateLocked(store.state); err == nil {
		t.Fatal("expected temporary-file creation error")
	}
	targetDirectory := filepath.Join(t.TempDir(), "existing-directory")
	if err := os.Mkdir(targetDirectory, 0o700); err != nil {
		t.Fatalf("create rename target: %v", err)
	}
	store.path = targetDirectory
	if err := store.saveStateLocked(store.state); err == nil {
		t.Fatal("expected replace-directory error")
	}
}

func TestDashboardWebAuthnCeremoniesAreBoundedSingleUseAndExpire(t *testing.T) {
	wa := testDashboardWebAuthn(t)
	now := time.Unix(100, 0).UTC()
	wa.now = func() time.Time { return now }
	var next byte
	wa.randomRead = func(dst []byte) (int, error) {
		next++
		for i := range dst {
			dst[i] = next
		}
		return len(dst), nil
	}
	session := &webauthn.SessionData{Challenge: "challenge"}
	id, err := wa.begin("login", "client", session)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := wa.take(id, "register", "client"); err == nil {
		t.Fatal("wrong ceremony kind accepted")
	}
	if _, err := wa.take(id, "login", "client"); err == nil {
		t.Fatal("consumed ceremony replayed")
	}
	id, err = wa.begin("login", "client", session)
	if err != nil {
		t.Fatalf("begin expiring: %v", err)
	}
	now = now.Add(dashboardWebAuthnCeremonyTTL)
	if _, err := wa.take(id, "login", "client"); err == nil {
		t.Fatal("expired ceremony accepted")
	}
	for range dashboardWebAuthnMaxPerClient {
		if _, err := wa.begin("login", "bounded", session); err != nil {
			t.Fatalf("fill per-client ceremonies: %v", err)
		}
	}
	if _, err := wa.begin("login", "bounded", session); err == nil {
		t.Fatal("expected per-client ceremony limit")
	}
	other, err := wa.begin("login", "other", session)
	if err != nil {
		t.Fatalf("other client begin: %v", err)
	}
	if _, err := wa.take(other, "login", "wrong-client"); err == nil {
		t.Fatal("ceremony accepted from another client")
	}
	wa.pending = make(map[string]dashboardWebAuthnCeremony)
	for i := range dashboardWebAuthnMaxPending {
		if _, err := wa.begin("login", fmt.Sprintf("client-%d", i), session); err != nil {
			t.Fatalf("fill global ceremonies: %v", err)
		}
	}
	if _, err := wa.begin("login", "overflow", session); err == nil {
		t.Fatal("expected global ceremony limit")
	}
	wa.randomRead = func([]byte) (int, error) { return 0, errors.New("entropy") }
	if _, err := wa.begin("login", "client", session); err == nil {
		t.Fatal("expected entropy error")
	}
}

func TestDashboardWebAuthnHTTPOptionsAndInvalidFinishes(t *testing.T) {
	wa := testDashboardWebAuthn(t)
	if err := wa.store.addCredential(webauthn.Credential{ID: []byte("key-one"), PublicKey: []byte("key")}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	auth := testDashboardWebAuthnAuth(t, wa)
	handler := auth.wrap(http.NotFoundHandler())
	cookie := dashboardWebAuthnPasswordLogin(t, handler)

	register := dashboardWebAuthnRequest(http.MethodPost, "/v1/auth/webauthn/register/options", http.NoBody, cookie)
	registerResponse := httptest.NewRecorder()
	handler.ServeHTTP(registerResponse, register)
	registerOptions := decodeDashboardWebAuthnOptions(t, registerResponse)
	if registerOptions.CeremonyID == "" {
		t.Fatal("registration ceremony ID is empty")
	}
	finishRegistration := dashboardWebAuthnRequest(http.MethodPost, "/v1/auth/webauthn/register/finish?ceremony_id="+registerOptions.CeremonyID, strings.NewReader(`{}`), cookie)
	finishRegistration.Header.Set("Content-Type", "application/json")
	finishRegistrationResponse := httptest.NewRecorder()
	handler.ServeHTTP(finishRegistrationResponse, finishRegistration)
	if finishRegistrationResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid registration finish = %d", finishRegistrationResponse.Code)
	}

	login := dashboardWebAuthnRequest(http.MethodPost, "/v1/auth/webauthn/login/options", http.NoBody, nil)
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	loginOptions := decodeDashboardWebAuthnOptions(t, loginResponse)
	finishLogin := dashboardWebAuthnRequest(http.MethodPost, "/v1/auth/webauthn/login/finish?ceremony_id="+loginOptions.CeremonyID, strings.NewReader(`{}`), nil)
	finishLogin.Header.Set("Content-Type", "application/json")
	finishLoginResponse := httptest.NewRecorder()
	handler.ServeHTTP(finishLoginResponse, finishLogin)
	if finishLoginResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid login finish = %d", finishLoginResponse.Code)
	}

	keys := httptest.NewRecorder()
	handler.ServeHTTP(keys, dashboardWebAuthnRequest(http.MethodGet, "/security-keys", http.NoBody, cookie))
	if keys.Code != http.StatusOK || !strings.Contains(keys.Body.String(), "1 of 16") {
		t.Fatalf("security-key page = %d %q", keys.Code, keys.Body.String())
	}
}

func TestDashboardWebAuthnRegistrationPersistsVerifiedCredential(t *testing.T) {
	const rpDomain = "example.org"
	wa, err := newDashboardWebAuthn(DashboardWebAuthnConfig{
		RPID: rpDomain, RPOrigins: []string{"https://" + rpDomain},
		StatePath: filepath.Join(t.TempDir(), "webauthn.json"),
	})
	if err != nil {
		t.Fatalf("new WebAuthn: %v", err)
	}
	auth := testDashboardWebAuthnAuth(t, wa)
	handler := auth.wrap(http.NotFoundHandler())
	cookie := dashboardWebAuthnPasswordLogin(t, handler)
	body, challenge := dashboardRegistrationTestVector(t)
	session := &webauthn.SessionData{
		Challenge: challenge, UserID: wa.store.user().id,
		CredParams: []protocol.CredentialParameter{{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256}},
	}

	finish := func() *httptest.ResponseRecorder {
		t.Helper()
		probe := dashboardWebAuthnRequest(http.MethodPost, "/", http.NoBody, nil)
		probe.Host = rpDomain
		client := dashboardClient(probe.RemoteAddr)
		id, err := wa.begin("register", client, session)
		if err != nil {
			t.Fatalf("begin registration: %v", err)
		}
		request := dashboardWebAuthnRequest(http.MethodPost, "/v1/auth/webauthn/register/finish?ceremony_id="+id, bytes.NewReader(body), cookie)
		request.Host = rpDomain
		request.Header.Set("Origin", "https://"+rpDomain)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	if response := finish(); response.Code != http.StatusNoContent {
		t.Fatalf("registration = %d: %s", response.Code, response.Body.String())
	}
	if wa.store.credentialCount() != 1 {
		t.Fatalf("credential count = %d", wa.store.credentialCount())
	}
	if response := finish(); response.Code != http.StatusInternalServerError {
		t.Fatalf("duplicate registration = %d: %s", response.Code, response.Body.String())
	}
}

func TestDashboardWebAuthnLoginVerifiesCredentialAndCreatesSession(t *testing.T) {
	const (
		rpDomain     = "webauthn.io"
		credentialID = "AI7D5q2P0LS-Fal9ZT7CHM2N5BLbUunF92T8b6iYC199bO2kagSuU05-5dZGqb1SP0A0lyTWng" //nolint:gosec // Public upstream WebAuthn test-vector credential ID.
		userHandle   = "0ToAAAAAAAAAAA"
		publicKey    = "pQMmIAEhWCAoCF-x0dwEhzQo-ABxHIAgr_5WL6cJceREc81oIwFn7iJYIHEHx8ZhBIE42L26-rSC_3l0ZaWEmsHAKyP9rgslApUdAQI"
	)
	decode := func(value string) []byte {
		data, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil {
			t.Fatalf("decode test vector: %v", err)
		}
		return data
	}
	wa, err := newDashboardWebAuthn(DashboardWebAuthnConfig{
		RPID: rpDomain, RPOrigins: []string{"https://" + rpDomain},
		StatePath: filepath.Join(t.TempDir(), "webauthn.json"),
	})
	if err != nil {
		t.Fatalf("new WebAuthn: %v", err)
	}
	wa.store.state.UserID = decode(userHandle)
	wa.store.state.Credentials = []webauthn.Credential{{ID: decode(credentialID), PublicKey: decode(publicKey)}}
	if err := wa.store.saveStateLocked(wa.store.state); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	auth := testDashboardWebAuthnAuth(t, wa)
	handler := auth.wrap(http.NotFoundHandler())
	session := &webauthn.SessionData{
		UserID: wa.store.user().id, Challenge: "E4PTcIH_HfX1pC6Sigk1SC9NAlgeztN0439vi8z_c9k",
		AllowedCredentialIDs: [][]byte{decode(credentialID)}, UserVerification: protocol.VerificationDiscouraged,
	}
	body := fmt.Sprintf(`{"id":%q,"rawId":%q,"type":"public-key","response":{"authenticatorData":"dKbqkhPJnC90siSSsyDPQCYqlMGpUKA5fyklC2CEHvBFXJJiGa3OAAI1vMYKZIsLJfHwVQMANwCOw-atj9C0vhWpfWU-whzNjeQS21Lpxfdk_G-omAtffWztpGoErlNOfuXWRqm9Uj9ANJck1p6lAQIDJiABIVggKAhfsdHcBIc0KPgAcRyAIK_-Vi-nCXHkRHPNaCMBZ-4iWCBxB8fGYQSBONi9uvq0gv95dGWlhJrBwCsj_a4LJQKVHQ","clientDataJSON":"eyJjaGFsbGVuZ2UiOiJFNFBUY0lIX0hmWDFwQzZTaWdrMVNDOU5BbGdlenROMDQzOXZpOHpfYzlrIiwibmV3X2tleXNfbWF5X2JlX2FkZGVkX2hlcmUiOiJkbyBub3QgY29tcGFyZSBjbGllbnREYXRhSlNPTiBhZ2FpbnN0IGEgdGVtcGxhdGUuIFNlZSBodHRwczovL2dvby5nbC95YWJQZXgiLCJvcmlnaW4iOiJodHRwczovL3dlYmF1dGhuLmlvIiwidHlwZSI6IndlYmF1dGhuLmdldCJ9","signature":"MEUCIBtIVOQxzFYdyWQyxaLR0tik1TnuPhGVhXVSNgFwLmN5AiEAnxXdCq0UeAVGWxOaFcjBZ_mEZoXqNboY5IkQDdlWZYc","userHandle":%q}}`, credentialID, credentialID, userHandle)
	login := func() *httptest.ResponseRecorder {
		t.Helper()
		probe := dashboardWebAuthnRequest(http.MethodPost, "/", http.NoBody, nil)
		client := dashboardClient(probe.RemoteAddr)
		id, err := wa.begin("login", client, session)
		if err != nil {
			t.Fatalf("begin login: %v", err)
		}
		request := dashboardWebAuthnRequest(http.MethodPost, "/v1/auth/webauthn/login/finish?ceremony_id="+id, strings.NewReader(body), nil)
		request.Host = rpDomain
		request.Header.Set("Origin", "https://"+rpDomain)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	response := login()
	if response.Code != http.StatusNoContent || len(response.Result().Cookies()) != 1 {
		t.Fatalf("login = %d: %s", response.Code, response.Body.String())
	}
	originalRand := auth.rand
	auth.rand = func([]byte) (int, error) { return 0, errors.New("entropy") }
	if response := login(); response.Code != http.StatusInternalServerError {
		t.Fatalf("session failure = %d: %s", response.Code, response.Body.String())
	}
	auth.rand = originalRand
	originalPath := wa.store.path
	wa.store.path = "/proc/dashboard-webauthn-state.json"
	if response := login(); response.Code != http.StatusInternalServerError {
		t.Fatalf("counter persistence failure = %d: %s", response.Code, response.Body.String())
	}
	wa.store.path = originalPath
}

func TestDashboardWebAuthnLoginUnavailableAndOriginRejected(t *testing.T) {
	wa := testDashboardWebAuthn(t)
	auth := testDashboardWebAuthnAuth(t, wa)
	handler := auth.wrap(http.NotFoundHandler())
	request := dashboardWebAuthnRequest(http.MethodPost, "/v1/auth/webauthn/login/options", http.NoBody, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("empty-store login = %d", response.Code)
	}
	request = dashboardWebAuthnRequest(http.MethodPost, "/v1/auth/webauthn/login/options", http.NoBody, nil)
	request.Header.Set("Origin", "https://attacker.example")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login = %d", response.Code)
	}
}

func TestDashboardWebAuthnHTTPRejectsInvalidCeremoniesAndThrottles(t *testing.T) {
	const (
		registerOptionsPath = "/v1/auth/webauthn/register/options"
		loginFinishPath     = "/v1/auth/webauthn/login/finish"
		attackerOrigin      = "https://attacker.example"
	)
	wa := testDashboardWebAuthn(t)
	if err := wa.store.addCredential(webauthn.Credential{ID: []byte("key-one"), PublicKey: []byte("key")}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	auth := testDashboardWebAuthnAuth(t, wa)
	handler := auth.wrap(http.NotFoundHandler())
	cookie := dashboardWebAuthnPasswordLogin(t, handler)

	tests := []struct {
		name   string
		target string
		cookie *http.Cookie
		origin string
		want   int
	}{
		{"registration-requires-session", registerOptionsPath, nil, "", http.StatusSeeOther},
		{"registration-rejects-origin", registerOptionsPath, cookie, attackerOrigin, http.StatusForbidden},
		{"registration-finish-rejects-origin", "/v1/auth/webauthn/register/finish", cookie, attackerOrigin, http.StatusForbidden},
		{"registration-finish-rejects-missing-ceremony", "/v1/auth/webauthn/register/finish", cookie, "", http.StatusBadRequest},
		{"registration-finish-rejects-unknown-ceremony", "/v1/auth/webauthn/register/finish?ceremony_id=missing", cookie, "", http.StatusBadRequest},
		{"login-finish-rejects-origin", loginFinishPath, nil, attackerOrigin, http.StatusForbidden},
		{"login-finish-rejects-missing-ceremony", loginFinishPath, nil, "", http.StatusBadRequest},
		{"login-finish-rejects-unknown-ceremony", "/v1/auth/webauthn/login/finish?ceremony_id=missing", nil, "", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := dashboardWebAuthnRequest(http.MethodPost, tc.target, http.NoBody, tc.cookie)
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("response = %d, want %d", response.Code, tc.want)
			}
		})
	}
	wa.store.state.Credentials = make([]webauthn.Credential, dashboardWebAuthnMaxKeys)
	request := dashboardWebAuthnRequest(http.MethodPost, registerOptionsPath, http.NoBody, cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("credential-limit registration = %d", response.Code)
	}
	wa.store.state.Credentials = []webauthn.Credential{{ID: []byte("key-one"), PublicKey: []byte("key")}}
	originalRPID := wa.rp.Config.RPID
	wa.rp.Config.RPID = ""
	request = dashboardWebAuthnRequest(http.MethodPost, registerOptionsPath, http.NoBody, cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("invalid-RP registration = %d", response.Code)
	}
	wa.rp.Config.RPID = originalRPID

	wa.randomRead = func([]byte) (int, error) { return 0, errors.New("entropy") }
	for _, target := range []string{registerOptionsPath, "/v1/auth/webauthn/login/options"} {
		request := dashboardWebAuthnRequest(http.MethodPost, target, http.NoBody, cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("entropy failure for %s = %d", target, response.Code)
		}
	}

	client := dashboardClient(dashboardWebAuthnRequest(http.MethodPost, "/", http.NoBody, nil).RemoteAddr)
	auth.attempts[client] = dashboardAttempt{start: auth.now().UTC(), failed: dashboardAttemptLimit}
	for _, target := range []string{"/v1/auth/webauthn/login/options", loginFinishPath} {
		request := dashboardWebAuthnRequest(http.MethodPost, target, http.NoBody, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("throttled %s = %d", target, response.Code)
		}
	}
}

func TestDashboardWebAuthnJSONRejectsUnsupportedValue(t *testing.T) {
	response := httptest.NewRecorder()
	writeDashboardWebAuthnJSON(response, make(chan int))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d", response.Code)
	}
}

func TestDashboardWebAuthnRejectsInvalidStoreConfiguration(t *testing.T) {
	_, err := newDashboardWebAuthn(DashboardWebAuthnConfig{
		RPID: "example.test", RPOrigins: []string{"https://example.test"}, StatePath: "relative.json",
	})
	if err == nil {
		t.Fatal("expected invalid state-path error")
	}
}

func TestDashboardWebAuthnServerBootstrapRules(t *testing.T) {
	nodes := nodectrl.NewRegistry(nil)
	workspaces := workspacecatalog.NewStore(nil)
	config := DashboardWebAuthnConfig{
		RPID: dashboardTestHost, RPOrigins: []string{"https://" + dashboardTestHost},
		StatePath: filepath.Join(t.TempDir(), "state.json"),
	}
	if _, err := newDashboardServer("127.0.0.1:0", nil, &config, nodes, workspaces, nil); err == nil {
		t.Fatal("expected empty passwordless bootstrap rejection")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if server, err := newDashboardServer("127.0.0.1:0", hash, &config, nodes, workspaces, nil); err != nil || server == nil {
		t.Fatalf("password bootstrap server = %#v, %v", server, err)
	}
	store, err := openDashboardWebAuthnStore(config.StatePath)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	if err := store.addCredential(webauthn.Credential{ID: []byte("key")}); err != nil {
		t.Fatalf("add credential: %v", err)
	}
	if server, err := newDashboardServer("127.0.0.1:0", nil, &config, nodes, workspaces, nil); err != nil || server == nil {
		t.Fatalf("passwordless existing-key server = %#v, %v", server, err)
	}
	if server, err := newDashboardServer("", nil, &config, nodes, workspaces, nil); err != nil || server != nil {
		t.Fatalf("disabled dashboard server = %#v, %v", server, err)
	}
}

func testDashboardWebAuthn(t *testing.T) *dashboardWebAuthn {
	t.Helper()
	wa, err := newDashboardWebAuthn(DashboardWebAuthnConfig{
		RPID: dashboardTestHost, RPOrigins: []string{"https://" + dashboardTestHost},
		StatePath: filepath.Join(t.TempDir(), "webauthn.json"),
	})
	if err != nil {
		t.Fatalf("new WebAuthn: %v", err)
	}
	return wa
}

func testDashboardWebAuthnAuth(t *testing.T, wa *dashboardWebAuthn) *dashboardAuth {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	auth := newDashboardAuth(hash)
	auth.webAuthn = wa
	auth.audit = func(string, string) {}
	return auth
}

func dashboardWebAuthnPasswordLogin(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	response := dashboardLoginRequest(handler, "secret")
	if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
		t.Fatalf("password bootstrap login = %d", response.Code)
	}
	return response.Result().Cookies()[0]
}

func dashboardWebAuthnRequest(method, target string, body io.Reader, cookie *http.Cookie) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Host = dashboardTestHost
	request.Header.Set("Origin", "https://"+dashboardTestHost)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	return request
}

func decodeDashboardWebAuthnOptions(t *testing.T, response *httptest.ResponseRecorder) dashboardWebAuthnOptions {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("options response = %d: %s", response.Code, response.Body.String())
	}
	var options dashboardWebAuthnOptions
	if err := json.Unmarshal(response.Body.Bytes(), &options); err != nil {
		t.Fatalf("decode options: %v", err)
	}
	return options
}

func mustDashboardWebAuthnState(t *testing.T, state dashboardWebAuthnState) []byte {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return data
}

func dashboardRegistrationTestVector(t *testing.T) (body []byte, challenge string) {
	t.Helper()
	const (
		attestationObjectHex = "a363666d74646e6f6e656761747453746d74a068617574684461746158a4bfabc37432958b063360d3ad6461c9c4735ae7f8edd46592a5e0f01452b2e4b559000000008446ccb9ab1db374750b2367ff6f3a1f0020f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4a5010203262001215820afefa16f97ca9b2d23eb86ccb64098d20db90856062eb249c33a9b672f26df61225820930a56b87a2fca66334b03458abf879717c12cc68ed73290af2e2664796b9220"
		clientDataJSONHex    = "7b2274797065223a22776562617574686e2e637265617465222c226368616c6c656e6765223a22414d4d507434557878475453746e63647134313759447742466938767049612d7077386f4f755657345441222c226f726967696e223a2268747470733a2f2f6578616d706c652e6f7267222c2263726f73734f726967696e223a66616c73652c22657874726144617461223a22636c69656e74446174614a534f4e206d617920626520657874656e6465642077697468206164646974696f6e616c206669656c647320696e20746865206675747572652c207375636820617320746869733a20426b5165446a646354427258426941774a544c453551227d"
		credentialIDHex      = "f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4" //nolint:gosec // Public W3C registration test-vector credential ID.
	)
	decode := func(value string) []byte {
		data, err := hex.DecodeString(value)
		if err != nil {
			t.Fatalf("decode test vector: %v", err)
		}
		return data
	}
	credentialID := base64.RawURLEncoding.EncodeToString(decode(credentialIDHex))
	challenge = "AMMPt4UxxGTStncdq417YDwBFi8vpIa-pw8oOuVW4TA"
	var err error
	body, err = json.Marshal(map[string]any{
		"id": credentialID, "rawId": credentialID, "type": "public-key",
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(decode(attestationObjectHex)),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(decode(clientDataJSONHex)),
		},
	})
	if err != nil {
		t.Fatalf("encode test vector: %v", err)
	}
	return body, challenge
}
