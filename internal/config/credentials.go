package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	CredentialsStoreAuto    = "auto"
	CredentialsStoreKeyring = "keyring"
	CredentialsStoreFile    = "file"

	credentialsKeyringService = "reasonix"
	credentialClearedPrefix   = "# reasonix-cleared "
)

const (
	CredentialSourceEnvironment = "environment"
	CredentialSourceProjectEnv  = "project_env"
	CredentialSourceCredentials = "credentials"
	CredentialSourceHomeEnv     = "home_env"
	CredentialSourceLegacy      = "legacy_credentials"
)

type CredentialSource struct {
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	Label string `json:"label,omitempty"`
}

type CredentialResolution struct {
	Name     string             `json:"name"`
	Set      bool               `json:"set"`
	Value    string             `json:"-"`
	Source   CredentialSource   `json:"source,omitempty"`
	Shadowed []CredentialSource `json:"shadowed,omitempty"`
}

type trackedCredentialSource struct {
	source CredentialSource
	value  string
}

var credentialSourceTracker = struct {
	sync.Mutex
	byKey map[string]trackedCredentialSource
}{byKey: map[string]trackedCredentialSource{}}

// userCredentialEditMu serializes Reasonix-owned credential-store writes.
// LockUserCredentialEdits also takes a path-derived advisory file lock so a
// Desktop window, CLI process, or background catalog save can share one
// compare-and-apply boundary with credential rotation.
var userCredentialEditMu sync.Mutex

var storedCredentialValueLookup = storedCredentialValue

// legacyKeyringProbeLookup is the test-facing single-key hook returning the full
// four-state outcome under a caller-owned context (shared 1s migration budget).
var legacyKeyringProbeLookup = legacyKeyringProbe

// legacyKeyringLookupTimeout is the shared budget for one legacy keyring scan.
var legacyKeyringLookupTimeout = time.Second

// CredentialResolver resolves credentials repeatedly for one caller-owned view
// build. It keeps expensive global credential-store lookups bounded to one per
// key while preserving the same source/shadow reporting as the one-shot helpers.
type CredentialResolver struct {
	root string

	mu               sync.Mutex
	globalFirstCache map[string]CredentialResolution
}

// NewCredentialResolverForRoot returns a resolver scoped to a workspace root.
func NewCredentialResolverForRoot(root string) *CredentialResolver {
	return &CredentialResolver{root: resolveRoot(root)}
}

// ResolveGlobalFirst resolves key from Reasonix's global .env only. Repeated
// calls for the same key reuse the first result so UI views with multiple
// provider entries sharing api_key_env stay consistent.
func (r *CredentialResolver) ResolveGlobalFirst(key string) CredentialResolution {
	key = strings.TrimSpace(key)
	if key == "" {
		return CredentialResolution{Name: key}
	}
	if r == nil {
		return resolveCredentialForRootGlobalFirst(".", key)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.globalFirstCache == nil {
		r.globalFirstCache = map[string]CredentialResolution{}
	}
	if cached, ok := r.globalFirstCache[key]; ok {
		return cloneCredentialResolution(cached)
	}
	res := resolveCredentialForRootGlobalFirst(r.root, key)
	r.globalFirstCache[key] = cloneCredentialResolution(res)
	return res
}

func cloneCredentialResolution(res CredentialResolution) CredentialResolution {
	if len(res.Shadowed) > 0 {
		res.Shadowed = append([]CredentialSource(nil), res.Shadowed...)
	}
	return res
}

func normalizeCredentialsStore(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case CredentialsStoreKeyring:
		return CredentialsStoreKeyring
	case CredentialsStoreFile:
		return CredentialsStoreFile
	default:
		return CredentialsStoreAuto
	}
}

func credentialsStoreMode() string {
	if mode := strings.TrimSpace(os.Getenv("REASONIX_CREDENTIALS_STORE")); mode != "" {
		return normalizeCredentialsStore(mode)
	}
	var partial struct {
		CredentialsStore string `toml:"credentials_store"`
	}
	if path := userConfigLoadPath(); path != "" {
		_, _ = decodeTOMLFile(path, &partial)
	}
	return normalizeCredentialsStore(partial.CredentialsStore)
}

func credentialEnvNamesForRoot(root string) []string {
	root = resolveRoot(root)
	cfg := Default()

	projectTOML := "reasonix.toml"
	if root != "." {
		projectTOML = filepath.Join(root, "reasonix.toml")
	}
	if uc := userConfigLoadPath(); uc != "" {
		_ = mergeFile(cfg, uc)
	}
	_ = mergeFile(cfg, projectTOML)
	var tomlSources []string
	if uc := userConfigLoadPath(); uc != "" {
		tomlSources = append(tomlSources, uc)
	}
	tomlSources = append(tomlSources, projectTOML)
	if providers, _, _, ok, err := mergeTOMLProviders(tomlSources); err == nil && ok {
		cfg.Providers = providers
	}

	return credentialEnvNamesFromConfig(cfg)
}

func credentialEnvNamesFromConfig(cfg *Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, p := range cfg.Providers {
		add(p.APIKeyEnv)
	}
	for _, h := range cfg.Remote.Hosts {
		add(h.PassphraseEnv)
		add(h.PasswordEnv)
	}
	sort.Strings(out)
	return out
}

// CredentialEnvNames returns every environment-variable name whose value can
// be loaded from Reasonix's global credential store. This includes configured
// provider keys and stored keys that are no longer referenced by the
// current config: loadCredentialStoreForRoot loads the whole credential file,
// so stale entries must remain outside child-process environments too.
func (c *Config) CredentialEnvNames() []string {
	names := credentialEnvNamesFromConfig(c)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	if file, ok := readDotEnvFile(UserCredentialsPath()); ok {
		for name := range file.Values {
			name = strings.TrimSpace(name)
			if !isCredentialKey(name) || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func resolveProviderCredentialsForRoot(root string, cfg *Config) {
	if cfg == nil || len(cfg.Providers) == 0 {
		return
	}
	resolver := NewCredentialResolverForRoot(root)
	for i := range cfg.Providers {
		resolveProviderCredentialWithResolver(&cfg.Providers[i], resolver)
	}
}

func resolveProviderCredentialWithResolver(entry *ProviderEntry, resolver *CredentialResolver) {
	if entry == nil {
		return
	}
	key := strings.TrimSpace(entry.APIKeyEnv)
	if key == "" {
		entry.resolvedAPIKey = ""
		entry.resolvedSource = CredentialSource{}
		return
	}
	if resolver == nil {
		resolver = NewCredentialResolverForRoot(".")
	}
	res := resolver.ResolveGlobalFirst(key)
	if !res.Set || res.Value == "" {
		entry.resolvedAPIKey = ""
		entry.resolvedSource = CredentialSource{}
		return
	}
	entry.resolvedAPIKey = res.Value
	entry.resolvedSource = res.Source
}

func (e *ProviderEntry) ResolveAPIKeyForRoot(root string) {
	resolveProviderCredentialWithResolver(e, NewCredentialResolverForRoot(root))
}

func loadCredentialStoreForRoot(root string) {
	names := credentialEnvNamesForRoot(root)
	if len(names) == 0 {
		return
	}
	if p := UserCredentialsPath(); p != "" {
		loadDotEnvFileAs(p, CredentialSource{Kind: CredentialSourceCredentials, Path: p, Label: "Reasonix credentials (.env)"})
	}
}

// StoreCredentialLines stores KEY=value assignments in Reasonix's global .env
// and pins them into the current process environment.

// storeCredentialIfAbsentAndNotCleared writes key=value only when the current
// credential store still lacks a value and a cleared tombstone for key. The
// re-check and write share LockUserCredentialEdits so a concurrent settings
// save or tombstone cannot be overwritten by a stale keyring import.
// stored is false when the write was skipped because the key is already present
// or cleared; err is non-nil only on lock/IO failures.

// SetCredentialIfRevision stores one credential only when the global
// credential file still has expectedRevision. The comparison and write share
// the same process and advisory file lock, preventing a stale setup page in one
// Reasonix process from overwriting a credential saved by another process.

// IsValidCredentialKey reports whether key can be stored in Reasonix's dotenv
// credential file and exposed as an environment variable.

// LockUserCredentialEdits serializes credential-store compare/write
// transactions in this process and across Reasonix processes. When both the
// user config and credential store are needed, acquire LockUserConfigEdits
// first, then this lock.

// CredentialStoreRevision returns a content-derived revision for the current
// Reasonix credential store. Callers performing compare-and-apply must hold
// LockUserCredentialEdits from this read through their commit.

func recordExistingCredentialSource(key string) {
	key = strings.TrimSpace(key)
	value := os.Getenv(key)
	if key == "" || value == "" {
		return
	}
	credentialSourceTracker.Lock()
	defer credentialSourceTracker.Unlock()
	if current, ok := credentialSourceTracker.byKey[key]; ok && current.value == value {
		return
	}
	if _, ok := credentialSourceTracker.byKey[key]; ok {
		return
	}
	credentialSourceTracker.byKey[key] = trackedCredentialSource{
		source: CredentialSource{Kind: CredentialSourceEnvironment, Label: "environment variable"},
		value:  value,
	}
}

func recordCredentialSource(key, value string, source CredentialSource) {
	key = strings.TrimSpace(key)
	if key == "" || value == "" {
		return
	}
	source.Label = credentialSourceLabel(source)
	credentialSourceTracker.Lock()
	credentialSourceTracker.byKey[key] = trackedCredentialSource{source: source, value: value}
	credentialSourceTracker.Unlock()
}

func trackedCredential(key, value string) (CredentialSource, bool) {
	credentialSourceTracker.Lock()
	defer credentialSourceTracker.Unlock()
	current, ok := credentialSourceTracker.byKey[key]
	if !ok || current.value != value {
		return CredentialSource{}, false
	}
	return current.source, true
}

func credentialSourceLabel(source CredentialSource) string {
	if strings.TrimSpace(source.Label) != "" {
		return source.Label
	}
	switch source.Kind {
	case CredentialSourceProjectEnv:
		return "project .env"
	case CredentialSourceCredentials:
		return "Reasonix credentials"
	case CredentialSourceHomeEnv:
		return "home .env"
	case CredentialSourceLegacy:
		return "legacy Reasonix credentials"
	case CredentialSourceEnvironment:
		return "environment variable"
	default:
		return ""
	}
}

func ResolveCredential(key string) CredentialResolution {
	return ResolveCredentialForRoot(".", key)
}

func ResolveCredentialForRoot(root, key string) CredentialResolution {
	key = strings.TrimSpace(key)
	res := CredentialResolution{Name: key}
	if key == "" {
		return res
	}
	value := os.Getenv(key)
	if value == "" {
		return res
	}
	res.Set = true
	res.Value = value
	if source, ok := trackedCredential(key, value); ok {
		res.Source = source
	} else if source, ok := inferCredentialSource(root, key, value); ok {
		res.Source = source
	} else {
		res.Source = CredentialSource{Kind: CredentialSourceEnvironment, Label: credentialSourceLabel(CredentialSource{Kind: CredentialSourceEnvironment})}
	}
	res.Source.Label = credentialSourceLabel(res.Source)
	res.Shadowed = shadowedCredentialSources(root, key, value, res.Source)
	return res
}

func ResolveCredentialForRootGlobalFirst(root, key string) CredentialResolution {
	key = strings.TrimSpace(key)
	return NewCredentialResolverForRoot(root).ResolveGlobalFirst(key)
}

func resolveCredentialForRootGlobalFirst(root, key string) CredentialResolution {
	root = resolveRoot(root)
	res := CredentialResolution{Name: key}
	if key == "" {
		return res
	}
	if value, source, ok := storedCredentialValueLookup(key); ok {
		res.Set = true
		res.Value = value
		res.Source = source
		res.Source.Label = credentialSourceLabel(res.Source)
		res.Shadowed = shadowedCredentialSources(root, key, value, res.Source)
		return res
	}
	return res
}

func storedCredentialValue(key string) (string, CredentialSource, bool) {
	if p := UserCredentialsPath(); p != "" {
		if value, ok := envFileValue(p, key); ok && value != "" {
			return value, CredentialSource{Kind: CredentialSourceCredentials, Path: p, Label: "Reasonix credentials (.env)"}, true
		}
	}
	return "", CredentialSource{}, false
}

func inferCredentialSource(root, key, value string) (CredentialSource, bool) {
	for _, candidate := range credentialSourceCandidates(root) {
		if v, ok := envFileValue(candidate.Path, key); ok && v == value {
			candidate.Label = credentialSourceLabel(candidate)
			return candidate, true
		}
	}
	return CredentialSource{}, false
}

func shadowedCredentialSources(root, key, activeValue string, active CredentialSource) []CredentialSource {
	var out []CredentialSource
	for _, candidate := range credentialSourceCandidates(root) {
		if sameCredentialSource(candidate, active) {
			continue
		}
		if v, ok := envFileValue(candidate.Path, key); ok && v != activeValue {
			candidate.Label = credentialSourceLabel(candidate)
			out = append(out, candidate)
		}
	}
	return out
}

func credentialSourceCandidates(root string) []CredentialSource {
	root = resolveRoot(root)
	var out []CredentialSource
	dotEnvPath := ".env"
	if root != "" && root != "." {
		dotEnvPath = filepath.Join(root, ".env")
	}
	out = append(out, CredentialSource{Kind: CredentialSourceProjectEnv, Path: dotEnvPath})
	if p := UserCredentialsPath(); p != "" {
		out = append(out, CredentialSource{Kind: CredentialSourceCredentials, Path: p})
	}
	if IsolatedHomeDir() == "" {
		if home, err := os.UserHomeDir(); err == nil {
			out = append(out, CredentialSource{Kind: CredentialSourceHomeEnv, Path: filepath.Join(home, ".env")})
		}
	}
	return out
}

func sameCredentialSource(a, b CredentialSource) bool {
	if a.Kind != b.Kind {
		return false
	}
	if a.Path == "" || b.Path == "" {
		return a.Path == b.Path
	}
	return samePath(a.Path, b.Path)
}
