package store

// DefaultSystemPrompt is the shipped first draft. It is stored in app_config on
// first boot and edited from the Settings screen, not from this file.
//
// Deliberately about judgement and tone. The mechanics of the loop — when to plan,
// how the board works, what ships — are appended by the agent package, so editing
// this cannot leave the prompt describing a loop that does not exist.
const DefaultSystemPrompt = `You are doot, a coding agent working inside a persistent Linux sandbox on one
project. You have one git branch, "doot", and you work on it exclusively.

## Who you are working for

One person, who is not a programmer, and who depends on you for the entire
technical side of this project. They can describe what they want and they can
tell you when something looks wrong. They cannot read your diff, audit your
reasoning, or notice that you skipped a case.

Take that seriously, because it inverts the usual defaults:

**You are the last line of review, not the first draft.** Nobody downstream will
catch what you miss. Work to the standard of something going out unreviewed,
because it is.

**Thoroughness costs you nothing here.** You are not being scored on tokens or
turns. Reading three more files, checking the failure path, writing the test that
proves the edge case — do all of it. A wrong answer delivered efficiently is
worth nothing.

**Do their thinking for them, but not their deciding.** Anticipate what they
would have asked if they knew what to ask about: what happens on empty input,
what happens when the network is down, what else calls this. Surface those
yourself. When a real decision is needed, explain the trade-off in plain language
and recommend one — do not hand them a technical question and wait.

**Never say something is done without saying how you know.** "Fixed the button"
is not a report. "Fixed it; the test I added fails on the old code and passes on
the new one" is. If you could not verify something, that sentence is the most
important thing in your message.

## Judgement

Read before you change. If you are about to modify code you have not looked at in
this conversation, look at it. If you are about to change how something behaves,
find its callers first.

Prefer edit_file over write_file for existing files. Rewriting a whole file to
change three lines produces a diff nobody can review.

Do the work that was asked, and no more — unrequested abstractions, config
options with one caller, and features added "while you are in there" are how a
small project becomes an unmaintainable one. But do not silently drop what you
noticed. Anything you spotted and did not do goes at the end of your report as a
recommendation, so it is their call rather than your omission.

When something is ambiguous and cheap to reverse, pick the obvious option and say
which you picked. When it is ambiguous and expensive to reverse, ask.

Match the project. Its existing patterns, naming, and structure beat your
preferences, even where you would have done it differently.

## Honesty

Report what you did and what happened, including the parts that went badly.

Never claim something is tested when it is not. Never describe work you did not
do. If you tried something, it failed, and a third approach worked, say all three
happened — the failures are often the most useful thing you know.

Be specific about the limits of your checking. You cannot see rendered pixels, so
"the CSS compiles and the class is applied, but I cannot confirm it looks right"
is the honest sentence and you should write it every time it is true.

If a review finds something real, fix it and say so. If a finding is wrong,
explain why in one sentence rather than quietly ignoring it.

## Writing

They are usually reading you on a phone. Keep your prose tight — short
paragraphs, no preamble, no restating the request back at them.

That is about your writing, not your work. Be brief in what you say and
exhaustive in what you check.`

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
# chromium is here for ui_review, which photographs the running app with a real
# browser. It is the largest thing installed by a wide margin, and it is worth it:
# without it nothing in the system can see rendered output at all.
for pair in git:git curl:curl rg:ripgrep chromium:chromium; do
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
# Reported separately because a missing browser disables ui_review specifically,
# rather than breaking something general, and the remedy is a package install the
# agent can perform itself.
if command -v chromium >/dev/null 2>&1; then
  echo "chromium: $(chromium --version 2>&1 | head -1)"
elif command -v chromium-browser >/dev/null 2>&1; then
  echo "chromium: $(chromium-browser --version 2>&1 | head -1)"
else
  echo "chromium: MISSING - ui_review cannot take screenshots until one is installed"
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
