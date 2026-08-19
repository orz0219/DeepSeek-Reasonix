import { type ToolFileDiff } from "./tools";
import type { BalanceInfo, CheckpointMeta, ContextInfo, EffortInfo, JobView, Meta, TopicActivationEvent, WireApproval, WireAsk, WireCompletionSummary, WireDecisionReceipt, WireExtensionCard, WireExtensionForm, WireExtensionSurface, WireUsage, WireShellExecution } from "./types";
import { ToolStatus, SubagentProgress } from "./controller_subagent";
export type LiveStream = {
    id: string;
    text: string;
    reasoning: string;
    reasoningComplete: boolean;
    reasoningStartedAt?: number;
    reasoningCompletedAt?: number;
};
/** Speculative journal for one sampling attempt — rolled back on discard. */
type StreamAttemptJournal = {
    id: string;
    baselineLive?: LiveStream;
    baselineTurnArgChars: number;
    /** Tool cards created by this attempt (running, no result yet). */
    createdToolIds: string[];
    /** Prior state of tools that existed before this attempt and were patched. */
    priorTools: Record<string, Extract<Item, {
        kind: "tool";
    }>>;
};
export type ControllerLiveStore = {
    subscribe: (tabId: string | undefined, listener: () => void) => () => void;
    getSnapshot: (tabId: string | undefined) => LiveStream | undefined;
    getModelActiveAt?: (tabId: string | undefined) => number | undefined;
};
export type MessageActionScope = "fork" | "summ-from" | "summ-upto" | "conversation" | "code" | "both";
export type MessageActionState = {
    turn: number;
    scope: MessageActionScope;
};
export type HydrateReason = "switch-tab" | "new-session" | "resume-session" | "open-topic" | "startup" | "rewind";
export type SyncActiveTabOptions = {
    preserveCachedHistory?: boolean;
};
// A ticketed StartTopicActivation in flight. Only the latest one is tracked:
// superseded requests get "cancelled" from the backend and are ignored.
export type PendingTopicActivation = {
    requestId: string;
    navigationSeq: number;
    tabId?: string;
    placeholderItems?: Item[];
    /** Terminal event that arrived before the ticket resolved. */
    terminal?: TopicActivationEvent;
};
export type ModelSwitchQueueResult = "applied" | "superseded";
export type ModelSwitchQueueRequest = {
    name: string;
    resolve: (result: ModelSwitchQueueResult) => void;
    reject: (err: unknown) => void;
};
export type ModelSwitchQueueState = {
    running: boolean;
    pending?: ModelSwitchQueueRequest;
    fallbackBalance?: BalanceInfo;
};
export const HISTORY_PAGE_TURNS = 60;
export type TurnPhaseName = "working" | "checking" | "verifying" | "reviewing" | string;
export type Item = {
    kind: "user";
    id: string;
    submissionId?: string;
    text: string;
    submitText?: string;
    failed?: boolean;
    createdAt?: number;
    checkpointTurn?: number;
} | {
    kind: "assistant";
    id: string;
    text: string;
    reasoning: string;
    streaming: boolean;
    reasoningComplete?: boolean;
    reasoningDurationMs?: number;
    workDurationMs?: number;
} | {
    kind: "phase";
    id: string;
    text: string;
} | {
    kind: "notice";
    id: string;
    level: "info" | "warn";
    text: string;
    detail?: string;
    title?: string;
    variant?: "delivery" | "completion";
    action?: "continue_delivery" | "open_changes";
    decisionReceipt?: WireDecisionReceipt;
} | {
    kind: "compaction";
    id: string;
    pending: boolean;
    trigger: string;
    messages: number;
    summary: string;
    archive: string;
} | {
    kind: "tool";
    id: string;
    name: string;
    args: string;
    readOnly: boolean;
    resolvedName?: string;
    capabilityId?: string;
    status: ToolStatus;
    output?: string;
    error?: string;
    truncated?: boolean;
    dataArchived?: boolean; // args/output trimmed for memory; full data available via backend
    durationMs?: number;
    subject?: string; // stable collapsed subject from archived history payloads
    summary?: string; // stable collapsed readout kept even after args/output archive
    fileDiff?: ToolFileDiff; // previewed whole-file diff from writer dispatch
    isShell?: boolean; // bash tool or !command — structured shell card presentation
    execution?: WireShellExecution; // local shell metadata
    parentId?: string; // a sub-agent call nests under the `task` call with this id
    profile?: {
        model?: string;
        effort?: string;
    }; // subagent model/effort from tool event
    argChars?: number; // args still streaming from the model: cumulative chars received
    subagentProgress?: SubagentProgress; // in-memory-only preview, never hydrated from history
} | {
    kind: "extension";
    id: string;
    // surfaceKey is "<pluginId>:<surfaceId>"; a re-published card replaces the
    // previous one in place instead of appending a duplicate transcript entry.
    surfaceKey: string;
    pluginId: string;
    surfaceId: string;
    generation?: number;
    card: WireExtensionCard;
};
export type ToolItem = Extract<Item, {
    kind: "tool";
}>;
export type ExtensionItem = Extract<Item, {
    kind: "extension";
}>;
// Extension UI surfaces (stage 8b2) — per-tab state fed by extension_surface /
// extension_status wire events. Statuses and generations key on
// "<pluginId>:<surfaceId>"; the form is the single pending form surface (a new
// form replaces the old, matching the backend's one-blocking-prompt model);
// notifications queue until the App drains them into the toast system.
export interface ExtensionStatusEntry {
    pluginId: string;
    surfaceId: string;
    label: string;
    detail?: string;
    severity?: string;
    progress?: number;
    generation?: number;
}
export interface ExtensionFormState {
    pluginId: string;
    surfaceId: string;
    generation?: number;
    form: WireExtensionForm;
}
export interface ExtensionNotificationEntry {
    id: string;
    pluginId: string;
    title: string;
    body?: string;
    severity?: string;
}
// extensionSurfaceKey is the identity a sidecar re-publishes under to replace
// one of its surfaces.
export function extensionSurfaceKey(surface: Pick<WireExtensionSurface, "pluginId" | "surfaceId">): string {
    return `${surface.pluginId}:${surface.surfaceId}`;
}
// acceptsExtensionGeneration drops a re-ordered surface publication: within
// one runtime, a sidecar's generation is monotonic, so anything older than the
// last accepted generation for the same surface is stale. Events without a
// generation always pass (they carry no ordering claim).
export function acceptsExtensionGeneration(stored: number | undefined, incoming: number | undefined): boolean {
    return incoming === undefined || stored === undefined || incoming >= stored;
}
// Mid-turn steer messages are recorded as info notices carrying this prefix —
// both live (the "steer" event below) and in replayed history (desktop/app.go
// prefixes persisted steers the same way). The prefix is the only durable
// marker, so display code identifies steers by it.
export const STEER_NOTICE_PREFIX = "↪ ";
export function isSteerNoticeText(text: string): boolean {
    return text.startsWith(STEER_NOTICE_PREFIX);
}
export interface State {
    items: Item[];
    running: boolean;
    turnActive: boolean;
    pendingPrompt: boolean;
    backgroundJobs: number;
    cancelRequested: boolean;
    cancellable: boolean;
    /** Host turn phase from turn_phase events (working|checking|verifying|reviewing). */
    turnPhase?: TurnPhaseName;
    /** Latest content-free turn quality summary, shown on demand in the change panel. */
    completionSummary?: WireCompletionSummary;
    approval?: WireApproval;
    ask?: WireAsk;
    usage?: WireUsage;
    context: ContextInfo;
    meta?: Meta;
    balance?: BalanceInfo;
    effort?: EffortInfo;
    jobs: JobView[];
    checkpoints: CheckpointMeta[];
    hydrating: boolean;
    hydrateReason?: HydrateReason;
    hydrateError?: string;
    hydrateHistoryLoaded?: boolean;
    hydratePlaceholderItems?: Item[];
    historyStartTurn: number;
    historyTotalTurns: number;
    historyHasOlder: boolean;
    historyOlderLoading: boolean;
    historyRevision?: number;
    historyDigest?: string;
    backendActivationPending: boolean;
    messageAction?: MessageActionState;
    currentAssistant?: string;
    live?: LiveStream;
    pendingUser?: string;
    pendingSubmissionId?: string;
    deliveryRecoveryActive: boolean;
    discardTurn?: boolean;
    turnStartAt: number;
    turnDoneAt: number;
    // Completion tokens accumulated across executor usage events within the
    // current turn. ReasoningTokens is a subset of CompletionTokens.
    turnOutputTokens: number;
    turnOutputChars: number;
    // Live text/reasoning characters already covered by the accumulated usage.
    // This lets the composer estimate only the in-flight provider request.
    turnOutputCharsAtUsage: number;
    // True when any output-token count in the current turn is estimated.
    turnOutputEstimated: boolean;
    // Active provider-output intervals for the current turn. Tool execution and
    // gaps between provider requests are intentionally excluded from TPS.
    turnModelActiveAt?: number;
    turnModelActiveMs: number;
    // Time spent waiting on the user (approval/ask) within the current turn.
    // Closed intervals accumulate here; an open interval uses promptWaitStartedAt
    // so background tabs keep counting while not rendered by Composer.
    turnWaitAccumMs: number;
    // Last completed turn's values — preserved across turn boundaries so the
    // status bar can display the most recent completed turn's TPS and token
    // counts until the current turn finishes and overwrites them.
    lastTurnOutputTokens: number;
    lastTurnStartAt: number;
    lastTurnDoneAt: number;
    lastTurnWaitAccumMs: number;
    lastTurnModelMs: number;
    lastTurnOutputEstimated: boolean;
    // Per-request rate (null when unmeasurable) and pending interval are tab-local.
    lastRequestTps?: number | null;
    pendingRequestModelMs?: number;
    promptWaitStartedAt?: number;
    // promptEventClock() reading taken when the CURRENT pending prompt first
    // arrived. Orders the prompt against reconciliation snapshots so a snapshot
    // fetched before the event cannot clear the prompt it never knew about
    // (#6429). Anchored to the prompt's first arrival and NOT advanced by a
    // same-id replay, so an authoritative idle snapshot taken after the user
    // answered is never mistaken for stale (#6432 reverse race).
    promptArrivedAt?: number;
    // Id of the prompt promptArrivedAt is anchored to. A replay re-emitting the
    // same id keeps the original arrival time; only a genuinely new prompt id
    // (backend ids are monotonic within a controller) re-anchors it.
    promptArrivedId?: string;
    // Id of the most recently user-resolved approval/ask (explicit answer,
    // cancel-through-mode-switch, etc). A replay carrying this same id is a
    // stale re-delivery of an already-answered prompt, not a new one — arming
    // it would resurrect a zombie no downstream snapshot may ever get a chance
    // to reject (#6432 round 2: idle-applied-before-replay, and
    // running=true/pendingPrompt=false snapshots that never clear approval/ask).
    resolvedPromptId?: string;
    // Monotonic per-tab prompt-id namespace generation. Approval/ask ids restart
    // from "1" whenever the backend controller is rebuilt, so any id captured
    // before the bump (an in-flight prompt answer or mode-switch RPC) must not
    // touch bookkeeping written after it. Late callbacks from the old controller
    // otherwise act on a different prompt that reused the same numeric id.
    promptEpoch: number;
    turnTokens: number;
    turnTotalTokens: number;
    turnCost: number;
    // Cumulative argument characters of the tool call currently streaming its
    // args (partial dispatch progress). Folded into the composer pill as an
    // estimated-token tail; cleared when the round's usage arrives (which then
    // includes those tokens for real) and on turn start.
    turnArgChars: number;
    sessionTokens: number;
    sessionCost: number;
    sessionCurrency: string;
    retry?: {
        attempt: number;
        max: number;
        observedAt: number;
    };
    seq: number;
    sessionGen: number;
    // Per-session counter bumped after hydration ancillary data (context, effort,
    // jobs) arrives. ContextPanel reads this (merged into refreshKey) so the
    // right-side panel re-fetches after a session rebind instead of showing stale
    // RequestCount / ElapsedMs / SessionCost from before the swap.
    contextPanelSeq: number;
    // Monotonic count of usage events from ANY source (executor, subagent,
    // title…). Drives right-panel snapshot refreshes so sub-agent activity keeps
    // the session metrics live; state.usage stays executor-gated for the gauge.
    usageSeq: number;
    // Bounded set of context_maintenance operationIds already shown as notices
    // so reconnect/replay does not insert duplicate timeline cards.
    seenMaintenanceOps: string[];
    // Extension UI surfaces (stage 8b2). See the ExtensionStatusEntry block
    // above for the keying/lifecycle rules.
    extensionStatuses: Record<string, ExtensionStatusEntry>;
    extensionForm?: ExtensionFormState;
    extensionNotifications: ExtensionNotificationEntry[];
    // Last accepted generation per extension surface key; guards against
    // re-ordered publications (acceptsExtensionGeneration).
    extensionGenerations: Record<string, number>;
    // Speculative sampling-attempt journal for Codex-style stream replay.
    // Host-local only; never hydrated from history.
    streamAttemptJournal?: StreamAttemptJournal;
}
export const initialState: State = {
    items: [],
    running: false,
    turnActive: false,
    pendingPrompt: false,
    backgroundJobs: 0,
    cancelRequested: false,
    cancellable: false,
    context: { used: 0, window: 0, sessionTokens: 0 },
    jobs: [],
    checkpoints: [],
    hydrating: false,
    historyStartTurn: 0,
    historyTotalTurns: 0,
    historyHasOlder: false,
    historyOlderLoading: false,
    backendActivationPending: false,
    deliveryRecoveryActive: false,
    promptEpoch: 0,
    turnStartAt: 0,
    turnDoneAt: 0,
    turnOutputTokens: 0,
    turnOutputChars: 0,
    turnOutputCharsAtUsage: 0,
    turnOutputEstimated: false,
    turnModelActiveMs: 0,
    turnWaitAccumMs: 0,
    lastTurnOutputTokens: 0,
    lastTurnStartAt: 0,
    lastTurnDoneAt: 0,
    lastTurnWaitAccumMs: 0,
    lastTurnModelMs: 0,
    lastTurnOutputEstimated: false,
    turnTokens: 0,
    turnTotalTokens: 0,
    turnCost: 0,
    turnArgChars: 0,
    sessionTokens: 0,
    sessionCost: 0,
    sessionCurrency: "¥",
    seq: 0,
    sessionGen: 0,
    contextPanelSeq: 0,
    usageSeq: 0,
    seenMaintenanceOps: [],
    extensionStatuses: {},
    extensionNotifications: [],
    extensionGenerations: {},
};

