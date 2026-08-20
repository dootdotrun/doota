package web

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// Markdown rendering for model prose.
//
// The model writes markdown whether or not anything renders it, so the choice was
// never "plain text or markdown" — it was "markdown, or markdown's punctuation
// shown literally". Headings, fenced code, and lists are how it structures an
// explanation, and reading `### ` and `- ` on a phone is the version that costs
// more attention.
//
// Two layers of escaping, deliberately:
//
//  1. goldmark runs without html.WithUnsafe, so raw HTML in the model's output is
//     dropped rather than passed through. This is the layer that matters.
//  2. bluemonday sanitises the result anyway. goldmark's own output is trustworthy;
//     this catches the thing goldmark still emits verbatim, which is link and image
//     URLs — a `[click](javascript:...)` is valid markdown and would otherwise
//     become a working link in the transcript.
//
// The agent's output is not hostile input in the usual sense, but it is text
// assembled from repository contents and fetched web pages, which is exactly the
// shape of thing that ends up carrying an injected payload.

var (
	markdownOnce sync.Once
	markdownMD   goldmark.Markdown
	markdownSan  *bluemonday.Policy
)

// Attribute value patterns, kept narrow enough that the policy below can be read
// against what goldmark actually emits.
var (
	// codeClass matches the language hint goldmark puts on a fenced block.
	codeClass     = regexp.MustCompile(`^language-[a-zA-Z0-9#+._-]+$`)
	tableAlign    = regexp.MustCompile(`^(left|right|center)$`)
	inputCheckbox = regexp.MustCompile(`^checkbox$`)
)

func markdownInit() {
	markdownMD = goldmark.New(
		goldmark.WithExtensions(
			// GFM minus the pieces that do not apply: tables and strikethrough and
			// task lists all appear in real model output, and linkify saves the model
			// from having to remember angle brackets around a URL.
			extension.Table,
			extension.Strikethrough,
			extension.TaskList,
			extension.Linkify,
		),
		goldmark.WithRendererOptions(
			// A single newline becomes a <br>. Without this, prose written with hard
			// wraps reflows into one paragraph and lists written without a blank line
			// above them collapse into the preceding sentence.
			html.WithHardWraps(),
			// No html.WithUnsafe: raw HTML in the source is omitted.
		),
	)

	// Built as an explicit allowlist rather than from bluemonday.UGCPolicy.
	//
	// UGCPolicy is a reasonable default for user-generated content, but it is a
	// wider surface than goldmark can even produce, and it permits <img src> — see
	// the note below. Listing the tags this renderer actually emits means the policy
	// can be read against the renderer and checked, and anything unexpected in the
	// output is dropped by default rather than by omission from a denylist.
	p := bluemonday.NewPolicy()

	// Block and inline structure.
	p.AllowElements("p", "br", "hr", "blockquote",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li",
		"strong", "em", "del", "code", "pre")

	// Fenced blocks carry their language so a highlighter could be added later, and
	// so the CSS can tell a code block from an indented one.
	p.AllowAttrs("class").Matching(codeClass).OnElements("code")

	// Tables, from the GFM extension.
	p.AllowElements("table", "thead", "tbody", "tr", "th", "td")
	p.AllowAttrs("align").Matching(tableAlign).OnElements("th", "td")

	// Task lists, from the TaskList extension. checkbox is the only type it emits,
	// so the attribute is pinned to it instead of left open.
	p.AllowAttrs("type").Matching(inputCheckbox).OnElements("input")
	p.AllowAttrs("checked", "disabled").OnElements("input")

	// Links, restricted to schemes that cannot execute. This is what stops
	// [click](javascript:alert(1)), which is valid markdown and would otherwise
	// render as a working link.
	p.AllowAttrs("href").OnElements("a")
	p.AllowAttrs("title").OnElements("a", "code")
	p.AllowURLSchemes("http", "https", "mailto")
	p.RequireParseableURLs(true)
	// A link in the transcript points somewhere outside the app; opening it in the
	// same tab would discard the chat scroll position on a phone.
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.RequireNoFollowOnLinks(true)

	// No <img>, deliberately.
	//
	// This content is assembled from repository files and fetched web pages, so a
	// crafted line in a README becomes ![](https://attacker/pixel) and the
	// operator's phone makes that request the moment the transcript renders. That is
	// a read receipt for "the agent surfaced my payload" plus a few hundred bytes of
	// exfiltration per render, in a transcript that contains source code. Agent
	// prose has no legitimate need to embed an image, so the element is absent from
	// the allowlist above rather than narrowed.

	markdownSan = p
}

// renderMarkdown converts model prose to sanitised HTML.
//
// Returns the empty string for empty input so a caller can branch on it without
// trimming again.
func renderMarkdown(src string) template.HTML {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	markdownOnce.Do(markdownInit)

	var buf bytes.Buffer
	if err := markdownMD.Convert([]byte(src), &buf); err != nil {
		// Convert only fails on a writer error, which a bytes.Buffer does not
		// produce. Falling back to escaped plain text keeps a rendering bug from
		// blanking a message.
		return template.HTML("<p>" + template.HTMLEscapeString(src) + "</p>")
	}
	return template.HTML(markdownSan.SanitizeBytes(buf.Bytes()))
}
