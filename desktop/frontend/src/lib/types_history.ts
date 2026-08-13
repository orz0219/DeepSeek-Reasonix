// Wire contract — mirrors desktop/wire.go (itself mirroring internal/serve/wire.go).
// One event channel carries every kind; `kind` discriminates the payload.
import type { Todo } from "./tools";
import type { ContextMaintenanceInfo } from "./contextMaintenanceTypes";
import { MemoryCitation, WireShellExecution, WireDecisionReceipt, CostQuote } from "./types_events";
import { TabMeta, SessionRuntimeView } from "./types_workspace";
import { CollaborationMode, ToolApprovalMode, TokenMode, AgentPreset, GoalStatus, GoalRuntime } from "./types_mode";
export interface UsageSourceStats {
    promptTokens: number;
    completionTokens: number;
    totalTokens: number;
    reasoningTokens: number;
    cacheHitTokens: number;
    cacheMissTokens: number;
    estimated?: boolean;
    requestCount: number;
    sessionCost?: number;
    sessionCurrency?: string;
    sessionCostUsd?: number;
}
export interface ReadFileRecord {
    path: string;
    turn: number;
    time: number;
    offset?: number;
    limit?: number;
    truncated?: boolean;
}
export interface ChangedFileInfo {
    path: string;
    oldPath?: string;
    sources: string[];
    gitStatus?: string;
    turns: number[];
    latestPrompt?: string;
    latestTime?: number;
}
// Bound-method payloads (desktop/app.go).
export interface HistoryMessage {
    role: string;
    content: string;
    detail?: string;
    code?: string;
    submitText?: string;
    checkpointTurn?: number;
    createdAt?: number;
    reasoning?: string;
    workDurationMs?: number;
    memoryCitations?: MemoryCitation[];
    level?: "info" | "warn";
    toolCalls?: HistoryToolCall[];
    toolCallId?: string;
    toolName?: string;
    toolResultArchived?: boolean;
    toolResultError?: string;
    execution?: WireShellExecution;
    pending?: boolean;
    trigger?: string;
    messages?: number;
    summary?: string;
    archive?: string;
    decisionReceipt?: WireDecisionReceipt;
}
export interface HistoryToolCall {
    id: string;
    name: string;
    arguments: string;
    resolvedName?: string;
    capabilityId?: string;
    resolvedReadOnly?: boolean;
    subject?: string;
    summary?: string;
    diff?: string;
    added?: number;
    removed?: number;
    argumentsArchived?: boolean;
}
export interface HistoryPage {
    messages: HistoryMessage[];
    startTurn: number;
    endTurn: number;
    totalTurns: number;
    hasOlder: boolean;
    revision?: number;
    digest?: string;
}
// ── Windowed history paging (desktop/history_slice.go) ──────────────────────
// HistorySliceForTab pages toward older history with an opaque cursor; the
// first call uses cursor "" for the newest page. Entry IDs are stable for the
// life of a session revision (s<file>:r<epoch>:m<msgIndex>:o<subOrder>).
export interface HistorySliceRequest {
    cursor: string; // "" = newest page; pass nextCursor to page older
    turns?: number;
    entries?: number;
    bytes?: number;
}
// HistoryContentRef marks a string field replaced inline by a ≤4KiB preview;
// the full value is fetchable in chunks via HistoryContentForTab.
export interface HistoryContentRef {
    entryId: string;
    field: string; // content|reasoning|submitText|detail|code|summary|archive|toolResultError|toolArguments|toolSubject|toolSummary|toolDiff
    size: number;
    chunks: number;
    toolCallId?: string;
    revision: number;
    revKnown?: boolean;
    digest: string;
}
export interface HistoryEntry {
    entryId: string;
    turn: number; // 1-based visible turn (0 = before the first turn)
    order: number; // absolute provider-message index
    message: HistoryMessage;
    refs: HistoryContentRef[];
}
export interface SessionClearResult {
    sessionPath: string;
    sessionRevision?: number;
    sessionDigest?: string;
    sessionGeneration: number;
}
export interface HistorySlice {
    entries: HistoryEntry[];
    nextCursor: string; // toward older; empty when none
    hasOlder: boolean;
    totalTurns: number;
    startTurn: number;
    endTurn: number;
    stale: boolean; // cursor bound to an older session revision: discard + reload
    revision: number;
    revisionKnown?: boolean;
    digest?: string;
    // Diagnostic read path: index|scan|event-log|live-index|live-fallback.
    source?: string;
    error?: string; // failed read; empty entries alone are not an error
}
export interface HistoryContentChunk {
    entryId: string;
    field: string;
    chunk: number;
    chunks: number;
    data: string;
    done: boolean;
    stale: boolean;
}
// ── Two-phase topic activation (desktop/topic_activation.go) ────────────────
export interface TopicActivationRequest {
    scope: string;
    workspaceRoot: string;
    topicId: string;
    sessionPath: string;
    requestId?: string;
}
export interface TopicActivationTicket {
    requestId: string;
    tabId: string;
    meta: TabMeta;
}
export type TopicActivationPhase = "starting" | "ready" | "failed" | "cancelled";
export interface TopicActivationEvent {
    requestId: string;
    tabId: string;
    phase: TopicActivationPhase;
    error?: string;
}
// tab:meta channel: a full refreshed Meta pushed after the background refresh
// of the expensive MetaForTab fields (git branch, image-input capability).
export interface TabMetaRefreshEvent {
    tabId: string;
    meta: Meta;
}
export interface PromptHistoryEntry {
    text: string;
    at: number; // unix ms
    sessionPath: string;
    turn: number;
}
export interface PromptHistoryResult {
    entries: PromptHistoryEntry[] | null;
    nonce: string;
    olderCursor?: string;
    hasOlder?: boolean;
}
// CheckpointMeta is one rewind point (a user turn) for the rewind UI.
export interface CheckpointMeta {
    turn: number;
    prompt: string;
    files: string[];
    fileCount?: number;
    filesTruncated?: boolean;
    turnFileCount?: number;
    time: number; // unix ms
    canCode?: boolean;
    canConversation?: boolean;
    coverage?: string;
    coverageGaps?: string[];
    expiredFilePayload?: boolean;
    activeWriters?: number;
    legacy?: boolean;
    canUndoFiles?: boolean;
    disabledReason?: string;
}
export interface RewindPlanView {
    planId?: string;
    turn?: number;
    scope?: string;
    coverage?: string;
    coverageGaps?: string[];
    legacy?: boolean;
    expiredFilePayload?: boolean;
    canFiles?: boolean;
    canConversation?: boolean;
    disabledReason?: string;
    conflicts?: string[];
    files?: string[];
    fileCount?: number;
    activeWriters?: number;
    path?: string;
    ok?: boolean;
    error?: string;
}
export interface RewindResultView {
    ok?: boolean;
    transactionId?: string;
    undoAvailable?: boolean;
    written?: string[];
    deleted?: string[];
    conversationOk?: boolean;
    error?: string;
    conflicts?: string[];
    coverage?: string;
}
export interface WorkspaceView {
    path: string;
    name: string;
    current: boolean;
}
export interface ContextInfo {
    used: number;
    window: number;
    sessionTokens: number;
    compactRatio?: number;
    sessionCost?: number;
    sessionCurrency?: string;
    cacheHitTokens?: number;
    cacheMissTokens?: number;
    estimated?: boolean;
    sessionCostComplete?: boolean;
    sessionCostQuote?: CostQuote;
    sources?: Record<string, UsageSourceStats>;
    maintenance?: ContextMaintenanceInfo;
}
export interface Meta {
    label: string;
    ready: boolean;
    runtime?: SessionRuntimeView;
    startupErr?: string;
    eventChannel: string;
    sessionPath?: string;
    sessionRevision?: number;
    sessionDigest?: string;
    sessionGeneration?: number;
    cwd: string;
    workspaceRoot?: string;
    workspaceName?: string;
    workspacePath?: string;
    gitBranch?: string;
    imageInputEnabled?: boolean;
    autoApproveTools?: boolean;
    bypass?: boolean; // legacy JSON key for YOLO/full-access tool auto-approval
    collaborationMode?: CollaborationMode;
    toolApprovalMode?: ToolApprovalMode;
    tokenMode?: TokenMode;
    /** Canonical role setting (light|balanced|delivery). Prefer over tokenMode. */
    agentPreset?: AgentPreset;
    goal?: string;
    goalStatus?: GoalStatus;
    goalRuntime?: GoalRuntime;
    canonicalTodos?: Todo[];
}

