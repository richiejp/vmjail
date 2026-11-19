package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/oops"
	"github.com/richiejp/vmjail/internal/trace"
)

const (
	DefaultSecretVsockPort  uint32 = 9200
	MaxSecretPayload               = 1 << 20
	maxSecretFrame                 = MaxSecretPayload + 4096
	maxSecretErrorBytes            = 1024
	secretReportPrefix             = "secret"
	credentialProviderCodex        = "codex"
)

type credentialStore interface {
	Install(*agentv1.SecretInstallRequest) error
	Cleanup() error
}

type secretDeps struct {
	listenVsock func(uint32) (net.Listener, error)
	prepare     func(containerProcess) (credentialStore, error)
}

type secretReplayStore struct {
	mu       sync.Mutex
	delivery string
	digest   [sha256.Size]byte
	install  func([]byte) error
	cleanup  func() error
	now      func() time.Time
}

func (s *secretReplayStore) Install(req *agentv1.SecretInstallRequest) error {
	if req.GetDeliveryId() == "" || len(req.GetDeliveryId()) > 128 {
		return errors.New("invalid delivery ID")
	}
	if req.GetKind() != agentv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON {
		return errors.New("unsupported secret kind")
	}
	if len(req.GetPayload()) == 0 || len(req.GetPayload()) > MaxSecretPayload {
		return errors.New("secret payload is empty or exceeds limit")
	}
	if !json.Valid(req.GetPayload()) {
		return errors.New("codex auth payload is not valid JSON")
	}
	expires := req.GetExpiresAt()
	if expires == nil || !expires.IsValid() || !expires.AsTime().After(s.now()) {
		return errors.New("secret delivery is expired")
	}
	digest := sha256.Sum256(req.GetPayload())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delivery != "" {
		if s.delivery == req.GetDeliveryId() && s.digest == digest {
			return nil
		}
		return errors.New("a different secret was already installed")
	}
	if err := s.install(req.GetPayload()); err != nil {
		return fmt.Errorf("install credential: %w", err)
	}
	s.delivery, s.digest = req.GetDeliveryId(), digest
	return nil
}

func (s *secretReplayStore) Cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivery = ""
	clear(s.digest[:])
	return s.cleanup()
}

func startSecretListener(ctx context.Context, w io.Writer, d runDeps) (func(), error) {
	provider := d.cfg.GetCredentialProvider()
	if provider == "" {
		return func() {}, nil
	}
	if provider != credentialProviderCodex || d.secret.listenVsock == nil || d.secret.prepare == nil {
		return nil, fmt.Errorf("agent: unsupported credential provider %q", provider)
	}
	store, err := d.secret.prepare(readContainerProcess(w, d.cfg.GetBundle()))
	if err != nil {
		return nil, fmt.Errorf("agent: prepare credential home: %w", err)
	}
	ln, err := d.secret.listenVsock(DefaultSecretVsockPort)
	if err != nil {
		_ = store.Cleanup()
		return nil, fmt.Errorf("agent: listen credential vsock %d: %w", DefaultSecretVsockPort, err)
	}
	writeReport(w, secretReportPrefix, fmt.Sprintf("ready vsock %d", DefaultSecretVsockPort))
	done := make(chan struct{})
	go runSecretListener(ctx, w, ln, store, done)
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = ln.Close()
			<-done
			oops.Skip("agent: cleanup credential home", store.Cleanup())
		})
	}, nil
}

func runSecretListener(ctx context.Context, w io.Writer, ln net.Listener, store credentialStore, done chan<- struct{}) {
	var handlers sync.WaitGroup
	defer func() { handlers.Wait(); close(done) }()
	_, span := trace.Start(ctx, "agent.secret.listener", trace.WithAttributes(trace.KV{Key: traceAttrVsockPort, Value: strconv.FormatUint(uint64(DefaultSecretVsockPort), 10)}))
	defer span.End()
	go func() { <-ctx.Done(); _ = ln.Close() }()
	//vmjail:lint:disable=forloopbound reason="bounded by listener close on VM cancellation"
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			writeReport(w, secretReportPrefix, "accept: "+err.Error())
			return
		}
		handlers.Add(1)
		go func() { defer handlers.Done(); handleSecret(conn, store) }()
	}
}

func handleSecret(conn net.Conn, store credentialStore) {
	defer func() { _ = conn.Close() }()
	req, err := ReadSecretInstallRequest(conn)
	if req != nil {
		defer clear(req.Payload)
	}
	resp := &agentv1.SecretInstallResponse{}
	if req != nil {
		resp.DeliveryId = req.GetDeliveryId()
	}
	if err == nil {
		err = store.Install(req)
	}
	if err != nil {
		resp.Error = boundedSecretError(err)
	} else {
		resp.Installed = true
	}
	_ = WriteSecretInstallResponse(conn, resp)
}

func boundedSecretError(err error) string {
	s := err.Error()
	if len(s) > maxSecretErrorBytes {
		return s[:maxSecretErrorBytes]
	}
	return s
}

func WriteSecretInstallRequest(w io.Writer, req *agentv1.SecretInstallRequest) error {
	if len(req.GetPayload()) > MaxSecretPayload {
		return errors.New("agent: secret payload exceeds limit")
	}
	return writeSecretProto(w, req)
}

func ReadSecretInstallRequest(r io.Reader) (*agentv1.SecretInstallRequest, error) {
	var req agentv1.SecretInstallRequest
	if err := readSecretProto(r, &req); err != nil {
		return nil, err
	}
	if len(req.GetPayload()) > MaxSecretPayload {
		clear(req.Payload)
		return nil, errors.New("agent: secret payload exceeds limit")
	}
	return &req, nil
}

func WriteSecretInstallResponse(w io.Writer, resp *agentv1.SecretInstallResponse) error {
	return writeSecretProto(w, resp)
}

func ReadSecretInstallResponse(r io.Reader) (*agentv1.SecretInstallResponse, error) {
	var resp agentv1.SecretInstallResponse
	return &resp, readSecretProto(r, &resp)
}

func writeSecretProto(w io.Writer, msg proto.Message) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("agent: marshal secret frame: %w", err)
	}
	defer clear(payload)
	if len(payload) > maxSecretFrame {
		return errors.New("agent: secret frame exceeds limit")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload))) //nolint:gosec // bounded above
	if _, err = w.Write(hdr[:]); err != nil {
		return fmt.Errorf("agent: write secret length: %w", err)
	}
	if _, err = w.Write(payload); err != nil {
		return fmt.Errorf("agent: write secret payload: %w", err)
	}
	return nil
}

func readSecretProto(r io.Reader, msg proto.Message) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("agent: read secret length: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxSecretFrame {
		return errors.New("agent: invalid secret frame length")
	}
	payload := make([]byte, n)
	defer clear(payload)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("agent: read secret payload: %w", err)
	}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return fmt.Errorf("agent: unmarshal secret frame: %w", err)
	}
	return nil
}
