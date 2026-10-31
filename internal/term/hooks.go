package term

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Files the host writes into ShimDir. The directory is 0700 and private to
// one session; everything in it is regenerated per session.
const (
	fileEvents  = "events"  // FIFO: shell hooks and the shim write lines here
	fileSudo    = "sudo"    // the shim, first in PATH
	fileAskpass = "askpass" // SUDO_ASKPASS helper used when the host supplies the password
	fileBashrc  = "bashrc"
	fileFish    = "hooks.fish"
	dirZsh      = "zdotdir"
)

// Environment the host sets for the visitor's shell.
const (
	envHook     = "PAIRDESK_HOOK"        // path of the events FIFO
	envShimDir  = "PAIRDESK_SHIM_DIR"    // ShimDir
	envRealSudo = "PAIRDESK_REAL_SUDO"   // real sudo binary
	envMarker   = "PAIRDESK_SUDO_MARKER" // password prompt marker
	envToken    = "PAIRDESK_ASKPASS_TOKEN"
)

// The sudo shim. It always asks the host what to do and waits for the
// verdict on a per-invocation FIFO; the host decides from its own tier so
// nothing the visitor can set in the environment changes the policy.
//
// Verdicts: "allow" runs real sudo with the marker prompt (the visitor
// types the password; the transcript redacts it), "askpass:<token>" runs
// real sudo with SUDO_ASKPASS so the host supplies the password without
// the visitor seeing it, "deny:<msg>" prints msg and exits 1.
const shimScript = `#!/bin/sh
# pairdesk sudo shim: generated per session, do not edit.
hook="$` + envHook + `"
dir="$` + envShimDir + `"
if [ -z "$hook" ] || [ ! -p "$hook" ] || [ -z "$dir" ] || [ ! -d "$dir" ]; then
  echo "pairdesk: sudo is unavailable: host link missing" >&2
  exit 1
fi
real="${` + envRealSudo + `:-/usr/bin/sudo}"
id=$$
resp="$dir/resp.$id"
cmd=$(printf '%s ' "$@" | tr '\n\t' '  ')
cmd=${cmd% }
mkfifo -m 600 "$resp" 2>/dev/null || { echo "pairdesk: sudo shim: cannot create reply pipe" >&2; exit 1; }
printf 'sudo\t%s\t%s\n' "$id" "$cmd" > "$hook"
IFS= read -r verdict < "$resp"
rm -f "$resp"
case "$verdict" in
  allow)
    "$real" -p "$` + envMarker + `" "$@"; rc=$? ;;
  askpass:*)
    ` + envToken + `="${verdict#askpass:}" SUDO_ASKPASS="$dir/` + fileAskpass + `" "$real" -A "$@"; rc=$? ;;
  deny:*)
    echo "${verdict#deny:}" >&2; rc=1 ;;
  *)
    echo "pairdesk: sudo needs the host's consent" >&2; rc=1 ;;
esac
printf 'sudoexit\t%s\t%s\n' "$id" "$rc" > "$hook"
exit $rc
`

// The askpass helper: asks the host for the password over the FIFO and
// prints it to sudo. The one-shot token issued with the verdict must match
// or the host answers with an empty line.
const askpassScript = `#!/bin/sh
hook="$` + envHook + `"
dir="$` + envShimDir + `"
[ -n "$hook" ] && [ -p "$hook" ] && [ -n "$dir" ] || exit 1
resp="$dir/pw.$$"
mkfifo -m 600 "$resp" 2>/dev/null || exit 1
printf 'pw\t%s\t%s\n' "$$" "$` + envToken + `" > "$hook"
IFS= read -r pw < "$resp"
rm -f "$resp"
printf '%s\n' "$pw"
`

const bashRC = `# pairdesk shell hooks: generated per session, do not edit.
if [ -f "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi
set -o history
HISTCONTROL=
HISTIGNORE=
__pd_ready=1
__pd_pre() {
  [ "$__pd_ready" = 1 ] || return 0
  [ -n "$COMP_LINE" ] && return 0
  case "$BASH_COMMAND" in __pd_*) return 0;; esac
  case "${PROMPT_COMMAND[*]}" in *"$BASH_COMMAND"*) return 0;; esac
  __pd_ready=0
  local line
  line=$(HISTTIMEFORMAT= builtin history 1)
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line#*[[:space:]]}"
  line="${line#"${line%%[![:space:]]*}"}"
  [ -n "$line" ] || line="$BASH_COMMAND"
  printf 'pre\t%s\t%s\n' "$PWD" "$line" > "$PAIRDESK_HOOK"
  return 0
}
__pd_post() {
  local e=$?
  if [ "$__pd_ready" = 0 ]; then
    printf 'post\t%s\t%s\n' "$e" "$PWD" > "$PAIRDESK_HOOK"
  fi
  __pd_ready=1
  return 0
}
if [[ "$(declare -p PROMPT_COMMAND 2>/dev/null)" == "declare -a"* ]]; then
  PROMPT_COMMAND=(__pd_post "${PROMPT_COMMAND[@]}")
else
  PROMPT_COMMAND="__pd_post${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
fi
trap '__pd_pre' DEBUG
`

const zshRC = `# pairdesk shell hooks: generated per session, do not edit.
ZDOTDIR="$HOME"
[ -f "$HOME/.zshrc" ] && source "$HOME/.zshrc"
__pd_pre() { printf 'pre\t%s\t%s\n' "$PWD" "$1" > "$PAIRDESK_HOOK" }
__pd_post() { local e=$?; printf 'post\t%s\t%s\n' "$e" "$PWD" > "$PAIRDESK_HOOK" }
autoload -Uz add-zsh-hook
add-zsh-hook preexec __pd_pre
add-zsh-hook precmd __pd_post
`

const zshEnv = `[ -f "$HOME/.zshenv" ] && source "$HOME/.zshenv"
`

const fishHooks = `# pairdesk shell hooks: generated per session, do not edit.
function __pd_pre --on-event fish_preexec
    printf 'pre\t%s\t%s\n' "$PWD" "$argv[1]" > "$PAIRDESK_HOOK"
end
function __pd_post --on-event fish_postexec
    set -l e $status
    printf 'post\t%s\t%s\n' "$e" "$PWD" > "$PAIRDESK_HOOK"
end
`

// writeShimDir populates dir with the shim, askpass helper, rc files and
// the events FIFO. dir must already exist with mode 0700.
func writeShimDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("term: shim dir %s is not a directory", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	files := []struct {
		name string
		body string
		mode os.FileMode
	}{
		{fileSudo, shimScript, 0o700},
		{fileAskpass, askpassScript, 0o700},
		{fileBashrc, bashRC, 0o600},
		{fileFish, fishHooks, 0o600},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), f.mode); err != nil {
			return err
		}
	}
	zd := filepath.Join(dir, dirZsh)
	if err := os.MkdirAll(zd, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(zd, ".zshrc"), []byte(zshRC), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(zd, ".zshenv"), []byte(zshEnv), 0o600); err != nil {
		return err
	}
	fifo := filepath.Join(dir, fileEvents)
	_ = os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return fmt.Errorf("term: mkfifo: %w", err)
	}
	return nil
}
