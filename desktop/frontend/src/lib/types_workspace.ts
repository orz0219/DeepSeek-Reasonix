import { Mode, CollaborationMode, ToolApprovalMode, TokenMode, AgentPreset, GoalStatus } from "./types_mode";
import { CostQuote } from "./types_events";
import { UsageSourceStats, ReadFileRecord, ChangedFileInfo } from "./types_history";
export type WorkspaceWatchState = "active" | "degraded" | "unavailable";
export type WorkspaceChangeOp = "create" | "write" | "remove" | "rename" | "unknown";
export interface WorkspaceRevisions {
    content: number;
    tree: number;
    workingTree: number;
    gitMeta: number;
    session: number;
}
export interface WorkspacePathChange {
    path: string;
    oldPath?: string;
    op: WorkspaceChangeOp;
}
export interface WireWorkspaceChanged {
    revisions: WorkspaceRevisions;
    changes: WorkspacePathChange[];
    allPaths: boolean;
    source: "agent" | "filesystem" | "git" | "mixed" | "reconcile";
    watchState: WorkspaceWatchState;
}
export type SessionRuntimePhase = "starting" | "ready" | "lease_blocked" | "failed" | "closing";
export interface SessionRuntimeIssue {
    code: "session_lease_held" | "startup_failed";
    message: string;
    retryable: boolean;
    holderPid?: number;
    holderHost?: string;
    acquiredAt?: string;
}
export interface SessionRuntimeView {
    phase: SessionRuntimePhase;
    epoch: string;
    issue?: SessionRuntimeIssue;
}
export interface WireFinalReadiness {
    attempts?: number;
    missing?: string[];
}
// Tab management types (desktop/tabs.go).
export interface TabMeta {
    id: string;
    tabType?: "session" | "file";
    scope: string;
    workspaceRoot: string;
    workspaceName: string;
    workspacePath?: string;
    gitBranch?: string;
    isolatedWorktree?: boolean;
    topicId: string;
    topicTitle: string;
    sessionPath?: string;
    sessionRevision?: number;
    sessionDigest?: string;
    sessionGeneration?: number;
    readOnly?: boolean;
    filePath?: string;
    projectColor?: string;
    label: string;
    ready: boolean;
    runtime?: SessionRuntimeView;
    running: boolean;
    pendingPrompt?: boolean;
    backgroundJobs?: number;
    cancelRequested?: boolean;
    cancellable?: boolean;
    mode: Mode;
    collaborationMode?: CollaborationMode;
    toolApprovalMode?: ToolApprovalMode;
    tokenMode?: TokenMode;
    /** Canonical role setting (light|balanced|delivery). Prefer over tokenMode. */
    agentPreset?: AgentPreset;
    goal?: string;
    goalStatus?: GoalStatus;
    recovered?: boolean;
    recoveryReason?: string;
    recoveryDigest?: string;
    recoveryParentId?: string;
    startupErr?: string;
    active: boolean;
    cwd: string;
}
export interface TerminalSessionView {
    id: string;
    title: string;
    shell: string;
    cwd: string;
    createdAt: number;
    exitCode?: number;
    running: boolean;
}
export interface TerminalShellView {
    id: string;
    label: string;
}
export interface TerminalWorkspaceView {
    available: boolean;
    readOnly: boolean;
    reason?: string;
    sessions: TerminalSessionView[];
    shells: TerminalShellView[];
}
export interface ProjectNode {
    key: string;
    kind: "project" | "topic" | "session" | "global_folder" | "global_topic" | "global_session";
    label: string;
    root?: string;
    topicId?: string;
    sessionPath?: string;
    projectColor?: string;
    turns?: number;
    turnsState?: "unknown" | "valid" | "corrupt" | string;
    health?: "ok" | "missing" | "corrupt" | "degraded" | string;
    createdAt?: number;
    lastActivityAt?: number;
    open?: boolean;
    running?: boolean;
    status?: ProjectTopicStatus;
    pinned?: boolean;
    recovered?: boolean;
    recoveryReason?: string;
    recoveryDigest?: string;
    recoveryParentId?: string;
    recoveryState?: "normal" | "repairing" | "adopted" | "preferred" | "diverged" | "recovery_only" | string;
    recoveryBranchCount?: number;
    recoveryUnresolvedCount?: number;
    recoveryCleanupEligibleCount?: number;
    isolatedWorktree?: boolean;
    children?: ProjectNode[];
}
export interface RecoveryLineageMember {
    path: string;
    role: "normal" | "covered_copy" | "adopted" | "preferred" | "diverged" | string;
    canonical: boolean;
    turns: number;
    open: boolean;
    running: boolean;
}
export interface RecoveryLineageView {
    groupId: string;
    state: string;
    branchCount: number;
    unresolved: number;
    cleanupEligible: number;
    members: RecoveryLineageMember[];
}
export interface RecoveryCleanupRequest {
    scope: string;
    workspaceRoot?: string;
    topicId: string;
    apply: boolean;
}
export interface RecoveryPreferenceRequest {
    scope: string;
    workspaceRoot?: string;
    topicId: string;
    path: string;
}
export interface RecoveryCleanupItem {
    path: string;
    status: "eligible" | "moved" | "busy" | "kept" | string;
    error?: string;
}
export interface RecoveryCleanupResult {
    eligible: number;
    moved: number;
    busy: number;
    kept: number;
    dryRun: boolean;
    items: RecoveryCleanupItem[];
}
export interface DeliveryWorktreeAvailability {
    available: boolean;
    reason?: string;
    repoRoot?: string;
    branch?: string;
    sourceDirty?: boolean;
}
export interface DeliveryWorktreeOpenResult {
    workspaceRoot: string;
    worktreeRoot: string;
    sourceRoot: string;
    branch: string;
    sourceDirty: boolean;
    tab: TabMeta;
}
export type ProjectTopicStatus = "thinking" | "streaming" | "waiting_confirmation" | "background_job" | "paused" | "error" | "diverged_recovery";
export interface TopicMeta {
    id: string;
    title: string;
    createdAt: number;
}
export interface SessionRecoveryEvent {
    originalPath?: string;
    recoveryPath: string;
    scope?: string;
    workspaceRoot?: string;
    topicId?: string;
    topicTitle?: string;
    recoveryReason?: string;
    recoveryDigest?: string;
    recoveryParentId?: string;
    existing?: boolean;
}
export interface SessionRecoveryFailedEvent {
    reason?: "lease_held" | "lease_unavailable" | string;
}
export interface ContextPanelInfo {
    usedTokens: number;
    windowTokens: number;
    promptTokens: number;
    completionTokens: number;
    totalTokens: number;
    reasoningTokens: number;
    cacheHitTokens: number;
    cacheMissTokens: number;
    estimated?: boolean;
    sessionCacheHitTokens: number;
    sessionCacheMissTokens: number;
    sessionCompletionTokens: number;
    sessionEstimated?: boolean;
    requestCount?: number;
    elapsedMs?: number;
    sessionCost?: number;
    sessionCurrency?: string;
    // Deprecated compatibility alias. Prefer sessionCost + sessionCurrency.
    sessionCostUsd?: number;
    sessionCostComplete?: boolean;
    sessionCostEstimated?: boolean;
    sessionBillingMode?: string;
    sessionCostQuote?: CostQuote;
    sources?: Record<string, UsageSourceStats>;
    mock?: boolean;
    readFiles: ReadFileRecord[];
    changedFiles: ChangedFileInfo[];
}

