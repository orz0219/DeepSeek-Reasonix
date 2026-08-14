//go:build windows

package hook

import "strings"

// The batch/cmd command-line builders are only referenced from
// batch_command_windows.go, so they live behind the windows build tag: on
// other platforms nothing compiles them.

// windowsBatchCommandLine wraps a command string (as typed for a .cmd or
// .bat hook whose executable is already quoted) for cmd.exe. Go's default
// encoder follows CommandLineToArgvW, but cmd.exe has different quote rules
// that can escape the quotes into the command name; preserve the argument
// tail byte-for-byte so valid batch syntax is not reinterpreted.
func windowsBatchCommandLine(command string) (string, bool) {
	command = strings.TrimSpace(command)
	if len(command) < 2 || command[0] != '"' {
		return "", false
	}
	closingQuote := strings.IndexByte(command[1:], '"')
	if closingQuote < 0 {
		return "", false
	}
	closingQuote++
	executable := normalizeWindowsBatchExecutable(command[1:closingQuote])
	if !isWindowsBatchExecutable(executable) {
		return "", false
	}
	tail := command[closingQuote+1:]
	if tail != "" && !isShellWhitespace(tail[0]) {
		return "", false
	}
	if !isSimpleWindowsBatchTail(tail) {
		return "", false
	}
	// /s strips the first and last quotes around the /c string, leaving the
	// quoted executable and its untouched argument tail for cmd.exe to parse.
	return `cmd.exe /d /s /c ""` + executable + `"` + tail + `"`, true
}

func windowsBatchArgvCommandLine(command string, args []string) (string, bool) {
	executable := normalizeWindowsBatchExecutable(command)
	if !isWindowsBatchExecutable(executable) || strings.ContainsAny(executable, "\"%!\r\n") {
		return "", false
	}

	var b strings.Builder
	b.WriteString(`cmd.exe /d /s /c ""`)
	b.WriteString(executable)
	b.WriteByte('"')
	for _, arg := range args {
		rendered, ok := renderWindowsBatchArg(arg)
		if !ok {
			return "", false
		}
		b.WriteByte(' ')
		b.WriteString(rendered)
	}
	b.WriteByte('"')
	return b.String(), true
}

// windowsCmdCommandLine wraps a raw shell-form script without tokenizing or
// re-rendering it. cmd.exe owns all quote, variable, pipeline, and chaining
// semantics inside the /c string.
func windowsCmdCommandLine(command string) string {
	return `cmd.exe /d /s /c "` + command + `"`
}

func normalizeWindowsBatchExecutable(executable string) string {
	return strings.ReplaceAll(strings.TrimSpace(executable), "/", `\`)
}

func isSimpleWindowsBatchTail(tail string) bool {
	quoted := false
	for i := range len(tail) {
		switch tail[i] {
		case '\r', '\n':
			return false
		case '"':
			quoted = !quoted
		case '&', '|', ';', '<', '>', '(', ')':
			if !quoted {
				return false
			}
		}
	}
	return !quoted
}

func renderWindowsBatchArg(arg string) (string, bool) {
	// cmd.exe expands percent variables even inside quotes, and delayed
	// expansion can do the same for exclamation marks. Keep argv-form support
	// deliberately narrow instead of silently changing a literal argument.
	if strings.ContainsAny(arg, "\"%!\r\n") {
		return "", false
	}
	if arg == "" || strings.ContainsAny(arg, " \t&|;<>()^[]{}=' +,`~") {
		return `"` + arg + `"`, true
	}
	return arg, true
}
