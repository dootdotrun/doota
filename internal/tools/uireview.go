package tools

import (
	"context"
	"encoding/json"
	"strings"
)

// UI review modes.
const (
	// UIModeDesign is consulted before building: what should this look like.
	UIModeDesign = "design"
	// UIModeVerify is consulted after building: does it actually look like that.
	UIModeVerify = "verify"
)

// UIReviewRequest asks the UI subagent for a design brief or a rendered verdict.
//
// The URL is required in verify mode because there is nothing to look at without a
// running server, and optional in design mode where the question may be about
// something that does not exist yet.
type UIReviewRequest struct {
	Mode    string   `json:"mode"`
	Intent  string   `json:"intent"`
	URL     string   `json:"url"`
	Paths   []string `json:"paths"`
	Focus   string   `json:"focus"`
	Devices []string `json:"devices"`
}

type uiReviewTool struct{}

func (uiReviewTool) Name() string { return "ui_review" }

func (uiReviewTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "ui_review",
		Description: "Hand the interface to a designer who can actually see it. In `design` mode, before you " +
			"build: describe what you are about to make and get back a concrete design brief — layout, " +
			"spacing, hierarchy, states, responsive behaviour. In `verify` mode, after you have built it and " +
			"after `review`: it opens your running server in a real browser at phone and desktop widths, " +
			"looks at the screenshots, and reports what is actually wrong on screen. " +
			"For verify you must have a server running (bash_bg) and pass its URL.",
		Params: object(map[string]Param{
			"mode":   {Type: "string", Enum: []string{UIModeDesign, UIModeVerify}, Description: "design before building, verify after."},
			"intent": {Type: "string", Description: "What this screen is for and what it should do, in plain language. In verify mode, what you built and what it should look like."},
			"url":    {Type: "string", Description: "Base URL of your running server inside the sandbox, e.g. http://localhost:3000. Required for verify."},
			"paths": {Type: "array", Items: &Param{Type: "string"},
				Description: "Paths to look at, e.g. [\"/\", \"/settings\"]. Defaults to \"/\"."},
			"focus": {Type: "string", Description: "Optional: anything specific to judge."},
			"devices": {Type: "array", Items: &Param{Type: "string"}, Enum: []string{"phone", "desktop"},
				Description: "Which widths to check. Defaults to both."},
		}, "mode", "intent"),
	}
}

func (uiReviewTool) Execute(_ context.Context, in json.RawMessage, _ *Env) (Result, error) {
	var args UIReviewRequest
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}

	args.Mode = strings.TrimSpace(strings.ToLower(args.Mode))
	switch args.Mode {
	case UIModeDesign, UIModeVerify:
	default:
		return fail("mode must be %q or %q.", UIModeDesign, UIModeVerify), nil
	}

	args.Intent = strings.TrimSpace(args.Intent)
	if args.Intent == "" {
		return fail("intent is required: say what this screen is for and what it should do."), nil
	}

	args.URL = strings.TrimRight(strings.TrimSpace(args.URL), "/")
	if args.Mode == UIModeVerify {
		if args.URL == "" {
			return fail("url is required to verify: start your server with bash_bg, then pass its " +
				"address, e.g. http://localhost:3000."), nil
		}
		if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
			return fail("url must start with http:// or https://."), nil
		}
	}

	args.Paths = trimAll(args.Paths)
	if len(args.Paths) == 0 {
		args.Paths = []string{"/"}
	}
	for i, p := range args.Paths {
		if !strings.HasPrefix(p, "/") {
			args.Paths[i] = "/" + p
		}
	}
	// More than a handful of pages per review is a request to look at everything,
	// which produces a shallow answer about all of it rather than a useful one about
	// any of it.
	if len(args.Paths) > 4 {
		return fail("at most 4 paths per review. Pick the ones that matter and call it again for the rest."), nil
	}

	args.Devices = trimAll(args.Devices)
	if len(args.Devices) == 0 {
		args.Devices = []string{"phone", "desktop"}
	}
	for _, d := range args.Devices {
		if d != "phone" && d != "desktop" {
			return fail("devices must be \"phone\", \"desktop\", or both."), nil
		}
	}

	args.Focus = strings.TrimSpace(args.Focus)

	verb := "Asking for a design brief."
	if args.Mode == UIModeVerify {
		verb = "Opening it in a browser to look at it."
	}
	return Result{Content: verb, Display: args}, nil
}
