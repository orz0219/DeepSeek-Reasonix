package bot

import (
	"sort"
	"strings"
	"time"
)

// AdapterHealth returns a stable snapshot of all configured adapter instances.
func (gw *BotGateway) AdapterHealth() []AdapterHealthSnapshot {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	out := make([]AdapterHealthSnapshot, 0, len(gw.adapterHealth))
	for _, health := range gw.adapterHealth {
		if health == nil {
			continue
		}
		out = append(out, *health)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (gw *BotGateway) setAdapterConfigured(binding AdapterBinding) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.ensureAdapterHealthLocked(binding).Status = "configured"
}

func (gw *BotGateway) markAdapterDisabled(binding AdapterBinding) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	health := gw.ensureAdapterHealthLocked(binding)
	health.Status = "disabled"
	health.Closed = true
}

func (gw *BotGateway) markAdapterStarted(binding AdapterBinding) {
	now := time.Now()
	gw.mu.Lock()
	defer gw.mu.Unlock()
	health := gw.ensureAdapterHealthLocked(binding)
	health.Status = "running"
	health.StartedAt = now
	health.LastError = ""
	health.Closed = false
}

func (gw *BotGateway) markAdapterStartFailed(binding AdapterBinding, err error) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	health := gw.ensureAdapterHealthLocked(binding)
	health.Status = "error"
	health.Closed = true
	health.LastErrorAt = time.Now()
	if err != nil {
		health.LastError = err.Error()
	}
}

func (gw *BotGateway) markAdapterMessage(binding AdapterBinding) {
	now := time.Now()
	gw.mu.Lock()
	defer gw.mu.Unlock()
	health := gw.ensureAdapterHealthLocked(binding)
	health.Status = "running"
	health.LastMessageAt = now
	health.Messages++
	health.Closed = false
}

func (gw *BotGateway) markAdapterClosed(binding AdapterBinding) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	health := gw.ensureAdapterHealthLocked(binding)
	if health.Status == "running" {
		health.Status = "closed"
	}
	health.Closed = true
}

func (gw *BotGateway) markAdapterSend(binding AdapterBinding, err error) {
	now := time.Now()
	gw.mu.Lock()
	defer gw.mu.Unlock()
	health := gw.ensureAdapterHealthLocked(binding)
	if err != nil {
		health.SendErrors++
		health.LastErrorAt = now
		health.LastError = err.Error()
		if health.Status == "running" {
			health.Status = "degraded"
		}
		return
	}
	health.Sends++
	health.LastSendAt = now
	if health.Status == "degraded" {
		health.Status = "running"
	}
}

func (gw *BotGateway) ensureAdapterHealthLocked(binding AdapterBinding) *AdapterHealthSnapshot {
	id := strings.TrimSpace(binding.ID)
	if id == "" && binding.Adapter != nil {
		id = binding.Adapter.Name()
	}
	if id == "" {
		id = string(binding.Platform)
	}
	health := gw.adapterHealth[id]
	if health == nil {
		health = &AdapterHealthSnapshot{ID: id}
		gw.adapterHealth[id] = health
	}
	health.Platform = binding.Platform
	health.Domain = strings.TrimSpace(binding.Domain)
	if binding.Adapter != nil {
		health.Name = binding.Adapter.Name()
	}
	if strings.TrimSpace(health.Status) == "" {
		health.Status = "configured"
	}
	return health
}
