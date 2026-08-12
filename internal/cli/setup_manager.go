package cli

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/i18n"
)

type providerSetupSession struct {
	cfg                *config.Config
	originalProviders  map[string]config.ProviderEntry
	originalDefault    string
	pendingCredentials map[string]string
	removed            map[string]bool
	accessDeclared     bool
	projectScoped      bool
	declaredProviders  []string
	operations         []providerSetupOperation
}

const setupManagerContinue = 2

type providerSetupOperationKind uint8

const (
	setupOpProvider providerSetupOperationKind = iota
	setupOpDefaultModel
	setupOpLanguage
	setupOpMaterializeAccess
	setupOpAccessMembership
)

type providerSetupOperation struct {
	kind           providerSetupOperationKind
	providerName   string
	beforeProvider *config.ProviderEntry
	afterProvider  *config.ProviderEntry
	beforeString   string
	afterString    string
	accessName     string
	projectScoped  bool
	beforeBool     bool
	afterBool      bool
}

type providerSetupConflictError struct {
	field string
}

type providerSetupFileSnapshot struct {
	exists bool
	body   []byte
}

func (e *providerSetupConflictError) Error() string {
	return e.field
}

func providerSetupEntryPtr(entry config.ProviderEntry) *config.ProviderEntry {
	copy := config.ProviderEntryConfigSnapshot(entry)
	return &copy
}

func readProviderSetupFileSnapshot(path string) (providerSetupFileSnapshot, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return providerSetupFileSnapshot{}, nil
		}
		return providerSetupFileSnapshot{}, err
	}
	return providerSetupFileSnapshot{exists: true, body: body}, nil
}

func providerSetupFileSnapshotEqual(a, b providerSetupFileSnapshot) bool {
	return a.exists == b.exists && bytes.Equal(a.body, b.body)
}

func newProviderSetupSession(cfg *config.Config) *providerSetupSession {
	s := &providerSetupSession{
		cfg:                cfg,
		originalProviders:  make(map[string]config.ProviderEntry, len(cfg.Providers)),
		originalDefault:    cfg.DefaultModel,
		pendingCredentials: map[string]string{},
		removed:            map[string]bool{},
	}
	for _, p := range cfg.Providers {
		s.originalProviders[p.Name] = p
	}
	return s
}

func newProviderSetupSessionForPath(cfg *config.Config, path string) *providerSetupSession {
	s := newProviderSetupSession(cfg)
	s.projectScoped = !config.IsUserConfigPath(path)
	declarations, err := config.InspectConfigFileDeclarations(path)
	if err != nil {
		// LoadForEdit already reports malformed/unreadable config and falls back;
		// keep the conservative policy here so setup never enables hidden siblings.
		s.accessDeclared = true
		return s
	}
	s.accessDeclared = declarations.DesktopProviderAccessDeclared
	s.declaredProviders = declarations.ProviderNames
	return s
}

func (s *providerSetupSession) recordProviderMutation(name string, before, after *config.ProviderEntry) {
	s.operations = append(s.operations, providerSetupOperation{
		kind:           setupOpProvider,
		providerName:   name,
		beforeProvider: before,
		afterProvider:  after,
	})
}

func (s *providerSetupSession) setLanguage(language string) {
	if s.cfg.Language == language {
		return
	}
	s.operations = append(s.operations, providerSetupOperation{
		kind:         setupOpLanguage,
		beforeString: s.cfg.Language,
		afterString:  language,
	})
	s.cfg.Language = language
}

func (s *providerSetupSession) applyDeepSeekOfficialDefaultPricing() {
	before := make(map[string]config.ProviderEntry, len(s.cfg.Providers))
	for _, provider := range s.cfg.Providers {
		before[provider.Name] = provider
	}
	s.cfg.ApplyDeepSeekOfficialDefaultPricing()
	for i := range s.cfg.Providers {
		after := s.cfg.Providers[i]
		previous, existed := before[after.Name]
		if existed && config.ProviderEntriesConfigEqual(previous, after) {
			delete(before, after.Name)
			continue
		}
		var previousPtr *config.ProviderEntry
		if existed {
			previousPtr = providerSetupEntryPtr(previous)
		}
		s.recordProviderMutation(after.Name, previousPtr, providerSetupEntryPtr(after))
		delete(before, after.Name)
	}
	for name, previous := range before {
		s.recordProviderMutation(name, providerSetupEntryPtr(previous), nil)
	}
}

func (s *providerSetupSession) resetProviderSummaryBaseline() {
	s.originalProviders = make(map[string]config.ProviderEntry, len(s.cfg.Providers))
	for _, provider := range s.cfg.Providers {
		s.originalProviders[provider.Name] = provider
	}
}

func (s *providerSetupSession) upsert(entries []config.ProviderEntry) error {
	for _, entry := range entries {
		var before *config.ProviderEntry
		if current, ok := s.cfg.Provider(entry.Name); ok {
			before = providerSetupEntryPtr(*current)
		}
		if err := s.cfg.UpsertProvider(entry); err != nil {
			return err
		}
		current, _ := s.cfg.Provider(entry.Name)
		if before == nil || !config.ProviderEntriesConfigEqual(*before, *current) {
			s.recordProviderMutation(entry.Name, before, providerSetupEntryPtr(*current))
		}
		delete(s.removed, entry.Name)
		s.repairDanglingDefaultFor(*current)
	}
	return nil
}

// repairDanglingDefaultFor re-points default_model at the provider's own default
// when an edit or model refresh dropped the exact model the ref named, mirroring
// the repair RemoveProvider performs on removal.
func (s *providerSetupSession) repairDanglingDefaultFor(p config.ProviderEntry) {
	if !config.ModelRefsProvider(s.cfg.DefaultModel, p.Name) || len(p.ModelList()) == 0 {
		return
	}
	if _, ok := s.cfg.ResolveModel(s.cfg.DefaultModel); ok {
		return
	}
	if err := s.setDefaultModel(p.Name); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func (s *providerSetupSession) add(entries []config.ProviderEntry) error {
	seen := make(map[string]bool, len(s.cfg.Providers)+len(entries))
	for _, provider := range s.cfg.Providers {
		seen[provider.Name] = true
	}
	for _, entry := range entries {
		if seen[entry.Name] {
			return fmt.Errorf(i18n.M.SetupProviderExistsFmt, entry.Name)
		}
		seen[entry.Name] = true
	}
	return s.upsert(entries)
}

func (s *providerSetupSession) remove(name string) error {
	current, ok := s.cfg.Provider(name)
	if !ok {
		return fmt.Errorf("remove provider: no provider %q", name)
	}
	before := providerSetupEntryPtr(*current)
	if err := s.cfg.RemoveProvider(name); err != nil {
		return err
	}
	s.recordProviderMutation(name, before, nil)
	s.removeProviderAccess(name)
	if _, existed := s.originalProviders[name]; existed {
		s.removed[name] = true
	}
	return nil
}

func (s *providerSetupSession) addProviderAccess(entries []config.ProviderEntry) {
	if len(entries) == 0 {
		return
	}
	before := append([]string(nil), s.cfg.Desktop.ProviderAccess...)
	// Preserve the legacy "undeclared means infer all configured providers"
	// behavior before turning provider_access into an explicit list. Project
	// setup only seeds providers declared by that project; cfg also contains
	// built-in defaults, which must not override the user's global access policy.
	if !s.accessDeclared && len(s.cfg.Desktop.ProviderAccess) == 0 {
		if s.projectScoped {
			for _, name := range s.declaredProviders {
				provider, ok := s.cfg.Provider(name)
				if ok && provider.Configured() && len(provider.ModelList()) > 0 {
					s.cfg.Desktop.ProviderAccess = append(s.cfg.Desktop.ProviderAccess, name)
				}
			}
		} else {
			config.NormalizeLegacyDesktopProviderAccess(s.cfg)
		}
		s.accessDeclared = true
		s.operations = append(s.operations, providerSetupOperation{
			kind:          setupOpMaterializeAccess,
			projectScoped: s.projectScoped,
		})
		before = append([]string(nil), s.cfg.Desktop.ProviderAccess...)
	}
	seen := make(map[string]bool, len(s.cfg.Desktop.ProviderAccess)+len(entries))
	for _, name := range s.cfg.Desktop.ProviderAccess {
		name = strings.TrimSpace(name)
		if name != "" {
			seen[name] = true
		}
	}
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" || seen[name] {
			continue
		}
		s.cfg.Desktop.ProviderAccess = append(s.cfg.Desktop.ProviderAccess, name)
		seen[name] = true
	}
	s.accessDeclared = true
	s.recordAccessTransition(before)
}

func (s *providerSetupSession) removeProviderAccess(name string) {
	name = strings.TrimSpace(name)
	if name == "" || len(s.cfg.Desktop.ProviderAccess) == 0 {
		return
	}
	before := append([]string(nil), s.cfg.Desktop.ProviderAccess...)
	out := s.cfg.Desktop.ProviderAccess[:0]
	for _, current := range s.cfg.Desktop.ProviderAccess {
		if strings.TrimSpace(current) != name {
			out = append(out, current)
		}
	}
	s.cfg.Desktop.ProviderAccess = out
	s.recordAccessTransition(before)
}

func (s *providerSetupSession) recordAccessTransition(before []string) {
	beforeSet := make(map[string]bool, len(before))
	afterSet := make(map[string]bool, len(s.cfg.Desktop.ProviderAccess))
	var order []string
	seen := map[string]bool{}
	for _, names := range [][]string{before, s.cfg.Desktop.ProviderAccess} {
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if !seen[name] {
				seen[name] = true
				order = append(order, name)
			}
		}
	}
	for _, name := range before {
		name = strings.TrimSpace(name)
		if name != "" {
			beforeSet[name] = true
		}
	}
	for _, name := range s.cfg.Desktop.ProviderAccess {
		name = strings.TrimSpace(name)
		if name != "" {
			afterSet[name] = true
		}
	}
	for _, name := range order {
		if beforeSet[name] == afterSet[name] {
			continue
		}
		s.operations = append(s.operations, providerSetupOperation{
			kind:       setupOpAccessMembership,
			accessName: name,
			beforeBool: beforeSet[name],
			afterBool:  afterSet[name],
		})
	}
}

func (s *providerSetupSession) setCredential(key, value string) error {
	key = strings.TrimSpace(key)
	if !config.IsValidCredentialKey(key) {
		return fmt.Errorf("invalid API key variable name %q", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("API key for %s contains a newline", key)
	}
	s.pendingCredentials[key] = value
	return nil
}

func (s *providerSetupSession) setDefaultModel(model string) error {
	before := s.cfg.DefaultModel
	if err := s.cfg.SetDefaultModel(model); err != nil {
		return err
	}
	if before != s.cfg.DefaultModel {
		s.operations = append(s.operations, providerSetupOperation{
			kind:         setupOpDefaultModel,
			beforeString: before,
			afterString:  s.cfg.DefaultModel,
		})
	}
	return nil
}

// providerUsable reports whether the provider would be selectable once this
// session saves: it lists models and either needs no key or has one resolvable
// from the credential store or staged in this session.
func (s *providerSetupSession) providerUsable(p *config.ProviderEntry) bool {
	if p == nil || len(p.ModelList()) == 0 {
		return false
	}
	return p.Configured() || s.pendingCredentials[p.APIKeyEnv] != ""
}

// defaultModelUsable reports whether default_model resolves to a provider the
// user could actually run once this session saves.
func (s *providerSetupSession) defaultModelUsable() bool {
	entry, ok := s.cfg.ResolveModel(s.cfg.DefaultModel)
	return ok && s.providerUsable(entry)
}

// promoteDefaultToNewProviders keeps the wizard's first-run contract: when the
// current default_model cannot run (unresolvable, or its key is neither stored
// nor staged), point it at the first usable provider the user just added, so a
// first run that only configures a custom provider boots on that provider
// instead of failing on the built-in default's missing key. A usable default is
// never hijacked.
func (s *providerSetupSession) promoteDefaultToNewProviders(entries []config.ProviderEntry) {
	if s.defaultModelUsable() {
		return
	}
	for _, entry := range entries {
		current, ok := s.cfg.Provider(entry.Name)
		if !ok || !s.providerUsable(current) {
			continue
		}
		if err := s.setDefaultModel(current.Name); err == nil {
			return
		}
	}
}

func (s *providerSetupSession) credentialLines() []string {
	keys := make([]string, 0, len(s.pendingCredentials))
	for key := range s.pendingCredentials {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+s.pendingCredentials[key])
	}
	return lines
}

func (s *providerSetupSession) summary() []string {
	var added, edited []string
	for _, p := range s.cfg.Providers {
		old, existed := s.originalProviders[p.Name]
		switch {
		case !existed:
			added = append(added, p.Name)
		case !providerSetupEqual(old, p):
			edited = append(edited, p.Name)
		}
	}
	var out []string
	if len(added) > 0 {
		out = append(out, fmt.Sprintf(i18n.M.SetupSummaryAddedFmt, strings.Join(added, ", ")))
	}
	if len(edited) > 0 {
		out = append(out, fmt.Sprintf(i18n.M.SetupSummaryEditedFmt, strings.Join(edited, ", ")))
	}
	if len(s.removed) > 0 {
		names := make([]string, 0, len(s.removed))
		for name := range s.removed {
			names = append(names, name)
		}
		sort.Strings(names)
		out = append(out, fmt.Sprintf(i18n.M.SetupSummaryRemovedFmt, strings.Join(names, ", ")))
	}
	if s.cfg.DefaultModel != s.originalDefault {
		out = append(out, fmt.Sprintf(i18n.M.SetupSummaryDefaultFmt, s.cfg.DefaultModel))
	}
	if len(s.pendingCredentials) > 0 {
		out = append(out, fmt.Sprintf(i18n.M.SetupSummaryKeysFmt, len(s.pendingCredentials)))
	}
	if len(out) == 0 {
		out = append(out, i18n.M.SetupSummaryNoChanges)
	}
	return out
}

// Render-level equality is unnecessary here: the manager only changes these
// fields, while advanced provider fields are preserved by editing a copy.

// After the new keys are staged, so usability sees them.
