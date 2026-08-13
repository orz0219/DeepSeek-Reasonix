import { sameTodoList } from "./todoVisibility";
import { modeHasAutoApproveTools, normalizeMode, normalizeToolApprovalMode } from "./types";
import type { CollaborationMode, Meta, TabMeta, ToolApprovalMode, WireUsage } from "./types";
import { State } from "./controller_state";
export function usageTotalTokens(usage?: WireUsage): number {
    if (!usage)
        return 0;
    if (usage.totalTokens > 0)
        return usage.totalTokens;
    const promptTokens = usage.promptTokens || usage.cacheHitTokens + usage.cacheMissTokens;
    return Math.max(0, promptTokens + usage.completionTokens);
}
export type RuntimeMetaSnapshot = {
    running: boolean;
    pendingPrompt?: boolean;
    backgroundJobs?: number;
    cancelRequested?: boolean;
    cancellable?: boolean;
};
export function foregroundRunningFromRuntimeMeta(meta: RuntimeMetaSnapshot): boolean {
    if (typeof meta.cancellable === "boolean")
        return meta.cancellable;
    if ((meta.backgroundJobs ?? 0) > 0 && !meta.pendingPrompt)
        return false;
    return Boolean(meta.running);
}
// Clock used to order live prompt events against runtime snapshot fetches.
// Monotonic (immune to wall-clock jumps) with sub-millisecond resolution, so
// an event and a snapshot initiated in the same millisecond still order
// correctly. Only ever compared against itself.
export function promptEventClock(): number {
    return typeof performance !== "undefined" ? performance.now() : Date.now();
}
// True when a runtime snapshot was fetched before the tab's live approval/ask
// event arrived. Such a snapshot reports the tab idle only because it predates
// the prompt (pre-attach ListTabs, activation-time metas); applying it would
// clear the only UI able to answer the prompt — and, since it also carries
// pendingPrompt=false, skip the compensating replay (#6429, #5561, #5481).
// Ties count as stale: keeping a prompt one extra round is recoverable, while
// clearing a live prompt is the bug this guards against.
export function runtimeSnapshotPredatesPrompt(state: {
    approval?: unknown;
    ask?: unknown;
    promptArrivedAt?: number;
} | undefined, snapshotAt: number | undefined): boolean {
    if (!state || (!state.approval && !state.ask))
        return false;
    if (snapshotAt === undefined || state.promptArrivedAt === undefined)
        return false;
    return snapshotAt <= state.promptArrivedAt;
}
export function runtimeSnapshotPredatesRetry(state: Pick<State, "retry"> | undefined, snapshotAt: number | undefined): boolean {
    if (snapshotAt === undefined || state?.retry?.observedAt === undefined)
        return false;
    return snapshotAt <= state.retry.observedAt;
}
export function updatesContextGauge(usage?: WireUsage): boolean {
    const source = usage?.source?.trim();
    return !source || source === "executor";
}
export function metaFromTab(tab: TabMeta, existing?: Meta): Meta {
    const cwd = tab.cwd || tab.workspaceRoot || existing?.cwd || "";
    const toolApprovalMode = normalizeToolApprovalMode(tab.toolApprovalMode, normalizeMode(tab.mode), modeHasAutoApproveTools(tab.mode), (tab.toolApprovalMode ?? "").trim() === "" ? existing?.toolApprovalMode : undefined);
    const autoApproveTools = toolApprovalMode === "yolo";
    return {
        label: tab.label || existing?.label || "",
        ready: tab.ready,
        runtime: tab.runtime,
        startupErr: tab.startupErr,
        eventChannel: existing?.eventChannel ?? "agent:event",
        cwd,
        workspaceRoot: tab.workspaceRoot || existing?.workspaceRoot || cwd,
        workspaceName: tab.workspaceName || existing?.workspaceName,
        workspacePath: tab.workspacePath || tab.workspaceRoot || existing?.workspacePath,
        sessionPath: tab.sessionPath !== undefined ? tab.sessionPath : existing?.sessionPath,
        sessionRevision: tab.sessionRevision !== undefined ? tab.sessionRevision : existing?.sessionRevision,
        sessionDigest: tab.sessionDigest !== undefined ? tab.sessionDigest : existing?.sessionDigest,
        sessionGeneration: tab.sessionGeneration !== undefined ? tab.sessionGeneration : existing?.sessionGeneration,
        gitBranch: tab.gitBranch || existing?.gitBranch,
        autoApproveTools,
        bypass: autoApproveTools,
        collaborationMode: tab.collaborationMode ?? existing?.collaborationMode ?? "normal",
        toolApprovalMode,
        tokenMode: tab.tokenMode ?? existing?.tokenMode ?? "full",
        goal: tab.goal ?? existing?.goal,
        goalStatus: tab.goalStatus ?? existing?.goalStatus,
        canonicalTodos: existing?.canonicalTodos,
    };
}
export function countsTowardCurrentTurn(state: State): boolean {
    return state.turnActive || state.running;
}
export function sameMeta(a?: Meta, b?: Meta): boolean {
    if (a === b)
        return true;
    if (!a || !b)
        return false;
    return (a.label === b.label &&
        a.ready === b.ready &&
        a.runtime?.phase === b.runtime?.phase &&
        a.runtime?.epoch === b.runtime?.epoch &&
        a.runtime?.issue?.code === b.runtime?.issue?.code &&
        a.runtime?.issue?.message === b.runtime?.issue?.message &&
        a.runtime?.issue?.retryable === b.runtime?.issue?.retryable &&
        a.runtime?.issue?.holderPid === b.runtime?.issue?.holderPid &&
        a.runtime?.issue?.holderHost === b.runtime?.issue?.holderHost &&
        a.runtime?.issue?.acquiredAt === b.runtime?.issue?.acquiredAt &&
        a.startupErr === b.startupErr &&
        a.eventChannel === b.eventChannel &&
        a.cwd === b.cwd &&
        a.workspaceRoot === b.workspaceRoot &&
        a.workspaceName === b.workspaceName &&
        a.workspacePath === b.workspacePath &&
        a.sessionPath === b.sessionPath &&
        a.sessionRevision === b.sessionRevision &&
        a.sessionDigest === b.sessionDigest &&
        a.sessionGeneration === b.sessionGeneration &&
        a.gitBranch === b.gitBranch &&
        a.imageInputEnabled === b.imageInputEnabled &&
        a.autoApproveTools === b.autoApproveTools &&
        a.bypass === b.bypass &&
        a.collaborationMode === b.collaborationMode &&
        a.toolApprovalMode === b.toolApprovalMode &&
        a.tokenMode === b.tokenMode &&
        a.goal === b.goal &&
        a.goalStatus === b.goalStatus &&
        sameTodoList(a.canonicalTodos, b.canonicalTodos));
}
export function runtimeReadyForSubmit(meta?: Meta): boolean {
    if (!meta || meta.ready !== true || meta.startupErr)
        return false;
    return !meta.runtime || meta.runtime.phase === "ready";
}
// normalizeTurnSubmit is the final frontend boundary before optimistic
// transcript state is created. Display text may intentionally be shorter than
// the provider input, but a visible-only message must never start an empty model
// turn (#6869).
export function normalizeTurnSubmit(displayText: string, submitText: string): {
    display: string;
    submit: string;
} {
    const display = displayText.trim();
    const submit = submitText.trim();
    if (!submit)
        throw new Error("Message cannot be empty.");
    return { display, submit };
}
const frontendSubmissionEpoch = typeof globalThis.crypto?.randomUUID === "function"
    ? globalThis.crypto.randomUUID()
    : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
export function createTurnSubmissionId(tabId: string, sessionGen: number, seq: number, runtimeEpoch?: string): string {
    return JSON.stringify([frontendSubmissionEpoch, tabId, sessionGen, runtimeEpoch ?? "", seq]);
}
export function acceptsRuntimeEventEpoch(acceptedEpoch: string | undefined, eventEpoch: string | undefined): boolean {
    return !eventEpoch || !acceptedEpoch || acceptedEpoch === eventEpoch;
}
export function composerProfileApplicationKey(runtimeEpoch: string | undefined, collaborationMode: CollaborationMode, toolApprovalMode: ToolApprovalMode, goal: string): string {
    return JSON.stringify([runtimeEpoch ?? "", collaborationMode, toolApprovalMode, goal]);
}
export function metaWithoutCanonicalTodos(meta?: Meta): Meta | undefined {
    if (!meta || meta.canonicalTodos === undefined)
        return meta;
    return { ...meta, canonicalTodos: undefined };
}
export const STALE_TURN_RECONCILE_MS = 30000;
export const CANCEL_RECONCILE_DELAYS_MS = [0, 100, 300, 1000] as const;
// After a stale runtime snapshot is rejected (its fetch predates the live
// prompt), refetch authoritative backend state once. Short enough to be barely
// perceptible, long enough to let any other in-flight replay events land first
// so the refetch reflects settled backend truth (#6432).
export const STALE_PROMPT_RECONCILE_MS = 150;
export const STARTUP_READY_META_RECONCILE_MS = 250;
export const STARTUP_READY_META_RECONCILE_ATTEMPTS = 60;
export function shouldReconcileStaleTurn(state: Pick<State, "running" | "turnActive"> | undefined, lastTurnActivityAt: number, now = Date.now(), timeoutMs = STALE_TURN_RECONCILE_MS): boolean {
    if (!state?.running || !state.turnActive || lastTurnActivityAt <= 0)
        return false;
    return Math.max(0, now - lastTurnActivityAt) >= timeoutMs;
}
export function hasCachedLiveTurn(state: State | undefined): boolean {
    if (!state?.running && !state?.turnActive)
        return false;
    if (state.live || state.currentAssistant || state.pendingUser !== undefined)
        return true;
    return state.items.some((item) => (item.kind === "assistant" && item.streaming) ||
        (item.kind === "tool" && item.status === "running"));
}
export function hasReusableCachedTranscript(state: State | undefined, sessionPath?: string, revision?: number, digest?: string): boolean {
    if (!state || state.items.length === 0)
        return false;
    const expectedSessionPath = (sessionPath ?? "").trim();
    if (!expectedSessionPath)
        return true;
    if ((state.meta?.sessionPath ?? "").trim() !== expectedSessionPath)
        return false;
    if (typeof revision === "number" && revision > 0) {
        return state.historyRevision === revision && (digest ?? "") === (state.historyDigest ?? "");
    }
    if ((digest ?? "").trim() !== "")
        return state.historyDigest === digest;
    // A cached page with a known fingerprint must not be reused when the
    // backend temporarily cannot provide one (for example while its metadata
    // sidecar is being atomically replaced). Reloading is the only way to avoid
    // presenting a stale persisted transcript as current.
    return state.historyRevision === undefined && !state.historyDigest;
}
export function historyFingerprintMatchesMeta(history: {
    revision: number;
    revisionKnown?: boolean;
    digest?: string;
}, meta: Meta): boolean {
    const expectedDigest = (meta.sessionDigest ?? "").trim();
    if (expectedDigest && history.digest !== expectedDigest)
        return false;
    const expectedRevision = meta.sessionRevision ?? 0;
    if (expectedRevision > 0 && (!history.revisionKnown || history.revision !== expectedRevision))
        return false;
    return true;
}
/** Mirrors Go backend's ReadOnly() hints. */
export function isReadOnlyTool(name: string): boolean {
    switch (name) {
        case "read_file":
        case "ls":
        case "grep":
        case "glob":
        case "web_fetch":
        case "code_index":
        case "bash_output":
        case "waitJob":
        case "todo_write":
        case "read_skill":
            return true;
        default:
            return false;
    }
}

