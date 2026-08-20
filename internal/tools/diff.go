package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/sandbox"
)

// Diff is the UI payload for a file change.
//
// Carried in Result.Display so the phone can render a real diff while the model
// gets a one-line summary in Result.Content.
type Diff struct {
	Path      string `json:"path"`
	Unified   string `json:"unified"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Created   bool   `json:"created,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Stat renders the change as a short summary line.
func (d Diff) Stat() string {
	if d.Created {
		return fmt.Sprintf("%s (new file, %d lines)", d.Path, d.Added)
	}
	return fmt.Sprintf("%s (+%d -%d)", d.Path, d.Added, d.Removed)
}

// fileDiff describes what a write did, by asking git.
//
// This used to be a from-scratch unified diff: an LCS dynamic-programming table of
// int32s with prefix and suffix trimming, hunk merging when context windows
// touched, @@ header arithmetic including the zero-length-side off-by-one, a
// "\ No newline at end of file" marker, and a 2000-line bound past which it
// degraded to whole-block-replace because the table would otherwise reach 1.6 GB.
// Roughly 250 lines reimplementing a program that is already installed in the
// sandbox, and which the repository is already a checkout of.
//
// A newly created file gets a line count rather than a diff. The diff of a file
// against nothing is just the file, and nobody reads that as a diff.
func fileDiff(ctx context.Context, sb sandbox.Sandbox, abs string, created bool, newText string) Diff {
	display := relToRepo(abs)
	if created {
		return Diff{Path: display, Created: true, Added: countLines(newText)}
	}

	res, err := sb.Exec(ctx, sandbox.Command{
		Cmd: "git --no-pager diff --no-color -- " + shellQuote(display),
		Dir: project.RepoPath,
	})
	if err != nil || res.ExitCode != 0 {
		// Not worth failing a completed write over. The file is on disk; the
		// operator loses the rendered diff, not the change.
		return Diff{Path: display}
	}

	unified := res.Stdout
	d := Diff{Path: display}
	for _, line := range strings.Split(unified, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			d.Added++
		case strings.HasPrefix(line, "-"):
			d.Removed++
		}
	}
	if capped, cut := clip(unified, maxDiffBytes); cut {
		unified = capped + notice("diff larger than %s", byteCount(maxDiffBytes))
		d.Truncated = true
	}
	d.Unified = unified
	return d
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}
