package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/sandbox"
)

// ---------------------------------------------------------------------------
// http_check
// ---------------------------------------------------------------------------

type httpCheckTool struct{}

type httpCheckArgs struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func (httpCheckTool) Name() string { return "http_check" }

func (httpCheckTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "http_check",
		Description: "Make an HTTP request to the sandbox's own localhost and return the status, headers, " +
			"and body. This is the main way to verify a dev server actually serves what you think it does. " +
			"Restricted to localhost: use bash with curl if you genuinely need to reach the internet.",
		Params: object(map[string]Param{
			"url":     {Type: "string", Description: `A localhost URL, for example "http://localhost:3000/api/health".`},
			"method":  {Type: "string", Description: "HTTP method. Default GET."},
			"headers": {Type: "object", Description: "Request headers."},
			"body":    {Type: "string", Description: "Request body."},
		}, "url"),
	}
}

// httpCheckTimeout bounds the whole request.
const httpCheckTimeout = 30 * time.Second

func (httpCheckTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args httpCheckArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}

	target, port, failure := parseLocalURL(args.URL)
	if failure != "" {
		return fail("%s", failure), nil
	}

	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	if args.Body != "" {
		body = strings.NewReader(args.Body)
	}

	reqCtx, cancel := context.WithTimeout(ctx, httpCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, target.String(), body)
	if err != nil {
		return fail("could not build the request: %s", err), nil
	}
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}

	// The connection is tunnelled into the sandbox rather than shelled out to curl.
	//
	// It reaches the same place — DialPort connects to 127.0.0.1 inside the sandbox,
	// which is what the preview proxy already relies on — and it removes a
	// dependency on curl being installed, gives real status and header parsing
	// instead of scraping text, and keeps body truncation in Go where the limit is
	// declared.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
				return sb.DialPort(dialCtx, port)
			},
			DisableKeepAlives: true,
		},
		// Redirects are reported, not followed: a 302 where a 200 was expected is a
		// finding, and quietly following it would hide the misconfiguration.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       httpCheckTimeout,
	}

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return Result{}, err
		}
		return Result{
			Content: fmt.Sprintf("%s %s failed after %s: %v\n\nNothing is listening on port %d, or it closed the "+
				"connection. Check the process is up with read_logs.",
				method, target, time.Since(started).Round(time.Millisecond), err, port),
			Display: map[string]any{"url": target.String(), "method": method, "port": port, "failed": true},
			IsError: true,
		}, nil
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes+1))
	elapsed := time.Since(started)

	bodyText := string(raw)
	truncated := false
	if len(raw) > maxHTTPBodyBytes {
		bodyText = string(raw[:maxHTTPBodyBytes])
		truncated = true
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%s %s\n%s (%s)\n\n", method, target, resp.Status, elapsed.Round(time.Millisecond))

	names := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(&out, "%s: %s\n", k, strings.Join(resp.Header[k], ", "))
	}

	out.WriteByte('\n')
	if bodyText == "" {
		out.WriteString("(empty body)")
	} else {
		out.WriteString(bodyText)
	}
	if truncated {
		out.WriteString(notice("body cut at %s", byteCount(maxHTTPBodyBytes)))
	}
	if readErr != nil {
		fmt.Fprintf(&out, "\n[reading the body failed part way through: %v]", readErr)
	}

	// A non-2xx is reported as a tool error so the agent treats it as a finding
	// rather than reading past it. The full response is still in Content.
	return Result{
		Content: out.String(),
		Display: map[string]any{
			"url": target.String(), "method": method, "status": resp.StatusCode,
			"duration_ms": elapsed.Milliseconds(), "truncated": truncated,
		},
		IsError: resp.StatusCode >= 400,
	}, nil
}

// localHosts are the only hosts http_check will talk to.
var localHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"[::1]":     true,
	"0.0.0.0":   true,
}

// parseLocalURL validates a localhost URL and extracts its port.
//
// The restriction is deliberate and not a limitation to work around: this is a
// verification tool, and an agent with unrestricted outbound HTTP is a different
// security proposition. Anything genuinely external goes through bash with curl,
// where it is at least explicit in the transcript.
func parseLocalURL(raw string) (*url.URL, int, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, 0, "url is required."
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, 0, fmt.Sprintf("%q is not a valid URL: %s", raw, err)
	}
	if u.Scheme != "http" {
		return nil, 0, fmt.Sprintf("http_check speaks plain http, not %q. A dev server on localhost does not need TLS.", u.Scheme)
	}

	host := u.Hostname()
	if !localHosts[host] {
		return nil, 0, fmt.Sprintf("http_check only reaches localhost, not %q. Use bash with curl if you need an external host.", host)
	}

	portText := u.Port()
	if portText == "" {
		return nil, 0, "include the port your dev server listens on, for example http://localhost:3000/."
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return nil, 0, fmt.Sprintf("%q is not a valid port.", portText)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u, port, ""
}

// ---------------------------------------------------------------------------
// expose_port
// ---------------------------------------------------------------------------

type exposePortTool struct{}

type exposePortArgs struct {
	Port int `json:"port"`
}

func (exposePortTool) Name() string { return "expose_port" }

func (exposePortTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "expose_port",
		Description: "Point the operator's preview at the port your dev server listens on. Call this once the " +
			"server is up so they can open it. Idempotent.",
		Params: object(map[string]Param{
			"port": {Type: "integer", Description: "The internal port your server listens on."},
		}, "port"),
	}
}

func (exposePortTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args exposePortArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	p, err := env.needProject()
	if err != nil {
		return Result{}, err
	}
	if args.Port <= 0 || args.Port > 65535 {
		return fail("port must be between 1 and 65535."), nil
	}

	// Recording the port is the whole job: the app's preview proxy tunnels straight
	// to it, which is what makes the operator's preview link work.
	//
	// There used to be a second mechanism here. It shelled out to socat to forward
	// the sandbox's own public port 8080 to this one, which meant probing for socat
	// so its absence could be reported as such, killing any previous forwarder by
	// pidfile under a reserved name, waiting 400ms and checking liveness because
	// socat exits after starting if 8080 is held, and three paragraphs of prose for
	// the three outcomes. All of it produced a second URL for the same thing, and
	// its own comment conceded the preview worked regardless of whether it
	// succeeded. One way to see the preview is enough.
	if err := env.Store.SetPreviewPort(ctx, p.ID, args.Port); err != nil {
		return Result{}, err
	}
	p.PreviewPort = args.Port

	return Result{
		Content: fmt.Sprintf("Preview port set to %d. The operator can open it at /preview/ "+
			"and it is proxied to this port inside the sandbox.", args.Port),
		Display: map[string]any{"port": args.Port, "url": "/preview/"},
	}, nil
}
