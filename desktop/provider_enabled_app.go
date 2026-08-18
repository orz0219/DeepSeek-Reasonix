package main

import (
	"fmt"

	"reasonix/internal/config"
)

func (a *App) SetProviderEnabled(names []string, enabled bool) error {
	names = uniqueNonEmptyStrings(names)
	if len(names) == 0 {
		return fmt.Errorf("set provider enabled: provider list is empty")
	}
	return a.applyConfigChange(func(c *config.Config) error {
		for _, name := range names {
			if _, ok := c.Provider(name); !ok {
				return fmt.Errorf("set provider enabled: provider %q not found", name)
			}
		}
		if enabled {
			removeProviderDisabled(c, names...)
			return nil
		}
		addProviderDisabled(c, names...)
		return nil
	})
}
