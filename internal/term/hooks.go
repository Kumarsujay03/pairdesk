package term

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ShimDir layout. The directory is 0700 and private to one session;
// everything in it is regenerated per session.
//
//	ShimDir/bin/   sudo shim, askpass helper, rc files; first in the
//	               visitor's PATH and bind-mounted read-only into the sandbox
//	ShimDir/ipc/   events and pw FIFOs; covered by an empty tmpfs inside
//	               the sandbox, so from the visitor's side it holds only
//	               the shim's lock directory
const (
	dirBin      = "bin"
	dirIPC      = "ipc"
	fileEvents  = "events"  // FIFO: shell hooks, the shim and askpass write lines here
	filePw      = "pw"      // FIFO: the host writes one password line per approved askpass
	fileSudo    = "sudo"    // the shim
	fileAskpass = "askpass" // SUDO_ASKPASS helper, run by real sudo outside the sandbox
	fileBashrc  = "bashrc"
	fileFish    = "hooks.fish"
	dirZsh      = "zdotdir"
	dirLock     = "lock" // under ipc: mkdir mutex between concurrent shims

	// Inherited file descriptors inside the visitor shell (ExtraFiles).
	fdEvents = 3 // write end of ipc/events
	fdReply  = 4 // read end of the reply FIFO (verdicts and exit codes only)

	askpassPromptPrefix = "pairdesk-askpass-" // real sudo passes its prompt to the askpass helper as $1
)

// The sudo shim. Nothing it does depends on the environment: the paths it
// needs are embedded when the host writes it, and its only link to the
// host is the inherited fds 3 (events, write) and 4 (replies, read).
//
// It never runs sudo itself. It reports the request (cwd and the exact
// argv, hex-encoded) and waits for the host, which applies the tier and,
// when allowed, runs real sudo outside the sandbox on a second PTY that
// is relayed to the visitor. So the visitor's namespace never holds a
// password, an askpass FIFO or a process that has either.
//
// Replies on fd 4 are "<id>\tdone\t<rc>" or "<id>\tdeny\t<msg>". Several
// shims (sudo a & sudo b) serialize on a mkdir lock so only one reads the
// shared fd at a time; lines for another id are discarded.
func shimScript(ipcDir string) string {
	return `#!/bin/sh
# pairdesk sudo shim: generated per session, do not edit.
# The host decides what runs; nothing in the environment changes that.
lock=` + shellQuote(filepath.Join(ipcDir, dirLock)) + `
tab=$(printf '\t')
if ! { true >&3; } 2>/dev/null || ! { true <&4; } 2>/dev/null; then
  echo "pairdesk: sudo is unavailable: host link missing" >&2
  exit 1
fi
id=$$
args=$(printf '%s\0' "$@" | od -An -v -tx1 | tr -d ' \n')
n=0
while ! mkdir "$lock" 2>/dev/null; do
  p=$(cat "$lock/pid" 2>/dev/null)
  if [ -n "$p" ] && ! kill -0 "$p" 2>/dev/null; then rm -rf "$lock"; continue; fi
  n=$((n+1))
  if [ "$n" -gt 600 ]; then echo "pairdesk: sudo shim: another sudo is still waiting" >&2; exit 1; fi
  sleep 0.1
done
echo "$id" > "$lock/pid"
trap 'rm -rf "$lock"' EXIT
printf 'sudo\t%s\t%s\t%s\n' "$id" "$PWD" "$args" >&3
rc=1
while IFS= read -r line <&4; do
  case "$line" in
    "$id${tab}done${tab}"*) rc=${line##*"$tab"}; break ;;
    "$id${tab}deny${tab}"*) echo "${line#*"${tab}deny${tab}"}" >&2; rc=1; break ;;
  esac
done
exit "$rc"
`
}

// The askpass helper. Real sudo runs it outside the sandbox (the host
// spawns sudo) with the one-shot token in the prompt argument. It reports
// the token to the host over the events FIFO and reads the password from
// the pw FIFO; both live under ShimDir/ipc, which the visitor's mount
// namespace cannot see. A wrong or reused token gets an empty line.
func askpassScript(ipcDir string) string {
	return `#!/bin/sh
# pairdesk askpass helper: generated per session, do not edit.
ev=` + shellQuote(filepath.Join(ipcDir, fileEvents)) + `
pw=` + shellQuote(filepath.Join(ipcDir, filePw)) + `
t=${1##*` + askpassPromptPrefix + `}
t=${t%%[!0-9a-f]*}
[ -n "$t" ] && [ -p "$ev" ] && [ -p "$pw" ] || exit 1
printf 'pw\t%s\n' "$t" > "$ev"
IFS= read -r p < "$pw"
printf '%s\n' "$p"
`
}

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
  printf 'pre\t%s\t%s\n' "$PWD" "$line" >&3 2>/dev/null
  return 0
}
__pd_post() {
  local e=$?
  if [ "$__pd_ready" = 0 ]; then
    printf 'post\t%s\t%s\n' "$e" "$PWD" >&3 2>/dev/null
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
__pd_pre() { printf 'pre\t%s\t%s\n' "$PWD" "$1" >&3 2>/dev/null }
__pd_post() { local e=$?; printf 'post\t%s\t%s\n' "$e" "$PWD" >&3 2>/dev/null }
autoload -Uz add-zsh-hook
add-zsh-hook preexec __pd_pre
add-zsh-hook precmd __pd_post
`

const zshEnv = `[ -f "$HOME/.zshenv" ] && source "$HOME/.zshenv"
`

const fishHooks = `# pairdesk shell hooks: generated per session, do not edit.
function __pd_pre --on-event fish_preexec
    printf 'pre\t%s\t%s\n' "$PWD" "$argv[1]" >&3 2>/dev/null
end
function __pd_post --on-event fish_postexec
    set -l e $status
    printf 'post\t%s\t%s\n' "$e" "$PWD" >&3 2>/dev/null
end
`

// writeShimDir populates dir/bin and dir/ipc. dir must exist with mode
// 0700. Previous contents of both subdirectories are discarded.
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
	bin, ipc := filepath.Join(dir, dirBin), filepath.Join(dir, dirIPC)
	for _, d := range []string{bin, ipc} {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
	}
	files := []struct {
		name string
		body string
		mode os.FileMode
	}{
		{fileSudo, shimScript(ipc), 0o700},
		{fileAskpass, askpassScript(ipc), 0o700},
		{fileBashrc, bashRC, 0o600},
		{fileFish, fishHooks, 0o600},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(bin, f.name), []byte(f.body), f.mode); err != nil {
			return err
		}
	}
	zd := filepath.Join(bin, dirZsh)
	if err := os.MkdirAll(zd, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(zd, ".zshrc"), []byte(zshRC), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(zd, ".zshenv"), []byte(zshEnv), 0o600); err != nil {
		return err
	}
	for _, f := range []string{fileEvents, filePw} {
		if err := syscall.Mkfifo(filepath.Join(ipc, f), 0o600); err != nil {
			return fmt.Errorf("term: mkfifo %s: %w", f, err)
		}
	}
	return nil
}

// decodeHexArgs turns the shim's hex-encoded, NUL-separated argv back
// into a slice.
func decodeHexArgs(h string) ([]string, bool) {
	if len(h)%2 != 0 {
		return nil, false
	}
	raw := make([]byte, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		var b byte
		for _, c := range h[i : i+2] {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				b |= byte(c-'a') + 10
			default:
				return nil, false
			}
		}
		raw = append(raw, b)
	}
	if len(raw) == 0 {
		return nil, true
	}
	if raw[len(raw)-1] != 0 {
		return nil, false
	}
	return strings.Split(string(raw[:len(raw)-1]), "\x00"), true
}
