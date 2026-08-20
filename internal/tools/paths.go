package tools

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/project"
)

// repoPath resolves a tool-supplied path to a sandbox-absolute one under the
// workspace, and refuses anything that escapes it.
//
// Paths arrive two ways in practice: relative to the repo root, which is what the
// tool descriptions ask for, and sandbox-absolute, which models produce anyway
// after reading an absolute path out of a build error. Both are accepted.
//
// The containment check is not a security boundary — bash can reach the whole
// filesystem, and the sandbox is the isolation. It is there because a file tool
// that silently reads /etc/passwd when the model meant ./etc/passwd turns a typo
// into a confusing result, and because scoping the file tools to the workspace
// makes their behaviour predictable enough to describe in one line.
func repoPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("path is required")
	}

	var abs string
	if strings.HasPrefix(p, "/") {
		abs = path.Clean(p)
		if abs != project.WorkspacePath && !strings.HasPrefix(abs, project.WorkspacePath+"/") {
			return "", fmt.Errorf("%s is outside the workspace (%s); use bash if you genuinely need it",
				abs, project.WorkspacePath)
		}
	} else {
		abs = path.Join(project.RepoPath, p)
		if abs != project.RepoPath && !strings.HasPrefix(abs, project.RepoPath+"/") {
			return "", fmt.Errorf("%q escapes the repository root", p)
		}
	}
	return abs, nil
}

// relToRepo renders a sandbox-absolute path relative to the repo root, for
// display. Paths outside the repo are returned unchanged.
func relToRepo(abs string) string {
	if abs == project.RepoPath {
		return "."
	}
	if rel := strings.TrimPrefix(abs, project.RepoPath+"/"); rel != abs {
		return rel
	}
	return abs
}

// workDir resolves an optional cwd argument, defaulting to the repo root.
//
// bash is allowed anywhere in the sandbox: it is a shell, and pretending
// otherwise would just push the agent into `cd /tmp && ...` to get around it.
func workDir(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return project.RepoPath, nil
	}
	if strings.HasPrefix(cwd, "/") {
		return path.Clean(cwd), nil
	}
	abs := path.Join(project.RepoPath, cwd)
	if abs != project.RepoPath && !strings.HasPrefix(abs, project.RepoPath+"/") {
		return "", fmt.Errorf("%q escapes the repository root; pass an absolute path instead", cwd)
	}
	return abs, nil
}

// processName is the accepted shape of a bash_bg name.
//
// Constrained because the name becomes a filename: the log and pid paths are
// derived from it, so a name containing a slash or a quote would either escape the
// log directory or break the launcher's shell line.
var processName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateProcessName checks a background process name.
func validateProcessName(name string) error {
	if !processName.MatchString(name) {
		return fmt.Errorf("name must be 1-64 characters of letters, digits, dot, dash, or underscore, starting with a letter or digit")
	}
	return nil
}

// logPath is where a background process's output is captured.
func logPath(name string) string { return project.LogDir + "/" + name + ".log" }

// pidPath is where a background process's process-group id is recorded.
//
// The pid lives in the sandbox rather than in a background_process column
// deliberately. It is only meaningful inside that filesystem, and a checkpoint
// restore rewinds the pidfile along with everything else — whereas a database
// column would survive the rewind still naming a process that no longer exists.
func pidPath(name string) string { return project.LogDir + "/" + name + ".pid" }
