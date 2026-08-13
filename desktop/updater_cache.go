package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/desktop/internal/update"
	"reasonix/internal/config"
)

type cachedUpdate struct {
	Version       string `json:"version"`
	Channel       string `json:"channel"`
	Platform      string `json:"platform"`
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	SHA256        string `json:"sha256"`
	DownloadedAt  string `json:"downloadedAt"`
	ArtifactKind  string `json:"artifactKind,omitempty"`  // tarball | deb
	SignaturePath string `json:"signaturePath,omitempty"` // required for deb
}

var updateCacheBaseDir = defaultUpdateCacheBaseDir

func defaultUpdateCacheBaseDir() (string, error) {
	if cd := config.CacheDir(); cd != "" {
		return filepath.Join(cd, "updates"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "Reasonix", "updates"), nil
}

func updateCacheDir() (string, error) {
	dir, err := updateCacheBaseDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func updateMetadataPath() (string, error) {
	dir, err := updateCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "downloaded.json"), nil
}

func assetFileName(asset update.Asset, version string) string {
	if u, err := url.Parse(asset.URL); err == nil {
		if base := filepath.Base(u.Path); base != "." && base != "/" {
			return base
		}
	}
	clean := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-").Replace(version)
	return "Reasonix-" + clean + "-" + update.CurrentPlatform() + ".update"
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func saveCachedUpdateForChannel(selected, version string, asset update.Asset, data []byte, kind string, signature []byte) (*cachedUpdate, error) {
	selected = normalizeUpdateChannel(selected)
	if err := checkSHA256(data, asset.SHA256); err != nil {
		return nil, err
	}
	kind = artifactKindFromMeta(kind)
	dir, err := updateCacheDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, assetFileName(asset, version))
	if err := writeAtomic(path, data, 0o600); err != nil {
		return nil, err
	}
	meta := &cachedUpdate{
		Version:      version,
		Channel:      selected,
		Platform:     update.CurrentPlatform(),
		Path:         path,
		Size:         int64(len(data)),
		SHA256:       asset.SHA256,
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		ArtifactKind: kind,
	}
	if kind == artifactKindDeb {
		if len(signature) == 0 {
			return nil, fmt.Errorf("update: deb cache requires a signature")
		}
		sigPath := path + ".minisig"
		if err := writeAtomic(sigPath, signature, 0o600); err != nil {
			return nil, err
		}
		meta.SignaturePath = sigPath
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	metadataPath, err := updateMetadataPath()
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(metadataPath, append(raw, '\n'), 0o600); err != nil {
		return nil, err
	}
	return meta, nil
}

func loadCachedUpdate() (*cachedUpdate, error) {
	path, err := updateMetadataPath()
	if err != nil {
		return nil, err
	}
	raw, err := readFileUTF8(path)
	if err != nil {
		return nil, err
	}
	var meta cachedUpdate
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	if meta.Version == "" || meta.Channel == "" || meta.Platform == "" || meta.Path == "" || meta.SHA256 == "" {
		return nil, fmt.Errorf("update: cached metadata is incomplete")
	}
	return &meta, nil
}

func cachedUpdateMatchesForChannel(selected, version string, asset update.Asset, kind string) bool {
	selected = normalizeUpdateChannel(selected)
	meta, err := loadCachedUpdate()
	if err != nil {
		return false
	}
	kind = artifactKindFromMeta(kind)
	metaKind := artifactKindFromMeta(meta.ArtifactKind)

	if kind == artifactKindDeb {
		if metaKind != artifactKindDeb || meta.SignaturePath == "" {
			return false
		}
		if _, err := os.Stat(meta.SignaturePath); err != nil {
			return false
		}
	} else if metaKind != artifactKindTarball {
		return false
	}
	return meta.Version == version &&
		meta.Channel == selected &&
		meta.Platform == update.CurrentPlatform() &&
		strings.EqualFold(meta.SHA256, asset.SHA256) &&
		meta.Size == asset.Size &&
		fileSHA256Matches(meta.Path, meta.SHA256)
}

func fileSHA256Matches(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want)
}

func readVerifiedCachedUpdateForChannel(selected string) (*cachedUpdate, []byte, error) {
	selected = normalizeUpdateChannel(selected)
	meta, err := loadCachedUpdate()
	if err != nil {
		return nil, nil, err
	}
	if meta.Channel != selected {
		return nil, nil, fmt.Errorf("update: cached update is for %s channel, selected channel is %s", meta.Channel, selected)
	}
	if meta.Platform != update.CurrentPlatform() {
		return nil, nil, fmt.Errorf("update: cached update is for %s, current platform is %s", meta.Platform, update.CurrentPlatform())
	}
	data, err := os.ReadFile(meta.Path)
	if err != nil {
		return nil, nil, err
	}
	if err := checkSHA256(data, meta.SHA256); err != nil {
		return nil, nil, err
	}
	meta.ArtifactKind = artifactKindFromMeta(meta.ArtifactKind)
	if meta.ArtifactKind == artifactKindDeb {
		if meta.SignaturePath == "" {
			return nil, nil, fmt.Errorf("update: cached deb is missing its signature")
		}
		if _, err := os.Stat(meta.SignaturePath); err != nil {
			return nil, nil, fmt.Errorf("update: cached deb signature is missing")
		}
	}
	return meta, data, nil
}
