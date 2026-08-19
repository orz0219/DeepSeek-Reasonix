import { asArray } from "./array";
import { formatContextMaintenanceNotice, isNewMaintenanceOperation, rememberMaintenanceOperation } from "./contextMaintenanceTypes";
import { formatGuardianAssessmentNotice } from "./guardianEvents";
import { completionSummaryNeedsAttention, completionSummaryNotice, normalizeCompletionSummary } from "./completionSummary";
import { completeLiveReasoning } from "./streamDeltaBatch";
import { t } from "./i18n";
import { fileDiffFromWire, summarize, summarizeFileDiff } from "./tools";
import type { WireEvent, WireExtensionCard, WireExtensionStatus, WireExtensionSurface } from "./types";
import { State, extensionSurfaceKey, acceptsExtensionGeneration, ExtensionStatusEntry, Item, ExtensionNotificationEntry, ToolItem, STEER_NOTICE_PREFIX } from "./controller_state";
import { applyTurnCheckpoint, ensureAssistant, applyDeltaSegments, liveReasoningDurationMs, currentTurnDurationMs, compactArchivedToolItems } from "./controller_history";
import { promptEventClock, countsTowardCurrentTurn, updatesContextGauge, usageTotalTokens } from "./controller_meta";
import { SUBAGENT_PROGRESS_TOOLS, freshSubagentProgress, touchSubagentParent, ToolStatus, isGroupSubagentTool, isTerminalSubagentPhase, terminalStatusOf, isSubagentProgressName, applySubagentProgress } from "./controller_subagent";
import { appendNoticeToState, deliveryReadinessDetail } from "./controller_notice";
import { endTurnModelActivity, confirmPendingUser, resetTurnTiming, beginTurnModelActivity, beginPromptWait, snapshotCompletedTurnTelemetry, endPromptWait } from "./controller_reducer";
// applyExtensionSurfaceEvent reduces one extension_surface / extension_status
// wire event. Every publication passes the per-surface generation fence first
// (withAcceptedExtensionGeneration); the per-tab runtime-epoch fence in the
// onEvent handler has already dropped anything from an older runtime
// generation.
function applyExtensionSurfaceEvent(s: State, surface: WireExtensionSurface | undefined): State {
    if (!surface)
        return s;
    const gated = withAcceptedExtensionGeneration(s, surface);
    if (gated === null)
        return s;
    s = gated;
    const kind = surface.kind || (surface.status ? "status" : "");
    switch (kind) {
        case "status":
            return applyExtensionStatus(s, surface);
        case "card":
            return applyExtensionCard(s, surface);
        case "form":
            return applyExtensionForm(s, surface);
        case "notification":
            return applyExtensionNotification(s, surface);
        default:
            return s;
    }
}
// withAcceptedExtensionGeneration applies the per-surface generation fence.
// Returns null when the event is a stale re-ordering and must be dropped;
// otherwise returns state with the accepted generation recorded.
function withAcceptedExtensionGeneration(s: State, surface: WireExtensionSurface): State | null {
    const key = extensionSurfaceKey(surface);
    if (!acceptsExtensionGeneration(s.extensionGenerations[key], surface.generation))
        return null;
    if (surface.generation === undefined || s.extensionGenerations[key] === surface.generation)
        return s;
    return { ...s, extensionGenerations: { ...s.extensionGenerations, [key]: surface.generation } };
}
function applyExtensionStatus(s: State, surface: WireExtensionSurface): State {
    const status: WireExtensionStatus | undefined = surface.status;
    if (!status)
        return s;
    const entry: ExtensionStatusEntry = {
        pluginId: surface.pluginId,
        surfaceId: surface.surfaceId,
        label: status.label,
        detail: status.detail,
        severity: status.severity,
        progress: status.progress,
        generation: surface.generation,
    };
    return { ...s, extensionStatuses: { ...s.extensionStatuses, [extensionSurfaceKey(surface)]: entry } };
}
function applyExtensionCard(s: State, surface: WireExtensionSurface): State {
    const card: WireExtensionCard | undefined = surface.card;
    if (!card)
        return s;
    const key = extensionSurfaceKey(surface);
    const idx = s.items.findIndex((it) => it.kind === "extension" && it.surfaceKey === key);
    if (idx >= 0) {
        const next = [...s.items];
        const prev = next[idx];
        if (prev.kind === "extension")
            next[idx] = { ...prev, generation: surface.generation, card };
        return { ...s, items: next };
    }
    return {
        ...s,
        seq: s.seq + 1,
        items: [
            ...s.items,
            { kind: "extension", id: `x${s.seq}`, surfaceKey: key, pluginId: surface.pluginId, surfaceId: surface.surfaceId, generation: surface.generation, card },
        ],
    };
}
function applyExtensionForm(s: State, surface: WireExtensionSurface): State {
    const form = surface.form;
    if (!form)
        return s;
    return {
        ...s,
        extensionForm: { pluginId: surface.pluginId, surfaceId: surface.surfaceId, generation: surface.generation, form },
    };
}
function applyStreamAttempt(s: State, e: WireEvent): State {
    const sa = e.streamAttempt;
    if (!sa?.id || !sa.action)
        return s;
    switch (sa.action) {
        case "begin": {
            // Snapshot only what this attempt may replace in the visible stream.
            // Provider activity timing is closed at discard but remains accumulated so
            // retry backoff is not counted in the completed TPS denominator.
            const baselineLive = s.live
                ? {
                    id: s.live.id,
                    text: s.live.text,
                    reasoning: s.live.reasoning,
                    reasoningComplete: s.live.reasoningComplete,
                    reasoningStartedAt: s.live.reasoningStartedAt,
                    reasoningCompletedAt: s.live.reasoningCompletedAt,
                }
                : undefined;
            return {
                ...s,
                running: true,
                turnActive: true,
                cancellable: true,
                turnStartAt: s.turnStartAt || Date.now(),
                streamAttemptJournal: {
                    id: sa.id,
                    baselineLive,
                    baselineTurnArgChars: s.turnArgChars,
                    createdToolIds: [],
                    priorTools: {},
                },
            };
        }
        case "discard": {
            const journal = s.streamAttemptJournal;
            if (!journal || journal.id !== sa.id) {
                // Stale/out-of-order discard for an older attempt — leave the current
                // journal (and live speculative UI) untouched.
                return s;
            }
            const remove = new Set(journal.createdToolIds);
            const items = s.items
                .filter((it) => !(it.kind === "tool" && remove.has(it.id)))
                .map((it) => {
                if (it.kind !== "tool")
                    return it;
                const prior = journal.priorTools[it.id];
                return prior ? { ...prior } : it;
            });
            // Restore live to the pre-attempt snapshot so partial text/reasoning is
            // replaced, not concatenated with the next attempt.
            const live = journal.baselineLive
                ? { ...journal.baselineLive }
                : s.live
                    ? { ...s.live, text: "", reasoning: "", reasoningComplete: false, reasoningStartedAt: undefined, reasoningCompletedAt: undefined }
                    : undefined;
            return {
                ...endTurnModelActivity(s),
                items,
                live,
                turnArgChars: journal.baselineTurnArgChars,
                streamAttemptJournal: undefined,
                running: true,
                turnActive: true,
                cancellable: true,
            };
        }
        case "commit": {
            // Commit clears bookkeeping only; subsequent tool_dispatch/result are real.
            if (s.streamAttemptJournal && s.streamAttemptJournal.id !== sa.id) {
                return s;
            }
            return { ...s, streamAttemptJournal: undefined };
        }
        default:
            return s;
    }
}
/** Record a tool card mutation against the active sampling-attempt journal.
 * Only parent-sampling partials with a matching attemptId are journaled —
 * background sub-agent tools (parentId) and committed full dispatches are not.
 */
function noteToolInJournal(s: State, toolId: string, existedBefore: boolean, prior: Extract<Item, {
    kind: "tool";
}> | undefined, meta?: {
    attemptId?: string;
    parentId?: string;
    partial?: boolean;
}): State {
    const journal = s.streamAttemptJournal;
    if (!journal || !toolId)
        return s;
    // Require explicit attempt membership — do not journal by arrival time alone.
    if (!meta?.attemptId || meta.attemptId !== journal.id)
        return s;
    if (meta.parentId)
        return s;
    if (meta.partial === false)
        return s;
    if (!existedBefore) {
        if (journal.createdToolIds.includes(toolId))
            return s;
        return {
            ...s,
            streamAttemptJournal: {
                ...journal,
                createdToolIds: [...journal.createdToolIds, toolId],
            },
        };
    }
    if (prior && !journal.priorTools[toolId] && !journal.createdToolIds.includes(toolId)) {
        return {
            ...s,
            streamAttemptJournal: {
                ...journal,
                priorTools: { ...journal.priorTools, [toolId]: { ...prior } },
            },
        };
    }
    return s;
}
function applyExtensionNotification(s: State, surface: WireExtensionSurface): State {
    const notification = surface.notification;
    if (!notification)
        return s;
    const entry: ExtensionNotificationEntry = {
        id: `xn${s.seq}`,
        pluginId: surface.pluginId,
        title: notification.title,
        body: notification.body,
        severity: notification.severity,
    };
    return { ...s, seq: s.seq + 1, extensionNotifications: [...s.extensionNotifications, entry] };
}
export function applyEvent(s: State, e: WireEvent): State {
    if (s.discardTurn) {
        if (e.kind === "turn_done") {
            return {
                ...s,
                items: applyTurnCheckpoint(s.items, e.submissionId, e.checkpointTurn),
                discardTurn: false,
                running: false,
                turnActive: false,
                pendingPrompt: false,
                cancelRequested: false,
                cancellable: false,
                currentAssistant: undefined,
                live: undefined,
            };
        }
        return s;
    }
    s = confirmPendingUser(s, e.submissionId);
    if (e.kind === "mcp_surface_ready") {
        // Background readiness remains a no-op unless the sink explicitly
        // correlates it to this submit in the common preamble above.
        return s;
    }
    if (e.kind === "extension_surface" || e.kind === "extension_status") {
        // Sidecar publications without exact sink correlation remain background-
        // only and must not clear the retry indicator.
        return applyExtensionSurfaceEvent(s, e.extension);
    }
    if (e.kind === "retrying") {
        // Retrying is emitted synchronously from inside the foreground provider
        // request, immediately before its cancellation-aware backoff. Treat it as
        // authoritative proof that the turn is still active. An idle ListTabs
        // snapshot fetched before this event can otherwise clear `running`,
        // regardless of which one reaches the reducer first, and leave the
        // composer showing "retrying (n/m)" without its Stop button (or Escape
        // cancellation) until all retries are exhausted.
        return {
            ...s,
            retry: {
                attempt: e.retryAttempt ?? 0,
                max: e.retryMax ?? 0,
                observedAt: promptEventClock(),
            },
            running: true,
            turnActive: true,
            cancellable: true,
            turnStartAt: s.turnStartAt || Date.now(),
        };
    }
    if (e.kind === "stream_attempt") {
        return applyStreamAttempt(s, e);
    }
    if (s.retry)
        s = { ...s, retry: undefined };
    switch (e.kind) {
        case "turn_started": {
            // Pre-create an empty assistant bubble
            // immediately so the user sees their message + a blinking cursor the
            // instant the backend acknowledges the turn — no dead gap waiting for
            // the first text/reasoning token.
            const { items, id, seq } = ensureAssistant(s);
            return {
                ...s,
                items,
                currentAssistant: id,
                seq,
                live: { id, text: "", reasoning: "", reasoningComplete: false },
                running: true,
                turnActive: true,
                turnPhase: "working",
                completionSummary: undefined,
                pendingPrompt: false,
                cancelRequested: false,
                cancellable: true,
                ...resetTurnTiming(),
            };
        }
        case "turn_phase": {
            const phase = (e.phase ?? e.text ?? "").trim();
            if (!phase)
                return s;
            return { ...s, turnPhase: phase, running: true, turnActive: true, cancellable: true };
        }
        case "completion_summary": {
            if (!e.completion)
                return s;
            const completionSummary = normalizeCompletionSummary(e.completion);
            if (!completionSummaryNeedsAttention(completionSummary)) {
                return { ...s, completionSummary };
            }
            const notice = completionSummaryNotice(completionSummary, t);
            return {
                ...s,
                completionSummary,
                seq: s.seq + 1,
                items: [...s.items, {
                        kind: "notice",
                        id: `q${s.seq}`,
                        level: "warn",
                        variant: "completion",
                        title: notice.title,
                        text: notice.body,
                        action: "open_changes",
                    }],
            };
        }
        case "text":
        case "reasoning": {
            return applyDeltaSegments(s, [{ kind: e.kind, delta: e.text ?? e.reasoning ?? "" }]);
        }
        case "message": {
            const existingAssistant = s.currentAssistant === undefined
                ? undefined
                : s.items.find((it): it is Extract<Item, {
                    kind: "assistant";
                }> => it.kind === "assistant" && it.id === s.currentAssistant);
            const text = e.text ?? s.live?.text ?? existingAssistant?.text ?? "";
            const reasoning = e.reasoning ?? s.live?.reasoning ?? existingAssistant?.reasoning ?? "";
            if (text.trim() === "" && reasoning.trim() === "") {
                const items = existingAssistant && existingAssistant.text.trim() === "" && existingAssistant.reasoning.trim() === ""
                    ? s.items.filter((it) => !(it.kind === "assistant" && it.id === existingAssistant.id))
                    : s.items;
                return { ...endTurnModelActivity(s, Date.now(), true), items, live: undefined, currentAssistant: undefined, turnOutputCharsAtUsage: 0 };
            }
            const now = Date.now();
            const settled = endTurnModelActivity(s, now, true);
            const { items, id, seq } = ensureAssistant(settled);
            const streamedChars = settled.live?.id === id ? settled.live.text.length + settled.live.reasoning.length : 0;
            const turnOutputChars = Math.max(0, settled.turnOutputChars - streamedChars + text.length + reasoning.length);
            const completedLive = settled.live?.id === id ? completeLiveReasoning({ ...settled.live, text, reasoning }, now) : undefined;
            const reasoningDurationMs = liveReasoningDurationMs(completedLive);
            const workDurationMs = currentTurnDurationMs(settled, now);
            const next = items.map((it) => it.kind === "assistant" && it.id === id
                ? (() => {
                    return {
                        ...it,
                        text,
                        reasoning,
                        streaming: false,
                        reasoningComplete: reasoning !== "" || it.reasoningComplete,
                        reasoningDurationMs: reasoningDurationMs ?? it.reasoningDurationMs,
                        workDurationMs: Math.max(it.workDurationMs ?? 0, workDurationMs ?? 0) || undefined,
                    };
                })()
                : it);
            return { ...settled, items: next, live: undefined, currentAssistant: undefined, turnOutputChars, turnOutputCharsAtUsage: 0, seq };
        }
        case "tool_dispatch": {
            const t = e.tool;
            if (!t)
                return s;
            // A partial dispatch (args still streaming from the model) upserts a
            // lightweight "receiving" card immediately. Dropping it entirely — the
            // old behavior — left a 30KB write_file body streaming for a minute with
            // zero visible activity, indistinguishable from a hang. The full
            // dispatch that follows merges by ID and fills in args/summary.
            if (t.partial) {
                const activeState = t.parentId ? s : beginTurnModelActivity(s);
                const turnArgChars = t.argChars && t.argChars > 0 ? t.argChars : s.turnArgChars;
                // Some OpenAI-compatible streams surface the call name before its ID.
                // Without a stable ID the card could never be merged with the full
                // dispatch (a synthetic `tool${seq}` id would orphan it as a forever-
                // running duplicate), so count the progress but wait for the ID before
                // creating the card.
                if (!t.id)
                    return { ...activeState, turnArgChars };
                const id = t.id;
                const idx = activeState.items.findIndex((it) => it.kind === "tool" && it.id === id);
                if (idx >= 0) {
                    const next = [...activeState.items];
                    const it = next[idx];
                    if (it.kind === "tool" && it.status === "running" && !it.args) {
                        const prior = it;
                        next[idx] = { ...it, argChars: t.argChars || it.argChars };
                        return noteToolInJournal({ ...activeState, items: next, turnArgChars }, id, true, prior, {
                            attemptId: t.attemptId, parentId: t.parentId, partial: true,
                        });
                    }
                    return { ...activeState, turnArgChars };
                }
                return noteToolInJournal({
                    ...activeState,
                    turnArgChars,
                    seq: activeState.seq + 1,
                    items: [...activeState.items, { kind: "tool", id, name: t.name, args: "", readOnly: t.readOnly, resolvedName: t.resolvedName, capabilityId: t.capabilityId, status: "running", argChars: t.argChars || undefined, parentId: t.parentId, subagentProgress: SUBAGENT_PROGRESS_TOOLS.has(t.name) ? freshSubagentProgress() : undefined }],
                }, id, false, undefined, { attemptId: t.attemptId, parentId: t.parentId, partial: true });
            }
            const settled = t.parentId ? s : endTurnModelActivity(s, Date.now(), true);
            const id = t.id || `tool${s.seq}`;
            const idx = settled.items.findIndex((it) => it.kind === "tool" && it.id === id);
            if (idx >= 0) {
                const next = [...settled.items];
                const it = next[idx];
                if (it.kind === "tool") {
                    const args = t.args ? t.args : it.args;
                    const fileDiff = fileDiffFromWire(t);
                    const summary = summarizeFileDiff(fileDiff) || summarize(t.name, args) || (t.name === it.name && args === it.args ? it.summary : undefined);
                    next[idx] = { ...it, name: t.name, args, readOnly: t.readOnly, resolvedName: t.resolvedName ?? it.resolvedName, capabilityId: t.capabilityId ?? it.capabilityId, profile: t.profile ?? it.profile, summary, fileDiff, argChars: undefined, isShell: it.isShell || t.name === "bash" || id.startsWith("shell-"), execution: t.execution ?? it.execution, subagentProgress: it.subagentProgress ?? (SUBAGENT_PROGRESS_TOOLS.has(t.name) ? freshSubagentProgress() : undefined) };
                }
                if (t.parentId)
                    touchSubagentParent(next, t.parentId);
                return { ...settled, items: next };
            }
            const args = t.args ?? "";
            const fileDiff = fileDiffFromWire(t);
            const created: ToolItem = { kind: "tool", id, name: t.name, args, readOnly: t.readOnly, resolvedName: t.resolvedName, capabilityId: t.capabilityId, status: "running", summary: summarizeFileDiff(fileDiff) || summarize(t.name, args), fileDiff, isShell: t.name === "bash" || id.startsWith("shell-"), execution: t.execution, parentId: t.parentId, profile: t.profile, subagentProgress: SUBAGENT_PROGRESS_TOOLS.has(t.name) ? freshSubagentProgress() : undefined };
            const items = [...settled.items, created];
            // A sub-agent call nested under a task card refreshes that card's
            // recent activity and switches its phase to "tool".
            if (t.parentId)
                touchSubagentParent(items, t.parentId);
            return { ...settled, seq: settled.seq + 1, items };
        }
        case "tool_result": {
            const t = e.tool;
            if (!t)
                return s;
            const next = [...s.items];
            let idx = t.id ? next.findIndex((it) => it.kind === "tool" && it.id === t.id) : -1;
            if (idx < 0) {
                for (let i = next.length - 1; i >= 0; i--) {
                    const it = next[i];
                    if (it.kind === "tool" && it.status === "running") {
                        idx = i;
                        break;
                    }
                }
            }
            if (idx >= 0) {
                const it = next[idx];
                if (it.kind === "tool") {
                    // Archive immediately: collapsed cards only show tool name + command
                    // subject (from args). Drop output entirely; full data is loaded on
                    // demand via app.ToolResultForTab when the card is expanded.
                    const existing = it;
                    const summary = t.err ? undefined : existing.summary || summarize(existing.name, existing.args, t.output);
                    let status: ToolStatus = t.err ? "error" : "done";
                    if (existing.subagentProgress) {
                        // Sub-agent progress owns the card's final visual: a background
                        // call that returned a job id stays running while the child
                        // works; a cancelled child keeps its stopped semantics even when
                        // the aggregate result carries an error. Group cards
                        // (parallel_tasks/fleet) settle only from their own lifecycle
                        // terminal event — the backend emits running at start and exactly
                        // one terminal at the end (including validation failures and
                        // zero-child cancellation) — never from inferring the children
                        // observed so far, since a background group's children dispatch
                        // asynchronously and a fast child can finish before later ones
                        // even appear.
                        if (isGroupSubagentTool(existing.name)) {
                            status = isTerminalSubagentPhase(existing.subagentProgress.phase)
                                ? terminalStatusOf(existing.subagentProgress.phase)
                                : "running";
                        }
                        else if (!isTerminalSubagentPhase(existing.subagentProgress.phase)) {
                            status = "running";
                        }
                        else {
                            status = terminalStatusOf(existing.subagentProgress.phase);
                        }
                    }
                    next[idx] = {
                        ...existing,
                        readOnly: t.readOnly,
                        resolvedName: t.resolvedName ?? existing.resolvedName,
                        capabilityId: t.capabilityId ?? existing.capabilityId,
                        status,
                        output: t.output,
                        error: t.err,
                        truncated: t.truncated,
                        durationMs: t.durationMs,
                        summary,
                        isShell: existing.isShell || existing.name === "bash" || t.name === "bash",
                        execution: t.execution ?? existing.execution,
                    };
                }
            }
            // A nested result refreshes its sub-agent parent's recent activity.
            if (t.parentId)
                touchSubagentParent(next, t.parentId);
            return { ...s, items: compactArchivedToolItems(next) };
        }
        case "tool_progress": {
            const t = e.tool;
            if (!t?.id)
                return s;
            // Reserved sub-agent progress channels update the card's in-memory
            // preview; they never touch tool.output or the parent's live stream.
            if (isSubagentProgressName(t.name)) {
                return applySubagentProgress(s, t);
            }
            const idx = s.items.findIndex((it) => it.kind === "tool" && it.id === t.id);
            if (idx < 0)
                return s;
            const next = [...s.items];
            const it = next[idx];
            if (it.kind === "tool")
                next[idx] = { ...it, output: (it.output ?? "") + (t.output ?? "") };
            // Streaming output of a sub-agent's real tool refreshes its card.
            if (t.parentId)
                touchSubagentParent(next, t.parentId);
            return { ...s, items: next };
        }
        case "usage": {
            if (!countsTowardCurrentTurn(s))
                return s;
            const updateContextGauge = updatesContextGauge(e.usage);
            // Only executor usage belongs to the foreground model stream. Planner,
            // subagent, and auxiliary usage still contributes to session totals and
            // usageSeq, but must not close or inflate the executor TPS interval.
            const settled = updateContextGauge ? endTurnModelActivity(s, Date.now(), true) : s;
            const hasRequestCompletion = (e.usage?.contextCompletionTokens ?? 0) > 0;
            const requestModelMs = updateContextGauge ? (settled.pendingRequestModelMs ?? 0) : 0;
            const requestTokens = updateContextGauge ? (hasRequestCompletion ? (e.usage?.contextCompletionTokens ?? 0) : (e.usage?.completionTokens ?? 0)) : 0;
            const lastRequestTps = updateContextGauge ? (requestTokens > 0 && requestModelMs >= 500 ? requestTokens / (requestModelMs / 1000) : null) : s.lastRequestTps;
            // Context* is the latest sampling attempt; other token fields are billable aggregates.
            let used = settled.context.used;
            if (e.usage && settled.context.window && updateContextGauge)
                used = (e.usage.contextPromptTokens ?? 0) > 0
                    ? (e.usage.contextPromptTokens ?? 0)
                    : (e.usage.promptTokens ?? 0);
            const turnTokens = settled.turnTokens + (e.usage?.completionTokens ?? 0);
            const turnOutputTokens = updateContextGauge
                ? settled.turnOutputTokens + (e.usage?.completionTokens ?? 0)
                : settled.turnOutputTokens;
            const turnOutputCharsAtUsage = updateContextGauge
                ? (settled.live?.text.length ?? 0) + (settled.live?.reasoning.length ?? 0)
                : settled.turnOutputCharsAtUsage;
            const turnOutputEstimated = updateContextGauge
                ? settled.turnOutputEstimated || Boolean(e.usage?.estimated)
                : settled.turnOutputEstimated;
            const usageTokens = usageTotalTokens(e.usage);
            const turnTotalTokens = settled.turnTotalTokens + usageTokens;
            const sessionTokens = settled.sessionTokens + usageTokens;
            const usageCost = e.usage?.cost ?? e.usage?.costUsd ?? 0;
            const turnCost = settled.turnCost + usageCost;
            const sessionCost = settled.sessionCost + usageCost;
            const sessionCurrency = e.usage?.currency || settled.sessionCurrency || "¥";
            const usage = updateContextGauge ? e.usage : settled.usage;
            // The completed round's usage now accounts for the streamed tool-call
            // arguments, so drop the live estimate rather than double-count it.
            return { ...settled, usage, context: { ...settled.context, used, sessionTokens }, turnTokens, turnOutputTokens, turnOutputCharsAtUsage, turnOutputEstimated, turnTotalTokens, turnCost, turnArgChars: updateContextGauge ? 0 : settled.turnArgChars, sessionTokens, sessionCost, sessionCurrency, usageSeq: settled.usageSeq + 1, lastRequestTps, pendingRequestModelMs: updateContextGauge ? undefined : settled.pendingRequestModelMs };
        }
        case "notice":
            return appendNoticeToState(s, e.level ?? "info", e.text ?? "", e.detail, e.code, e.decisionReceipt);
        case "context_maintenance": {
            const m = e.maintenance;
            if (!m || m.status === "noop")
                return s;
            if (!isNewMaintenanceOperation(s.seenMaintenanceOps, m.operationId))
                return s;
            const next = appendNoticeToState(s, m.status === "failed" ? "warn" : "info", formatContextMaintenanceNotice(m, t), m.reason);
            return { ...next, seenMaintenanceOps: rememberMaintenanceOperation(s.seenMaintenanceOps, m.operationId) };
        }
        case "phase":
            return { ...s, seq: s.seq + 1, items: [...s.items, { kind: "phase", id: `p${s.seq}`, text: e.text ?? "" }] };
        case "compaction_started":
            return { ...s, seq: s.seq + 1, items: [...s.items, { kind: "compaction", id: `c${s.seq}`, pending: true, trigger: e.compaction?.trigger ?? "", messages: 0, summary: "", archive: "" }] };
        case "compaction_done": {
            const c = e.compaction;
            const idx = [...s.items].reverse().findIndex((it) => it.kind === "compaction" && it.pending);
            const at = idx < 0 ? -1 : s.items.length - 1 - idx;
            if (!c?.summary) {
                const items = at < 0 ? s.items : s.items.filter((_, i) => i !== at);
                return { ...s, running: s.turnActive ? s.running : false, items };
            }
            const filled: Item = { kind: "compaction", id: at < 0 ? `c${s.seq}` : (s.items[at] as Extract<Item, {
                    kind: "compaction";
                }>).id, pending: false, trigger: c.trigger ?? "", messages: c.messages ?? 0, summary: c.summary, archive: c.archive ?? "" };
            const items = at < 0 ? [...s.items, filled] : s.items.map((it, i) => (i === at ? filled : it));
            return { ...s, running: s.turnActive ? s.running : false, seq: s.seq + 1, items };
        }
        case "steer":
            return { ...s, seq: s.seq + 1, items: [...s.items, { kind: "notice", id: `s${s.seq}`, level: "info", text: `${STEER_NOTICE_PREFIX}${e.text ?? ""}` }] };
        case "approval_request": {
            if (s.cancelRequested)
                return s;
            // A delayed re-delivery of a prompt the user already answered locally
            // (clearApproval) must not resurrect it — no downstream snapshot is
            // guaranteed to ever reject it again (#6432 round 2).
            if (e.approval?.id !== undefined && e.approval.id === s.resolvedPromptId)
                return s;
            return beginPromptWait({
                ...s,
                approval: e.approval,
                // A replay of the SAME prompt (post-answer delayed delivery, or the
                // #6429 re-arm after activation) keeps the original arrival time; only
                // a genuinely new prompt id re-anchors it (#6432 reverse race).
                promptArrivedAt: e.approval?.id === s.promptArrivedId ? s.promptArrivedAt : promptEventClock(),
                promptArrivedId: e.approval?.id,
                pendingPrompt: true,
                running: true,
                turnActive: true,
                cancellable: true,
            });
        }
        case "ask_request": {
            if (s.cancelRequested)
                return s;
            if (e.ask?.id !== undefined && e.ask.id === s.resolvedPromptId)
                return s;
            return beginPromptWait({
                ...s,
                ask: e.ask,
                promptArrivedAt: e.ask?.id === s.promptArrivedId ? s.promptArrivedAt : promptEventClock(),
                promptArrivedId: e.ask?.id,
                pendingPrompt: true,
                running: true,
                turnActive: true,
                cancellable: true,
            });
        }
        case "guardian_assessment": {
            if (!e.guardian)
                return s;
            const level = e.guardian.outcome === "deny" ? "warn" : "info";
            return { ...s, seq: s.seq + 1, items: [...s.items, { kind: "notice", id: `g${s.seq}`, level, text: formatGuardianAssessmentNotice(e.guardian) }] };
        }
        case "turn_done": {
            const now = Date.now();
            s = snapshotCompletedTurnTelemetry(s, now);
            const workDurationMs = currentTurnDurationMs(s, now);
            let lastUserIndex = -1;
            let lastAssistantIndex = -1;
            for (let i = 0; i < s.items.length; i++) {
                if (s.items[i].kind === "user") {
                    lastUserIndex = i;
                    lastAssistantIndex = -1;
                }
                else if (i > lastUserIndex && s.items[i].kind === "assistant") {
                    lastAssistantIndex = i;
                }
            }
            const finalized = s.items.map((it, index) => {
                if (it.kind === "assistant" && s.live && it.id === s.live.id) {
                    const completedLive = completeLiveReasoning(s.live, now);
                    return {
                        ...it,
                        text: completedLive.text,
                        reasoning: completedLive.reasoning,
                        streaming: false,
                        reasoningComplete: completedLive.reasoning !== "" || completedLive.reasoningComplete,
                        reasoningDurationMs: liveReasoningDurationMs(completedLive) ?? it.reasoningDurationMs,
                        workDurationMs: index === lastAssistantIndex
                            ? Math.max(it.workDurationMs ?? 0, workDurationMs ?? 0) || undefined
                            : it.workDurationMs,
                    };
                }
                if (it.kind === "assistant") {
                    return {
                        ...it,
                        streaming: false,
                        workDurationMs: index === lastAssistantIndex
                            ? Math.max(it.workDurationMs ?? 0, workDurationMs ?? 0) || undefined
                            : it.workDurationMs,
                    };
                }
                if (it.kind === "tool" && it.status === "running")
                    return { ...it, status: "stopped" as const };
                return it;
            });
            let items: Item[] = s.deliveryRecoveryActive && !e.err
                ? finalized.filter((item) => item.kind !== "notice" || item.variant !== "delivery")
                : finalized;
            if (e.outcome === "final_readiness") {
                const previous = items.map((item) => item.kind === "notice" && item.variant === "delivery"
                    ? { ...item, action: undefined }
                    : item);
                items = [...previous, {
                        kind: "notice",
                        id: `e${s.seq}`,
                        level: "info",
                        variant: "delivery",
                        title: t("notice.deliveryIncompleteTitle"),
                        text: t("notice.deliveryIncompleteBody"),
                        detail: deliveryReadinessDetail(e.readiness, e.err),
                        action: "continue_delivery",
                    }];
            }
            else if (e.outcome === "recovery_paused") {
                // Informational pause — not a send failure. Composer is immediately free.
                items = [...finalized, {
                        kind: "notice",
                        id: `e${s.seq}`,
                        level: "info",
                        title: t("notice.recoveryPausedTitle"),
                        text: t("notice.recoveryPausedBody"),
                    }];
            }
            else if (e.err) {
                items = [...finalized, { kind: "notice", id: `e${s.seq}`, level: "warn", text: e.err }];
            }
            // Plan approval can arrive before turn_done on some Wails event paths.
            // Keep that gate visible instead of clearing the only UI that can answer it.
            const keepPlanApproval = s.approval?.tool === "exit_plan_mode";
            let next: State = {
                ...s,
                items: applyTurnCheckpoint(items, e.submissionId, e.checkpointTurn),
                live: undefined,
                streamAttemptJournal: undefined,
                running: keepPlanApproval,
                turnActive: keepPlanApproval,
                turnPhase: keepPlanApproval ? s.turnPhase : undefined,
                pendingPrompt: keepPlanApproval,
                cancelRequested: false,
                cancellable: keepPlanApproval,
                currentAssistant: undefined,
                approval: keepPlanApproval ? s.approval : undefined,
                ask: undefined,
                deliveryRecoveryActive: false,
                seq: s.seq + 1,
            };
            // Close user-wait unless the plan approval gate remains open.
            if (!keepPlanApproval)
                next = endPromptWait(next, now);
            return next;
        }
        default: return s;
    }
}

