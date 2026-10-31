package screen

import (
	"github.com/bendahl/uinput"
)

// Key is a decoded terminal keystroke: a Linux KEY_* code plus the modifiers
// needed to reproduce the character the terminal delivered.
type Key struct {
	Code  int
	Shift bool
	Ctrl  bool
	Alt   bool
}

// Events expands a chord into the press/release sequence the host must inject.
func (k Key) Events() []InputEvent {
	var mods []int
	if k.Ctrl {
		mods = append(mods, uinput.KeyLeftctrl)
	}
	if k.Alt {
		mods = append(mods, uinput.KeyLeftalt)
	}
	if k.Shift {
		mods = append(mods, uinput.KeyLeftshift)
	}
	evs := make([]InputEvent, 0, 2*len(mods)+2)
	for _, m := range mods {
		evs = append(evs, InputEvent{Kind: KindKey, Code: m, Press: true})
	}
	evs = append(evs,
		InputEvent{Kind: KindKey, Code: k.Code, Press: true},
		InputEvent{Kind: KindKey, Code: k.Code, Press: false},
	)
	for i := len(mods) - 1; i >= 0; i-- {
		evs = append(evs, InputEvent{Kind: KindKey, Code: mods[i], Press: false})
	}
	return evs
}

// asciiKeys maps printable US-ASCII characters to their key and shift state.
var asciiKeys = func() map[byte]Key {
	m := map[byte]Key{}
	plain := map[byte]int{
		' ': uinput.KeySpace, '-': uinput.KeyMinus, '=': uinput.KeyEqual,
		'[': uinput.KeyLeftbrace, ']': uinput.KeyRightbrace, '\\': uinput.KeyBackslash,
		';': uinput.KeySemicolon, '\'': uinput.KeyApostrophe, '`': uinput.KeyGrave,
		',': uinput.KeyComma, '.': uinput.KeyDot, '/': uinput.KeySlash,
	}
	shifted := map[byte]int{
		'_': uinput.KeyMinus, '+': uinput.KeyEqual,
		'{': uinput.KeyLeftbrace, '}': uinput.KeyRightbrace, '|': uinput.KeyBackslash,
		':': uinput.KeySemicolon, '"': uinput.KeyApostrophe, '~': uinput.KeyGrave,
		'<': uinput.KeyComma, '>': uinput.KeyDot, '?': uinput.KeySlash,
		'!': uinput.Key1, '@': uinput.Key2, '#': uinput.Key3, '$': uinput.Key4,
		'%': uinput.Key5, '^': uinput.Key6, '&': uinput.Key7, '*': uinput.Key8,
		'(': uinput.Key9, ')': uinput.Key0,
	}
	for c, code := range plain {
		m[c] = Key{Code: code}
	}
	for c, code := range shifted {
		m[c] = Key{Code: code, Shift: true}
	}
	letters := []int{
		uinput.KeyA, uinput.KeyB, uinput.KeyC, uinput.KeyD, uinput.KeyE, uinput.KeyF,
		uinput.KeyG, uinput.KeyH, uinput.KeyI, uinput.KeyJ, uinput.KeyK, uinput.KeyL,
		uinput.KeyM, uinput.KeyN, uinput.KeyO, uinput.KeyP, uinput.KeyQ, uinput.KeyR,
		uinput.KeyS, uinput.KeyT, uinput.KeyU, uinput.KeyV, uinput.KeyW, uinput.KeyX,
		uinput.KeyY, uinput.KeyZ,
	}
	for i, code := range letters {
		m['a'+byte(i)] = Key{Code: code}
		m['A'+byte(i)] = Key{Code: code, Shift: true}
	}
	digits := []int{
		uinput.Key0, uinput.Key1, uinput.Key2, uinput.Key3, uinput.Key4,
		uinput.Key5, uinput.Key6, uinput.Key7, uinput.Key8, uinput.Key9,
	}
	for i, code := range digits {
		m['0'+byte(i)] = Key{Code: code}
	}
	return m
}()

// escapeKeys maps complete escape sequences (without modifier variants) to keys.
// Both CSI (ESC [) and SS3 (ESC O) forms are listed because terminals differ
// in application vs normal cursor-key mode.
var escapeKeys = map[string]Key{
	"\x1b[A": {Code: uinput.KeyUp}, "\x1bOA": {Code: uinput.KeyUp},
	"\x1b[B": {Code: uinput.KeyDown}, "\x1bOB": {Code: uinput.KeyDown},
	"\x1b[C": {Code: uinput.KeyRight}, "\x1bOC": {Code: uinput.KeyRight},
	"\x1b[D": {Code: uinput.KeyLeft}, "\x1bOD": {Code: uinput.KeyLeft},
	"\x1b[H": {Code: uinput.KeyHome}, "\x1bOH": {Code: uinput.KeyHome}, "\x1b[1~": {Code: uinput.KeyHome}, "\x1b[7~": {Code: uinput.KeyHome},
	"\x1b[F": {Code: uinput.KeyEnd}, "\x1bOF": {Code: uinput.KeyEnd}, "\x1b[4~": {Code: uinput.KeyEnd}, "\x1b[8~": {Code: uinput.KeyEnd},
	"\x1b[2~": {Code: uinput.KeyInsert},
	"\x1b[3~": {Code: uinput.KeyDelete},
	"\x1b[5~": {Code: uinput.KeyPageup},
	"\x1b[6~": {Code: uinput.KeyPagedown},
	"\x1b[Z":  {Code: uinput.KeyTab, Shift: true},
	"\x1bOP":  {Code: uinput.KeyF1}, "\x1b[11~": {Code: uinput.KeyF1}, "\x1b[[A": {Code: uinput.KeyF1},
	"\x1bOQ": {Code: uinput.KeyF2}, "\x1b[12~": {Code: uinput.KeyF2}, "\x1b[[B": {Code: uinput.KeyF2},
	"\x1bOR": {Code: uinput.KeyF3}, "\x1b[13~": {Code: uinput.KeyF3}, "\x1b[[C": {Code: uinput.KeyF3},
	"\x1bOS": {Code: uinput.KeyF4}, "\x1b[14~": {Code: uinput.KeyF4}, "\x1b[[D": {Code: uinput.KeyF4},
	"\x1b[15~": {Code: uinput.KeyF5}, "\x1b[[E": {Code: uinput.KeyF5},
	"\x1b[17~": {Code: uinput.KeyF6},
	"\x1b[18~": {Code: uinput.KeyF7},
	"\x1b[19~": {Code: uinput.KeyF8},
	"\x1b[20~": {Code: uinput.KeyF9},
	"\x1b[21~": {Code: uinput.KeyF10},
	"\x1b[23~": {Code: uinput.KeyF11},
	"\x1b[24~": {Code: uinput.KeyF12},
}

// csiFinalKeys maps the final byte of a modified CSI sequence
// (ESC [ 1 ; <mod> <final>) to a key.
var csiFinalKeys = map[byte]int{
	'A': uinput.KeyUp, 'B': uinput.KeyDown, 'C': uinput.KeyRight, 'D': uinput.KeyLeft,
	'H': uinput.KeyHome, 'F': uinput.KeyEnd,
	'P': uinput.KeyF1, 'Q': uinput.KeyF2, 'R': uinput.KeyF3, 'S': uinput.KeyF4,
}

// csiTildeKeys maps the numeric parameter of ESC [ <n> ; <mod> ~ to a key.
var csiTildeKeys = map[string]int{
	"1": uinput.KeyHome, "2": uinput.KeyInsert, "3": uinput.KeyDelete, "4": uinput.KeyEnd,
	"5": uinput.KeyPageup, "6": uinput.KeyPagedown, "7": uinput.KeyHome, "8": uinput.KeyEnd,
	"11": uinput.KeyF1, "12": uinput.KeyF2, "13": uinput.KeyF3, "14": uinput.KeyF4,
	"15": uinput.KeyF5, "17": uinput.KeyF6, "18": uinput.KeyF7, "19": uinput.KeyF8,
	"20": uinput.KeyF9, "21": uinput.KeyF10, "23": uinput.KeyF11, "24": uinput.KeyF12,
}

// applyXtermModifier decodes the xterm modifier parameter (1 + bitmask where
// 1 = shift, 2 = alt, 4 = ctrl).
func applyXtermModifier(k Key, mod byte) Key {
	if mod < '1' || mod > '8' {
		return k
	}
	bits := mod - '1'
	k.Shift = k.Shift || bits&1 != 0
	k.Alt = k.Alt || bits&2 != 0
	k.Ctrl = k.Ctrl || bits&4 != 0
	return k
}

// LookupKey translates one complete terminal input sequence into a Key.
// It understands printable ASCII, control characters (ctrl+letter, tab,
// enter, backspace, escape), xterm/VT escape sequences for cursor keys,
// editing keys and F1-F12 with optional ";<mod>" modifiers, and ESC-prefixed
// bytes as alt+key. ok is false for sequences it does not know.
func LookupKey(seq []byte) (k Key, ok bool) {
	if len(seq) == 0 {
		return Key{}, false
	}
	if len(seq) == 1 {
		return lookupSingle(seq[0])
	}
	if k, ok := escapeKeys[string(seq)]; ok {
		return k, true
	}
	if seq[0] != 0x1b {
		return Key{}, false
	}
	// ESC [ 1 ; <mod> <final>   or   ESC [ <n> ; <mod> ~
	if len(seq) >= 6 && seq[1] == '[' {
		final := seq[len(seq)-1]
		body := string(seq[2 : len(seq)-1])
		if i := indexByte(body, ';'); i > 0 && i == len(body)-2 {
			param, mod := body[:i], body[i+1]
			if final == '~' {
				if code, ok := csiTildeKeys[param]; ok {
					return applyXtermModifier(Key{Code: code}, mod), true
				}
			} else if param == "1" {
				if code, ok := csiFinalKeys[final]; ok {
					return applyXtermModifier(Key{Code: code}, mod), true
				}
			}
		}
	}
	// ESC O <mod> <final> (some terminals send SS3 with a modifier digit)
	if len(seq) == 4 && seq[1] == 'O' {
		if code, ok := csiFinalKeys[seq[3]]; ok {
			return applyXtermModifier(Key{Code: code}, seq[2]), true
		}
	}
	// ESC <single> = alt+<single>
	if len(seq) == 2 {
		if k, ok := lookupSingle(seq[1]); ok {
			k.Alt = true
			return k, true
		}
	}
	return Key{}, false
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lookupSingle(b byte) (Key, bool) {
	switch b {
	case 0x1b:
		return Key{Code: uinput.KeyEsc}, true
	case '\t':
		return Key{Code: uinput.KeyTab}, true
	case '\r', '\n':
		return Key{Code: uinput.KeyEnter}, true
	case 0x7f, 0x08:
		return Key{Code: uinput.KeyBackspace}, true
	case 0x00:
		return Key{Code: uinput.KeySpace, Ctrl: true}, true
	case 0x1c:
		return Key{Code: uinput.KeyBackslash, Ctrl: true}, true
	case 0x1d:
		return Key{Code: uinput.KeyRightbrace, Ctrl: true}, true
	case 0x1e:
		return Key{Code: uinput.Key6, Ctrl: true, Shift: true}, true
	case 0x1f:
		return Key{Code: uinput.KeyMinus, Ctrl: true, Shift: true}, true
	}
	if b >= 0x01 && b <= 0x1a {
		k := asciiKeys['a'+b-1]
		k.Ctrl = true
		return k, true
	}
	if k, ok := asciiKeys[b]; ok {
		return k, true
	}
	return Key{}, false
}

// SplitSequences cuts raw terminal bytes into individual key sequences. A
// lone ESC at the end of the buffer is treated as the Escape key, because a
// terminal delivers a whole escape sequence in one read.
func SplitSequences(buf []byte) [][]byte {
	var out [][]byte
	for i := 0; i < len(buf); {
		if buf[i] != 0x1b || i+1 >= len(buf) {
			out = append(out, buf[i:i+1])
			i++
			continue
		}
		j := i + 1
		switch buf[j] {
		case '[':
			// CSI: parameters 0x30-0x3f, intermediates 0x20-0x2f, final 0x40-0x7e.
			// Linux console F1-F5 use "ESC [ [ X"; handle that first.
			j++
			if j < len(buf) && buf[j] == '[' {
				j++
			}
			for j < len(buf) && buf[j] >= 0x20 && buf[j] <= 0x3f {
				j++
			}
			if j < len(buf) {
				j++ // final byte
			}
		case 'O':
			// SS3: optional modifier digit then final byte.
			j++
			for j < len(buf) && buf[j] >= '0' && buf[j] <= '9' {
				j++
			}
			if j < len(buf) {
				j++
			}
		default:
			j++ // alt+<byte>
		}
		if j > len(buf) {
			j = len(buf)
		}
		out = append(out, buf[i:j])
		i = j
	}
	return out
}
