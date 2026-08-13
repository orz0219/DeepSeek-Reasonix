import type { WireTool } from "./types";
import { State, Item } from "./controller_state";
export type ToolStatus = "running" | "done" | "error" | "stopped";
// Reserved ToolProgress channel names for sub-agent progress previews (the Go
// tracker emits these; ordinary tool progress must never use them).
export const SUBAGENT_PROGRESS_STATUS = "reasonix.subagent.status";
export const SUBAGENT_PROGRESS_REASONING = "reasonix.subagent.reasoning";
export const SUBAGENT_PROGRESS_TEXT = "reasonix.subagent.text";
export const SUBAGENT_PROGRESS_NOTICE = "reasonix.subagent.notice";
// Reserved names are matched by prefix so a future channel never falls back
// to ordinary tool output on older frontends.
const SUBAGENT_PROGRESS_PREFIX = "reasonix.subagent.";
export const TURN_ACTIVITY_KINDS = new Set(["turn_started", "text", "reasoning", "message", "tool_dispatch", "tool_progress", "tool_result"]);
const SUBAGENT_PROGRESS_PHASES = new Set([
    "queued", "running", "reasoning", "responding", "tool", "retrying", "completed", "failed", "cancelled",
]);
// Tool names that initialize a sub-agent progress card. parallel_tasks/fleet
// are group cards: they settle when their whole child progress tree is
// terminal, since they never receive a terminal status of their own.
export const SUBAGENT_PROGRESS_TOOLS = new Set(["task", "read_only_task", "parallel_tasks", "fleet"]);
// Per-channel preview retention. The backend already bounds what it sends
// (8 KiB pending per child); these caps keep one hot card from dominating the
// live conversation memory.
const SUBAGENT_PREVIEW_REASONING_LIMIT = 8 << 10;
const SUBAGENT_PREVIEW_TEXT_LIMIT = 8 << 10;
const SUBAGENT_PREVIEW_NOTICE_LIMIT = 2 << 10;
export type SubagentPhase = "queued" | "running" | "reasoning" | "responding" | "tool" | "retrying" | "completed" | "failed" | "cancelled";
// In-memory-only sub-agent progress preview. Never persisted: history
// hydration rebuilds tool items from the transcript without these fields, and
// the full sub-agent transcript stays the source of truth after a restart.
export type SubagentProgress = {
    phase: SubagentPhase;
    reasoning: string;
    text: string;
    notice: string;
    lastActivityAt: number;
    truncated: boolean;
    durationMs?: number;
    startedAt: number;
};
export function isSubagentProgressName(name: string | undefined): boolean {
    return !!name && name.startsWith(SUBAGENT_PROGRESS_PREFIX);
}
export function isTerminalSubagentPhase(phase: string | undefined): boolean {
    return phase === "completed" || phase === "failed" || phase === "cancelled";
}
export function isGroupSubagentTool(name: string): boolean {
    return name === "parallel_tasks" || name === "fleet";
}
export function terminalStatusOf(phase: string): ToolStatus {
    switch (phase) {
        case "completed": return "done";
        case "failed": return "error";
        case "cancelled": return "stopped";
    }
    return "running";
}
export function freshSubagentProgress(): SubagentProgress {
    const now = Date.now();
    return { phase: "running", reasoning: "", text: "", notice: "", lastActivityAt: now, truncated: false, startedAt: now };
}
/** Keeps the most recent `limit` code points; surrogate pairs stay intact. */
function tailPreview(text: string, limit: number): string {
    if (text.length <= limit)
        return text;
    const pts = Array.from(text);
    return pts.slice(pts.length - limit).join("");
}
// --- Sub-agent progress reducer helpers --------------------------------------
// Applies one reserved ToolProgress event to the target card's in-memory
// preview. The card must exist (its dispatch always precedes progress events)
// and have been initialized by the dispatch. Never writes tool.output, never
// touches the parent LiveStream, and never produces history data.
export function applySubagentProgress(s: State, t: WireTool): State {
    if (!t.id)
        return s;
    const idx = s.items.findIndex((it) => it.kind === "tool" && it.id === t.id);
    if (idx < 0)
        return s;
    const next = [...s.items];
    const it = next[idx];
    if (it.kind !== "tool" || !it.subagentProgress)
        return s;
    const sp: SubagentProgress = { ...it.subagentProgress, lastActivityAt: Date.now() };
    switch (t.name) {
        case SUBAGENT_PROGRESS_STATUS: {
            const phase = t.output ?? "";
            if (!SUBAGENT_PROGRESS_PHASES.has(phase))
                return s; // unknown phase: ignore
            sp.phase = phase as SubagentPhase;
            if (isTerminalSubagentPhase(phase) && typeof t.durationMs === "number")
                sp.durationMs = t.durationMs;
            break;
        }
        case SUBAGENT_PROGRESS_REASONING:
            sp.reasoning = tailPreview(sp.reasoning + (t.output ?? ""), SUBAGENT_PREVIEW_REASONING_LIMIT);
            sp.truncated = sp.truncated || !!t.truncated;
            break;
        case SUBAGENT_PROGRESS_TEXT:
            sp.text = tailPreview(sp.text + (t.output ?? ""), SUBAGENT_PREVIEW_TEXT_LIMIT);
            sp.truncated = sp.truncated || !!t.truncated;
            break;
        case SUBAGENT_PROGRESS_NOTICE:
            sp.notice = tailPreview(sp.notice + (t.output ?? ""), SUBAGENT_PREVIEW_NOTICE_LIMIT);
            sp.truncated = sp.truncated || !!t.truncated;
            break;
        default:
            return s;
    }
    const status = isTerminalSubagentPhase(sp.phase) ? terminalStatusOf(sp.phase) : it.status;
    next[idx] = { ...it, subagentProgress: sp, status };
    return { ...s, items: next };
}
// Nested real tool activity refreshes its sub-agent parent's recent activity
// and switches the phase to "tool". Terminal parents are left untouched.
export function touchSubagentParent(next: Item[], parentId: string): void {
    const idx = next.findIndex((it) => it.kind === "tool" && it.id === parentId && it.subagentProgress);
    if (idx < 0)
        return;
    const it = next[idx];
    if (it.kind !== "tool" || !it.subagentProgress || isTerminalSubagentPhase(it.subagentProgress.phase))
        return;
    next[idx] = { ...it, subagentProgress: { ...it.subagentProgress, phase: "tool", lastActivityAt: Date.now() } };
}

