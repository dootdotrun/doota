package sandbox

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalProvider runs "sandboxes" as directories on the host, executing commands
// with the local shell.
//
// This exists so the project lifecycle, the Project screen, and the preview
// proxy can be verified end to end without a Fly Sprites account: real git
// clones, real branch checkouts, real dev servers, real proxied HTTP. It is
// selected only by DOOT_SANDBOX_PROVIDER=local and logs loudly at startup.
//
// It is not a mock. Commands really run and files are really written; the only
// difference is that isolation is a directory rather than a VM. That makes it a
// faithful stand-in for provisioning logic and a completely inadequate one for
// running untrusted agent output, which is why it is never the default.
type LocalProvider struct {
	base string
}

// NewLocalProvider stores its sandboxes under base.
func NewLocalProvider(base string) (*LocalProvider, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("create local sandbox base: %w", err)
	}
	return &LocalProvider{base: base}, nil
}

func (p *LocalProvider) Kind() string { return "local" }

func (p *LocalProvider) Close() error { return nil }

func (p *LocalProvider) dir(name string) string   { return filepath.Join(p.base, name) }
func (p *LocalProvider) fsDir(name string) string { return filepath.Join(p.dir(name), "fs") }

func (p *LocalProvider) Create(ctx context.Context, name string) (Sandbox, error) {
	if err := os.MkdirAll(filepath.Join(p.fsDir(name), "workspace"), 0o755); err != nil {
		return nil, fmt.Errorf("create local sandbox: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(p.fsDir(name), "tmp", "doot-logs"), 0o755); err != nil {
		return nil, fmt.Errorf("create local log dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(p.fsDir(name), "root"), 0o755); err != nil {
		return nil, fmt.Errorf("create local home dir: %w", err)
	}
	return &localSandbox{provider: p, name: name}, nil
}

func (p *LocalProvider) Get(ctx context.Context, name string) (Sandbox, error) {
	if _, err := os.Stat(p.fsDir(name)); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get local sandbox: %w", err)
	}
	return &localSandbox{provider: p, name: name}, nil
}

func (p *LocalProvider) Delete(ctx context.Context, name string) error {
	if err := os.RemoveAll(p.dir(name)); err != nil {
		return fmt.Errorf("delete local sandbox: %w", err)
	}
	return nil
}

type localSandbox struct {
	provider *LocalProvider
	name     string
}

func (s *localSandbox) Name() string { return s.name }

// resolve maps a sandbox-absolute path onto the host directory backing it, so
// callers can use the same paths they would use against a real Sprite.
func (s *localSandbox) resolve(path string) string {
	if path == "" {
		path = "/"
	}
	clean := filepath.Clean("/" + strings.TrimPrefix(path, "/"))
	return filepath.Join(s.provider.fsDir(s.name), clean)
}

func (s *localSandbox) State(ctx context.Context) (State, error) {
	if _, err := os.Stat(s.provider.fsDir(s.name)); err != nil {
		if os.IsNotExist(err) {
			return StateMissing, nil
		}
		return StateUnknown, err
	}
	return StateRunning, nil
}

// Wake is a no-op: a directory is never asleep.
func (s *localSandbox) Wake(ctx context.Context) error { return nil }

// URL is empty: there is no public address. Previews go through the app's proxy,
// which is exactly how the real provider is used too.
func (s *localSandbox) URL(ctx context.Context) (string, error) { return "", nil }

// Path maps a sandbox-absolute path onto the backing directory.
//
// Commands run in the host's shell, so an absolute path written into a command
// string would otherwise resolve against the real filesystem root - reading the
// host's /tmp instead of the sandbox's.
func (s *localSandbox) Path(sandboxPath string) string { return s.resolve(sandboxPath) }

// homeDir is the sandbox's HOME, kept inside the sandbox so tools that write
// dotfiles do not touch the real home directory.
func (s *localSandbox) homeDir() string { return s.resolve("/root") }

func (s *localSandbox) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	var stdout, stderr strings.Builder
	exitCode, err := s.ExecStream(ctx, cmd, &stdout, &stderr)
	return ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}, err
}

func (s *localSandbox) ExecStream(ctx context.Context, cmd Command, stdout, stderr io.Writer) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(cmd))
	defer cancel()

	dir := s.resolve(cmd.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return -1, fmt.Errorf("prepare working dir: %w", err)
	}
	// Tools that write dotfiles need HOME to exist. A real sandbox image has one
	// already; here it has to be created.
	if err := os.MkdirAll(s.homeDir(), 0o755); err != nil {
		return -1, fmt.Errorf("prepare home dir: %w", err)
	}

	c := exec.CommandContext(ctx, "/bin/sh", "-c", shellLine(cmd))
	c.Dir = dir
	c.Stdout = stdout
	c.Stderr = stderr
	c.Env = append(os.Environ(), "HOME="+s.homeDir())

	err := c.Run()

	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("exec locally: %w", err)
	}
	return 0, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

func (s *localSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	data, err := os.ReadFile(s.resolve(path))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

func (s *localSandbox) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	target := s.resolve(path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create parent of %s: %w", path, err)
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// DialPort connects to the host, since a local sandbox shares the host network.
func (s *localSandbox) DialPort(ctx context.Context, port int) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("dial port %d: %w", port, err)
	}
	return conn, nil
}

func runHost(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
