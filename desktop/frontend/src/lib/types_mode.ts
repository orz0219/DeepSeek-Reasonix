export type CollaborationMode = "normal" | "plan" | "goal";
export type ToolApprovalMode = "ask" | "auto" | "yolo";
// TokenMode is the dual-write wire value for Agent role settings (角色设定).
// Canonical product ids are light|balanced|delivery; economy/full remain one
// compatibility version of persisted/API values.
export type TokenMode = "full" | "economy" | "delivery" | "light" | "balanced";
export type AgentPreset = "light" | "balanced" | "delivery";
export type GoalStatus = "running" | "complete" | "blocked" | "stopped";
// Optional Goal runtime summary; absent for old hosts or when no goal is active.
export interface GoalRuntime {
    turnsUsed: number;
    turnsLimit: number; // Deprecated: Goal exposes no turn limit and returns 0.
    tokensUsed: number;
    requestsUsed?: number;
    workDurationMs?: number;
    /** @deprecated Goal has no hard token limit; retained as 0 for old hosts/clients. */
    tokensLimit: number;
    noProgressTurns: number;
    /** @deprecated No longer enforced; retained for old hosts/clients. */
    noProgressLimit: number;
    lastReason?: string;
    stopCause?: string;
    budgetExtensions: number; // Deprecated: resumes no longer extend a numeric quota.
}
export function normalizeCollaborationMode(mode?: string, goal?: string, legacyMode?: Mode): CollaborationMode {
    if (mode === "plan" || mode === "goal" || mode === "normal")
        return mode;
    if (legacyMode && modeHasPlan(legacyMode))
        return "plan";
    if ((goal ?? "").trim())
        return "goal";
    return "normal";
}
export function normalizeToolApprovalMode(mode?: string, legacyMode?: Mode, legacyAutoApproveTools?: boolean, fallbackMode?: ToolApprovalMode): ToolApprovalMode {
    const normalized = typeof mode === "string" ? mode.trim().toLowerCase() : "";
    if (normalized === "auto" || normalized === "yolo" || normalized === "ask")
        return normalized as ToolApprovalMode;
    if (legacyAutoApproveTools || (legacyMode && modeHasAutoApproveTools(legacyMode)))
        return "yolo";
    if (fallbackMode === "auto" && normalized === "")
        return "auto";
    return "ask";
}
export function normalizeTokenMode(mode?: string): TokenMode {
    const m = (mode ?? "").trim().toLowerCase();
    if (m === "economy" || m === "light" || m === "lite" || m === "eco")
        return "economy";
    if (m === "delivery" || m === "deliver" || m === "quality")
        return "delivery";
    // balanced | full | empty | unknown → balanced wire value "full"
    return "full";
}
/** Canonical product id for the three Agent role settings. */
export function normalizeAgentPreset(mode?: string): AgentPreset {
    const wire = normalizeTokenMode(mode);
    if (wire === "economy" || wire === "light")
        return "light";
    if (wire === "delivery")
        return "delivery";
    return "balanced";
}
export function tokenModeFromAgentPreset(preset: AgentPreset): TokenMode {
    switch (preset) {
        case "light":
            return "economy";
        case "delivery":
            return "delivery";
        default:
            return "full";
    }
}
// Mode is the compatibility string for two independent composer axes:
// plan (plan-first workflow) and yolo (tool auto-approval).
export type Mode = "normal" | "plan" | "yolo" | "plan-yolo";
export function normalizeMode(mode?: string): Mode {
    if (mode === "plan" || mode === "yolo" || mode === "plan-yolo" || mode === "yolo-plan") {
        return mode === "yolo-plan" ? "plan-yolo" : mode;
    }
    return "normal";
}
export function modeHasPlan(mode: Mode): boolean {
    return mode === "plan" || mode === "plan-yolo";
}
export function modeHasAutoApproveTools(mode: Mode): boolean {
    return mode === "yolo" || mode === "plan-yolo";
}
export function modeFromAxes(plan: boolean, autoApproveTools: boolean): Mode {
    if (plan && autoApproveTools)
        return "plan-yolo";
    if (plan)
        return "plan";
    if (autoApproveTools)
        return "yolo";
    return "normal";
}
export function modeWithPlan(mode: Mode, plan: boolean): Mode {
    return modeFromAxes(plan, modeHasAutoApproveTools(mode));
}
export function modeWithAutoApproveTools(mode: Mode, autoApproveTools: boolean): Mode {
    return modeFromAxes(modeHasPlan(mode), autoApproveTools);
}
export interface CommandInfo {
    name: string; // without the leading slash
    description: string;
    hint?: string;
    kind: "builtin" | "custom" | "mcp" | "skill" | "subagent";
    group?: "actions" | "management" | "subagents" | "skills" | "integrations";
    plugin?: string;
    color?: string;
}
export interface DirEntry {
    name: string;
    path?: string;
    isDir: boolean;
    displayName?: string;
    displayPath?: string;
}
export interface DroppedItem {
    kind: "workspace" | "attachment";
    path: string;
    isDir?: boolean;
    displayPath?: string;
    previewUrl?: string;
}
export interface FilePreview {
    path: string;
    body: string;
    size: number;
    truncated: boolean;
    binary: boolean;
    kind?: "image" | "pdf";
    mime?: string;
    url?: string;
    err?: string;
}
export interface WorkspaceChangeView {
    path: string;
    oldPath?: string;
    sources: string[];
    gitStatus?: string;
    turns?: number[];
    latestPrompt?: string;
    latestTime?: number;
    canSessionRevert?: boolean;
}
export interface WorkspaceChangesView {
    files: WorkspaceChangeView[];
    gitAvailable: boolean;
    gitErr?: string;
    gitBranch?: string;
}
export interface WorkspaceChangeDetailView {
    diff?: string;
    source?: "git" | "session";
    added?: number;
    removed?: number;
    binary?: boolean;
    truncated?: boolean;
}
export interface GitCommitView {
    hash: string;
    author: string;
    date: string;
    message: string;
}
export interface GitCommitDetailView {
    diff?: string;
    files?: string[];
}
export interface ComposerInsertRequest {
    id: number;
    text: string;
    mode?: "insert" | "replace" | "prefix";
}

