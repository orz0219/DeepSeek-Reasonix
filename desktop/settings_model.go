package main

import (
	"fmt"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
)

// SetDefaultModel sets the config default and switches the live model to it.
func (a *App) SetDefaultModel(ref string) error {
	tab := a.activeTab()
	if tab == nil {
		return fmt.Errorf("no active tab")
	}

	a.mu.Lock()
	prev := tab.model
	tab.model = ref
	a.mu.Unlock()
	if err := a.applyConfigChange(func(c *config.Config) error {
		resolved, err := selectableDesktopModelRef(c, ref)
		if err != nil {
			return err
		}
		c.DefaultModel = resolved
		a.mu.Lock()
		tab.model = resolved
		a.mu.Unlock()
		return nil
	}); err != nil {
		a.mu.Lock()
		tab.model = prev
		a.mu.Unlock()
		return err
	}
	return nil
}

// SetPlannerModel sets (or, with "", clears) the two-model planner.
func (a *App) SetPlannerModel(ref string) error {
	return a.applyConfigChange(func(c *config.Config) error {
		if ref != "" {
			resolved, err := selectableDesktopModelRef(c, ref)
			if err != nil {
				return err
			}
			ref = resolved
		}
		c.Agent.PlannerModel = ref
		return nil
	})
}

// SetSubagentModel sets (or clears) the default model used by subagent entry points.
func (a *App) SetSubagentModel(ref string) error {
	return a.applyConfigChange(func(c *config.Config) error {
		ref = strings.TrimSpace(ref)
		if ref != "" {
			resolved, err := selectableDesktopModelRef(c, ref)
			if err != nil {
				return err
			}
			ref = resolved
		}
		c.Agent.SubagentModel = ref
		return nil
	})
}

func selectableDesktopModelRef(c *config.Config, ref string) (string, error) {
	entry, ok := c.ResolveModel(ref)
	if !ok {
		return "", fmt.Errorf("unknown model %q", ref)
	}
	if !modelProviderAccessAllowed(c.Desktop.ProviderAccess, entry.Name) {
		return "", fmt.Errorf("model %q is not available because provider %q is not added", ref, entry.Name)
	}
	if !entry.Configured() {
		return "", fmt.Errorf("model %q is not available because provider %q has no key", ref, entry.Name)
	}
	return entry.Name + "/" + entry.Model, nil
}

// SetSubagentEffort sets (or clears) the default effort used by subagent entry points.
func (a *App) SetSubagentEffort(level string) error {
	return a.applyConfigChange(func(c *config.Config) error {
		level = strings.TrimSpace(level)
		if level == "" || level == "auto" {
			c.Agent.SubagentEffort = ""
			return nil
		}
		model := strings.TrimSpace(c.Agent.SubagentModel)
		if model == "" {
			model = c.DefaultModel
		}
		entry, ok := c.ResolveModel(model)
		if !ok {
			return fmt.Errorf("unknown subagent model %q", model)
		}
		effort, err := config.NormalizeEffort(entry, level)
		if err != nil {
			return err
		}
		c.Agent.SubagentEffort = effort
		return nil
	})
}

// deleteSubagentOverrideAliases removes every underscore/hyphen alias entry
// for name (boot.SubagentModelKeys — the same key set runtime dispatch
// reads). Deleting only the exact key would leave a legacy alias entry (e.g.
// `security_review` for the security-review skill) silently active.
func deleteSubagentOverrideAliases(overrides map[string]string, name string) {
	for _, key := range boot.SubagentModelKeys(name) {
		delete(overrides, key)
	}
}

// SetSubagentProfileModel sets (or clears) a per-name model override for a
// subagent — the only way to influence a built-in subagent's model in the
// Subagents settings page, since built-ins have no editable frontmatter file
// to carry a `model:` line. Writes into the same cfg.Agent.SubagentModels map
// internal/boot's subagentModelRef already reads at dispatch time. Set and
// clear both sweep the underscore/hyphen alias keys so a legacy alias entry
// can neither shadow the new value nor survive a clear.
func (a *App) SetSubagentProfileModel(name, ref string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return a.applyConfigChange(func(c *config.Config) error {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			deleteSubagentOverrideAliases(c.Agent.SubagentModels, name)
			return nil
		}
		resolved, err := selectableDesktopModelRef(c, ref)
		if err != nil {
			return err
		}
		if c.Agent.SubagentModels == nil {
			c.Agent.SubagentModels = map[string]string{}
		}
		deleteSubagentOverrideAliases(c.Agent.SubagentModels, name)
		c.Agent.SubagentModels[name] = resolved
		return nil
	})
}

// SetSubagentProfileEffort sets (or clears) a per-name effort override. See
// SetSubagentProfileModel.
func (a *App) SetSubagentProfileEffort(name, level string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return a.applyConfigChange(func(c *config.Config) error {
		level = strings.TrimSpace(level)
		if level == "" || level == "auto" {
			deleteSubagentOverrideAliases(c.Agent.SubagentEfforts, name)
			return nil
		}

		model := subagentOverrideFor(c.Agent.SubagentModels, name)
		if model == "" {
			model = strings.TrimSpace(c.Agent.SubagentModel)
		}
		if model == "" {
			model = c.DefaultModel
		}
		entry, ok := c.ResolveModel(model)
		if !ok {
			return fmt.Errorf("unknown subagent model %q", model)
		}
		effort, err := config.NormalizeEffort(entry, level)
		if err != nil {
			return err
		}
		if c.Agent.SubagentEfforts == nil {
			c.Agent.SubagentEfforts = map[string]string{}
		}
		deleteSubagentOverrideAliases(c.Agent.SubagentEfforts, name)
		c.Agent.SubagentEfforts[name] = effort
		return nil
	})
}

func desktopMaxSubagentDepth(depth int) int {
	if depth <= 0 {
		return agent.DefaultMaxSubagentDepth
	}
	if depth == 1 {
		return 1
	}
	return agent.DefaultMaxSubagentDepth
}

// SetMaxSubagentDepth controls whether first-layer subagents may delegate once more.
func (a *App) SetMaxSubagentDepth(depth int) error {
	return a.applyConfigChange(func(c *config.Config) error {
		c.Agent.MaxSubagentDepth = desktopMaxSubagentDepth(depth)
		return nil
	})
}

func desktopSubagentConcurrency(n int) int {
	total, _ := agent.NormalizeConcurrencyLimits(n, 0)
	return total
}

func desktopParallelWriters(writers, total int) int {
	_, w := agent.NormalizeConcurrencyLimits(total, writers)
	return w
}

// SetMaxSubagentConcurrency sets the session-wide sub-agent concurrency cap (1–32).
func (a *App) SetMaxSubagentConcurrency(n int) error {
	return a.applyConfigChange(func(c *config.Config) error {
		total, writers := agent.NormalizeConcurrencyLimits(n, c.Agent.MaxParallelWriters)
		c.Agent.MaxSubagentConcurrency = total
		c.Agent.MaxParallelWriters = writers
		return nil
	})
}

// SetMaxParallelWriters sets the concurrent writer cap (1–32, ≤ total concurrency).
func (a *App) SetMaxParallelWriters(n int) error {
	return a.applyConfigChange(func(c *config.Config) error {
		total, writers := agent.NormalizeConcurrencyLimits(c.Agent.MaxSubagentConcurrency, n)
		c.Agent.MaxSubagentConcurrency = total
		c.Agent.MaxParallelWriters = writers
		return nil
	})
}

// SetAutoPlan is retained for older frontend bundles. Automatic plan mode is
// retired, so "off" is an idempotent compatibility call and enabling it is
// rejected without mutating user configuration or live controllers.
func (a *App) SetAutoPlan(mode string) error {
	return config.Default().SetAutoPlan(mode)
}

// SetDefaultToolApprovalMode updates the global Ask/Auto/YOLO default used only
// for newly-created desktop sessions. Existing tabs keep their persisted mode.
func (a *App) SetDefaultToolApprovalMode(mode string) error {
	return a.applyConfigOnly(func(c *config.Config) error {
		return c.SetDesktopDefaultToolApprovalMode(mode)
	})
}

// SetDefaultAutoRecoveryCheckpoint is retained as a no-op Wails surface for
// older generated frontends. Auto Guard is always built into Auto.
func (a *App) SetDefaultAutoRecoveryCheckpoint(_ bool) error { return nil }
