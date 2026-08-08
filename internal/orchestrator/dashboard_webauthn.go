package orchestrator

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	dashboardWebAuthnUserIDBytes  = 32
	dashboardWebAuthnMaxFile      = 1 << 20
	dashboardWebAuthnMaxKeys      = 16
	dashboardWebAuthnMaxPending   = 64
	dashboardWebAuthnMaxPerClient = 4
	dashboardWebAuthnCeremonyTTL  = 5 * time.Minute
)

type DashboardWebAuthnConfig struct {
	RPID      string
	RPOrigins []string
	StatePath string
}

type dashboardWebAuthnState struct {
	UserID      []byte                `json:"user_id"`
	Credentials []webauthn.Credential `json:"credentials"`
}

type dashboardWebAuthnStore struct {
	mu    sync.Mutex
	path  string
	state dashboardWebAuthnState
}

type dashboardWebAuthnUser struct {
	id          []byte
	credentials []webauthn.Credential
}

func (u dashboardWebAuthnUser) WebAuthnID() []byte { return u.id }
func (dashboardWebAuthnUser) WebAuthnName() string { return "administrator" }

func (dashboardWebAuthnUser) WebAuthnDisplayName() string                  { return "darkaifactory administrator" }
func (u dashboardWebAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func openDashboardWebAuthnStore(path string) (*dashboardWebAuthnStore, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("dashboard WebAuthn state path must be absolute")
	}
	store := &dashboardWebAuthnStore{path: path}
	data, err := os.ReadFile(path) // #nosec G304 -- explicit operator-selected state path.
	if errors.Is(err, os.ErrNotExist) {
		store.state.UserID = make([]byte, dashboardWebAuthnUserIDBytes)
		if _, err := rand.Read(store.state.UserID); err != nil {
			return nil, fmt.Errorf("create dashboard WebAuthn user ID: %w", err)
		}
		store.state.Credentials = []webauthn.Credential{}
		if err := store.saveStateLocked(store.state); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read dashboard WebAuthn state: %w", err)
	}
	if len(data) > dashboardWebAuthnMaxFile {
		return nil, errors.New("dashboard WebAuthn state exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("decode dashboard WebAuthn state: %w", err)
	}
	if len(store.state.UserID) != dashboardWebAuthnUserIDBytes {
		return nil, errors.New("dashboard WebAuthn state has invalid user ID")
	}
	if len(store.state.Credentials) > dashboardWebAuthnMaxKeys {
		return nil, errors.New("dashboard WebAuthn state has too many credentials")
	}
	return store, nil
}

func (s *dashboardWebAuthnStore) user() dashboardWebAuthnUser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return dashboardWebAuthnUser{
		id:          bytes.Clone(s.state.UserID),
		credentials: append([]webauthn.Credential(nil), s.state.Credentials...),
	}
}

func (s *dashboardWebAuthnStore) credentialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.state.Credentials)
}

func (s *dashboardWebAuthnStore) addCredential(credential webauthn.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.Credentials) >= dashboardWebAuthnMaxKeys {
		return errors.New("dashboard WebAuthn credential limit reached")
	}
	for i := range s.state.Credentials {
		if bytes.Equal(s.state.Credentials[i].ID, credential.ID) {
			return errors.New("dashboard WebAuthn credential already exists")
		}
	}
	next := dashboardWebAuthnState{UserID: bytes.Clone(s.state.UserID), Credentials: append([]webauthn.Credential(nil), s.state.Credentials...)}
	next.Credentials = append(next.Credentials, credential)
	if err := s.saveStateLocked(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *dashboardWebAuthnStore) updateCredential(credential webauthn.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Credentials {
		if !bytes.Equal(s.state.Credentials[i].ID, credential.ID) {
			continue
		}
		next := dashboardWebAuthnState{UserID: bytes.Clone(s.state.UserID), Credentials: append([]webauthn.Credential(nil), s.state.Credentials...)}
		next.Credentials[i] = credential
		if err := s.saveStateLocked(next); err != nil {
			return err
		}
		s.state = next
		return nil
	}
	return errors.New("dashboard WebAuthn credential not found")
}

func (s *dashboardWebAuthnStore) saveStateLocked(state dashboardWebAuthnState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode dashboard WebAuthn state: %w", err)
	}
	if len(data) > dashboardWebAuthnMaxFile {
		return errors.New("dashboard WebAuthn state exceeds 1 MiB")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create dashboard WebAuthn state directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".dashboard-webauthn-*")
	if err != nil {
		return fmt.Errorf("create dashboard WebAuthn state temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure dashboard WebAuthn state: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write dashboard WebAuthn state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync dashboard WebAuthn state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close dashboard WebAuthn state: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace dashboard WebAuthn state: %w", err)
	}
	return nil
}

type dashboardWebAuthnCeremony struct {
	kind    string
	client  string
	session webauthn.SessionData
	expires time.Time
}

type dashboardWebAuthn struct {
	rp         *webauthn.WebAuthn
	store      *dashboardWebAuthnStore
	mu         sync.Mutex
	pending    map[string]dashboardWebAuthnCeremony
	now        func() time.Time
	randomRead func([]byte) (int, error)
	keysPage   *template.Template
}

func newDashboardWebAuthn(config DashboardWebAuthnConfig) (*dashboardWebAuthn, error) {
	rp, err := webauthn.New(&webauthn.Config{
		RPID: config.RPID, RPDisplayName: "darkaifactory", RPOrigins: config.RPOrigins,
		AttestationPreference: protocol.PreferNoAttestation,
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: dashboardWebAuthnCeremonyTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: dashboardWebAuthnCeremonyTTL},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure dashboard WebAuthn: %w", err)
	}
	store, err := openDashboardWebAuthnStore(config.StatePath)
	if err != nil {
		return nil, err
	}
	return &dashboardWebAuthn{
		rp: rp, store: store, pending: make(map[string]dashboardWebAuthnCeremony),
		now: time.Now, randomRead: rand.Read,
		keysPage: template.Must(template.New("security-keys").Parse(dashboardWebAuthnKeysHTML)),
	}, nil
}

func (w *dashboardWebAuthn) begin(kind, client string, session *webauthn.SessionData) (string, error) {
	token := make([]byte, 32)
	if _, err := w.randomRead(token); err != nil {
		return "", fmt.Errorf("create WebAuthn ceremony ID: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(token)
	now := w.now().UTC()
	w.mu.Lock()
	defer w.mu.Unlock()
	for key := range w.pending {
		if !now.Before(w.pending[key].expires) {
			delete(w.pending, key)
		}
	}
	clientPending := 0
	for key := range w.pending {
		if w.pending[key].client == client {
			clientPending++
		}
	}
	if len(w.pending) >= dashboardWebAuthnMaxPending || clientPending >= dashboardWebAuthnMaxPerClient {
		return "", errors.New("too many pending WebAuthn ceremonies")
	}
	w.pending[id] = dashboardWebAuthnCeremony{kind: kind, client: client, session: *session, expires: now.Add(dashboardWebAuthnCeremonyTTL)}
	return id, nil
}

func (w *dashboardWebAuthn) take(id, kind, client string) (webauthn.SessionData, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pending, ok := w.pending[id]
	delete(w.pending, id)
	if !ok || pending.kind != kind || pending.client != client || !w.now().UTC().Before(pending.expires) {
		return webauthn.SessionData{}, errors.New("invalid or expired WebAuthn ceremony")
	}
	return pending.session, nil
}

type dashboardWebAuthnOptions struct {
	CeremonyID string `json:"ceremony_id"`
	PublicKey  any    `json:"publicKey"`
}

func decodeDashboardCeremonyRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	ceremonyID := r.URL.Query().Get("ceremony_id")
	if ceremonyID == "" {
		return "", errors.New("missing ceremony_id")
	}
	r.Body = http.MaxBytesReader(w, r.Body, dashboardWebAuthnMaxFile)
	return ceremonyID, nil
}

func writeDashboardWebAuthnJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, "encode WebAuthn response", http.StatusInternalServerError)
	}
}

func (a *dashboardAuth) registerWebAuthnRoutes(mux *http.ServeMux) {
	mux.Handle("GET /security-keys", a.requireSession(http.HandlerFunc(a.serveWebAuthnKeys)))
	mux.Handle("POST /v1/auth/webauthn/register/options", a.requireSession(http.HandlerFunc(a.beginWebAuthnRegistration)))
	mux.Handle("POST /v1/auth/webauthn/register/finish", a.requireSession(http.HandlerFunc(a.finishWebAuthnRegistration)))
	mux.HandleFunc("POST /v1/auth/webauthn/login/options", a.beginWebAuthnLogin)
	mux.HandleFunc("POST /v1/auth/webauthn/login/finish", a.finishWebAuthnLogin)
}

func (a *dashboardAuth) serveWebAuthnKeys(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data := struct{ Count, Limit int }{a.webAuthn.store.credentialCount(), dashboardWebAuthnMaxKeys}
	if err := a.webAuthn.keysPage.Execute(w, data); err != nil {
		http.Error(w, "render security keys", http.StatusInternalServerError)
	}
}

func (a *dashboardAuth) beginWebAuthnRegistration(w http.ResponseWriter, r *http.Request) {
	client := dashboardClient(r.RemoteAddr)
	if !dashboardSameOrigin(r) {
		a.audit("webauthn_register_origin_rejected", client)
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	if a.webAuthn.store.credentialCount() >= dashboardWebAuthnMaxKeys {
		a.audit("webauthn_register_limit_rejected", client)
		http.Error(w, "WebAuthn credential limit reached", http.StatusConflict)
		return
	}
	user := a.webAuthn.store.user()
	creation, session, err := a.webAuthn.rp.BeginRegistration(
		user,
		webauthn.WithExclusions(webauthn.Credentials(user.credentials).CredentialDescriptors()),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementDiscouraged, UserVerification: protocol.VerificationPreferred,
		}),
	)
	if err != nil {
		a.audit("webauthn_register_begin_failed", client)
		http.Error(w, "begin WebAuthn registration", http.StatusInternalServerError)
		return
	}
	id, err := a.webAuthn.begin("register", client, session)
	if err != nil {
		a.audit("webauthn_register_begin_failed", client)
		http.Error(w, "begin WebAuthn registration", http.StatusServiceUnavailable)
		return
	}
	writeDashboardWebAuthnJSON(w, dashboardWebAuthnOptions{CeremonyID: id, PublicKey: creation.Response})
}

func (a *dashboardAuth) finishWebAuthnRegistration(w http.ResponseWriter, r *http.Request) {
	client := dashboardClient(r.RemoteAddr)
	if !dashboardSameOrigin(r) {
		a.audit("webauthn_register_origin_rejected", client)
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	id, err := decodeDashboardCeremonyRequest(w, r)
	if err != nil {
		http.Error(w, "invalid WebAuthn registration", http.StatusBadRequest)
		return
	}
	session, err := a.webAuthn.take(id, "register", client)
	if err != nil {
		a.audit("webauthn_register_replay_rejected", client)
		http.Error(w, "invalid WebAuthn registration", http.StatusBadRequest)
		return
	}
	credential, err := a.webAuthn.rp.FinishRegistration(a.webAuthn.store.user(), session, r)
	if err != nil {
		a.audit("webauthn_register_failed", client)
		http.Error(w, "invalid WebAuthn registration", http.StatusBadRequest)
		return
	}
	if err := a.webAuthn.store.addCredential(*credential); err != nil {
		a.audit("webauthn_register_store_failed", client)
		http.Error(w, "store WebAuthn credential", http.StatusInternalServerError)
		return
	}
	a.audit("webauthn_registered", client)
	w.WriteHeader(http.StatusNoContent)
}

func (a *dashboardAuth) beginWebAuthnLogin(w http.ResponseWriter, r *http.Request) {
	client := dashboardClient(r.RemoteAddr)
	if !dashboardSameOrigin(r) {
		a.audit("webauthn_login_origin_rejected", client)
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	if a.rateLimited(client, a.now().UTC()) {
		a.audit("webauthn_login_throttled", client)
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	user := a.webAuthn.store.user()
	assertion, session, err := a.webAuthn.rp.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		a.audit("webauthn_login_unavailable", client)
		http.Error(w, "WebAuthn login unavailable", http.StatusConflict)
		return
	}
	id, err := a.webAuthn.begin("login", client, session)
	if err != nil {
		a.audit("webauthn_login_begin_failed", client)
		http.Error(w, "begin WebAuthn login", http.StatusServiceUnavailable)
		return
	}
	writeDashboardWebAuthnJSON(w, dashboardWebAuthnOptions{CeremonyID: id, PublicKey: assertion.Response})
}

func (a *dashboardAuth) finishWebAuthnLogin(w http.ResponseWriter, r *http.Request) {
	client := dashboardClient(r.RemoteAddr)
	if !dashboardSameOrigin(r) {
		a.audit("webauthn_login_origin_rejected", client)
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	now := a.now().UTC()
	if a.rateLimited(client, now) {
		a.audit("webauthn_login_throttled", client)
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	id, err := decodeDashboardCeremonyRequest(w, r)
	if err != nil {
		http.Error(w, "invalid WebAuthn login", http.StatusBadRequest)
		return
	}
	session, err := a.webAuthn.take(id, "login", client)
	if err != nil {
		a.recordFailure(client, now)
		a.audit("webauthn_login_replay_rejected", client)
		http.Error(w, "invalid WebAuthn login", http.StatusUnauthorized)
		return
	}
	credential, err := a.webAuthn.rp.FinishLogin(a.webAuthn.store.user(), session, r)
	if err != nil {
		a.recordFailure(client, now)
		a.audit("webauthn_login_failed", client)
		http.Error(w, "invalid WebAuthn login", http.StatusUnauthorized)
		return
	}
	if err := a.webAuthn.store.updateCredential(*credential); err != nil {
		a.audit("webauthn_login_store_failed", client)
		http.Error(w, "store WebAuthn credential", http.StatusInternalServerError)
		return
	}
	if err := a.establishSession(w, client, now); err != nil {
		a.audit("webauthn_login_session_failed", client)
		http.Error(w, "create session", http.StatusInternalServerError)
		return
	}
	a.audit("webauthn_login_succeeded", client)
	w.WriteHeader(http.StatusNoContent)
}

const dashboardWebAuthnJS = `
const b64u=s=>{const p='='.repeat((4-s.length%4)%4);const b=atob((s+p).replace(/-/g,'+').replace(/_/g,'/'));return Uint8Array.from(b,c=>c.charCodeAt(0))};
const enc=b=>{let s='';new Uint8Array(b).forEach(v=>s+=String.fromCharCode(v));return btoa(s).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'')};
const decodeCreate=o=>{o.challenge=b64u(o.challenge);o.user.id=b64u(o.user.id);(o.excludeCredentials||[]).forEach(c=>c.id=b64u(c.id));return o};
const decodeGet=o=>{o.challenge=b64u(o.challenge);(o.allowCredentials||[]).forEach(c=>c.id=b64u(c.id));return o};
const credentialJSON=c=>{const r=c.response;const out={id:c.id,type:c.type,rawId:enc(c.rawId),response:{clientDataJSON:enc(r.clientDataJSON)}};for(const k of ['attestationObject','authenticatorData','signature','userHandle'])if(r[k])out.response[k]=enc(r[k]);if(r.getTransports)out.response.transports=r.getTransports();out.clientExtensionResults=c.getClientExtensionResults();out.authenticatorAttachment=c.authenticatorAttachment;return out};
async function webAuthnLogin(){let r=await fetch('/v1/auth/webauthn/login/options',{method:'POST'});if(!r.ok)throw new Error(await r.text());const o=await r.json();const c=await navigator.credentials.get({publicKey:decodeGet(o.publicKey)});r=await fetch('/v1/auth/webauthn/login/finish?ceremony_id='+encodeURIComponent(o.ceremony_id),{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(credentialJSON(c))});if(!r.ok)throw new Error(await r.text());location.href='/'}
async function webAuthnRegister(){let r=await fetch('/v1/auth/webauthn/register/options',{method:'POST'});if(!r.ok)throw new Error(await r.text());const o=await r.json();const c=await navigator.credentials.create({publicKey:decodeCreate(o.publicKey)});r=await fetch('/v1/auth/webauthn/register/finish?ceremony_id='+encodeURIComponent(o.ceremony_id),{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(credentialJSON(c))});if(!r.ok)throw new Error(await r.text());location.reload()}
`

const dashboardWebAuthnKeysHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Security keys · darkaifactory</title><style>body{font:16px system-ui;max-width:42rem;margin:4rem auto;padding:1rem;background:#111827;color:#e5e7eb}section{padding:1.5rem;background:#1f2937;border-radius:.75rem}button{font:inherit;padding:.75rem;cursor:pointer}.error{color:#fca5a5}</style></head><body><main><p><a href="/">← dashboard</a></p><h1>Security keys</h1><section><p>{{.Count}} of {{.Limit}} credentials enrolled.</p><button id="register-key" type="button">Register security key</button><div id="key-error" class="error" role="alert"></div></section></main><script>` + dashboardWebAuthnJS + `document.getElementById('register-key').addEventListener('click',()=>webAuthnRegister().catch(e=>document.getElementById('key-error').textContent=e.message));</script></body></html>`
