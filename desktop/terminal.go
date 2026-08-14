package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"reasonix/internal/config"
)

const (
	terminalOutputChannel    = "terminal:output"
	terminalExitChannel      = "terminal:exit"
	maxTerminalsPerWorkspace = 10
	terminalCloseWait        = 2 * time.Second
	defaultTerminalColumns   = 80
	defaultTerminalRows      = 24
	maxTerminalColumns       = 1000
	maxTerminalRows          = 500
	maxTerminalSnapshotBytes = 128 * 1024
)

var (
	errTerminalStaleTab   = errors.New("terminal request is no longer for the active tab")
	errTerminalOutside    = errors.New("terminal directory is outside the workspace")
	errTerminalManagerOff = errors.New("terminal manager is not available")
)

// TerminalSessionView is the renderer-safe snapshot of an interactive shell.
type TerminalSessionView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Shell     string `json:"shell"`
	Cwd       string `json:"cwd"`
	CreatedAt int64  `json:"createdAt"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Running   bool   `json:"running"`
}

// TerminalShellView is a backend-approved shell choice. The renderer sends the
// stable ID back; it never sends an executable path.
type TerminalShellView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// TerminalWorkspaceView describes terminal capability for the active tab. All
// slices are initialized so Wails serializes empty values as [] rather than null.
type TerminalWorkspaceView struct {
	Available bool                  `json:"available"`
	ReadOnly  bool                  `json:"readOnly"`
	Reason    string                `json:"reason,omitempty"`
	Sessions  []TerminalSessionView `json:"sessions"`
	Shells    []TerminalShellView   `json:"shells"`
}

type terminalTarget struct {
	tabID         string
	workspaceRoot string
	workspaceKey  string
	readOnly      bool
}

type terminalCommand struct {
	path  string
	args  []string
	label string
}

type terminalStartSpec struct {
	command terminalCommand
	dir     string
	env     []string
	cols    int
	rows    int
}

type terminalProcess interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Wait() (int, error)
}

type terminalSession struct {
	view         TerminalSessionView
	tabID        string
	workspaceKey string
	process      terminalProcess
	readDone     chan struct{}
	done         chan struct{}
	output       []byte
}

type terminalManager struct {
	app *App

	mu            sync.Mutex
	sessions      map[string]*terminalSession
	byWorkspace   map[string][]string
	starting      map[string]int
	tabGeneration map[string]uint64
	closedTabIDs  map[string]struct{}
	closed        bool
	start         func(terminalStartSpec) (terminalProcess, error)
}

func newTerminalManager(app *App) *terminalManager {
	return &terminalManager{
		app:           app,
		sessions:      make(map[string]*terminalSession),
		byWorkspace:   make(map[string][]string),
		starting:      make(map[string]int),
		tabGeneration: make(map[string]uint64),
		closedTabIDs:  make(map[string]struct{}),
		start:         startTerminalProcess,
	}
}

func emptyTerminalWorkspaceView() TerminalWorkspaceView {
	return TerminalWorkspaceView{
		Sessions: []TerminalSessionView{},
		Shells:   []TerminalShellView{},
	}
}

// TerminalWorkspaceForTab returns the terminal state for the currently active
// tab. The backend owns workspace resolution; renderer-supplied filesystem roots
// are never accepted.
func (a *App) TerminalWorkspaceForTab(tabID string) (TerminalWorkspaceView, error) {
	view := emptyTerminalWorkspaceView()
	target, err := a.terminalTargetForTab(tabID, false)
	if err != nil {
		return view, err
	}
	view.ReadOnly = target.readOnly
	available, reason := terminalPlatformAvailable()
	view.Available = available
	view.Reason = reason
	view.Shells = terminalShellOptions()
	if a.terminals != nil {
		view.Sessions = a.terminals.list(target.workspaceKey)
	}
	return view, nil
}

// TerminalOutputForTab returns a bounded snapshot of the selected session's
// output. It is an explicit user action for adding terminal context to chat;
// terminal output is never injected into provider prompts automatically.
func (a *App) TerminalOutputForTab(tabID, sessionID string) (string, error) {
	target, err := a.terminalTargetForTab(tabID, false)
	if err != nil {
		return "", err
	}
	if a.terminals == nil {
		return "", errTerminalManagerOff
	}
	return a.terminals.snapshot(target.workspaceKey, sessionID), nil
}

// CreateTerminalForTab starts an interactive shell at a workspace-relative
// file or directory. Files resolve to their parent directory after an os.Stat;
// symlinked directories are checked against the canonical workspace root.
func (a *App) CreateTerminalForTab(tabID, rel, shellID string) (TerminalSessionView, error) {
	target, err := a.terminalTargetForTab(tabID, true)
	if err != nil {
		return TerminalSessionView{}, err
	}
	available, reason := terminalPlatformAvailable()
	if !available {
		return TerminalSessionView{}, errors.New(reason)
	}
	dir, err := resolveTerminalStartDir(target.workspaceRoot, rel)
	if err != nil {
		return TerminalSessionView{}, err
	}
	command, err := resolveTerminalCommand(target.workspaceRoot, shellID)
	if err != nil {
		return TerminalSessionView{}, err
	}
	if err := a.revalidateTerminalTarget(target, true); err != nil {
		return TerminalSessionView{}, err
	}
	if a.terminals == nil {
		return TerminalSessionView{}, errTerminalManagerOff
	}
	return a.terminals.create(target.tabID, target.workspaceKey, dir, command)
}

func (a *App) WriteTerminalForTab(tabID, sessionID, data string) error {
	target, err := a.terminalTargetForTab(tabID, true)
	if err != nil {
		return err
	}
	if a.terminals == nil {
		return errTerminalManagerOff
	}
	return a.terminals.write(target.workspaceKey, sessionID, []byte(data))
}

func (a *App) ResizeTerminalForTab(tabID, sessionID string, cols, rows int) error {
	target, err := a.terminalTargetForTab(tabID, true)
	if err != nil {
		return err
	}
	if a.terminals == nil {
		return errTerminalManagerOff
	}
	return a.terminals.resize(target.workspaceKey, sessionID, cols, rows)
}

func (a *App) CloseTerminalForTab(tabID, sessionID string) error {
	target, err := a.terminalTargetForTab(tabID, true)
	if err != nil {
		return err
	}
	if a.terminals == nil {
		return errTerminalManagerOff
	}
	return a.terminals.closeTerminal(target.workspaceKey, sessionID)
}

func (a *App) RenameTerminalForTab(tabID, sessionID, title string) error {
	target, err := a.terminalTargetForTab(tabID, true)
	if err != nil {
		return err
	}
	if a.terminals == nil {
		return errTerminalManagerOff
	}
	return a.terminals.rename(target.workspaceKey, sessionID, title)
}

func (a *App) terminalTargetForTab(tabID string, requireWritable bool) (terminalTarget, error) {
	tabID = strings.TrimSpace(tabID)
	a.mu.RLock()
	activeID := a.activeTabID
	if tabID == "" {
		tabID = activeID
	}
	if tabID == "" || tabID != activeID {
		a.mu.RUnlock()
		return terminalTarget{}, errTerminalStaleTab
	}
	tab := a.tabByIDLocked(tabID)
	if tab == nil {
		a.mu.RUnlock()
		return terminalTarget{}, errTerminalStaleTab
	}
	root := tab.WorkspaceRoot
	readOnly := tab.ReadOnly
	a.mu.RUnlock()

	if requireWritable && readOnly {
		return terminalTarget{}, readOnlyChannelErr()
	}
	base, err := workspaceBaseFromRoot(root)
	if err != nil {
		return terminalTarget{}, err
	}
	base, err = canonicalDirectory(base)
	if err != nil {
		return terminalTarget{}, fmt.Errorf("resolve terminal workspace: %w", err)
	}
	return terminalTarget{
		tabID:         tabID,
		workspaceRoot: base,
		workspaceKey:  tabID + "\x00" + filepath.Clean(base),
		readOnly:      readOnly,
	}, nil
}

func (a *App) revalidateTerminalTarget(target terminalTarget, requireWritable bool) error {
	a.mu.RLock()
	tab := a.tabByIDLocked(target.tabID)
	valid := tab != nil && a.activeTabID == target.tabID
	readOnly := valid && tab.ReadOnly
	root := ""
	if valid {
		root = tab.WorkspaceRoot
	}
	a.mu.RUnlock()
	if !valid {
		return errTerminalStaleTab
	}
	if requireWritable && readOnly {
		return readOnlyChannelErr()
	}
	base, err := workspaceBaseFromRoot(root)
	if err != nil {
		return err
	}
	base, err = canonicalDirectory(base)
	if err != nil || filepath.Clean(base) != target.workspaceRoot {
		return errTerminalStaleTab
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", real)
	}
	return filepath.Clean(real), nil
}

func resolveTerminalStartDir(workspaceRoot, rel string) (string, error) {
	workspaceRoot, err := canonicalDirectory(workspaceRoot)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", errTerminalOutside
	}
	target, ok, err := workspacePathForBase(workspaceRoot, rel)
	if err != nil || !ok {
		return "", errTerminalOutside
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("resolve terminal directory: %w", err)
	}
	if !info.IsDir() {
		target = filepath.Dir(target)
	}
	target, err = canonicalDirectory(target)
	if err != nil {
		return "", err
	}
	relToRoot, err := filepath.Rel(workspaceRoot, target)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(os.PathSeparator)) {
		return "", errTerminalOutside
	}
	return target, nil
}

func terminalShellOptions() []TerminalShellView {
	options := []TerminalShellView{{ID: "default", Label: "Default shell"}}
	seen := map[string]bool{"default": true}
	add := func(id, label, binary string) {
		if seen[id] {
			return
		}
		if _, err := exec.LookPath(binary); err == nil {
			seen[id] = true
			options = append(options, TerminalShellView{ID: id, Label: label})
		}
	}
	if runtime.GOOS == "windows" {
		add("powershell", "PowerShell", "pwsh.exe")
		add("windows-powershell", "Windows PowerShell", "powershell.exe")
		add("cmd", "Command Prompt", "cmd.exe")
		add("bash", "Bash", "bash.exe")
		return options
	}
	add("zsh", "zsh", "zsh")
	add("bash", "bash", "bash")
	add("fish", "fish", "fish")
	add("sh", "sh", "sh")
	return options
}

func resolveTerminalCommand(_ string, shellID string) (terminalCommand, error) {
	shellID = strings.ToLower(strings.TrimSpace(shellID))
	if shellID == "" || shellID == "auto" {
		shellID = "default"
	}
	if shellID == "default" {
		if cfg, err := config.LoadUserConfigReadOnly(); err == nil {
			if command, ok := terminalCommandFromConfig(cfg.Tools.Shell.Prefer, cfg.Tools.Shell.Path); ok {
				return command, nil
			}
		}
		return defaultTerminalCommand()
	}
	return namedTerminalCommand(shellID)
}

func terminalCommandFromConfig(prefer, configuredPath string) (terminalCommand, bool) {
	prefer = strings.ToLower(strings.TrimSpace(prefer))
	configuredPath = strings.TrimSpace(configuredPath)
	if prefer == "" || prefer == "auto" {
		return terminalCommand{}, false
	}
	if prefer != "bash" && prefer != "powershell" && prefer != "pwsh" {
		return terminalCommand{}, false
	}
	if configuredPath != "" {
		if path, err := exec.LookPath(configuredPath); err == nil {
			label := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			return commandForShellPath(path, label), true
		}
	}
	command, err := namedTerminalCommand(prefer)
	return command, err == nil
}

func defaultTerminalCommand() (terminalCommand, error) {
	if runtime.GOOS != "windows" {
		if path := strings.TrimSpace(os.Getenv("SHELL")); path != "" {
			if resolved, err := exec.LookPath(path); err == nil {
				return commandForShellPath(resolved, filepath.Base(resolved)), nil
			}
		}
		for _, id := range []string{"zsh", "bash", "fish", "sh"} {
			if command, err := namedTerminalCommand(id); err == nil {
				return command, nil
			}
		}
		return terminalCommand{}, errors.New("no interactive shell was found")
	}
	for _, id := range []string{"powershell", "windows-powershell", "cmd", "bash"} {
		if command, err := namedTerminalCommand(id); err == nil {
			return command, nil
		}
	}
	return terminalCommand{}, errors.New("no interactive shell was found")
}

func namedTerminalCommand(shellID string) (terminalCommand, error) {
	var binary, label string
	switch shellID {
	case "bash":
		binary, label = "bash", "bash"
	case "zsh":
		binary, label = "zsh", "zsh"
	case "fish":
		binary, label = "fish", "fish"
	case "sh":
		binary, label = "sh", "sh"
	case "powershell", "pwsh":
		binary, label = "pwsh", "PowerShell"
		if runtime.GOOS == "windows" {
			binary = "pwsh.exe"
		}
	case "windows-powershell":
		binary, label = "powershell.exe", "Windows PowerShell"
	case "cmd":
		binary, label = "cmd.exe", "Command Prompt"
	default:
		return terminalCommand{}, fmt.Errorf("unsupported terminal shell %q", shellID)
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return terminalCommand{}, fmt.Errorf("terminal shell %q is not installed", shellID)
	}
	return commandForShellPath(path, label), nil
}

func commandForShellPath(path, label string) terminalCommand {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	args := []string{}
	switch base {
	case "bash", "zsh", "sh", "ksh", "fish":
		args = []string{"-l"}
	case "pwsh", "powershell":
		args = []string{"-NoLogo"}
	case "cmd":
		args = []string{"/Q"}
	}
	return terminalCommand{path: path, args: args, label: label}
}

func terminalEnvironment(base []string) []string {
	env := make([]string, 0, len(base)+2)
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok && (strings.EqualFold(key, "TERM") || strings.EqualFold(key, "COLORTERM")) {
			continue
		}
		env = append(env, item)
	}
	return append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
}

// detachForTab closes the creation gate and removes every registered session
// without waiting on process I/O. Callers can use it while serializing an App
// capability transition, then close the returned processes after releasing
// App.mu.
