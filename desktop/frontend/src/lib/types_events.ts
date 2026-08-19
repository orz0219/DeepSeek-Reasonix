import type { WireContextMaintenance } from "./contextMaintenanceTypes";
import { WireFinalReadiness, WireWorkspaceChanged } from "./types_workspace";
export type EventKind = "turn_started" | "reasoning" | "text" | "message" | "tool_dispatch" | "tool_result" | "tool_progress" | "usage" | "notice" | "phase" | "approval_request" | "ask_request" | "turn_done" | "compaction_started" | "compaction_done" | "mcp_surface_ready" | "retrying" | "steer" | "guardian_assessment" | "extension_surface" | "extension_status" | "stream_attempt" | "context_maintenance" | "workspace_changed" | "turn_phase" | "completion_summary";
export type StreamAttemptAction = "begin" | "discard" | "commit";
export interface WireStreamAttempt {
    id: string;
    action: StreamAttemptAction;
    attempt?: number;
    max?: number;
    /** Fixed enum only: connection_reset | premature_eof | idle_timeout */
    reason?: string;
}
export interface WireCompaction {
    trigger?: string; // "auto" | "manual"
    messages?: number; // done: how many messages were folded into the summary
    summary?: string; // done: the briefing (empty on an aborted pass)
    archive?: string; // done: archive path, if any
}
export interface WireProfile {
    model?: string;
    effort?: string;
}
export interface WireShellExecution {
    kind?: string;
    shell?: string;
    shellVersion?: string;
    platform?: string;
    supportsAndAnd?: boolean;
    state?: string;
    failurePhase?: string;
    exitCode?: number;
    outputTail?: string;
    mutationRisk?: string;
    verification?: string;
    durationMs?: number;
}
export interface WireTool {
    id?: string;
    name: string;
    args?: string;
    resolvedName?: string;
    capabilityId?: string;
    output?: string;
    err?: string;
    readOnly: boolean;
    truncated?: boolean;
    durationMs?: number;
    partial?: boolean; // an early dispatch (name only) — a full one with args follows
    argChars?: number; // partial only: cumulative argument chars streamed so far
    refreshed?: boolean; // same-ID full dispatch with a preview recomputed after an earlier write
    parentId?: string; // set on a sub-agent's calls — the parent `task` call's id
    /** Host-local stream_attempt id for speculative parent partials only. */
    attemptId?: string;
    diff?: string;
    added?: number;
    removed?: number;
    profile?: WireProfile; // subagent model/effort resolved for this call
    execution?: WireShellExecution; // local shell metadata; never provider-visible
}
export interface WireCacheDiagnostics {
    prefixHash: string;
    prefixChanged: boolean;
    prefixChangeReasons?: string[];
    systemHash: string;
    toolsHash: string;
    logRewriteVersion: number;
    toolSchemaTokens: number;
    cacheMissTokens: number;
    cacheHitTokens: number;
}
export interface WireUsage {
    promptTokens: number;
    completionTokens: number;
    totalTokens: number;
    cacheHitTokens: number;
    cacheMissTokens: number;
    reasoningTokens?: number;
    estimated?: boolean;
    source?: string;
    cacheDiagnostics?: WireCacheDiagnostics;
    // Session-cumulative cache tokens — the status bar shows the aggregate
    // hit-rate (Σhit/Σ(hit+miss)), steadier than the single-turn cacheHitTokens.
    sessionCacheHitTokens: number;
    sessionCacheMissTokens: number;
    /** Latest single-request shape for context gauges; omit → use billable totals. */
    contextPromptTokens?: number;
    contextCompletionTokens?: number;
    contextReasoningTokens?: number;
    contextCacheHitTokens?: number;
    contextCacheMissTokens?: number;
    cost?: number;
    currency?: string;
    currencyCode?: string;
    // Deprecated compatibility alias. Prefer cost + currencyCode / costQuote.
    costUsd?: number;
    costComplete?: boolean;
    displayComplete?: boolean;
    displayStatus?: string;
    aggregateMode?: string;
    originalTotals?: Money[];
    /** Host-side structured quote; prefer over cost/currency aliases. */
    costQuote?: CostQuote;
}
export interface Money {
    amount: string;
    currency: string;
}
export interface CostQuote {
    original: Money;
    originalTotals?: Money[];
    valuations?: Record<string, {
        money: Money;
        basis: string;
        source: string;
        asOf: string;
        rateSnapshot?: {
            base: string;
            quote: string;
            rate: number;
            source: string;
            asOf: string;
            stale?: boolean;
        };
        stale?: boolean;
    }>;
    selected?: Money;
    billingMode?: string;
    estimated: boolean;
    costComplete?: boolean;
    displayComplete?: boolean;
    complete: boolean;
    displayStatus?: "matched" | "fallback_original" | "bucketed" | "unavailable" | string;
    aggregateMode?: "single_currency" | "common_valuation" | "currency_buckets" | string;
    modelRef?: string;
    usageSource?: string;
    pricingFingerprint?: string;
    rateDate?: string;
    incompleteReason?: string;
    legacyEstimate?: boolean;
    catalogSource?: string;
}
export interface WireRecoveryApproval {
    source_agent?: string;
    failed_tool?: string;
    failed_summary?: string;
    diagnosis?: string;
    next_tool?: string;
    next_action?: string;
    change_kind?: string;
    change_rationale?: string;
    review_rationale?: string;
    plan_before?: string;
    plan_after?: string;
    can_grant_task?: boolean;
    task_grant_scope?: string;
}
export interface WireApproval {
    id: string;
    tool: string;
    subject: string;
    reason?: string;
    fresh?: boolean;
    kind?: "tool" | "plan" | "recovery" | string;
    recovery?: WireRecoveryApproval;
}
export interface WireGuardian {
    id: string;
    tool: string;
    subject: string;
    outcome: string;
    risk_level?: string;
    user_authorization?: string;
    rationale?: string;
    duration_ms?: number;
    usage?: WireUsage;
}
export interface WireDecisionReceipt {
    id: string;
    kind: string;
    tool?: string;
    subject?: string;
    outcome: string;
}
export interface WireAskOption {
    label: string;
    description?: string;
}
export interface WireAskInput {
    recommended?: string;
    multiline?: boolean;
    required?: boolean;
}
export interface WireAskQuestion {
    id: string;
    header?: string;
    prompt: string;
    options: WireAskOption[];
    input?: WireAskInput;
    multi?: boolean;
}
export interface WireAsk {
    id: string;
    questions: WireAskQuestion[];
}
// Extension UI surfaces (stage 8a) — structured-only documents published by
// extension sidecars through the host UI hub. Exactly one sub-struct is set,
// selected by `kind`.
export interface WireExtensionStatus {
    label: string;
    detail?: string;
    severity?: string; // "info" | "warn" | "error"
    progress?: number;
}
export interface WireExtensionKeyValue {
    key: string;
    value: string;
}
export interface WireExtensionActionRef {
    actionId: string;
    label: string;
}
export interface WireExtensionCard {
    title?: string;
    markdown?: string;
    text?: string;
    fields?: WireExtensionKeyValue[];
    progress?: number;
    actions?: WireExtensionActionRef[];
}
export interface WireExtensionFormField {
    key: string;
    label?: string;
    kind?: string; // "confirm" | "input" | "select" | "multiselect"
    options?: string[];
    default?: unknown;
    required?: boolean;
}
export interface WireExtensionForm {
    title?: string;
    message?: string;
    fields: WireExtensionFormField[];
}
export interface WireExtensionNotification {
    title: string;
    body?: string;
    severity?: string; // "info" | "warn" | "error"
}
export interface WireExtensionSurface {
    pluginId: string;
    surfaceId: string;
    sessionId?: string;
    generation?: number;
    kind: string; // "status" | "card" | "form" | "notification"
    status?: WireExtensionStatus;
    card?: WireExtensionCard;
    form?: WireExtensionForm;
    notification?: WireExtensionNotification;
}
// ExtensionActionView is one handshake-declared extension UI action, the JSON
// twin of desktop's ExtensionActionView (stage 8b2). Slash is the public
// invocation name, "/<plugin>:<action>".
export interface ExtensionActionView {
    plugin: string;
    action: string;
    slash: string;
    description?: string;
}
// QuestionAnswer is the reply for one question, sent back via AnswerQuestion.
export interface QuestionAnswer {
    questionId: string;
    selected: string[];
}
export interface WireEvent {
    kind: EventKind;
    text?: string;
    detail?: string;
    // Stable notice id for localization; empty/absent = localize by text match.
    code?: string;
    reasoning?: string;
    level?: "info" | "warn";
    tool?: WireTool;
    usage?: WireUsage;
    approval?: WireApproval;
    ask?: WireAsk;
    compaction?: WireCompaction;
    maintenance?: WireContextMaintenance;
    guardian?: WireGuardian;
    decisionReceipt?: WireDecisionReceipt;
    extension?: WireExtensionSurface;
    err?: string;
    checkpointTurn?: number; // Authoritative TurnDone rewind target; zero is valid.
    submissionId?: string; // Opaque correlation for the exact optimistic user submission.
    outcome?: "final_readiness" | "recovery_paused";
    readiness?: WireFinalReadiness;
    retryAttempt?: number;
    retryMax?: number;
    /** Optional: "headers" | "stream". Older clients ignore unknown fields. */
    retryScope?: "headers" | "stream";
    streamAttempt?: WireStreamAttempt;
    /** Durable session-inbox item id for steer / TurnDone correlation. */
    itemId?: string;
    workspace?: WireWorkspaceChanged;
    /** turn_phase: working | checking | verifying | reviewing */
    phase?: string;
    /** completion_summary: content-free quality summary for role settings */
    completion?: WireCompletionSummary;
    tabId?: string; // Go's tabEventSink tags events for the correct per-tab reducer.
    runtimeEpoch?: string;
    sessionHitTokens?: number;
    sessionMissTokens?: number;
    sessionCost?: number;
    sessionCurrency?: string;
    // Deprecated compatibility alias. Prefer sessionCost + sessionCurrency.
    sessionCostUsd?: number;
}
export interface WireCompletionSummary {
    preset: string;
    verdict: string;
    mutations: number;
    checks_passed: number;
    checks_failed: number;
    checks_suppressed: number;
    review: string;
    gap_kinds?: string[];
    constraint_degraded: boolean;
}

