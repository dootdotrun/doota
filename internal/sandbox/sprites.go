package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// SpritesProvider is the Fly Sprites implementation.
//
// Sizing is deliberately not set. SpriteConfig accepts optional CPUs, RamMB, and
// StorageGB overrides, and all of them are left nil so the platform scales the
// Sprite by itself and bills actual usage. Choosing a size up front means
// guessing wrong in one of two directions; not choosing removes both.
type SpritesProvider struct {
	client *sprites.Client
}

// NewSpritesProvider builds a provider from an API token.
func NewSpritesProvider(token string) *SpritesProvider {
	return &SpritesProvider{client: sprites.New(token)}
}

func (p *SpritesProvider) Kind() string { return "sprites" }

func (p *SpritesProvider) Close() error { return p.client.Close() }

func (p *SpritesProvider) Create(ctx context.Context, name string) (Sandbox, error) {
	// nil config: no CPU, memory, or disk override. See the type comment.
	s, err := p.client.CreateSprite(ctx, name, nil)
	if err != nil {
		return nil, fmt.Errorf("create sprite %s: %w", name, err)
	}
	return &spriteSandbox{client: p.client, sprite: s, name: name}, nil
}

func (p *SpritesProvider) Get(ctx context.Context, name string) (Sandbox, error) {
	s, err := p.client.GetSprite(ctx, name)
	if err != nil {
		if isSpriteNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get sprite %s: %w", name, err)
	}
	return &spriteSandbox{client: p.client, sprite: s, name: name}, nil
}

func (p *SpritesProvider) Delete(ctx context.Context, name string) error {
	if err := p.client.DeleteSprite(ctx, name); err != nil {
		if isSpriteNotFound(err) {
			return nil // already gone; deleting is idempotent
		}
		return fmt.Errorf("delete sprite %s: %w", name, err)
	}
	return nil
}

func isSpriteNotFound(err error) bool {
	if apiErr := sprites.IsAPIError(err); apiErr != nil {
		if apiErr.StatusCode == 404 {
			return true
		}
	}
	// Fall back to the message: the SDK does not export a sentinel for this.
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

type spriteSandbox struct {
	client *sprites.Client
	sprite *sprites.Sprite
	name   string
}

func (s *spriteSandbox) Name() string { return s.name }

// normalizeGone preserves operation-specific 404s (such as a missing file or
// checkpoint) unless a follow-up Sprite lookup confirms the whole sandbox was
// deleted after its handle was acquired.
func (s *spriteSandbox) normalizeGone(ctx context.Context, err error) error {
	if !isSpriteNotFound(err) {
		return err
	}
	if _, probeErr := s.client.GetSprite(ctx, s.name); probeErr != nil && isSpriteNotFound(probeErr) {
		return ErrNotFound
	}
	return err
}

func (s *spriteSandbox) State(ctx context.Context) (State, error) {
	info, err := s.client.GetSprite(ctx, s.name)
	if err != nil {
		if isSpriteNotFound(err) {
			return StateMissing, nil
		}
		return StateUnknown, fmt.Errorf("sprite state: %w", err)
	}
	s.sprite = info

	// The status strings the API actually returns are "created", "cold", "warm",
	// and "running" — not the started/stopped/suspended vocabulary this switch
	// originally guessed at. Those are kept as aliases in case the platform ever
	// uses them, but the four real ones are what matter.
	//
	// "warm" counts as running: the Sprite is up and will serve a command without
	// waking. "cold" and "created" both mean it exists and is not running, which
	// is exactly what Sleeping describes — and since an exec or an HTTP request
	// wakes it automatically, the distinction between never-started and
	// slept-since costs the caller nothing.
	switch strings.ToLower(info.Status) {
	case "running", "warm", "started", "warming":
		return StateRunning, nil
	case "cold", "created", "stopped", "suspended", "sleeping", "paused":
		return StateSleeping, nil
	default:
		// Deliberately not an error. An unrecognised status means the platform
		// added a word we do not know yet, and reporting Unknown lets the Project
		// screen say so instead of the app refusing to work.
		return StateUnknown, nil
	}
}

// Wake runs a trivial command, which forces the Sprite to start if it was
// asleep. Cheaper than a dedicated start call and works regardless of which
// status string the platform reports.
func (s *spriteSandbox) Wake(ctx context.Context) error {
	res, err := s.Exec(ctx, Command{Cmd: "true", Timeout: 90 * time.Second})
	if err != nil {
		return fmt.Errorf("wake sprite: %w", err)
	}
	if res.ExitCode != 0 {
		// Output rather than Stderr: this transport merges the two, so Stderr is
		// empty and the reason would have been dropped from the message.
		return fmt.Errorf("wake sprite: exit %d: %s", res.ExitCode, res.Output())
	}
	return nil
}

// Path is the identity function: a Sprite's shell sees the same filesystem the
// API writes to.
func (s *spriteSandbox) Path(sandboxPath string) string { return sandboxPath }

func (s *spriteSandbox) URL(ctx context.Context) (string, error) {
	info, err := s.client.GetSprite(ctx, s.name)
	if err != nil {
		return "", fmt.Errorf("sprite url: %w", s.normalizeGone(ctx, err))
	}
	return info.URL, nil
}

func (s *spriteSandbox) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	var stdout, stderr bytes.Buffer
	exitCode, err := s.execInto(ctx, cmd, &stdout, &stderr)
	return ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (s *spriteSandbox) ExecStream(ctx context.Context, cmd Command, stdout, stderr io.Writer) (int, error) {
	return s.execInto(ctx, cmd, stdout, stderr)
}

// execInto runs a command, writing output into the supplied writers.
//
// The stderr writer will not receive anything: the Sprites exec transport carries
// a single merged stream and the SDK delivers all of it to Stdout. It is still
// wired up so the signature matches the interface and so a future SDK that does
// separate them needs no change here. Interleaving order between the two is not
// guaranteed either — a line written to stderr can arrive before an earlier
// stdout line.
func (s *spriteSandbox) execInto(ctx context.Context, cmd Command, stdout, stderr io.Writer) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(cmd))
	defer cancel()

	c := s.sprite.CommandContext(ctx, "/bin/sh", "-c", shellLine(cmd))
	c.Dir = cmd.Dir
	c.Stdout = stdout
	c.Stderr = stderr

	err := c.Run()

	// A non-zero exit is information for the caller, not a transport failure.
	var exitErr *sprites.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("exec in sprite: %w", s.normalizeGone(ctx, err))
	}
	return 0, nil
}

// ReadFile reads through the filesystem API rather than the control channel.
//
// FsReadControl looks like the right call and is not: in sprites-go v0.1.0 it
// tries to JSON-decode the raw file body, so it fails on any content that is not
// itself JSON ("invalid character 'c' looking for beginning of value" for a file
// starting with "content"). Worse, a failed control read leaves the control
// channel wedged — every subsequent Exec on the same sandbox then times out, so a
// single unreadable file takes out the whole sandbox until it is recreated.
//
// Filesystem().ReadFile goes over HTTP instead and behaves. It takes no context,
// which is the one thing lost here; a read that hangs is bounded by the caller's
// own timeout rather than by ctx.
func (s *spriteSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := s.sprite.Filesystem().ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, s.normalizeGone(ctx, err))
	}
	return data, nil
}

func (s *spriteSandbox) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	if err := s.sprite.Filesystem().WriteFileContext(ctx, path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, s.normalizeGone(ctx, err))
	}
	return nil
}

// DialPort tunnels a TCP connection into the Sprite.
//
// This is what lets the preview proxy serve a dev server on any port. The
// Sprite's own public URL only routes to 8080; dialling the port directly
// sidesteps that entirely for the human preview path.
func (s *spriteSandbox) DialPort(ctx context.Context, port int) (net.Conn, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := s.client.ProxySocket(ctx, "tcp", s.name, addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s in sprite: %w", addr, s.normalizeGone(ctx, err))
	}
	return conn, nil
}
