package hook

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/sandbox"
)

var windowsHookBash struct {
	sync.Once
	path string
	err  error
}

var windowsDefaultHookShell struct {
	sync.Once
	shell sandbox.Shell
	err   error
}

// These helpers preserve explicit `sh -c` / `bash -c` hook contracts on
// Windows while allowing the caller to supply the effective configured Bash.
func windowsPOSIXShellArgvInvocationWith(command string, args []string, resolve func() (string, error)) (string, []string, bool, error) {
	if !isBarePOSIXShellWord(command) || !hasCommandStringFlag(args) {
		return "", nil, false, nil
	}
	path, err := resolve()
	if err != nil {
		return "", nil, true, err
	}
	return path, append([]string(nil), args...), true, nil
}

func windowsPOSIXShellInvocationWith(command string, resolve func() (string, error)) (string, []string, bool, error) {
	fields, _, _, ok := parseSimpleHookCommandFields(command)
	if !ok || len(fields) < 3 || !isBarePOSIXShellWord(fields[0]) || !hasCommandStringFlag(fields[1:]) {
		return "", nil, false, nil
	}
	path, err := resolve()
	if err != nil {
		return "", nil, true, err
	}
	return path, append([]string(nil), fields[1:]...), true, nil
}

func isWindowsBatchExecutable(executable string) bool {
	lower := strings.ToLower(executable)
	return strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat")
}

func isPOSIXShellScriptFile(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" || isWindowsBatchExecutable(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	body, readErr := io.ReadAll(io.LimitReader(file, 512))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(body) < 3 || body[0] != '#' || body[1] != '!' {
		return false
	}
	line := strings.TrimSpace(strings.SplitN(string(body[2:]), "\n", 2)[0])
	if line == "" {
		return false
	}
	for field := range strings.FieldsSeq(line) {
		field = strings.Trim(strings.ToLower(field), `"'`)
		field = strings.TrimSuffix(filepath.Base(filepath.ToSlash(field)), ".exe")
		switch field {
		case "sh", "bash", "dash", "zsh", "ksh":
			return true
		}
	}
	return false
}

func isBarePOSIXShellWord(word string) bool {
	word = strings.TrimSpace(word)
	if strings.ContainsAny(word, `/\:`) {
		return false
	}
	word = strings.ToLower(word)
	return word == "sh" || word == "sh.exe" || word == "bash" || word == "bash.exe"
}

func hasCommandStringFlag(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-" || arg == "--" || !strings.HasPrefix(arg, "-") {
			return false
		}
		if after, ok := strings.CutPrefix(arg, "--"); ok {
			name, _, hasInlineValue := strings.Cut(after, "=")
			if !hasInlineValue && bashLongOptionNeedsOperand(name) {
				if i+1 >= len(args) {
					return false
				}
				i++
			}
			continue
		}
		options := strings.TrimPrefix(arg, "-")
		for optionIndex := 0; optionIndex < len(options); optionIndex++ {
			switch options[optionIndex] {
			case 'c':
				return i+1 < len(args)
			case 'o', 'O':
				// -o/-O consume an option name. Any remaining bytes in this
				// argument are that operand, not more single-letter flags.
				if optionIndex+1 == len(options) {
					if i+1 >= len(args) {
						return false
					}
					i++
				}
				optionIndex = len(options)
			}
		}
	}
	return false
}

func bashLongOptionNeedsOperand(name string) bool {
	return name == "init-file" || name == "rcfile"
}

func cachedWindowsHookBash() (string, error) {
	windowsHookBash.Do(func() {
		windowsHookBash.path, windowsHookBash.err = discoverWindowsHookBash("")
	})
	return windowsHookBash.path, windowsHookBash.err
}

func resolveWindowsHookBash(preferredPath string) (string, error) {
	if strings.TrimSpace(preferredPath) == "" {
		return cachedWindowsHookBash()
	}
	return discoverWindowsHookBash(preferredPath)
}

func discoverWindowsHookBash(preferredPath string) (string, error) {
	shell := sandbox.ResolveShell("bash", preferredPath, nil)
	if shell.Kind != sandbox.ShellBash {
		return "", missingWindowsHookBashError()
	}
	path, err := resolvedHookShellPath(shell)
	if err != nil {
		return "", missingWindowsHookBashError()
	}
	return path, nil
}

func cachedWindowsDefaultHookShell() (sandbox.Shell, error) {
	windowsDefaultHookShell.Do(func() {
		sh := sandbox.ResolveShell("", "", nil)
		path, err := resolvedHookShellPath(sh)
		if err != nil {
			windowsDefaultHookShell.err = errors.New("hook requires a shell on Windows, but neither Git Bash nor PowerShell is usable")
			return
		}
		sh.Path = path
		windowsDefaultHookShell.shell = sh
	})
	return windowsDefaultHookShell.shell, windowsDefaultHookShell.err
}

func resolvedHookShellPath(shell sandbox.Shell) (string, error) {
	path := strings.TrimSpace(shell.Path)
	if path == "" {
		path = shell.Kind.String()
	}
	if resolved, err := exec.LookPath(path); err == nil {
		return resolved, nil
	}
	if filepath.IsAbs(path) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("hook shell %q is not executable", path)
}

func missingWindowsHookBashError() error {
	return errors.New("hook requires a POSIX shell on Windows, but no usable Git Bash was found; install Git for Windows or replace the POSIX shell hook with a native portable command")
}

// decodeHookOutput keeps UTF-8-native runtimes such as Node byte-for-byte,
// while recovering legacy Windows cmd.exe output (notably CP936/GB18030) before
// it reaches the desktop renderer. Hook stdout/stderr are text contracts, so a
// final valid-UTF-8 guard is safer than surfacing raw invalid bytes.
func decodeHookOutput(raw []byte, truncated bool) string {
	if len(raw) == 0 {
		return ""
	}
	decoded := raw
	if !utf8.Valid(raw) {
		if prefix, ok := truncatedUTF8Prefix(raw, truncated); ok {
			decoded = prefix
		} else {
			decoded = fileencoding.DecodeToUTF8(raw)
		}
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(decoded), "\uFFFD"))
}

func truncatedUTF8Prefix(raw []byte, truncated bool) ([]byte, bool) {
	if !truncated {
		return nil, false
	}
	for suffixLen := 1; suffixLen < utf8.UTFMax && suffixLen <= len(raw); suffixLen++ {
		prefix := raw[:len(raw)-suffixLen]
		suffix := raw[len(raw)-suffixLen:]
		if utf8.Valid(prefix) && !utf8.FullRune(suffix) {
			return prefix, true
		}
	}
	return nil, false
}
