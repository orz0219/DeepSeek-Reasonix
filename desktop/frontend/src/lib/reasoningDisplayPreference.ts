import { useSyncExternalStore } from "react";

export type ReasoningFoldBehavior = "open" | "half" | "closed";
export type ResolvedReasoningFoldBehavior = ReasoningFoldBehavior | "pending";

const LEGACY_SUMMARY_KEY = "reasonix-reasoning-summary";
const DISPLAY_EVENT = "reasonix:reasoning-fold-behavior";

let currentMode: ResolvedReasoningFoldBehavior = "open";
let currentModeExplicit = false;
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) listener();
  if (typeof window !== "undefined") window.dispatchEvent(new CustomEvent(DISPLAY_EVENT, { detail: currentMode }));
}

function normalizeMode(value: unknown): ReasoningFoldBehavior | undefined {
  if (value === "open" || value === "half" || value === "closed") return value;
  // Legacy display modes (hidden/summary/auto) map to closed — thinking stays
  // at least glanceable instead of forcing the new fully-open default on users
  // who explicitly chose a more collapsed presentation before.
  if (value === "hidden" || value === "summary" || value === "auto") return "closed";
  return undefined;
}

function legacySummaryValue(): "on" | "off" | undefined {
  if (typeof localStorage === "undefined") return undefined;
  const stored = localStorage.getItem(LEGACY_SUMMARY_KEY);
  if (stored === "1") return "on";
  if (stored === "0") return "off";
  return undefined;
}

export function resolveReasoningFoldBehavior(
  configuredMode: unknown,
  explicit: boolean,
): ResolvedReasoningFoldBehavior {
  const normalized = normalizeMode(configuredMode);
  if (explicit && normalized) return normalized;
  if (legacySummaryValue() !== undefined) return "closed";
  return normalized ?? "open";
}

export function getReasoningFoldBehavior(): ResolvedReasoningFoldBehavior {
  if (!currentModeExplicit && currentMode !== "pending") {
    if (legacySummaryValue() !== undefined) return "closed";
  }
  return currentMode;
}

export function setReasoningFoldBehaviorPending(): void {
  if (currentMode === "pending") return;
  currentMode = "pending";
  currentModeExplicit = false;
  emit();
}

/** Hydrates the frontend mirror from the authoritative Wails startup payload. */
export function hydrateReasoningFoldBehavior(configuredMode: unknown, explicit = false): void {
  const next = resolveReasoningFoldBehavior(configuredMode, explicit);
  if (next === currentMode && currentModeExplicit === explicit) return;
  currentMode = next;
  currentModeExplicit = explicit;
  emit();
}

/** Applies a successfully persisted user selection and completes legacy migration. */
export function applyReasoningFoldBehavior(mode: ReasoningFoldBehavior): void {
  if (typeof localStorage !== "undefined") localStorage.removeItem(LEGACY_SUMMARY_KEY);
  if (mode === currentMode && currentModeExplicit) return;
  currentMode = mode;
  currentModeExplicit = true;
  emit();
}

export function onReasoningFoldBehaviorChange(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

export function useReasoningFoldBehavior(): ResolvedReasoningFoldBehavior {
  return useSyncExternalStore(onReasoningFoldBehaviorChange, getReasoningFoldBehavior, getReasoningFoldBehavior);
}
