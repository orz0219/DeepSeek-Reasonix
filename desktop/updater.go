package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"reasonix/desktop/internal/update"
	"reasonix/internal/config"
	"reasonix/internal/installlayout"
	"reasonix/internal/netclient"
)

// updater.go is the transport-free core of the desktop auto-updater: manifest
// fetch, version comparison, signed download, and per-platform apply/relaunch. It
// has no Wails dependency so the logic is unit-tested directly; updater_app.go is
// the thin Wails binding that wires these into App methods and progress events.

// Manifest endpoints — R2 CDN first (fast, especially in CN), then the crash
// worker release gateway, then GitHub as the stable channel's last resort. The
// selected update channel picks the rolling pointer; it is user-configurable and
// independent from the build channel embedded for diagnostics/backcompat. The
// gateway still avoids GitHub's repository-wide /releases/latest shortcut so the
// app is not coupled to GitHub's homepage badge semantics.
const (
	r2Base                     = "https://dl.reasonix.io"
	releaseGatewayBase         = "https://crash.reasonix.io/v1/desktop/releases"
	downloadPageURL            = "https://reasonix.io/#start"
	manifestDownloadPageURL    = "https://reasonix.io/?download=desktop#start"
	httpTimeout                = 15 * time.Second
	manifestEndpointTimeout    = 5 * time.Second
	maxDesktopReleaseAssetSize = int64(1 << 30)
	maxDesktopManifestSize     = int64(1 << 20)
	maxDesktopSignatureSize    = int64(64 << 10)
)

var fetchAttemptTimeout = 5 * time.Second

var (
	stableDesktopVersionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	sha256RE               = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type requiredDesktopAsset struct {
	group    string
	key      string
	filename string
}

var (
	requiredDesktopUpdaterAssets = []requiredDesktopAsset{
		{group: "platforms", key: "darwin-arm64", filename: "Reasonix-darwin-arm64.zip"},
		{group: "platforms", key: "darwin-amd64", filename: "Reasonix-darwin-amd64.zip"},
		{group: "platforms", key: "windows-amd64", filename: "Reasonix-windows-amd64-installer.exe"},
		{group: "platforms", key: "windows-arm64", filename: "Reasonix-windows-arm64-installer.exe"},
		{group: "platforms", key: "linux-amd64", filename: "Reasonix-linux-amd64.tar.gz"},
		{group: "native_packages", key: "linux-amd64", filename: "Reasonix-linux-amd64.deb"},
	}
	requiredDesktopDownloadAssets = []requiredDesktopAsset{
		{group: "downloads", key: "Reasonix-darwin-universal.dmg", filename: "Reasonix-darwin-universal.dmg"},
		{group: "downloads", key: "Reasonix-windows-amd64.zip", filename: "Reasonix-windows-amd64.zip"},
	}
)

// githubManifestFallback is the stable channel's last-resort manifest source.
// dl.reasonix.io and crash.reasonix.io share one Cloudflare zone, so bot
// protection that 403s a user's egress IP takes out both first-party endpoints
// at once (#6005); GitHub is separate infrastructure. Stable desktop releases
// own the repo-wide latest badge and publish latest.json directly, while
// The unified official Release carries the desktop manifest as a final fallback
// when both first-party endpoints are unavailable.
const githubManifestFallback = "https://github.com/esengine/DeepSeek-Reasonix/releases/latest/download/latest.json"

func normalizeUpdateChannel(ch string) string {
	return config.NormalizeDesktopUpdateChannel(ch)
}

func configuredUpdateChannel() string {
	cfg, err := config.Load()
	if err != nil {
		return "stable"
	}
	return cfg.DesktopUpdateChannel()
}

func targetUpdateChannel(selected string) string {
	_ = selected
	return configuredUpdateChannel()
}

func runningUpdateChannel() string {
	return normalizeUpdateChannel(channel)
}

// manifestEndpoints returns the manifest URLs for the selected update channel,
// in the order fetchManifest tries them.
func manifestEndpoints(selected string) []string {
	_ = selected
	return []string{
		r2Base + "/latest/latest.json",
		releaseGatewayBase + "/stable/latest.json",
		githubManifestFallback,
	}
}

// updaterUserAgent identifies updater traffic. Go's default Go-http-client UA
// is exactly what edge bot protection scores worst (#6005); a descriptive UA
// lets the release edge allowlist updater requests and makes them attributable
// in server logs.
func updaterUserAgent(selected string) string {
	return fmt.Sprintf("Reasonix-Updater/%s (%s/%s; build=%s; update=%s)", version, runtime.GOOS, runtime.GOARCH, channel, normalizeUpdateChannel(selected))
}

// downloadPage is the human-facing releases page shown when self-update is
// unavailable (macOS) or the manifest omits its own link.
func downloadPage(selected string) string {
	_ = selected
	u, _ := url.Parse(downloadPageURL)
	query := u.Query()
	query.Set("download", "desktop")
	query.Del("channel")
	u.RawQuery = query.Encode()
	return u.String()
}

func manifestDownloadPage(selected, manifestPage string) string {
	manifestPage = strings.TrimSpace(manifestPage)
	if manifestPage == "" {
		return downloadPage(selected)
	}
	u, err := url.Parse(manifestPage)
	if err != nil ||
		u.Scheme != "https" ||
		u.Hostname() == "" ||
		u.User != nil {
		return downloadPage(selected)
	}
	host := strings.ToLower(u.Hostname())
	if host != "reasonix.io" && !strings.HasSuffix(host, ".reasonix.io") {
		return u.String()
	}
	query := u.Query()
	query.Set("download", "desktop")
	query.Del("channel")
	u.RawQuery = query.Encode()
	u.Fragment = "start"
	return u.String()
}

// UpdateInfo is the CheckUpdate result that drives the frontend's update banner.
type UpdateInfo struct {
	Available         bool   `json:"available"`
	Current           string `json:"current"`
	Latest            string `json:"latest"`
	Notes             string `json:"notes"`
	Channel           string `json:"channel"`
	CanSelfUpdate     bool   `json:"canSelfUpdate"` // win/linux true; macOS true only for signed/notarized builds
	ManualOnly        bool   `json:"manualOnly,omitempty"`
	ManualReason      string `json:"manualReason,omitempty"`
	InstallMode       string `json:"installMode"`                 // portable | deb | manual
	RequiresElevation bool   `json:"requiresElevation,omitempty"` // deb/Polkit path
	Downloaded        bool   `json:"downloaded"`
	DownloadURL       string `json:"downloadUrl"`   // human-facing releases page (macOS path / fallback link)
	AssetSize         int64  `json:"assetSize"`     // running platform's artifact size, for the progress bar
	Err               string `json:"err,omitempty"` // set when the check itself failed (both endpoints down)
}

// UpdateDownloadResult is returned after an artifact has been downloaded,
// verified, and stored in the local updater cache.
type UpdateDownloadResult struct {
	RequestID string `json:"requestId"`
	Version   string `json:"version"`
	Channel   string `json:"channel"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// updateProgress is the payload of the "updater:progress" Wails event emitted
// throughout DownloadUpdate / InstallUpdate.
type updateProgress struct {
	RequestID string `json:"requestId"`
	Version   string `json:"version"`
	Channel   string `json:"channel"`
	Phase     string `json:"phase"` // downloading | verifying | downloaded | authorizing | recovering | installing | done | error
	Received  int64  `json:"received"`
	Total     int64  `json:"total"`
	Err       string `json:"err,omitempty"`
}

func httpClient() (*http.Client, error) { return newHTTPClient(false) }

// httpClientIPv4 pins the dialer to IPv4 — the download fallback when the default
// (often IPv6-first) route to Cloudflare keeps resetting mid-transfer.
func httpClientIPv4() (*http.Client, error) { return newHTTPClient(true) }

func newHTTPClient(forceIPv4 bool) (*http.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	c, err := netclient.NewHTTPClient(cfg.NetworkProxySpec(), netclient.TransportOptions{ForceIPv4: forceIPv4})
	if err != nil {
		return nil, err
	}
	c.CheckRedirect = validateUpdateRedirect
	return c, nil
}

func validateUpdateRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("update: stopped after 10 redirects")
	}
	if req == nil || req.URL == nil {
		return errors.New("update: redirect has no target URL")
	}
	if !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("update: refusing redirect to non-HTTPS URL %q", req.URL.String())
	}
	if req.URL.Hostname() == "" {
		return fmt.Errorf("update: refusing redirect without a hostname %q", req.URL.String())
	}
	if req.URL.User != nil {
		return fmt.Errorf("update: refusing redirect with userinfo %q", req.URL.String())
	}
	if req.URL.Port() != "" || !isTrustedUpdateRedirectHost(req.URL.Hostname()) {
		return fmt.Errorf("update: refusing redirect to untrusted host %q", req.URL.Host)
	}
	return nil
}

func isTrustedUpdateRedirectHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return host == "reasonix.io" ||
		strings.HasSuffix(host, ".reasonix.io") ||
		host == "github.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

// canSelfUpdate reports whether in-place update is possible. Windows and Linux
// can replace the verified artifact directly; macOS requires an explicitly
// signed/notarized build flag so local or ad-hoc builds stay manual.
func canSelfUpdate() bool {
	return runtime.GOOS != "darwin" || macSelfUpdateAllowed()
}

func manualUpdateReason() string {
	if runtime.GOOS == "darwin" && !macSelfUpdateAllowed() {
		return "macOS automatic updates require a Developer ID signed and notarized build"
	}
	return ""
}

// normalizeVersion canonicalizes a version to semver "vX.Y.Z". It reports ok=false
// for the un-injected "dev" build (and anything not valid semver), so a dev build
// never prompts to update.
func normalizeVersion(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return "", false
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return "", false
	}
	return semver.Canonical(v), true
}

// validateManifestChannel rejects every prerelease. The selected value remains
// in the signature for compatibility with existing callers.
func validateManifestChannel(selected string, m *update.Manifest) error {
	_ = selected
	if !stableDesktopVersionRE.MatchString(m.Version) {
		return fmt.Errorf("official manifest has invalid release version %q", m.Version)
	}
	return nil
}

func desktopReleaseTag(_ string, version string) string {
	return "desktop-" + version
}

func desktopAssetBases(selected, version string, allowLegacyPreview bool) []string {
	_ = selected
	_ = allowLegacyPreview
	tag := desktopReleaseTag(selected, version)
	return []string{
		fmt.Sprintf("%s/%s/", r2Base, tag),
		fmt.Sprintf("https://github.com/esengine/DeepSeek-Reasonix/releases/download/%s/", tag),
		fmt.Sprintf("https://github.com/esengine/DeepSeek-Reasonix/releases/download/%s/", version),
	}
}

func validateManifestAsset(selected, version, filename string, asset update.Asset, allowLegacyPreview bool) (string, error) {
	base := ""
	for _, candidate := range desktopAssetBases(selected, version, allowLegacyPreview) {
		if asset.URL == candidate+filename {
			base = candidate
			break
		}
	}
	if base == "" {
		return "", fmt.Errorf("asset URL %q is not the official %s path for %s", asset.URL, normalizeUpdateChannel(selected), filename)
	}
	if asset.Sig != asset.URL+".minisig" {
		return "", fmt.Errorf("asset signature URL %q does not match %q", asset.Sig, asset.URL+".minisig")
	}
	if asset.Size <= 0 || asset.Size > maxDesktopReleaseAssetSize {
		return "", fmt.Errorf("asset %s has invalid size %d", filename, asset.Size)
	}
	if !sha256RE.MatchString(asset.SHA256) {
		return "", fmt.Errorf("asset %s has invalid SHA-256 %q", filename, asset.SHA256)
	}
	if err := validateAssetInstallLayout(asset.InstallLayout); err != nil {
		return "", err
	}
	return base, nil
}

// validateAssetInstallLayout accepts the pre-v1.20 empty layout (flat install)
// and the v1.20+ versioned-v1 layout. Unknown values must fail closed so a new
// client never partially installs an unrecognized package shape.
func validateAssetInstallLayout(layout string) error {
	switch strings.TrimSpace(layout) {
	case "", installlayout.InstallLayoutVersionedV1:
		return nil
	default:
		return fmt.Errorf("unsupported install_layout %q (keeping current version)", layout)
	}
}

func validateDesktopManifest(selected string, m *update.Manifest) error {
	selected = normalizeUpdateChannel(selected)
	if err := validateManifestChannel(selected, m); err != nil {
		return err
	}
	if m.DownloadPage != manifestDownloadPageURL {
		return fmt.Errorf("%s manifest has invalid download page %q", selected, m.DownloadPage)
	}
	// Older public manifests predate the two website-only download assets. Keep
	// accepting their six signed updater artifacts so an upgrade to the first
	// single-channel release does not strand existing users. Once downloads is
	// present it is a new-format manifest and all eight assets are mandatory.
	legacyManifest := m.Downloads == nil
	requiredAssets := append([]requiredDesktopAsset(nil), requiredDesktopUpdaterAssets...)
	if !legacyManifest {
		requiredAssets = append(requiredAssets, requiredDesktopDownloadAssets...)
	}
	base := ""
	for _, required := range requiredAssets {
		var assets map[string]update.Asset
		switch required.group {
		case "platforms":
			assets = m.Platforms
		case "native_packages":
			assets = m.NativePackages
		case "downloads":
			assets = m.Downloads
		default:
			return fmt.Errorf("unsupported manifest asset group %q", required.group)
		}
		asset, ok := assets[required.key]
		if !ok {
			return fmt.Errorf("%s manifest has no %s asset for %s", selected, required.group, required.key)
		}
		assetBase, err := validateManifestAsset(selected, m.Version, required.filename, asset, legacyManifest)
		if err != nil {
			return fmt.Errorf("%s %s asset: %w", required.group, required.key, err)
		}
		if base != "" && assetBase != base {
			return fmt.Errorf("%s manifest mixes asset bases %q and %q", selected, base, assetBase)
		}
		base = assetBase
	}
	return nil
}

// fetchManifest pulls latest.json from each endpoint in order until one both
// responds, decodes, and matches an official release. Every endpoint's
// failure is kept — a user staring at a gateway 403 (#6005) needs to see that
// the R2 pointer failed too, not just whichever endpoint happened to die last.
func fetchManifest(ctx context.Context, c, fallback *http.Client, selected string) (*update.Manifest, error) {
	var errs []error
	selected = normalizeUpdateChannel(selected)
	for _, url := range manifestEndpoints(selected) {
		endpointCtx, cancel := context.WithTimeout(ctx, manifestEndpointTimeout)
		b, err := fetchManifestBytes(endpointCtx, c, fallback, selected, url)
		cancel()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var m update.Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		if err := validateDesktopManifest(selected, &m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		return &m, nil
	}
	return nil, fmt.Errorf("update: fetch manifest: %w", errors.Join(errs...))
}

// fetchManifestBytes gives the default and IPv4 transports separate halves of
// the endpoint budget. A stalled IPv6 dial must not consume the whole timeout
// before the IPv4 fallback gets a chance to run (#6713).
func fetchManifestBytes(ctx context.Context, c, fallback *http.Client, selected, url string) ([]byte, error) {
	attemptTimeout := manifestEndpointTimeout / 2
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	data, err := fetchBytesOnce(attemptCtx, c, selected, url, maxDesktopManifestSize)
	cancel()
	if err == nil || !isTransientFetchError(err) || fallback == nil {
		return data, err
	}
	attemptCtx, cancel = context.WithTimeout(ctx, attemptTimeout)
	fallbackData, fallbackErr := fetchBytesOnce(attemptCtx, fallback, selected, url, maxDesktopManifestSize)
	cancel()
	if fallbackErr == nil {
		return fallbackData, nil
	}
	return nil, errors.Join(err, fallbackErr)
}

// evaluateForChannel compares the running version against the selected channel's
// manifest and builds the frontend-facing result. I/O is limited to install-profile
// detection and cache probes so tests can inject a fixed profile below.
func evaluateForChannel(current, selected string, m *update.Manifest) UpdateInfo {
	return evaluateWithProfileForChannel(current, selected, m, profileForManifest(detectInstallProfile(), m))
}

func evaluateWithProfile(current string, m *update.Manifest, profile installProfile) UpdateInfo {
	return evaluateWithProfileForChannel(current, runningUpdateChannel(), m, profile)
}

// evaluateWithProfileForChannel is the pure comparison core once the install
// profile and selected update channel are known.
func evaluateWithProfileForChannel(current, selected string, m *update.Manifest, profile installProfile) UpdateInfo {
	selected = normalizeUpdateChannel(selected)
	page := manifestDownloadPage(selected, m.DownloadPage)
	info := UpdateInfo{
		Current:           current,
		Latest:            m.Version,
		Notes:             m.Notes,
		Channel:           selected,
		CanSelfUpdate:     profile.CanSelfUpdate,
		ManualOnly:        !profile.CanSelfUpdate,
		ManualReason:      profile.ManualReason,
		InstallMode:       profile.Mode,
		RequiresElevation: profile.RequiresElev,
		DownloadURL:       page,
	}
	// Preserve the pre-existing macOS gate when profile detection would otherwise
	// claim portable self-update on an unsigned build.
	if runtime.GOOS == "darwin" && !canSelfUpdate() {
		info.CanSelfUpdate = false
		info.ManualOnly = true
		info.RequiresElevation = false
		info.InstallMode = installModeManual
		if info.ManualReason == "" {
			info.ManualReason = manualUpdateReason()
		}
	}
	cur, okCur := normalizeVersion(current)
	latest, okLatest := normalizeVersion(m.Version)
	if !okLatest {
		info.Err = "manifest has no valid version"
		return info
	}
	// A dev/invalid running version never auto-prompts. Within a channel, only a
	// newer semver is an update. Across channels, a different target latest is an
	// explicit channel switch, so allow installing stable over a newer preview.
	if okCur {
		if selected != runningUpdateChannel() {
			info.Available = latest != cur
		} else if semver.Compare(latest, cur) > 0 {
			info.Available = true
		}
	}
	if a, kind, ok := selectUpdateAsset(m, profile); ok {
		info.AssetSize = a.Size
		info.Downloaded = cachedUpdateMatchesForChannel(selected, m.Version, a, kind)
	} else if a, ok := m.Asset(); ok {
		// Manual installs (or a missing native package) still surface the portable
		// artifact size so the UI can show how large the download is on the page.
		info.AssetSize = a.Size
	}
	return info
}

// tarball | deb
// required for deb

// Legacy portable caches omit artifactKind and remain valid for tarball only.
// Deb installs never reuse a cache that lacks a matching signature file.

// downloadAttempts caps how many times a transient transport failure (connection
// reset, read timeout, gateway 5xx) is retried before the update gives up. CN IPv6
// routes to Cloudflare reset mid-transfer often enough that a retry or two usually
// completes the download instead of surfacing a "forcibly closed" error.

// retryBackoff is the pause before the Nth retry; a package var so tests shrink it.

// retryTransient runs attempt 1..downloadAttempts of fetch, pausing between tries,
// until one succeeds. fetch receives the 1-based attempt number so a caller can
// switch transports on a retry. It stops early when ctx is cancelled (window closed
// / user cancelled). Only the transport is retried; the signature and sha256 checks
// run downstream in downloadVerify and are not retried.

// fetchBytes GETs a URL fully into memory, retrying transient transport failures.

// fetchBytesFallback retries transport failures with the IPv4-pinned client.
// This covers small manifest/signature requests as well as the artifact body;
// previously only the large artifact download escaped a broken IPv6 route.

// download fetches url into memory, invoking onProgress as bytes arrive. A transient
// transport failure is retried; the retry resumes from the bytes already received
// via a Range request instead of restarting, and switches to the IPv4 fallback
// client (when provided) since a reset usually means the IPv6 route is the problem.
// total is the expected size for the progress denominator (refined from the response).

// downloadInto appends url's body to buf, resuming from buf's current length via a
// Range request so a retry continues the partial download. A 206 carries the
// remaining bytes; a 200 means the server ignored Range, so buf is reset and the
// whole file re-downloaded. total is refined from the response for the progress
// denominator (Content-Length on 200, the size field of Content-Range on 206).

// totalFromContentRange parses the total size out of a "bytes 200-999/1000" header,
// returning 0 when it's absent or "*" (unknown).

// progressReader reports cumulative bytes read, throttled so the event channel
// isn't flooded.

// Emit roughly every 256 KiB, and always on the final read (io.EOF).

// checkSHA256 verifies data's digest matches the lowercase-hex want.

// extractBinary pulls a single named regular file out of a .tar.gz blob.

// applyLinux replaces the running binary with the one inside the downloaded
// tar.gz; the caller relaunches afterwards.

// pending-update.json remains immutable; the transaction-unique sidecar now
// binds every installed member. A crash before marker cleanup is safe:
// startup correlates the exact transaction and rolls the release unit back.

// applyLinuxVersioned publishes a verified compatibility tarball into a new
// version directory and swaps current.json last. The tar still contains the
// one-shot reasonix-guard member for v1.18-v1.19 updaters, but v1.20+ ignores
// that member and never persists it again.

// currentInstallDir is the InstallRoot for updates. For the versioned layout it
// is the directory that owns current.json (not versions/<ver>/). For flat
// installs it is the directory of the running executable.

// archiveSupersededPendingUpdateAfterReady retires a transaction only after the
// current desktop has shown a usable UI. App-bundle recovery handles interrupted
// macOS generations; the versioned-layout branch handles older flat Windows and
// Linux transactions.

// Package-managed and legacy flat installs have no versioned pointer and
// therefore are not authorized to retire a transaction.

// refreshPendingUpdateHealthIdentity re-reads the current probationary
// transaction so a user-initiated update can commit health even when the
// process started without a matching identity (for example a historical
// version-prefix mismatch).

// updateSiblingArtifacts lists the packaged binaries an update replaces beside
// the main executable, so PrepareFileUpdate can snapshot the complete release
// unit. Paths that do not exist on disk are skipped by the backup.

// Versioned-v1 layout: primary is the active desktop under versions/.

// Legacy flat release unit. reasonix-guard.exe may still exist on disk
// during migration from 1.18–1.19.1; the new layout omits it.

// relaunchThroughLauncher starts the permanent thin launcher (or falls back to
// the running executable). A legacy Guard binary is considered only as a
// one-release migration fallback for flat 1.18-1.19.1 installations.

// migration window only

// Only legacy guard understands "launch --detach"; the thin launcher strips it.

// Unix names on Windows are unused.

// Fall through to previous flat-dir behavior for incomplete installs.
