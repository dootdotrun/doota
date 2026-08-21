package store

// DefaultSystemPrompt is the shipped first draft. It is stored in app_config on
// first boot and edited from the Settings screen, not from this file.
//
// Deliberately about judgement and tone. The mechanics of the loop — when to plan,
// how the board works, what ships — are appended by the agent package, so editing
// this cannot leave the prompt describing a loop that does not exist.
const DefaultSystemPrompt = `You are doot, a coding agent working inside a persistent Linux sandbox on one
project. You have one git branch, "doot", and you work on it exclusively.

You are working for a single developer who is reading you on a phone, usually
while doing something else. That shapes everything: be brief, be concrete, and
make it obvious what happened.

## Judgement

Prefer edit_file over write_file for existing files. Rewriting a whole file to
change three lines wastes tokens and produces a diff nobody can review.

Read before you change. If you are about to modify code you have not looked at
in this conversation, look at it.

Do the work that was asked and stop. Adding an abstraction nobody requested, a
config option with one caller, or a feature "while you are in there" is how a
small project becomes an unmaintainable one.

When something is ambiguous and the cost of guessing wrong is high, ask. When it
is ambiguous and cheap to reverse, pick the obvious option and say which you
picked.

## Honesty

Report what you did and what happened, including failures. Never claim something
is tested when it is not, and never describe work you did not do.

If you are unsure whether something works, say that plainly. "I changed the CSS
but could not verify it renders correctly" is useful. "Done!" is not.

If you tried something and it failed, say so rather than quietly trying a third
approach and reporting only the success.`

// DefaultSetupScript runs once when a project is created.
//
// The Sprite filesystem persists indefinitely, so this is a one-time cost rather
// than something re-run on every wake.
//
// POSIX sh only, and deliberately so. provision.go executes this as
// `sh /tmp/doot-setup.sh`, which ignores the shebang entirely, and the sandbox
// image's /bin/sh is dash. Bash arrays and `set -o pipefail` are parse errors
// there, and dash aborts the whole file on a parse error rather than the line —
// so a single bash-ism near the top silently skips every install below it. That
// failure is close to invisible: provisioning continues, the clone succeeds, and
// the only symptom is that `search` finds nothing because ripgrep was never
// installed. This script had that bug; keep it POSIX.
const DefaultSetupScript = `#!/bin/sh
# Runs once at project creation. The filesystem persists, so this is not re-run.
#
# POSIX sh only: this is run by dash, where bash arrays are a syntax error that
# aborts the entire script.
set -eu

# Sprites run commands as an unprivileged "sprite" user with passwordless sudo, so
# package installs need escalating. Root is still handled, since the local provider
# and other images may not have sudo at all.
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo -n"
  else
    echo "warning: not root and no sudo available; package installs will be skipped"
  fi
fi

install_pkgs() {
  if command -v apt-get >/dev/null 2>&1; then
    ${SUDO} env DEBIAN_FRONTEND=noninteractive apt-get update -qq
    ${SUDO} env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "$@"
  elif command -v dnf >/dev/null 2>&1; then
    ${SUDO} dnf install -y -q "$@"
  elif command -v apk >/dev/null 2>&1; then
    ${SUDO} apk add --no-cache "$@"
  else
    echo "no supported package manager found" >&2
    return 1
  fi
}

# Keyed by the binary to look for, because a package name is not always the
# command it installs - ripgrep provides "rg", and ca-certificates provides no
# binary at all. Checking the package name instead means the probe never succeeds
# and apt runs on every provision, which this setup is supposed to do once.
#
# A space-separated string rather than an array: word splitting is exactly the
# behaviour wanted at the call site, and unlike an array it is portable.
need=""
for pair in git:git curl:curl rg:ripgrep; do
  bin=${pair%%:*}
  pkg=${pair#*:}
  command -v "${bin}" >/dev/null 2>&1 || need="${need} ${pkg}"
done
# No binary to probe for, so ask the package manager whether it is already there.
if command -v dpkg >/dev/null 2>&1; then
  dpkg -s ca-certificates >/dev/null 2>&1 || need="${need} ca-certificates"
fi
if [ -n "${need}" ]; then
  echo "installing:${need}"
  # Unquoted on purpose: these are package names from the fixed list above, and
  # splitting them into separate arguments is the point.
  # shellcheck disable=SC2086
  install_pkgs ${need} || echo "warning: some packages were not installed"
else
  echo "toolchain already present"
fi

mkdir -p /tmp/doot-logs

# Report each tool separately rather than through a pipeline. Without pipefail a
# missing binary produces an empty string instead of "missing", which reads as
# success in the provision log.
echo "setup complete"
if command -v git >/dev/null 2>&1; then
  echo "git:  $(git --version)"
else
  echo "git:  MISSING - clone, commit, and push will all fail"
fi
if command -v rg >/dev/null 2>&1; then
  echo "rg:   $(rg --version | head -1)"
else
  echo "rg:   MISSING - the search tool has no fallback and will return nothing"
fi
if command -v curl >/dev/null 2>&1; then
  echo "curl: $(curl --version | head -1)"
else
  echo "curl: MISSING"
fi
# No binary to check, so ask the package manager where there is one to ask.
if command -v dpkg >/dev/null 2>&1; then
  if dpkg -s ca-certificates >/dev/null 2>&1; then
    echo "ca-certificates: present"
  else
    echo "ca-certificates: MISSING - HTTPS to github and the model API may fail"
  fi
fi
`
