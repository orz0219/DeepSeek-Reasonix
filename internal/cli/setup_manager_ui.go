package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/i18n"
)

func providerSetupEqual(a, b config.ProviderEntry) bool {

	return a.Name == b.Name && a.Kind == b.Kind && a.BaseURL == b.BaseURL &&
		a.Model == b.Model && strings.Join(a.Models, "\x00") == strings.Join(b.Models, "\x00") &&
		a.Default == b.Default && a.APIKeyEnv == b.APIKeyEnv
}

func runProviderSetupManager(s *providerSetupSession, configPath, envPath string) int {
	cfg := s.cfg
	repaired, repairs := repairInvalidProviderKeyEnvs(cfg.Providers)
	for i := range repaired {
		if config.ProviderEntriesConfigEqual(cfg.Providers[i], repaired[i]) {
			continue
		}
		before := providerSetupEntryPtr(cfg.Providers[i])
		cfg.Providers[i] = repaired[i]
		s.recordProviderMutation(repaired[i].Name, before, providerSetupEntryPtr(repaired[i]))
	}
	for _, repair := range repairs {
		fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf(i18n.M.RepairedAPIKeyEnvFmt, repair.provider, repair.old, repair.new)))
	}
	for {
		items := providerManagerItems(s)
		idx, err := selectOne(i18n.M.SetupManagerTitle, items)
		if err != nil {
			fmt.Fprintln(os.Stderr, "\n"+i18n.M.SetupCancelled)
			return 1
		}
		providerCount := len(cfg.Providers)
		switch idx {
		case providerCount:
			if !addProviderToSession(s, false) {
				continue
			}
		case providerCount + 1:
			if !addProviderToSession(s, true) {
				continue
			}
		case providerCount + 2:
			rc := saveProviderSetupSession(s, configPath, envPath)
			if rc == setupManagerContinue {
				continue
			}
			return rc
		case providerCount + 3:
			fmt.Println(i18n.M.SetupCancelled)
			return 1
		default:
			manageProvider(s, idx)
		}
	}
}

func providerManagerItems(s *providerSetupSession) []menuItem {
	cfg := s.cfg
	items := make([]menuItem, 0, len(cfg.Providers)+4)
	for _, p := range cfg.Providers {
		models := p.ModelList()
		keyStatus := i18n.M.SetupKeyMissing
		if p.APIKeyEnv == "" || config.CredentialIsSet(p.APIKeyEnv) || s.pendingCredentials[p.APIKeyEnv] != "" {
			keyStatus = i18n.M.SetupKeySet
		}
		desc := fmt.Sprintf("%s · %d %s · %s", p.Kind, len(models), i18n.M.SetupModelsUnit, keyStatus)
		if cfg.DefaultModel == p.Name || config.ModelRefsProvider(cfg.DefaultModel, p.Name) {
			desc += " · " + i18n.M.SetupDefaultBadge
		}
		items = append(items, menuItem{name: p.Name, desc: desc})
	}
	return append(items,
		menuItem{name: i18n.M.SetupAddOpenAI, desc: i18n.M.CustomProviderDesc},
		menuItem{name: i18n.M.SetupAddAnthropic, desc: i18n.M.AnthropicProviderDesc},
		menuItem{name: i18n.M.SetupSaveExit, desc: i18n.M.SetupSaveExitDesc},
		menuItem{name: i18n.M.SetupCancel, desc: i18n.M.SetupCancelDesc},
	)
}

func addProviderToSession(s *providerSetupSession, anthropic bool) bool {
	var result providerPromptResult
	var err error
	if anthropic {
		result, err = promptAnthropicProvider()
	} else {
		result, err = promptCustomProvider()
	}
	if err != nil {
		if !errors.Is(err, errCancelled) {
			fmt.Fprintln(os.Stderr, err)
		}
		return false
	}
	for _, entry := range result.entries {
		if !confirmSharedCredential(s.cfg, entry, "") {
			return false
		}
	}
	if err := s.add(result.entries); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	s.addProviderAccess(result.entries)
	for key, value := range result.credentials {
		if err := s.setCredential(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return false
		}
	}

	s.promoteDefaultToNewProviders(result.entries)
	return true
}

func manageProvider(s *providerSetupSession, providerIndex int) {
	if providerIndex < 0 || providerIndex >= len(s.cfg.Providers) {
		return
	}
	p := s.cfg.Providers[providerIndex]
	idx, err := selectOne(fmt.Sprintf(i18n.M.SetupProviderActionsFmt, p.Name), []menuItem{
		{name: i18n.M.SetupEditProvider},
		{name: i18n.M.SetupUpdateKey},
		{name: i18n.M.SetupTestRefresh},
		{name: i18n.M.SetupSetDefault},
		{name: i18n.M.SetupRemoveProvider},
		{name: i18n.M.SetupBack},
	})
	if err != nil || idx == 5 {
		return
	}
	switch idx {
	case 0:
		editProvider(s, p)
	case 1:
		updateProviderKey(s, p)
	case 2:
		testAndRefreshProvider(s, p)
	case 3:
		setDefaultProvider(s, p)
	case 4:
		removeProviderFromSession(s, p)
	}
}

func editProvider(s *providerSetupSession, current config.ProviderEntry) {
	in := bufio.NewScanner(os.Stdin)
	edited := current
	edited.BaseURL = ask(in, os.Stdout, i18n.M.CustomPromptBaseURL, current.BaseURL)
	models := ask(in, os.Stdout, i18n.M.SetupPromptModels, strings.Join(current.ModelList(), ","))
	edited.Models = splitModels(models)
	if len(edited.Models) == 1 {
		edited.Model = edited.Models[0]
	} else {
		edited.Model = ""
	}
	if len(edited.Models) > 0 && !containsString(edited.Models, edited.Default) {
		edited.Default = edited.Models[0]
	}
	edited.APIKeyEnv = promptOptionalAPIKeyEnvName(in, os.Stdout, i18n.M.CustomPromptKeyEnv, current.APIKeyEnv)
	if !confirmSharedCredential(s.cfg, edited, current.Name) {
		return
	}
	if err := s.upsert([]config.ProviderEntry{edited}); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func promptOptionalAPIKeyEnvName(in *bufio.Scanner, w io.Writer, label, def string) string {
	for {
		key := ask(in, w, label, def)
		if key == "" || config.IsValidCredentialKey(key) {
			return key
		}
		fmt.Fprintf(w, i18n.M.InvalidAPIKeyEnvFmt+"\n", key)
	}
}

func splitModels(raw string) []string {
	seen := map[string]bool{}
	var models []string
	for model := range strings.SplitSeq(raw, ",") {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			seen[model] = true
			models = append(models, model)
		}
	}
	return models
}

func confirmSharedCredential(cfg *config.Config, candidate config.ProviderEntry, ignoreName string) bool {
	if candidate.APIKeyEnv == "" {
		return true
	}
	for _, p := range cfg.Providers {
		if p.Name == ignoreName || p.Name == candidate.Name || p.APIKeyEnv != candidate.APIKeyEnv || p.BaseURL == candidate.BaseURL {
			continue
		}
		in := bufio.NewScanner(os.Stdin)
		answer := ask(in, os.Stdout, fmt.Sprintf(i18n.M.SetupSharedKeyWarningFmt, candidate.APIKeyEnv, p.Name, p.BaseURL), "y/N")
		return answer == "y" || answer == "Y"
	}
	return true
}

func updateProviderKey(s *providerSetupSession, p config.ProviderEntry) {
	in := bufio.NewScanner(os.Stdin)
	keyEnvChanged := false
	if p.APIKeyEnv == "" {
		p.APIKeyEnv = promptAPIKeyEnvName(in, os.Stdout, i18n.M.CustomPromptKeyEnv, apiKeyEnvFromProviderName(p.Name))
		keyEnvChanged = true
	}
	if !confirmSharedCredential(s.cfg, p, p.Name) {
		return
	}
	if keyEnvChanged {
		if err := s.upsert([]config.ProviderEntry{p}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
	}
	value := ask(in, os.Stdout, fmt.Sprintf(i18n.M.SetupPromptAPIKeyFmt, p.APIKeyEnv), "")
	if value == "" {
		return
	}
	if err := s.setCredential(p.APIKeyEnv, value); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func testAndRefreshProvider(s *providerSetupSession, p config.ProviderEntry) {
	restore := temporarilySetCredential(p.APIKeyEnv, s.pendingCredentials[p.APIKeyEnv])
	defer restore()
	p.ResolveAPIKeyFromProcessEnvForProbe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := p.FetchModels(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, i18n.M.FetchModelsFailedFmt+"\n", p.Name, err)
		return
	}
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, i18n.M.CustomFetchEmpty)
		return
	}
	items := make([]menuItem, len(models))
	for i, model := range models {
		items[i] = menuItem{name: model}
	}
	idxs, err := selectMany(fmt.Sprintf(i18n.M.SelectModelsLabel, p.Name), items)
	if err != nil || len(idxs) == 0 {
		return
	}
	selected := make([]string, 0, len(idxs))
	for _, idx := range idxs {
		selected = append(selected, models[idx])
	}
	p.Models = selected
	p.Model = ""
	if !containsString(selected, p.Default) {
		p.Default = selected[0]
	}
	if err := s.upsert([]config.ProviderEntry{p}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.FetchModelsSuccessFmt, len(models), p.Name)))
}

func temporarilySetCredential(key, value string) func() {
	if key == "" || value == "" {
		return func() {}
	}
	old, existed := os.LookupEnv(key)
	_ = os.Setenv(key, value)
	return func() {
		if existed {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	}
}

func setDefaultProvider(s *providerSetupSession, p config.ProviderEntry) {
	models := p.ModelList()
	if len(models) == 0 {
		return
	}
	items := make([]menuItem, len(models))
	for i, model := range models {
		items[i] = menuItem{name: model}
	}
	idx, err := selectOne(i18n.M.SetupSelectDefaultModel, items)
	if err != nil {
		return
	}
	if err := s.setDefaultModel(p.Name + "/" + models[idx]); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func removeProviderFromSession(s *providerSetupSession, p config.ProviderEntry) {
	in := bufio.NewScanner(os.Stdin)
	answer := ask(in, os.Stdout, fmt.Sprintf(i18n.M.SetupConfirmRemoveFmt, p.Name), "y/N")
	if answer != "y" && answer != "Y" {
		return
	}
	if err := s.remove(p.Name); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func (s *providerSetupSession) replayOperations(cfg *config.Config, accessDeclared *bool, declaredProviders []string) error {
	for _, operation := range s.operations {
		switch operation.kind {
		case setupOpProvider:
			current, exists := cfg.Provider(operation.providerName)
			if operation.beforeProvider == nil {
				if exists {
					return &providerSetupConflictError{field: fmt.Sprintf("provider %q", operation.providerName)}
				}
			} else if !exists || !config.ProviderEntriesConfigEqual(*current, *operation.beforeProvider) {
				return &providerSetupConflictError{field: fmt.Sprintf("provider %q", operation.providerName)}
			}
			if operation.afterProvider == nil {
				if err := cfg.RemoveProvider(operation.providerName); err != nil {
					return fmt.Errorf("replay remove provider %q: %w", operation.providerName, err)
				}
			} else if err := cfg.UpsertProviderPreservingRuntime(*operation.afterProvider); err != nil {
				return fmt.Errorf("replay provider %q: %w", operation.providerName, err)
			}
		case setupOpDefaultModel:
			if cfg.DefaultModel != operation.beforeString {
				return &providerSetupConflictError{field: "default_model"}
			}
			if err := cfg.SetDefaultModel(operation.afterString); err != nil {
				return fmt.Errorf("replay default_model: %w", err)
			}
		case setupOpLanguage:
			if cfg.Language != operation.beforeString {
				return &providerSetupConflictError{field: "language"}
			}
			cfg.Language = operation.afterString
		case setupOpMaterializeAccess:
			if *accessDeclared {
				return &providerSetupConflictError{field: "desktop.provider_access"}
			}
			cfg.Desktop.ProviderAccess = nil
			if operation.projectScoped {
				for _, name := range declaredProviders {
					provider, ok := cfg.Provider(name)
					if ok && provider.Configured() && len(provider.ModelList()) > 0 {
						cfg.Desktop.ProviderAccess = append(cfg.Desktop.ProviderAccess, name)
					}
				}
			} else {
				config.NormalizeLegacyDesktopProviderAccess(cfg)
			}
			*accessDeclared = true
			if cfg.Desktop.ProviderAccess == nil {
				cfg.Desktop.ProviderAccess = []string{}
			}
		case setupOpAccessMembership:
			current := providerSetupAccessContains(cfg.Desktop.ProviderAccess, operation.accessName)
			if current != operation.beforeBool {
				return &providerSetupConflictError{field: fmt.Sprintf("desktop.provider_access[%q]", operation.accessName)}
			}
			if operation.afterBool {
				cfg.Desktop.ProviderAccess = append(cfg.Desktop.ProviderAccess, operation.accessName)
			} else {
				out := cfg.Desktop.ProviderAccess[:0]
				for _, name := range cfg.Desktop.ProviderAccess {
					if strings.TrimSpace(name) != operation.accessName {
						out = append(out, name)
					}
				}
				cfg.Desktop.ProviderAccess = out
			}
		default:
			return fmt.Errorf("unknown provider setup operation %d", operation.kind)
		}
	}
	return nil
}

func providerSetupAccessContains(names []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, name := range names {
		if strings.TrimSpace(name) == want {
			return true
		}
	}
	return false
}

func commitProviderSetupSession(s *providerSetupSession, configPath string) (bool, error) {
	if len(s.operations) == 0 {
		return false, nil
	}
	unlock, err := config.LockConfigFileEdits(configPath)
	if err != nil {
		return false, err
	}
	defer unlock()

	before, err := readProviderSetupFileSnapshot(configPath)
	if err != nil {
		return false, err
	}
	declarations, err := config.InspectConfigFileDeclarations(configPath)
	if err != nil {
		return false, err
	}
	fresh, err := config.LoadForEditReadOnlyStrict(configPath)
	if err != nil {
		return false, err
	}
	accessDeclared := declarations.DesktopProviderAccessDeclared
	if err := s.replayOperations(fresh, &accessDeclared, declarations.ProviderNames); err != nil {
		return false, err
	}
	current, err := readProviderSetupFileSnapshot(configPath)
	if err != nil {
		return false, err
	}
	if !providerSetupFileSnapshotEqual(before, current) {
		return false, &providerSetupConflictError{field: "configuration file"}
	}
	if err := fresh.SaveTo(configPath); err != nil {
		return false, err
	}
	return true, nil
}

func saveProviderSetupSession(s *providerSetupSession, configPath, envPath string) int {
	fmt.Println()
	fmt.Println(i18n.M.SetupSummaryTitle)
	for _, line := range s.summary() {
		fmt.Println("  " + line)
	}
	in := bufio.NewScanner(os.Stdin)
	answer := ask(in, os.Stdout, i18n.M.SetupConfirmSave, "Y/n")
	if answer == "n" || answer == "N" {
		return setupManagerContinue
	}
	configWritten, err := commitProviderSetupSession(s, configPath)
	if err != nil {
		var conflict *providerSetupConflictError
		if errors.As(err, &conflict) {
			fmt.Fprintf(os.Stderr, i18n.M.SetupConcurrentChangeFmt+"\n", conflict.field)
		} else {
			fmt.Fprintln(os.Stderr, i18n.M.WriteConfigErr, err)
		}
		return 1
	}
	if configWritten {
		fmt.Printf("\n%s %s\n", green("✓"), fmt.Sprintf(i18n.M.WroteFileFmt, displayPath(configPath)))
	}
	if lines := s.credentialLines(); len(lines) > 0 {
		target, err := config.StoreCredentialLines(lines)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.WriteEnvErr, err)
			return 1
		}
		if target == "" {
			target = envPath
		}
		fmt.Printf("%s %s\n", green("✓"), fmt.Sprintf(i18n.M.WroteFileFmt, displayPath(target)))
	}
	fmt.Printf("\n%s %s\n", accent("◆"), i18n.M.SetupComplete)
	return 0
}
