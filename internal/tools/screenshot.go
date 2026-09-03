package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/sandbox"
)

// Screenshots are taken by a real browser inside the sandbox.
//
// Inside, not here, because the thing being photographed is a dev server on
// localhost:<port> in the sandbox. Running the browser next to it means no port
// forwarding, no proxy in the path, and no way for the app's own machine to need a
// browser installed.
//
// The bytes come back through Sandbox.ReadFile rather than base64 through stdout.
// Command output is capped at 30KB — see truncate.go — and a screenshot is an order
// of magnitude past that, so piping it through the shell would silently truncate
// every capture into a corrupt PNG.

// shotDir is where captures land inside the sandbox.
//
// A fixed directory, so a reviewer can list what it has already taken and the files
// survive for the operator to find if a review says something surprising.
const shotDir = "/tmp/doot-shots"

// Viewport bounds. Wide enough for a desktop layout, tall enough to be useful, and
// capped so a mistaken argument cannot ask for a 20000px canvas the browser will
// spend a minute rendering.
const (
	minViewport     = 240
	maxViewportW    = 2560
	maxViewportH    = 4000
	defaultWidth    = 1280
	defaultHeight   = 900
	maxScreenshotMB = 5
)

// browserCandidates are the binaries to look for, in order of preference.
var browserCandidates = []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"}

// ScreenshotResult is the display payload for one capture.
type ScreenshotResult struct {
	Label  string `json:"label"`
	URL    string `json:"url"`
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int    `json:"bytes"`
}

type screenshotArgs struct {
	URL     string `json:"url"`
	Label   string `json:"label"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	WaitMS  int    `json:"wait_ms"`
	Element string `json:"element"`
}

type screenshotTool struct{}

func (screenshotTool) Name() string { return "screenshot" }

func (screenshotTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "screenshot",
		Description: "Photograph a page with a real browser and look at it. Point it at a server already " +
			"running in the sandbox, e.g. http://localhost:3000/settings. Captures the viewport at the size " +
			"you ask for, so take a phone width and a desktop width separately — that is where layout breaks. " +
			"It is not a full-page capture: to see further down the page, ask for a taller viewport.",
		Params: object(map[string]Param{
			"url":     {Type: "string", Description: "Full URL, reachable from inside the sandbox. Usually http://localhost:<port>/<path>."},
			"label":   {Type: "string", Description: "Short name for this shot, e.g. \"settings-phone\". Used as the filename."},
			"width":   {Type: "integer", Description: "Viewport width in CSS pixels. Default 1280. Use 390 for a phone."},
			"height":  {Type: "integer", Description: "Viewport height in CSS pixels. Default 900."},
			"wait_ms": {Type: "integer", Description: "How long to let the page settle before capturing. Default 1200."},
		}, "url", "label"),
	}
}

func (screenshotTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args screenshotArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}

	// Arguments are validated before the sandbox is touched. The other way round, a
	// missing label came back as "no sandbox available" — an infrastructure error,
	// which strands the whole run, for something the model could have corrected
	// itself from a one-line result.
	args.URL = strings.TrimSpace(args.URL)
	if args.URL == "" {
		return fail("url is required."), nil
	}
	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		return fail("url must start with http:// or https://. To photograph a server you started in the " +
			"sandbox, use http://localhost:<port>."), nil
	}

	label := sanitiseLabel(args.Label)
	if label == "" {
		return fail("label is required, and must contain some letters or digits."), nil
	}

	width := clampViewport(args.Width, defaultWidth, maxViewportW)
	height := clampViewport(args.Height, defaultHeight, maxViewportH)
	wait := args.WaitMS
	if wait <= 0 {
		wait = 1200
	}
	if wait > 15000 {
		wait = 15000
	}

	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}

	browser, err := findBrowser(ctx, sb)
	if err != nil {
		return fail("%s", err), nil
	}

	out := path.Join(shotDir, label+".png")
	// --no-sandbox because the sandbox already is one and Chromium's own sandbox
	// needs privileges a container does not have. --disable-dev-shm-usage because
	// /dev/shm is commonly tiny in a container and Chromium crashes silently on it.
	// --virtual-time-budget is the wait: it advances the page's clock rather than
	// sleeping, so timers and animations settle without a real delay.
	cmd := fmt.Sprintf(
		"mkdir -p %s && %s --headless --no-sandbox --disable-gpu --disable-dev-shm-usage "+
			"--hide-scrollbars --force-device-scale-factor=1 --window-size=%d,%d "+
			"--virtual-time-budget=%d --screenshot=%s %s 2>&1",
		shellQuote(shotDir), shellQuote(browser), width, height, wait,
		shellQuote(out), shellQuote(args.URL))

	res, execErr := sb.Exec(ctx, sandbox.Command{Cmd: cmd, Timeout: 2 * time.Minute})
	if execErr != nil {
		return Result{}, fmt.Errorf("screenshot: %w", execErr)
	}
	if res.ExitCode != 0 {
		return fail("the browser failed (exit %d): %s", res.ExitCode, clipMiddle(strings.TrimSpace(res.Output()), 2000)), nil
	}

	data, readErr := sb.ReadFile(ctx, out)
	if readErr != nil {
		return fail("the browser reported success but no image was written to %s. "+
			"Browser output: %s", out, clipMiddle(strings.TrimSpace(res.Output()), 1000)), nil
	}
	if len(data) == 0 {
		return fail("the capture at %s is empty. Is anything actually serving %s?", out, args.URL), nil
	}
	if len(data) > maxScreenshotMB<<20 {
		return fail("the capture is %s, too large to look at. Ask for a smaller viewport.", byteCount(len(data))), nil
	}

	return Result{
		Content: fmt.Sprintf("Captured %s at %dx%d (%s): %s", args.URL, width, height, byteCount(len(data)), out),
		Display: ScreenshotResult{Label: label, URL: args.URL, Path: out,
			Width: width, Height: height, Bytes: len(data)},
	}, nil
}

// findBrowser locates a usable browser, or explains how to get one.
//
// The setup script installs Chromium, but it only runs when a project is created, so
// any sandbox older than that feature has none. Rather than fail with a shell "command
// not found", this says exactly what to run — the agent has bash and passwordless
// sudo, so it can fix this itself without the operator recreating anything.
func findBrowser(ctx context.Context, sb sandbox.Sandbox) (string, error) {
	probe := "for b in " + strings.Join(browserCandidates, " ") +
		"; do command -v \"$b\" && exit 0; done; exit 1"
	res, err := sb.Exec(ctx, sandbox.Command{Cmd: probe, Timeout: 30 * time.Second})
	if err != nil {
		return "", fmt.Errorf("could not look for a browser: %w", err)
	}
	if res.ExitCode == 0 {
		if found := strings.TrimSpace(strings.Split(strings.TrimSpace(res.Output()), "\n")[0]); found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("no browser is installed in this sandbox, so nothing can be photographed. " +
		"Install one with bash:\n\n" +
		"  sudo apt-get update -qq && sudo apt-get install -y -qq --no-install-recommends chromium\n\n" +
		"Then take the screenshot again. If apt has no chromium package, try chromium-browser.")
}

// sanitiseLabel reduces a label to something safe as a filename.
//
// Not merely tidiness: the label is interpolated into a path, and a label containing
// a slash or a traversal would write outside the screenshot directory.
func sanitiseLabel(in string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(in)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteByte('-')
		}
	}
	label := strings.Trim(b.String(), "-")
	for strings.Contains(label, "--") {
		label = strings.ReplaceAll(label, "--", "-")
	}
	if len(label) > 60 {
		label = label[:60]
	}
	return label
}

func clampViewport(v, fallback, max int) int {
	if v <= 0 {
		return fallback
	}
	if v < minViewport {
		return minViewport
	}
	if v > max {
		return max
	}
	return v
}

// ReadScreenshot fetches a capture's bytes back out of the sandbox.
//
// Exported because the UI reviewer needs the image itself, and a tool result on this
// transport is a string — so the tool records where the file is and the caller
// attaches it to the next model request.
func ReadScreenshot(ctx context.Context, sb sandbox.Sandbox, shotPath string) ([]byte, error) {
	if !strings.HasPrefix(shotPath, shotDir+"/") {
		return nil, fmt.Errorf("refusing to read %q: screenshots live under %s", shotPath, shotDir)
	}
	return sb.ReadFile(ctx, shotPath)
}
