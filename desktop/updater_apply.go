package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"reasonix/internal/installlayout"
	"reasonix/internal/repair"
)

func extractLinuxReleaseUnit(targz []byte) (map[string][]byte, error) {
	const (
		desktop = "reasonix-desktop"
		guard   = "reasonix-guard"
		cli     = "reasonix"
	)
	want := map[string]struct{}{desktop: {}, guard: {}, cli: {}}
	found := make(map[string][]byte, len(want))
	gz, err := gzip.NewReader(bytes.NewReader(targz))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := path.Base(strings.TrimSpace(h.Name))
		if _, ok := want[name]; !ok {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size < 0 {
			return nil, fmt.Errorf("update: release member %q is not a regular file", name)
		}
		if _, duplicate := found[name]; duplicate {
			return nil, fmt.Errorf("update: release member %q appears more than once", name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		found[name] = body
	}
	if len(found) != len(want) {
		for name := range want {
			if _, ok := found[name]; !ok {
				return nil, fmt.Errorf("update: release member %q not found in archive", name)
			}
		}
	}
	return found, nil
}

// applyLinux replaces the running binary with the one inside the downloaded
// tar.gz; the caller relaunches afterwards.
func applyLinux(targz []byte, prepared *repair.UpdateTransaction) error {
	release, err := extractLinuxReleaseUnit(targz)
	if err != nil {
		return err
	}
	bin := release["reasonix-desktop"]
	guard := release["reasonix-guard"]
	cli := release["reasonix"]
	exe := currentExecutablePathForLinux()
	if exe == "" {
		return fmt.Errorf("update: current executable path is unavailable")
	}
	releasePaths := releaseUnitPathsFor(filepath.Dir(exe), "linux")
	if prepared == nil {
		return fmt.Errorf("update: prepared transaction is unavailable")
	}
	claimed, releaseClaim, err := repair.ClaimPendingFileUpdateExact(
		prepared.ToVersion,
		prepared.CreatedAt,
		repair.UpdateTransactionID(prepared),
		exe,
		releasePaths,
		2*time.Minute,
	)
	if err != nil {
		return fmt.Errorf("update: claim prepared transaction: %w", err)
	}
	defer releaseClaim()
	if err := repair.MarkUpdateApplyFailedExact(claimed, "Linux update publish did not complete"); err != nil {
		return fmt.Errorf("update: record recovery intent: %w", err)
	}
	receipts, err := applyLinuxReleaseUnit(claimed, exe, bin, guard, cli)
	if err != nil {
		return err
	}
	if _, err := repair.RecordClaimedFileUpdateInstalled(claimed, receipts...); err != nil {
		return fmt.Errorf("update: record installed release unit: %w", err)
	}

	_ = repair.ClearUpdateApplyFailureExact(claimed)
	return nil
}

// applyLinuxVersioned publishes a verified compatibility tarball into a new
// version directory and swaps current.json last. The tar still contains the
// one-shot reasonix-guard member for v1.18-v1.19 updaters, but v1.20+ ignores
// that member and never persists it again.
func applyLinuxVersioned(targz []byte, targetVersion string) error {
	release, err := extractLinuxReleaseUnit(targz)
	if err != nil {
		return err
	}
	root := currentInstallDirForLinuxUpdate()
	if _, err := installlayout.ReadCurrent(root); err != nil {
		return fmt.Errorf("update: resolve active Linux layout: %w", err)
	}
	targetVersion = strings.TrimSpace(targetVersion)
	if !strings.HasPrefix(targetVersion, "v") {
		targetVersion = "v" + targetVersion
	}
	if err := installlayout.ValidateVersionName(targetVersion); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".reasonix-linux-update-*")
	if err != nil {
		return fmt.Errorf("update: create Linux version staging: %w", err)
	}
	defer os.RemoveAll(staging)
	desktopPath := filepath.Join(staging, installlayout.DesktopBinaryName())
	cliPath := filepath.Join(staging, installlayout.CLIBinaryName())
	if err := os.WriteFile(desktopPath, release["reasonix-desktop"], 0o700); err != nil {
		return fmt.Errorf("update: stage Linux desktop: %w", err)
	}
	if err := os.WriteFile(cliPath, release["reasonix"], 0o700); err != nil {
		return fmt.Errorf("update: stage Linux CLI: %w", err)
	}
	if err := installlayout.ActivateVersion(installlayout.ActivationRequest{
		InstallRoot: root,
		Version:     targetVersion,
		RequestID:   "linux-" + targetVersion,
		Members: []installlayout.Member{
			{Name: installlayout.DesktopBinaryName(), Path: desktopPath, Mode: 0o700},
			{Name: installlayout.CLIBinaryName(), Path: cliPath, Mode: 0o700},
		},
		RequiredNames: []string{installlayout.DesktopBinaryName(), installlayout.CLIBinaryName()},
	}); err != nil {
		return fmt.Errorf("update: activate Linux version: %w", err)
	}
	_ = installlayout.RetainPreviousVersions(root, 0)
	return nil
}

var currentExecutablePathForLinux = currentExecutablePath
var currentInstallDirForLinuxUpdate = currentInstallDir

var applyLinuxReleaseUnit = func(
	claimed *repair.UpdateTransaction,
	exe string,
	bin, guard, cli []byte,
) ([]repair.FileUpdateInstallReceipt, error) {
	receipts := make([]repair.FileUpdateInstallReceipt, 0, 3)
	receipt, err := repair.PublishClaimedFileUpdateMemberExact(claimed, filepath.Join(filepath.Dir(exe), "reasonix"), cli, 0o700)
	if err != nil {
		return receipts, fmt.Errorf("update CLI sidecar: %w", err)
	}
	receipts = append(receipts, receipt)
	receipt, err = repair.PublishClaimedFileUpdateMemberExact(claimed, filepath.Join(filepath.Dir(exe), "reasonix-guard"), guard, 0o700)
	if err != nil {
		return receipts, fmt.Errorf("update Guard: %w", err)
	}
	receipts = append(receipts, receipt)
	receipt, err = repair.PublishClaimedFileUpdateMemberExact(claimed, exe, bin, 0o700)
	if err != nil {
		return receipts, fmt.Errorf("update desktop: %w", err)
	}
	receipts = append(receipts, receipt)
	return receipts, nil
}

func applyWindowsFile(path, expectedSHA256, targetVersion string, prepared *repair.UpdateTransaction) error {
	installDir := currentInstallDir()
	if installlayout.HasCurrent(installDir) {
		return startWindowsVersionedUpdateHandoff(
			path,
			expectedSHA256,
			installDir,
			currentLauncherPath(),
			targetVersion,
		)
	}
	if prepared == nil {
		return fmt.Errorf("update: prepared transaction is unavailable")
	}
	return startWindowsUpdateHandoff(
		path,
		expectedSHA256,
		installDir,
		currentLauncherPath(),
		prepared,
	)
}

func currentExecutablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

// currentInstallDir is the InstallRoot for updates. For the versioned layout it
// is the directory that owns current.json (not versions/<ver>/). For flat
// installs it is the directory of the running executable.
func currentInstallDir() string {
	exe := currentExecutablePath()
	if exe == "" {
		return ""
	}
	if root, err := installlayout.ResolveInstallRoot(exe); err == nil && root != "" {
		return root
	}
	return filepath.Dir(exe)
}

// archiveSupersededPendingUpdateAfterReady retires a transaction only after the
// current desktop has shown a usable UI. App-bundle recovery handles interrupted
// macOS generations; the versioned-layout branch handles older flat Windows and
// Linux transactions.
func archiveSupersededPendingUpdateAfterReady() (bool, error) {
	exe := currentExecutablePath()
	if exe == "" || version == "" || version == "dev" {
		return false, nil
	}
	if archived, err := repair.ArchiveSupersededPendingAppBundleUpdate(version); err != nil || archived {
		return archived, err
	}
	if runtime.GOOS == "darwin" {
		return false, nil
	}
	root, err := installlayout.ResolveInstallRoot(exe)
	if err != nil {
		return false, err
	}
	ptr, err := installlayout.ReadCurrent(root)
	if err != nil {

		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	running := strings.TrimSpace(version)
	if !strings.HasPrefix(running, "v") {
		running = "v" + running
	}
	if ptr.ActiveVersion != running {
		return false, fmt.Errorf("active install version %s does not match running version %s", ptr.ActiveVersion, running)
	}
	return repair.ArchiveSupersededPendingFileUpdate(running, root)
}

func capturePendingUpdateHealthIdentity(app *App) {
	if app == nil {
		return
	}
	tx, err := readPendingUpdateForHealth()
	if err != nil || tx == nil || !repair.UpdateVersionsEqual(tx.ToVersion, version) {
		return
	}
	app.healthyUpdateCreatedAt = tx.CreatedAt
	app.healthyUpdateTransactionID = repair.UpdateTransactionID(tx)
}

// refreshPendingUpdateHealthIdentity re-reads the current probationary
// transaction so a user-initiated update can commit health even when the
// process started without a matching identity (for example a historical
// version-prefix mismatch).
func refreshPendingUpdateHealthIdentity(app *App) {
	capturePendingUpdateHealthIdentity(app)
}

// updateSiblingArtifacts lists the packaged binaries an update replaces beside
// the main executable, so PrepareFileUpdate can snapshot the complete release
// unit. Paths that do not exist on disk are skipped by the backup.
func updateSiblingArtifacts() []string {
	dir := currentInstallDir()
	if dir == "" {
		return nil
	}
	paths := releaseUnitPathsFor(dir, runtime.GOOS)
	if len(paths) <= 1 {
		return nil
	}
	return paths[1:]
}

func releaseUnitPathsFor(dir, goos string) []string {
	if dir == "" {
		return nil
	}

	if goos == "windows" && installlayout.HasCurrent(dir) {
		paths := make([]string, 0, 6)
		if desktop, err := installlayout.ActiveDesktopPath(dir); err == nil {
			paths = append(paths, desktop)
		} else {
			paths = append(paths, filepath.Join(dir, "reasonix-desktop.exe"))
		}
		if helper, err := installlayout.ActiveUpdateHelperPath(dir); err == nil {
			paths = append(paths, helper)
		}
		if cli, err := installlayout.ActiveCLIPath(dir); err == nil {
			paths = append(paths, cli)
		}
		for _, name := range []string{"reasonix-launcher.exe", "reasonix-cli.exe", "Reasonix.exe"} {
			paths = append(paths, filepath.Join(dir, name))
		}
		return paths
	}
	names := updateSiblingNames(goos)
	paths := make([]string, 0, len(names)+1)
	switch goos {
	case "linux":
		paths = append(paths, filepath.Join(dir, "reasonix-desktop"))
	case "windows":
		paths = append(paths, filepath.Join(dir, "reasonix-desktop.exe"))
	}
	if len(names) == 0 {
		return paths
	}
	for _, name := range names {
		paths = append(paths, filepath.Join(dir, name))
	}
	return paths
}

func updateSiblingNames(goos string) []string {
	switch goos {
	case "windows":

		return []string{"reasonix-guard.exe", "reasonix-launcher.exe", "reasonix-update-helper.exe", "reasonix-cli.exe", "Reasonix.exe"}
	case "linux":
		return []string{"reasonix-guard", "reasonix"}
	default:
		return nil
	}
}

// relaunchThroughLauncher starts the permanent thin launcher (or falls back to
// the running executable). A legacy Guard binary is considered only as a
// one-release migration fallback for flat 1.18-1.19.1 installations.
func relaunchThroughLauncher() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(exe)
	if resolved, err := installlayout.ResolveInstallRoot(exe); err == nil && resolved != "" {
		root = resolved
	}
	candidates := []string{
		filepath.Join(root, "reasonix-launcher"),
		filepath.Join(root, "Reasonix.exe"),
		filepath.Join(root, "reasonix-guard"),
	}
	if runtime.GOOS == "windows" {
		candidates[0] += ".exe"
		candidates[2] += ".exe"
	}
	launcher := exe
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			launcher = path
			break
		}
	}
	args := []string{}

	if strings.Contains(strings.ToLower(filepath.Base(launcher)), "guard") {
		args = []string{"launch", "--detach"}
	}
	cmd := exec.Command(launcher, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Start()
}

func currentLauncherPath() string {
	exe := currentExecutablePath()
	if exe == "" {
		return ""
	}
	root := filepath.Dir(exe)
	if resolved, err := installlayout.ResolveInstallRoot(exe); err == nil && resolved != "" {
		root = resolved
	}
	for _, name := range []string{"reasonix-launcher.exe", "Reasonix.exe", "reasonix-launcher", "reasonix-guard.exe", "reasonix-guard"} {
		if runtime.GOOS != "windows" && strings.HasSuffix(name, ".exe") {
			continue
		}
		if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") && name != "Reasonix.exe" {

			if !strings.HasSuffix(name, ".exe") {
				continue
			}
		}
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	if runtime.GOOS == "windows" {
		guard := filepath.Join(filepath.Dir(exe), "reasonix-guard.exe")
		if _, err := os.Stat(guard); err == nil {
			return guard
		}
	}
	return exe
}
