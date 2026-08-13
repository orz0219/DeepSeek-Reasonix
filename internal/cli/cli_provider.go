package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/i18n"

	"golang.org/x/term"
)

// providerFamily is a wizard-only grouping of provider SKUs by vendor; it does
// not exist in config because users editing reasonix.toml deal with SKU names
// directly.

type providerPromptResult struct {
	entries     []config.ProviderEntry
	credentials map[string]string
}

func newProviderPromptResult(entries []config.ProviderEntry, key, value string) providerPromptResult {
	result := providerPromptResult{entries: entries}
	if key != "" && value != "" {
		result.credentials = map[string]string{key: value}
	}
	return result
}

// promptCustomProvider handles the custom provider entry flow.
func promptCustomProvider() (providerPromptResult, error) {
	methodIdx, err := selectOne(i18n.M.CustomAddMethodLabel, []menuItem{
		{name: i18n.M.CustomMethodManual},
		{name: i18n.M.CustomMethodURL},
	})
	if err != nil {
		return providerPromptResult{}, err
	}
	if methodIdx == 0 {
		return promptCustomProviderManual()
	}
	return promptCustomProviderFromURL()
}

// promptCustomProviderManual handles manual model entry.
func promptCustomProviderManual() (providerPromptResult, error) {
	return promptCustomProviderManualWith(bufio.NewScanner(os.Stdin), "", "", "")
}

// promptCustomProviderManualWith is the shared backend for manual entry.
// Pre-filled values (baseURL, keyEnv, apiKey) are reused as-is when non-empty
// so the URL-fetch flow can fall through to manual entry without re-asking
// the user for information they've already typed. An empty apiKey is allowed
// — the key step happens later in the wizard and Reasonix's global .env is updated then.
func promptCustomProviderManualWith(in *bufio.Scanner, baseURL, keyEnv, apiKey string) (providerPromptResult, error) {
	fmt.Println()
	if baseURL == "" {
		baseURL = ask(in, os.Stdout, i18n.M.CustomPromptBaseURL, "")
		if baseURL == "" {
			return providerPromptResult{}, fmt.Errorf("base URL is required")
		}
	}
	providerName := providerSlug("custom", baseURL)
	modelName := ask(in, os.Stdout, i18n.M.CustomPromptModel, "")
	if modelName == "" {
		return providerPromptResult{}, fmt.Errorf("model name is required")
	}
	if keyEnv == "" {
		keyEnv = promptAPIKeyEnvName(in, os.Stdout, i18n.M.CustomPromptKeyEnv, apiKeyEnvFromProviderName(providerName))
	} else if !config.IsValidCredentialKey(keyEnv) {
		return providerPromptResult{}, fmt.Errorf("invalid API key variable name %q", keyEnv)
	}
	if apiKey == "" {
		apiKey = ask(in, os.Stdout, i18n.M.CustomPromptAPIKey, "")
	}
	entry := config.ProviderEntry{
		Name: providerName, Kind: "openai", BaseURL: baseURL,
		Model: modelName, APIKeyEnv: keyEnv, ContextWindow: askContextWindow(in, os.Stdout),
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.CustomAddedFmt, entry.Name+"/"+modelName)))
	return newProviderPromptResult([]config.ProviderEntry{entry}, keyEnv, apiKey), nil
}

// promptCustomProviderFromURL tries the OpenAI-compatible GET /models
// endpoint and shows a checkbox of the returned models. If the call fails
// (network error, auth failure, or a vendor without /models) it falls
// through to manual entry, reusing the URL and key the user already typed.
func promptCustomProviderFromURL() (providerPromptResult, error) {
	in := bufio.NewScanner(os.Stdin)
	fmt.Println()

	baseURL := ask(in, os.Stdout, i18n.M.CustomPromptBaseURL, "")
	if baseURL == "" {
		return providerPromptResult{}, fmt.Errorf("base URL is required")
	}
	providerName := providerSlug("custom", baseURL)
	keyEnv := promptAPIKeyEnvName(in, os.Stdout, i18n.M.CustomPromptKeyEnv, apiKeyEnvFromProviderName(providerName))
	apiKey := ask(in, os.Stdout, i18n.M.CustomPromptAPIKey, "")

	fmt.Printf("  %s\n", dim(fmt.Sprintf(i18n.M.FetchingModelsFmt, "custom")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := fetchModelListCompat(ctx, baseURL, apiKey)
	if err != nil || len(models) == 0 {
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf(i18n.M.FetchModelsFailedFmt, "custom", err)))
		} else {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(i18n.M.CustomFetchEmpty))
		}
		return promptCustomProviderManualWith(in, baseURL, keyEnv, apiKey)
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.FetchModelsSuccessFmt, len(models), "custom")))

	items := make([]menuItem, len(models))
	for i, m := range models {
		items[i] = menuItem{name: m}
	}
	idxs, err := selectMany(fmt.Sprintf(i18n.M.SelectModelsLabel, "custom"), items)
	if err != nil || len(idxs) == 0 {
		return providerPromptResult{}, fmt.Errorf("no models selected")
	}
	var selected []string
	for _, i := range idxs {
		selected = append(selected, models[i])
	}
	entry := config.ProviderEntry{
		Name: providerName, Kind: "openai", BaseURL: baseURL,
		Models: selected, Model: selected[0], APIKeyEnv: keyEnv, ContextWindow: askContextWindow(in, os.Stdout),
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.CustomAddedFmt, entry.Name+"/"+selected[0])))
	return newProviderPromptResult([]config.ProviderEntry{entry}, keyEnv, apiKey), nil
}

// promptAnthropicProvider handles the Anthropic compatible provider entry flow.
func promptAnthropicProvider() (providerPromptResult, error) {
	methodIdx, err := selectOne(i18n.M.AnthropicAddMethodLabel, []menuItem{
		{name: i18n.M.AnthropicMethodManual},
		{name: i18n.M.AnthropicMethodURL},
	})
	if err != nil {
		return providerPromptResult{}, err
	}
	if methodIdx == 0 {
		return promptAnthropicProviderManual()
	}
	return promptAnthropicProviderFromURL()
}

// promptAnthropicProviderManual handles manual model entry.
func promptAnthropicProviderManual() (providerPromptResult, error) {
	return promptAnthropicProviderManualWith(bufio.NewScanner(os.Stdin), "", "", "")
}

// promptAnthropicProviderManualWith is the shared backend for manual entry
// of an Anthropic-compatible custom provider. Pre-filled values (baseURL,
// keyEnv, apiKey) are reused as-is when non-empty so the URL-fetch flow
// can fall through to manual entry without re-asking the user.
func promptAnthropicProviderManualWith(in *bufio.Scanner, baseURL, keyEnv, apiKey string) (providerPromptResult, error) {
	fmt.Println()
	if baseURL == "" {
		baseURL = ask(in, os.Stdout, i18n.M.AnthropicPromptBaseURL, "")
		if baseURL == "" {
			return providerPromptResult{}, fmt.Errorf("base URL is required")
		}
	}
	modelName := ask(in, os.Stdout, i18n.M.AnthropicPromptModel, "")
	if modelName == "" {
		return providerPromptResult{}, fmt.Errorf("model name is required")
	}
	if keyEnv == "" {
		keyEnv = promptAPIKeyEnvName(in, os.Stdout, i18n.M.AnthropicPromptKeyEnv, "ANTHROPIC_API_KEY")
	} else if !config.IsValidCredentialKey(keyEnv) {
		return providerPromptResult{}, fmt.Errorf("invalid API key variable name %q", keyEnv)
	}
	if apiKey == "" {
		apiKey = ask(in, os.Stdout, i18n.M.AnthropicPromptAPIKey, "")
	}
	entry := config.ProviderEntry{
		Name: providerSlug("anthropic", baseURL), Kind: "anthropic", BaseURL: baseURL,
		Model: modelName, APIKeyEnv: keyEnv, ContextWindow: askContextWindow(in, os.Stdout),
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.AnthropicAddedFmt, entry.Name+"/"+modelName)))
	return newProviderPromptResult([]config.ProviderEntry{entry}, keyEnv, apiKey), nil
}

// promptAnthropicProviderFromURL tries the OpenAI-compatible GET /models
// endpoint (some Anthropic-compatible proxies do expose one). Most don't
// — Anthropic's own API has no public model list — so on any failure the
// flow falls through to manual entry with the URL/key already filled in,
// rather than aborting the wizard.
func promptAnthropicProviderFromURL() (providerPromptResult, error) {
	in := bufio.NewScanner(os.Stdin)
	fmt.Println()

	baseURL := ask(in, os.Stdout, i18n.M.AnthropicPromptBaseURL, "")
	if baseURL == "" {
		return providerPromptResult{}, fmt.Errorf("base URL is required")
	}
	keyEnv := promptAPIKeyEnvName(in, os.Stdout, i18n.M.AnthropicPromptKeyEnv, "ANTHROPIC_API_KEY")
	apiKey := ask(in, os.Stdout, i18n.M.AnthropicPromptAPIKey, "")

	fmt.Printf("  %s\n", dim(fmt.Sprintf(i18n.M.AnthropicFetchingModelsFmt, "anthropic")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := fetchModelListCompat(ctx, baseURL, apiKey)
	if err != nil || len(models) == 0 {
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf(i18n.M.AnthropicFetchModelsFailedFmt, "anthropic", err)))
		} else {
			fmt.Fprintf(os.Stderr, "  %s\n", dim(i18n.M.AnthropicFetchEmpty))
		}
		return promptAnthropicProviderManualWith(in, baseURL, keyEnv, apiKey)
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.AnthropicFetchModelsSuccessFmt, len(models), "anthropic")))

	items := make([]menuItem, len(models))
	for i, m := range models {
		items[i] = menuItem{name: m}
	}
	idxs, err := selectMany(fmt.Sprintf(i18n.M.AnthropicSelectModelsLabel, "anthropic"), items)
	if err != nil || len(idxs) == 0 {
		return providerPromptResult{}, fmt.Errorf("no models selected")
	}
	var selected []string
	for _, i := range idxs {
		selected = append(selected, models[i])
	}
	entry := config.ProviderEntry{
		Name: providerSlug("anthropic", baseURL), Kind: "anthropic", BaseURL: baseURL,
		Models: selected, Model: selected[0], APIKeyEnv: keyEnv, ContextWindow: askContextWindow(in, os.Stdout),
	}
	fmt.Printf("  %s\n", green(fmt.Sprintf(i18n.M.AnthropicAddedFmt, entry.Name+"/"+selected[0])))
	return newProviderPromptResult([]config.ProviderEntry{entry}, keyEnv, apiKey), nil
}

// ask prints a prompt to w and returns the entered line, or def if input is empty.
func ask(in *bufio.Scanner, w io.Writer, label, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(w, "%s: ", label)
	}
	if !in.Scan() {
		return def
	}
	if v := strings.TrimSpace(in.Text()); v != "" {
		return v
	}
	return def
}

// isInteractive reports whether we're attached to a real terminal on both
// stdin and stdout — required for prompting. Redirected or piped I/O is not
// interactive, so wizards never block or auto-default in scripts and CI.
func isInteractive() bool {
	return isTTY(os.Stdin) && isTTY(os.Stdout)
}

func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// appendEnv merges KEY=value lines into a .env file. Existing assignments of
// any key that's about to be written are dropped first, then the new values
// are appended — so re-running `reasonix setup` with a corrected key replaces the
// stale one instead of stacking duplicates. The new values are also
// pinned into the current process env so a chat session started right after
// init picks up the fresh keys without a restart.

// readStdin reads piped input if present; an interactive terminal yields "".
func readStdin() string {
	stat, err := os.Stdin.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	data, _ := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(data))
}

func usage() {
	fmt.Print(i18n.M.UsageBody)
}

type ctrlKillerAdapter struct{ ctrl *control.Controller }

func (a ctrlKillerAdapter) Kill(sessionID, id string) bool {
	if sessionID != "" && agent.BranchID(a.ctrl.SessionPath()) != sessionID {
		return false
	}
	return a.ctrl.CancelJob(id)
}
