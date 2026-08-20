package tools

import (
	"fmt"
	"strings"
)

// Output budgets.
//
// Every tool result competes for the same context window, so each one is capped.
// The numbers are round rather than tuned: the point is that no single tool call
// can flood the conversation, not that these are the optimal values.
//
// This used to be eight named tiers backed by five separate truncation strategies,
// including a streaming head/tail io.Writer with three output branches, a
// middle-cut sampler, a byte-boundary heuristic that only snapped to a newline past
// the halfway point of the budget, a human-readable byte formatter used only inside
// notices, and one function that was never called at all.
const (
	maxFileLines     = 2000 // read_file
	maxFileBytes     = 250 << 10
	readCeilingBytes = 4 << 20 // refuse to read or edit anything larger
	maxDirEntries    = 400     // list_dir
	maxSearchHits    = 100     // search
	defaultLogTail   = 200     // read_logs
	maxLogTail       = 2000
	maxBashBytes     = 30 << 10 // bash, read_logs
	maxDiffBytes     = 60 << 10 // git_diff, file diffs
	maxHTTPBodyBytes = 10 << 10 // http_check
)

// notice is the one truncation marker shape, so the model learns to recognise it.
func notice(format string, args ...any) string {
	return "\n[truncated: " + fmt.Sprintf(format, args...) + "]"
}

// byteCount formats a size for a message a person or a model will read. Six call
// sites explain a refusal or a cap, and "4 MB" lands where "4194304" does not.
func byteCount(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// clip caps text at limit bytes, cutting back to the last newline so the result
// does not end mid-line. It reports whether it cut, because every caller wants to
// say something different about it.
func clip(text string, limit int) (string, bool) {
	if limit <= 0 || len(text) <= limit {
		return text, false
	}
	cut := text[:limit]
	if nl := strings.LastIndexByte(cut, '\n'); nl > 0 {
		cut = cut[:nl]
	}
	return cut, true
}

// clipMiddle keeps the start and the end, which is what matters for command output:
// the head says what ran and the tail says how it failed.
func clipMiddle(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	half := limit / 2
	head, tail := text[:half], text[len(text)-half:]
	if nl := strings.LastIndexByte(head, '\n'); nl > 0 {
		head = head[:nl]
	}
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl < len(tail)-1 {
		tail = tail[nl+1:]
	}
	return head + notice("%d bytes cut from the middle of %d", len(text)-len(head)-len(tail), len(text)) + "\n" + tail
}

// boundedBuffer collects streamed output and keeps the first and last half-limit
// bytes of it, because bash cannot know how much is coming.
type boundedBuffer struct {
	limit int
	buf   []byte
	total int
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

// Write keeps at most limit*2 bytes in flight, then compacts. It reports what it
// consumed, not what it kept, because that is an io.Writer's contract.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	b.buf = append(b.buf, p...)
	if len(b.buf) > 2*b.limit {
		// Keep the head and the most recent tail, dropping the middle.
		half := b.limit / 2
		b.buf = append(b.buf[:half:half], b.buf[len(b.buf)-half:]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return clipMiddle(string(b.buf), b.limit) }

// numberLines renders text with right-aligned line numbers starting at start.
//
// read_file returns numbered lines so the agent can quote a location back in an
// edit_file call or a review finding without counting.
func numberLines(text string, start int) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	width := len(fmt.Sprint(start + len(lines) - 1))

	var b strings.Builder
	b.Grow(len(text) + len(lines)*(width+2))
	for i, line := range lines {
		fmt.Fprintf(&b, "%*d  %s", width, start+i, line)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
