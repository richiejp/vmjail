//go:build linux && integration

package testdaemons

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// SSHServer is an unprivileged loopback OpenSSH server with a test-owned host
// key and client key. It gives deployment tests a genuine SSH transport
// boundary without requiring a mutable external machine.
type SSHServer struct {
	Destination    string
	IdentityFile   string
	KnownHostsFile string
	Port           string
	output         *outputCapture
}

func NewSSHServer(t *testing.T) *SSHServer {
	t.Helper()
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Fatalf("find sshd: %v", err)
	}
	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Fatalf("find ssh-keygen: %v", err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatalf("current SSH fixture user: %v", err)
	}
	base := t.TempDir()
	hostKey := filepath.Join(base, "ssh_host_ed25519_key")
	clientKey := filepath.Join(base, "client_ed25519_key")
	runCommand(t, sshKeygen, "-q", "-t", "ed25519", "-N", "", "-f", hostKey)
	runCommand(t, sshKeygen, "-q", "-t", "ed25519", "-N", "", "-f", clientKey)
	clientPublic, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatalf("read SSH client public key: %v", err)
	}
	authorizedKeys := filepath.Join(base, "authorized_keys")
	if err := os.WriteFile(authorizedKeys, clientPublic, 0o600); err != nil {
		t.Fatalf("write SSH authorized_keys: %v", err)
	}

	addr := freeLoopbackAddr(t)
	_, port, _ := strings.Cut(addr, ":")
	config := strings.Join([]string{
		"Port " + port,
		"ListenAddress 127.0.0.1",
		"HostKey " + hostKey,
		"PidFile " + filepath.Join(base, "sshd.pid"),
		"AuthorizedKeysFile " + authorizedKeys,
		"StrictModes no",
		"PubkeyAuthentication yes",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"UsePAM no",
		"PermitRootLogin no",
		"AllowUsers " + current.Username,
		"PrintMotd no",
		"LogLevel VERBOSE",
		"",
	}, "\n")
	configPath := filepath.Join(base, "sshd_config")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write sshd config: %v", err)
	}
	output := newOutputCapture(t, "sshd")
	startDaemon(t, output, nil, nil, sshd, "-D", "-e", "-f", configPath)
	waitForTCP(t, addr, daemonReadyTimeout)

	hostPublic, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatalf("read SSH host public key: %v", err)
	}
	knownHosts := filepath.Join(base, "known_hosts")
	line := fmt.Sprintf("[%s]:%s %s", "127.0.0.1", port, string(hostPublic))
	if err := os.WriteFile(knownHosts, []byte(line), 0o600); err != nil {
		t.Fatalf("write SSH known_hosts: %v", err)
	}
	return &SSHServer{
		Destination:    current.Username + "@127.0.0.1",
		IdentityFile:   clientKey,
		KnownHostsFile: knownHosts,
		Port:           port,
		output:         output,
	}
}

func (s *SSHServer) ClientArgs() []string {
	return []string{
		"-F", "/dev/null",
		"-T",
		"-p", s.Port,
		"-i", s.IdentityFile,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=3",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + s.KnownHostsFile,
		s.Destination,
	}
}

func (s *SSHServer) Run(t *testing.T, stdin io.Reader, remoteCommand string) string {
	t.Helper()
	args := append(s.ClientArgs(), remoteCommand)
	command := exec.CommandContext(t.Context(), "ssh", args...)
	command.Env = append(os.Environ(), "SSH_AUTH_SOCK=")
	command.Stdin = stdin
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("ssh %s: %v\n%s\n--- sshd ---\n%s", remoteCommand, err, output, s.output.Snapshot())
	}
	return string(output)
}

func (s *SSHServer) RunScript(t *testing.T, scriptPath string, args ...string) string {
	t.Helper()
	script, err := os.Open(scriptPath) //nolint:gosec // repository-owned test script.
	if err != nil {
		t.Fatalf("open remote script %s: %v", scriptPath, err)
	}
	defer func() { _ = script.Close() }()
	quoted := make([]string, len(args))
	for i := range args {
		quoted[i] = shellQuote(args[i])
	}
	return s.Run(t, script, "sh -s -- "+strings.Join(quoted, " "))
}

func (s *SSHServer) RunShell(t *testing.T, script string, args ...string) string {
	t.Helper()
	quoted := make([]string, len(args))
	for i := range args {
		quoted[i] = shellQuote(args[i])
	}
	return s.Run(t, strings.NewReader(script), "sh -s -- "+strings.Join(quoted, " "))
}

func (s *SSHServer) Upload(t *testing.T, localPath, remotePath string) {
	t.Helper()
	args := []string{
		"-F", "/dev/null",
		"-O",
		"-P", s.Port,
		"-i", s.IdentityFile,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=3",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + s.KnownHostsFile,
		localPath,
		s.Destination + ":" + remotePath,
	}
	runCommand(t, "scp", args...)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), name, args...) //nolint:gosec // test-owned executable and arguments.
	command.Env = append(os.Environ(), "SSH_AUTH_SOCK=")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}
