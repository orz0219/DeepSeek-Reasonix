import { applyHydrateErrorState } from "./hydrateErrorState";
import { State, Item, initialState } from "./controller_state";
import { compactArchivedToolItems, Action, historyMessagesToItems, historyPageItems, applyStreamBatch } from "./controller_history";
import { runtimeSnapshotPredatesPrompt, foregroundRunningFromRuntimeMeta, runtimeSnapshotPredatesRetry, sameMeta, metaWithoutCanonicalTodos } from "./controller_meta";
import { applyEvent } from "./controller_apply";
export function beginPromptWait(s: State, now = Date.now()): State {
    if (s.promptWaitStartedAt && s.promptWaitStartedAt > 0)
        return s;
    return { ...s, promptWaitStartedAt: now };
}
export function endPromptWait(s: State, now = Date.now()): State {
    if (!s.promptWaitStartedAt || s.promptWaitStartedAt <= 0) {
        return s.promptWaitStartedAt === undefined ? s : { ...s, promptWaitStartedAt: undefined };
    }
    const delta = Math.max(0, now - s.promptWaitStartedAt);
    return {
        ...s,
        turnWaitAccumMs: Math.max(0, s.turnWaitAccumMs || 0) + delta,
        promptWaitStartedAt: undefined,
    };
}
function endPromptWaitIfIdle(s: State, now = Date.now()): State {
    if (s.approval || s.ask)
        return s;
    return endPromptWait(s, now);
}
export function resetTurnTiming(now = Date.now()): Pick<State, "turnStartAt" | "turnDoneAt" | "turnWaitAccumMs" | "promptWaitStartedAt" | "turnTokens" | "turnTotalTokens" | "turnOutputTokens" | "turnOutputChars" | "turnOutputCharsAtUsage" | "turnOutputEstimated" | "turnModelActiveAt" | "turnModelActiveMs" | "turnCost" | "turnArgChars" | "pendingRequestModelMs"> {
    return {
        turnStartAt: now,
        turnDoneAt: 0,
        turnWaitAccumMs: 0,
        promptWaitStartedAt: undefined,
        turnTokens: 0,
        turnTotalTokens: 0,
        turnOutputTokens: 0,
        turnOutputChars: 0,
        turnOutputCharsAtUsage: 0,
        turnOutputEstimated: false,
        turnModelActiveAt: undefined,
        turnModelActiveMs: 0, pendingRequestModelMs: undefined,
        turnCost: 0,
        turnArgChars: 0,
    };
}
export function confirmPendingUser(s: State, submissionId: string | undefined): State {
    if (!submissionId || s.pendingSubmissionId !== submissionId)
        return s;
    return { ...s, pendingUser: undefined, pendingSubmissionId: undefined };
}
export function beginTurnModelActivity(s: State, now = Date.now()): State {
    return s.turnModelActiveAt && s.turnModelActiveAt > 0
        ? s
        : { ...s, turnModelActiveAt: now };
}
export function endTurnModelActivity(s: State, now = Date.now(), stashForUsage = false): State {
    if (!s.turnModelActiveAt || s.turnModelActiveAt <= 0)
        return s;
    const closedMs = Math.max(0, now - s.turnModelActiveAt);
    return { ...s, turnModelActiveAt: undefined, turnModelActiveMs: Math.max(0, s.turnModelActiveMs) + closedMs,
        pendingRequestModelMs: stashForUsage ? closedMs : s.pendingRequestModelMs };
}
export function snapshotCompletedTurnTelemetry(s: State, now = Date.now()): State {
    const settled = endTurnModelActivity(s, now);
    const liveChars = (settled.live?.text.length ?? 0) + (settled.live?.reasoning.length ?? 0);
    const inFlightChars = settled.turnOutputTokens > 0
        ? Math.max(0, liveChars - settled.turnOutputCharsAtUsage) + settled.turnArgChars
        : settled.turnOutputChars + settled.turnArgChars;
    const estimatedInFlightTokens = Math.round(inFlightChars / 4);
    return {
        ...settled,
        turnDoneAt: now,
        lastTurnOutputTokens: settled.turnOutputTokens + estimatedInFlightTokens,
        lastTurnStartAt: settled.turnStartAt,
        lastTurnDoneAt: now,
        lastTurnWaitAccumMs: settled.turnWaitAccumMs,
        lastTurnModelMs: settled.turnModelActiveMs,
        lastTurnOutputEstimated: settled.turnOutputEstimated || estimatedInFlightTokens > 0 || (settled.turnOutputTokens === 0 && settled.turnOutputChars > 0),
    };
}
export function reducer(s: State, a: Action): State {
    switch (a.type) {
        case "user": {
            const seq = a.seq !== undefined ? a.seq : s.seq;
            const userItemId = `u${seq}`;
            return {
                ...s,
                seq: seq + 1,
                items: [...s.items, { kind: "user", id: userItemId, submissionId: a.submissionId, text: a.text, submitText: a.submitText, createdAt: Date.now() }],
                running: true,
                pendingPrompt: false,
                cancelRequested: false,
                cancellable: true,
                ...resetTurnTiming(),
                // New turn epoch: forget the previous prompt anchor so a genuinely new
                // prompt re-anchors freshly instead of inheriting a stale id/time.
                promptArrivedAt: undefined,
                promptArrivedId: undefined,
                pendingUser: a.text,
                pendingSubmissionId: a.submissionId,
                deliveryRecoveryActive: Boolean(a.deliveryRecovery),
                discardTurn: false,
            };
        }
        case "unsend": {
            const cleared = endPromptWait({
                ...s,
                pendingUser: undefined,
                pendingSubmissionId: undefined,
                discardTurn: true,
                running: false,
                pendingPrompt: false,
                cancelRequested: true,
                cancellable: false,
                approval: undefined,
                ask: undefined,
                promptArrivedAt: undefined,
                promptArrivedId: undefined,
                live: undefined,
            });
            return cleared;
        }
        case "cancel_requested": {
            return endPromptWait({
                ...s,
                pendingPrompt: false,
                cancelRequested: true,
                approval: undefined,
                ask: undefined,
                promptArrivedAt: undefined,
                promptArrivedId: undefined,
                cancellable: s.running || s.turnActive,
            });
        }
        case "send_confirmed": return confirmPendingUser(s, a.submissionId);
        case "send_failed": {
            if (s.pendingSubmissionId !== a.submissionId)
                return s;
            const idx = s.items.findIndex((it) => it.kind === "user" && it.submissionId === a.submissionId);
            const items = idx >= 0 ? s.items.map((it, i) => (i === idx ? { ...it, submissionId: undefined, failed: true } : it)) : s.items;
            const notice: Item = { kind: "notice", id: `n${s.seq}`, level: "warn", text: a.error };
            return { ...s, pendingUser: undefined, pendingSubmissionId: undefined, deliveryRecoveryActive: false, running: false, turnActive: false, pendingPrompt: false, cancelRequested: false, cancellable: false, live: undefined, seq: s.seq + 1, items: [...items, notice] };
        }
        case "backend_status": {
            // A snapshot fetched before the live approval/ask event arrived cannot
            // know about the prompt; everything it reports about the turn lifecycle
            // is equally stale. Ignore it and let an explicit answer/cancel or a
            // fresher snapshot settle the state (#6429).
            if (runtimeSnapshotPredatesPrompt(s, a.snapshotAt))
                return s;
            const pendingPrompt = Boolean(a.pendingPrompt);
            const backgroundJobs = Math.max(0, a.backgroundJobs ?? s.backgroundJobs ?? 0);
            const cancelRequested = Boolean(a.cancelRequested);
            const foregroundRunning = foregroundRunningFromRuntimeMeta({ running: a.running, pendingPrompt, backgroundJobs, cancellable: a.cancellable });
            // A retry event is newer evidence of foreground activity than an idle
            // snapshot whose fetch started earlier. Keep the turn cancellable until
            // a snapshot started after the retry confirms that it is actually idle.
            if (!foregroundRunning && runtimeSnapshotPredatesRetry(s, a.snapshotAt))
                return s;
            const cancellable = foregroundRunning;
            const clearsRetry = !foregroundRunning && s.retry !== undefined;
            if (foregroundRunning === s.running &&
                pendingPrompt === s.pendingPrompt &&
                backgroundJobs === s.backgroundJobs &&
                cancelRequested === s.cancelRequested &&
                cancellable === s.cancellable &&
                !clearsRetry)
                return s;
            if (foregroundRunning) {
                return {
                    ...s,
                    running: true,
                    turnActive: true,
                    pendingPrompt,
                    backgroundJobs,
                    cancelRequested,
                    cancellable,
                    turnStartAt: s.turnStartAt || Date.now(),
                };
            }
            const telemetry = snapshotCompletedTurnTelemetry(s);
            const finalized = telemetry.items.map((it) => {
                if (it.kind === "assistant" && telemetry.live && it.id === telemetry.live.id)
                    return { ...it, text: telemetry.live.text, reasoning: telemetry.live.reasoning, streaming: false };
                if (it.kind === "assistant" && it.streaming)
                    return { ...it, streaming: false };
                if (it.kind === "tool" && it.status === "running")
                    return { ...it, status: "stopped" as const };
                return it;
            });
            return endPromptWait({
                ...telemetry,
                items: finalized,
                running: false,
                turnActive: false,
                pendingPrompt,
                backgroundJobs,
                cancelRequested,
                cancellable,
                live: undefined,
                currentAssistant: undefined,
                approval: undefined,
                ask: undefined,
                retry: undefined,
            });
        }
        case "meta": {
            const meta = a.meta.sessionPath === undefined && s.meta?.sessionPath !== undefined ? { ...a.meta, sessionPath: s.meta.sessionPath } : a.meta;
            return sameMeta(s.meta, meta) ? s : { ...s, meta };
        }
        case "optimistic_meta": return sameMeta(s.meta, a.meta) ? s : { ...s, meta: a.meta, hydrateError: undefined };
        case "context": {
            const sessionTokens = typeof a.context.sessionTokens === "number"
                ? Math.max(0, a.context.sessionTokens)
                : s.sessionTokens;
            const sessionCost = typeof a.context.sessionCost === "number" && a.context.sessionCost > 0
                ? a.context.sessionCost
                : s.sessionCost;
            const sessionCurrency = a.context.sessionCurrency || s.sessionCurrency;
            // Mid-turn snapshot refreshes can race a rebuilt executor whose
            // LastUsage is still nil: the backend then reports used=0 for a session
            // that visibly holds tokens, and the gauge collapses to "0/1M" until the
            // next executor usage arrives. Keep the last known fill while a turn is
            // live; genuine resets flow through the "reset" action or land when the
            // session is idle.
            const context = a.context.used === 0 && s.context.used > 0 && (s.running || s.turnActive) && a.context.window === s.context.window
                ? { ...a.context, used: s.context.used }
                : a.context;
            return { ...s, context, sessionTokens, sessionCost, sessionCurrency };
        }
        case "balance": return { ...s, balance: a.balance };
        case "effort": return { ...s, effort: a.effort };
        case "jobs": return { ...s, jobs: a.jobs };
        case "checkpoints": return { ...s, checkpoints: a.checkpoints };
        case "hydrate_start": return {
            ...s,
            hydrating: true,
            hydrateReason: a.reason,
            hydrateError: undefined,
            hydrateHistoryLoaded: false,
            hydratePlaceholderItems: a.placeholderItems?.length ? a.placeholderItems : undefined,
        };
        case "hydrate_done": return s.hydrating || s.hydrateReason || s.hydrateError || s.hydrateHistoryLoaded || s.hydratePlaceholderItems
            ? { ...s, hydrating: false, hydrateReason: undefined, hydrateError: undefined, hydrateHistoryLoaded: undefined, hydratePlaceholderItems: undefined }
            : s;
        case "hydrate_error": return applyHydrateErrorState(s, a.reason, a.error);
        case "backend_activation_start": return {
            ...s,
            // The target tab may contain a prompt event that was routed there while
            // frontend selection was ahead of backend activation. Reset that
            // uncertain lifecycle first; optimistic backend metadata is applied
            // immediately afterwards and restores a genuinely running target.
            backendActivationPending: true,
            pendingPrompt: false,
            approval: undefined,
            ask: undefined,
            // New tab epoch: drop the prompt anchor so the post-activation replay
            // re-anchors against this activation, keeping the #6429 stale-snapshot
            // guard armed for the freshly restored prompt.
            promptArrivedAt: undefined,
            promptArrivedId: undefined,
            running: false,
            turnActive: false,
            cancellable: false,
        };
        case "backend_activation_done": return s.backendActivationPending ? { ...s, backendActivationPending: false } : s;
        case "message_action_start": return { ...s, messageAction: a.action };
        case "message_action_done": return { ...s, messageAction: undefined };
        case "history": {
            const { items, seq } = historyMessagesToItems(a.messages, "h", s.seq);
            return { ...s, items: compactArchivedToolItems(items), pendingSubmissionId: undefined, seq, hydrateHistoryLoaded: true, hydratePlaceholderItems: undefined, historyStartTurn: 0, historyTotalTurns: 0, historyHasOlder: false, historyOlderLoading: false, historyRevision: undefined, historyDigest: undefined };
        }
        case "history_page": {
            const { items, seq } = historyPageItems(a.page);
            const nextItems = a.mode === "prepend" ? [...items, ...s.items] : items;
            return {
                ...s,
                items: compactArchivedToolItems(nextItems),
                pendingSubmissionId: a.mode === "replace" ? undefined : s.pendingSubmissionId,
                seq: Math.max(s.seq, seq),
                hydrateHistoryLoaded: true,
                hydratePlaceholderItems: undefined,
                historyStartTurn: a.page.startTurn,
                historyTotalTurns: a.page.totalTurns,
                historyHasOlder: a.page.hasOlder,
                historyOlderLoading: false,
                historyRevision: a.page.revision,
                historyDigest: a.page.digest,
            };
        }
        case "history_older_start": return s.historyOlderLoading ? s : { ...s, historyOlderLoading: true };
        case "history_older_error": return s.historyOlderLoading ? { ...s, historyOlderLoading: false } : s;
        case "history_replace":
            return {
                ...s,
                items: compactArchivedToolItems(a.items),
                pendingSubmissionId: undefined,
                hydrateHistoryLoaded: true,
                hydratePlaceholderItems: undefined,
                historyStartTurn: a.startTurn,
                historyTotalTurns: a.totalTurns,
                historyHasOlder: a.hasOlder,
                historyOlderLoading: false,
                historyRevision: a.revision,
                historyDigest: a.digest,
            };
        case "history_prepend": {
            const remove = a.removeIds.length > 0 ? new Set(a.removeIds) : undefined;
            const rest = remove ? s.items.filter((item) => !remove.has(item.id)) : s.items;
            return {
                ...s,
                items: compactArchivedToolItems([...a.items, ...rest]),
                hydrateHistoryLoaded: true,
                hydratePlaceholderItems: undefined,
                historyStartTurn: a.startTurn,
                historyTotalTurns: a.totalTurns,
                historyHasOlder: a.hasOlder,
                historyOlderLoading: false,
                historyRevision: a.revision,
                historyDigest: a.digest,
            };
        }
        // Ref-resolved full content landed for history items already on screen:
        // patch by stable item id so the live tail and untouched items keep their
        // identity.
        case "history_items_patch": {
            let changed = false;
            const next = s.items.map((item) => {
                const patch = a.patches[item.id];
                if (!patch)
                    return item;
                changed = true;
                return patch;
            });
            return changed ? { ...s, items: next } : s;
        }
        case "local_notice": return { ...s, running: false, turnActive: false, seq: s.seq + 1, items: [...s.items, { kind: "notice", id: `n${s.seq}`, level: a.level, text: a.text }] };
        case "clearApproval": {
            const next = { ...s, approval: undefined, pendingPrompt: Boolean(s.ask), resolvedPromptId: s.approval?.id ?? s.resolvedPromptId };
            return endPromptWaitIfIdle(next);
        }
        case "clearAsk": {
            const next = { ...s, ask: undefined, pendingPrompt: Boolean(s.approval), resolvedPromptId: s.ask?.id ?? s.resolvedPromptId };
            return endPromptWaitIfIdle(next);
        }
        case "clearExtensionForm": return s.extensionForm ? { ...s, extensionForm: undefined } : s;
        case "extension_notifications_drained": return s.extensionNotifications.length > 0 ? { ...s, extensionNotifications: [] } : s;
        // A tool-approval posture switch auto-allowed exactly these prompt ids on
        // the backend. Hide + tombstone the visible approval only when it is one
        // of them; anything else (plan/memory/sandbox-escape, ask-rule approvals
        // under auto) is still genuinely pending there and must stay visible —
        // tombstoning it would filter every future replay and strand the turn. The
        // drain result must also belong to this controller's prompt-id epoch.
        case "approval_drained": {
            if (s.promptEpoch !== a.epoch || !s.approval || !a.ids.includes(s.approval.id))
                return s;
            const next = { ...s, approval: undefined, pendingPrompt: Boolean(s.ask), resolvedPromptId: s.approval.id };
            return endPromptWaitIfIdle(next);
        }
        // The optimistic clearApproval/clearAsk tombstone was wrong: the backend
        // call that was supposed to actually resolve this id failed, so the
        // prompt is still genuinely pending there. Undo the tombstone so the next
        // replay (proactively requested by the caller) can re-arm it instead of
        // being silently swallowed forever. Only for the epoch the RPC was issued
        // in: after a controller rebuild the same numeric id names a DIFFERENT
        // prompt, and a late failure from the old controller must not erase the
        // new controller's tombstone.
        case "submit_prompt_failed":
            return s.resolvedPromptId === a.id && s.promptEpoch === a.epoch ? { ...s, resolvedPromptId: undefined } : s;
        // A controller rebuild (model/effort/token-mode switch) replaces the
        // backend controller in place and its approval/ask ids restart from "1"
        // (per-controller counters, see sound.ts). Any id-anchored bookkeeping
        // from the OLD controller is meaningless for the new one and must be
        // dropped, or a genuinely new prompt reusing an old id would be misread
        // as a stale replay of an already-answered prompt and silently ignored.
        case "controller_rebuilt":
            // A rebuild restarts the runtime's extension sidecars too, so extension
            // surface state (and the per-surface generation fence) from the old
            // runtime is meaningless for the new one and is dropped with the rest of
            // the id-anchored bookkeeping.
            return {
                ...s,
                items: s.items.map((item) => item.kind === "user" && item.submissionId ? { ...item, submissionId: undefined } : item),
                promptEpoch: s.promptEpoch + 1,
                pendingSubmissionId: undefined,
                resolvedPromptId: undefined,
                promptArrivedId: undefined,
                promptArrivedAt: undefined,
                extensionStatuses: {},
                extensionForm: undefined,
                extensionNotifications: [],
                extensionGenerations: {},
            };
        case "reset": return { ...initialState, meta: metaWithoutCanonicalTodos(s.meta), context: { used: 0, window: s.context.window, sessionTokens: 0, compactRatio: s.context.compactRatio }, balance: s.balance, effort: s.effort, jobs: s.jobs, hydrating: s.hydrating, hydrateReason: s.hydrateReason, hydrateError: s.hydrateError, hydrateHistoryLoaded: s.hydrateHistoryLoaded, hydratePlaceholderItems: s.hydratePlaceholderItems, backendActivationPending: s.backendActivationPending, sessionGen: s.sessionGen + 1, promptEpoch: s.promptEpoch + 1 };
        case "context_panel_refresh": return { ...s, contextPanelSeq: s.contextPanelSeq + 1 };
        case "event": return applyEvent(s, a.e);
        case "stream_batch": return applyStreamBatch(s, a.segments);
        default: return s;
    }
}
export { applyEvent as applyEvent } from "./controller_apply";

