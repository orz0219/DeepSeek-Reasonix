// useController is the frontend's state machine over the agent event stream. It keeps
// per-tab output, tool state, and approvals while the user switches tabs; components
// render the active tab's state.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { asArray } from "./array";
import { addBreadcrumb } from "./breadcrumbs";
import { app, onEvent, onReady, onRuntimeRebuilt, onTabMeta, onTopicActivation } from "./bridge";
import { invalidateCache } from "./composerHistory";
import { formatInboxCancelError, inboxSteerQueuedMessage } from "./inboxError";
import { invalidateSharedQuery } from "./queryCoalesce";
import { createRafBatch } from "./rafBatch";
import { aliasActivationRequest, noteActivationRequested, noteActivationSettled, noteActivationStarted } from "./sessionDiagnostics";
import { coalesceStreamDeltas, type StreamDeltaEntry } from "./streamDeltaBatch";
import { getTranscriptStore } from "./transcriptStore";
import { uiPerfTracker } from "./uiPerf";
import { getLocale, t } from "./i18n";
import { hydratePlaceholderItems as resolveHydratePlaceholders } from "./hydrateErrorState";
import { hydrateIdentityCurrent } from "./sessionIdentity";
import type { BalanceInfo, CollaborationMode, DeliveryWorktreeOpenResult, HistoryMessage, HistoryPage, MemoryView, Meta, Mode, QuestionAnswer, SessionMeta, TabMeta, TokenMode, ToolApprovalMode, TopicActivationEvent } from "./types";
import { TURN_ACTIVITY_KINDS } from "./controller_subagent";
import { ControllerLiveStore, MessageActionScope, HydrateReason, Item, initialState, SyncActiveTabOptions, PendingTopicActivation, ModelSwitchQueueResult, ModelSwitchQueueRequest, ModelSwitchQueueState, HISTORY_PAGE_TURNS } from "./controller_state";
import { foregroundRunningFromRuntimeMeta, promptEventClock, runtimeSnapshotPredatesPrompt, metaFromTab, runtimeReadyForSubmit, normalizeTurnSubmit, createTurnSubmissionId, acceptsRuntimeEventEpoch, composerProfileApplicationKey, shouldReconcileStaleTurn, RuntimeMetaSnapshot, STALE_TURN_RECONCILE_MS, CANCEL_RECONCILE_DELAYS_MS, STALE_PROMPT_RECONCILE_MS, STARTUP_READY_META_RECONCILE_MS, STARTUP_READY_META_RECONCILE_ATTEMPTS, hasCachedLiveTurn, hasReusableCachedTranscript, historyFingerprintMatchesMeta } from "./controller_meta";
import { Action, backendStatusFromRuntimeMeta } from "./controller_history";
import { reducer } from "./controller_reducer";
import { effortSwitchNoticeText, modelSwitchNoticeText, tokenModeSwitchNoticeText, TabStates, getOrCreateState, messageActionBusyText, errorMessage } from "./controller_notice";
export function replayPendingPromptsForActiveTab(activeTabId: string | undefined, replay: () => Promise<void> = () => app.ReplayPendingPrompts()): void {
    if (!activeTabId)
        return;
    void replay().catch(() => { });
}
export function useController() {
    const statesRef = useRef<TabStates>(new Map());
    const liveListenersByTabRef = useRef(new Map<string, Set<() => void>>());
    const balanceRefreshSeqByTab = useRef(new Map<string, number>());
    const modelSwitchSeqByTab = useRef(new Map<string, number>());
    const modelSwitchSuccessVersionByTab = useRef(new Map<string, number>());
    const modelSwitchQueueByTab = useRef(new Map<string, ModelSwitchQueueState>());
    const lastTurnActivityAtByTab = useRef(new Map<string, number>());
    const runtimeEpochByTabRef = useRef(new Map<string, string>());
    const appliedComposerProfileByTabRef = useRef(new Map<string, string>());
    const composerProfileInFlightByTabRef = useRef(new Map<string, {
        key: string;
        promise: Promise<boolean>;
    }>());
    const composerProfileQueueByTabRef = useRef(new Map<string, Promise<void>>());
    const composerProfileLifecycleByTabRef = useRef(new Map<string, number>());
    const cancelReconcileTimers = useRef(new Map<string, number>());
    const stalePromptReconcileTimers = useRef(new Map<string, number>());
    // Indirection so dispatchRuntimeStatusForTab (defined above reconcileTabRuntime)
    // can schedule an authoritative refetch after it rejects a stale snapshot.
    const scheduleStalePromptReconcileRef = useRef<(tabId: string) => void>(() => { });
    const [activeTabId, setActiveTabId] = useState<string | undefined>();
    const activeTabIdRef = useRef<string | undefined>(undefined);
    // Invalidates async navigation completions even for ABA switches where the
    // visible tab ID eventually returns to the original value.
    const activeNavigationSeqRef = useRef(0);
    // A render-triggering counter so that mutations to a non-active tab's state still
    // cause a re-render when that tab becomes active.
    const [, setVersion] = useState(0);
    const bump = useCallback(() => setVersion((v) => v + 1), []);
    const notifyLiveListeners = useCallback((tabId: string) => {
        for (const listener of liveListenersByTabRef.current.get(tabId) ?? [])
            listener();
    }, [t]);
    const disposeComposerProfileState = useCallback((tabId: string) => {
        appliedComposerProfileByTabRef.current.delete(tabId);
        composerProfileInFlightByTabRef.current.delete(tabId);
        composerProfileQueueByTabRef.current.delete(tabId);
        composerProfileLifecycleByTabRef.current.set(tabId, (composerProfileLifecycleByTabRef.current.get(tabId) ?? 0) + 1);
    }, []);
    const liveStore = useMemo<ControllerLiveStore>(() => ({
        subscribe(tabId, listener) {
            if (!tabId)
                return () => { };
            let listeners = liveListenersByTabRef.current.get(tabId);
            if (!listeners) {
                listeners = new Set();
                liveListenersByTabRef.current.set(tabId, listeners);
            }
            listeners.add(listener);
            return () => {
                listeners?.delete(listener);
                if (listeners?.size === 0)
                    liveListenersByTabRef.current.delete(tabId);
            };
        },
        getSnapshot(tabId) {
            return tabId ? statesRef.current.get(tabId)?.live : undefined;
        },
        getModelActiveAt(tabId) {
            return tabId ? statesRef.current.get(tabId)?.turnModelActiveAt : undefined;
        },
    }), []);
    const beginActiveNavigation = useCallback(() => {
        activeNavigationSeqRef.current += 1;
        return activeNavigationSeqRef.current;
    }, []);
    const isNavigationIntentCurrent = useCallback((seq: number): boolean => {
        return activeNavigationSeqRef.current === seq;
    }, []);
    const navigationCompletionCurrent = useCallback((seq: number, kind: string, tabId: string): boolean => {
        if (activeNavigationSeqRef.current === seq)
            return true;
        addBreadcrumb(kind, `stale ${tabId} seq=${seq} current=${activeNavigationSeqRef.current}`);
        return false;
    }, []);
    // The active tab's current state, with a stable identity for cancel().
    const activeState = activeTabId ? getOrCreateState(statesRef.current, activeTabId) : initialState;
    const stateRef = useRef(activeState);
    const backendActiveTabIdRef = useRef<string | undefined>(undefined);
    const backendActivationPromises = useRef(new Map<string, Promise<boolean>>());
    // The latest ticketed topic activation (StartTopicActivation). Registered
    // before the backend call returns so synchronously-emitted lifecycle events
    // always match; a terminal event arriving before the ticket is stashed and
    // replayed once the ticket lands.
    const pendingTopicActivationRef = useRef<PendingTopicActivation | undefined>(undefined);
    const topicActivationSeqRef = useRef(0);
    const readyMetaReconcileSeq = useRef(0);
    const readyMetaReconcileActive = useRef<{
        tabId: string;
        seq: number;
    } | undefined>(undefined);
    activeTabIdRef.current = activeTabId;
    stateRef.current = activeState;
    // Dispatch to a specific tab's state. If the tab doesn't have state yet, it's
    // created. Bumps the version so React re-renders when it becomes active.
    const dispatchTo = useCallback((tabId: string, action: Action) => {
        const states = statesRef.current;
        const prev = getOrCreateState(states, tabId);
        const next = reducer(prev, action);
        if (prev !== next) {
            states.set(tabId, next);
            // A tab with a live or in-flight turn is pinned out of transcript-store
            // eviction; its cached rows must survive until the turn settles.
            getTranscriptStore().setPinned(tabId, Boolean(next.running || next.turnActive || next.live));
            uiPerfTracker.onStateCommit();
            notifyLiveListeners(tabId);
            const streamDeltaOnly = (action.type === "stream_batch" ||
                (action.type === "event" && (action.e.kind === "text" || action.e.kind === "reasoning"))) &&
                prev.items === next.items &&
                prev.currentAssistant === next.currentAssistant &&
                prev.pendingUser === next.pendingUser &&
                prev.retry === next.retry;
            // Text/reasoning-only deltas only update the live stream — which the
            // frontend reads through its own subscription — so they must not bump the
            // full controller tree (the run-strip TPS estimate subscribes to the live
            // stream directly and updates itself).
            if (!streamDeltaOnly)
                bump();
        }
    }, [bump, notifyLiveListeners]);
    const clearBalanceForTab = useCallback((tabId: string): void => {
        invalidateSharedQuery("BalanceForTab", [tabId]);
        invalidateSharedQuery("MetaForTab", [tabId]);
        const seq = (balanceRefreshSeqByTab.current.get(tabId) ?? 0) + 1;
        balanceRefreshSeqByTab.current.set(tabId, seq);
        dispatchTo(tabId, { type: "balance", balance: { available: false, display: "" } });
    }, [dispatchTo]);
    const invalidateProviderStateForTab = useCallback((tabId: string): void => {
        balanceRefreshSeqByTab.current.set(tabId, (balanceRefreshSeqByTab.current.get(tabId) ?? 0) + 1);
        modelSwitchSeqByTab.current.set(tabId, (modelSwitchSeqByTab.current.get(tabId) ?? 0) + 1);
    }, []);
    const refreshBalanceForTab = useCallback(async (tabId: string, options: {
        apply?: () => boolean;
    } = {}): Promise<void> => {
        const seq = (balanceRefreshSeqByTab.current.get(tabId) ?? 0) + 1;
        balanceRefreshSeqByTab.current.set(tabId, seq);
        try {
            const balance = await app.BalanceForTab(tabId);
            if (balanceRefreshSeqByTab.current.get(tabId) !== seq)
                return;
            if (options.apply && !options.apply())
                return;
            if (balance.err?.trim())
                return;
            dispatchTo(tabId, { type: "balance", balance });
        }
        catch {
            // Balance is optional. Keep the last explicit cleared/unavailable state
            // instead of surfacing a provider-specific wallet failure in chat.
        }
    }, [dispatchTo]);
    const confirmBackendActiveTab = useCallback((tabId: string) => {
        backendActiveTabIdRef.current = tabId;
        dispatchTo(tabId, { type: "backend_activation_done" });
    }, [dispatchTo]);
    const reassertVisibleTabAfterStaleNavigation = useCallback(async (kind: string, staleTabId: string): Promise<void> => {
        // Backend navigation calls activate their result before returning. If a
        // newer tab click won in the frontend while that call was in flight, put
        // the backend back on the visible tab. Re-check after every await because
        // another click can supersede the target while SetActiveTab is running.
        for (;;) {
            const currentTabId = activeTabIdRef.current;
            if (!currentTabId)
                return;
            if (currentTabId === staleTabId) {
                confirmBackendActiveTab(currentTabId);
                return;
            }
            try {
                await app.SetActiveTab(currentTabId);
            }
            catch (err) {
                addBreadcrumb(kind, `stale reassert failed ${currentTabId}: ${errorMessage(err)}`);
                return;
            }
            if (activeTabIdRef.current === currentTabId) {
                confirmBackendActiveTab(currentTabId);
                addBreadcrumb(kind, `stale reasserted ${currentTabId}`);
                return;
            }
        }
    }, [confirmBackendActiveTab]);
    const trackBackendActivation = useCallback((tabId: string, promise: Promise<boolean>) => {
        backendActivationPromises.current.set(tabId, promise);
        void promise.finally(() => {
            if (backendActivationPromises.current.get(tabId) === promise) {
                backendActivationPromises.current.delete(tabId);
            }
        });
    }, []);
    const waitForBackendActiveTab = useCallback(async (tabId: string): Promise<boolean> => {
        const pending = backendActivationPromises.current.get(tabId);
        if (pending) {
            const activated = await pending.catch(() => false);
            if (!activated)
                return false;
        }
        return backendActiveTabIdRef.current === tabId && activeTabIdRef.current === tabId;
    }, []);
    const checkpointRefreshSeq = useRef(new Map<string, number>());
    const metaRefreshSeq = useRef(new Map<string, number>());
    const sessionLoadSeq = useRef(new Map<string, number>());
    const cancelHydrateSeq = useRef(new Map<string, number>());
    const sessionLoadInFlight = useRef(new Map<string, {
        sessionPath: string;
        revision?: number;
        digest?: string;
        promise: Promise<void>;
    }>());
    const transcriptSubscriptions = useRef(new Map<string, () => void>());
    const bumpMetaRefreshSeq = useCallback((tabId: string): number => {
        const seq = (metaRefreshSeq.current.get(tabId) ?? 0) + 1;
        metaRefreshSeq.current.set(tabId, seq);
        return seq;
    }, []);
    const metaRefreshCurrent = useCallback((tabId: string, seq: number): boolean => {
        return metaRefreshSeq.current.get(tabId) === seq;
    }, []);
    const bumpSessionLoadSeq = useCallback((tabId: string): number => {
        bumpMetaRefreshSeq(tabId);
        invalidateSharedQuery("MetaForTab", [tabId]);
        const seq = (sessionLoadSeq.current.get(tabId) ?? 0) + 1;
        sessionLoadSeq.current.set(tabId, seq);
        return seq;
    }, [bumpMetaRefreshSeq]);
    // Ref-resolved content updates flow from the transcript store into the tab's
    // state as id-keyed patches. Subscribed once per tab; released when the tab
    // state is dropped (close / single-surface prune).
    const ensureTranscriptSubscription = useCallback((tabId: string) => {
        if (transcriptSubscriptions.current.has(tabId))
            return;
        const unsubscribe = getTranscriptStore().subscribe(tabId, (change) => {
            if (!statesRef.current.has(tabId))
                return;
            dispatchTo(tabId, { type: "history_items_patch", patches: change.patches });
        });
        transcriptSubscriptions.current.set(tabId, unsubscribe);
    }, [dispatchTo]);
    const releaseTranscriptState = useCallback((tabId: string) => {
        transcriptSubscriptions.current.get(tabId)?.();
        transcriptSubscriptions.current.delete(tabId);
        getTranscriptStore().evictTab(tabId);
    }, []);
    const sessionLoadCurrent = useCallback((tabId: string, seq: number): boolean => {
        return sessionLoadSeq.current.get(tabId) === seq;
    }, []);
    const bumpCancelHydrateSeq = useCallback((tabId: string): number => {
        const seq = (cancelHydrateSeq.current.get(tabId) ?? 0) + 1;
        cancelHydrateSeq.current.set(tabId, seq);
        return seq;
    }, []);
    const cancelHydrateCurrent = useCallback((tabId: string, seq: number): boolean => {
        return cancelHydrateSeq.current.get(tabId) === seq;
    }, []);
    const loadMetaForTab = useCallback(async (tabId: string): Promise<Meta | undefined> => {
        const seq = bumpMetaRefreshSeq(tabId);
        const meta = await app.MetaForTab(tabId).catch(() => undefined);
        if (!metaRefreshCurrent(tabId, seq))
            return undefined;
        if (meta?.runtime?.epoch)
            runtimeEpochByTabRef.current.set(tabId, meta.runtime.epoch);
        return meta;
    }, [bumpMetaRefreshSeq, metaRefreshCurrent]);
    const refreshMetaOnlyForTab = useCallback(async (tabId: string): Promise<Meta | undefined> => {
        const meta = await loadMetaForTab(tabId);
        if (meta !== undefined)
            dispatchTo(tabId, { type: "meta", meta });
        return meta;
    }, [dispatchTo, loadMetaForTab]);
    const refreshMetaForTab = useCallback(async (tabId: string): Promise<void> => {
        const sessionSeq = sessionLoadSeq.current.get(tabId) ?? 0;
        const meta = await loadMetaForTab(tabId);
        if (meta === undefined || (sessionLoadSeq.current.get(tabId) ?? 0) !== sessionSeq)
            return;
        dispatchTo(tabId, { type: "meta", meta });
        const [context, effort] = await Promise.all([
            app.ContextUsageForTab(tabId).catch(() => undefined),
            app.EffortForTab(tabId).catch(() => undefined),
        ]);
        if ((sessionLoadSeq.current.get(tabId) ?? 0) !== sessionSeq)
            return;
        if (context !== undefined)
            dispatchTo(tabId, { type: "context", context });
        if (effort !== undefined)
            dispatchTo(tabId, { type: "effort", effort });
    }, [dispatchTo, loadMetaForTab]);
    const bumpCheckpointRefreshSeq = useCallback((tabId: string): number => {
        const seq = (checkpointRefreshSeq.current.get(tabId) ?? 0) + 1;
        checkpointRefreshSeq.current.set(tabId, seq);
        return seq;
    }, []);
    const refreshCheckpoints = useCallback(async (tabId: string) => {
        const seq = bumpCheckpointRefreshSeq(tabId);
        const checkpoints = await app.CheckpointsForTab(tabId).catch(() => undefined);
        if (checkpointRefreshSeq.current.get(tabId) !== seq || checkpoints === undefined)
            return;
        dispatchTo(tabId, { type: "checkpoints", checkpoints: asArray(checkpoints) });
    }, [bumpCheckpointRefreshSeq, dispatchTo]);
    const loadSessionDataForTab = useCallback(async (tabId: string, reset = false, reason: HydrateReason = "startup", options: {
        skipHistory?: boolean;
        placeholderItems?: Item[];
        preserveCachedHistory?: boolean;
        sessionPath?: string;
        sessionRevision?: number;
        sessionDigest?: string;
        sessionGeneration?: number;
        cancelHydrateGeneration?: number;
        deferResetUntilHistory?: boolean;
    } = {}) => {
        const stateMeta = statesRef.current.get(tabId)?.meta;
        const sessionPath = (options.sessionPath ?? stateMeta?.sessionPath ?? "").trim();
        const sessionRevision = options.sessionRevision ?? stateMeta?.sessionRevision;
        const sessionDigest = options.sessionDigest ?? stateMeta?.sessionDigest;
        const sessionGeneration = options.sessionGeneration ?? stateMeta?.sessionGeneration;
        const canJoinInFlight = !reset && !options.skipHistory;
        const shouldTrackInFlight = !options.skipHistory;
        if (canJoinInFlight) {
            const existing = sessionLoadInFlight.current.get(tabId);
            if (existing?.sessionPath === sessionPath && existing.revision === sessionRevision && existing.digest === sessionDigest)
                return existing.promise;
        }
        else {
            sessionLoadInFlight.current.delete(tabId);
        }
        const promise = (async () => {
            const cancelHydrateGeneration = options.cancelHydrateGeneration;
            if (cancelHydrateGeneration !== undefined && !cancelHydrateCurrent(tabId, cancelHydrateGeneration))
                return;
            const seq = bumpSessionLoadSeq(tabId);
            const hydrateStartedAt = Date.now();
            const skipHistory = Boolean(options.skipHistory ||
                (options.preserveCachedHistory && !reset && hasReusableCachedTranscript(statesRef.current.get(tabId), sessionPath, sessionRevision, sessionDigest)));
            const deferResetUntilHistory = Boolean((options.deferResetUntilHistory ?? true) && reset && !skipHistory);
            // Request seq alone cannot stop clear→mode-switch races: a load started
            // after clear with stale meta.sessionPath must also be rejected.
            const stillCurrent = () => {
                if (!sessionLoadCurrent(tabId, seq))
                    return false;
                if (cancelHydrateGeneration !== undefined && !cancelHydrateCurrent(tabId, cancelHydrateGeneration))
                    return false;
                const meta = statesRef.current.get(tabId)?.meta;
                return hydrateIdentityCurrent(sessionPath, sessionGeneration, meta?.sessionPath, meta?.sessionGeneration);
            };
            if (!stillCurrent())
                return;
            addBreadcrumb("tab.hydrate", `start ${reason} ${tabId}`);
            ensureTranscriptSubscription(tabId);
            dispatchTo(tabId, { type: "hydrate_start", reason, placeholderItems: resolveHydratePlaceholders(options.placeholderItems, statesRef.current.get(tabId)?.items) });
            if (reset && !deferResetUntilHistory && stillCurrent())
                dispatchTo(tabId, { type: "reset" });
            const requiresVisibleTab = reason === "startup" || reason === "switch-tab" || reason === "open-topic";
            const stillVisible = () => !requiresVisibleTab || activeTabIdRef.current === tabId;
            const foregroundTurnActive = (): boolean => {
                const state = statesRef.current.get(tabId);
                return Boolean(state?.running || state?.turnActive || state?.pendingPrompt);
            };
            const noteFailure = (label: string, err: unknown) => {
                addBreadcrumb("tab.hydrate", `${label} failed ${tabId}: ${errorMessage(err)}`);
            };
            const loadTimed = async <T,>(label: string, load: () => Promise<T>): Promise<T | undefined> => {
                const startedAt = Date.now();
                addBreadcrumb("tab.hydrate", `${label} start ${reason} ${tabId}`);
                try {
                    const value = await load();
                    addBreadcrumb("tab.hydrate", `${label} done ${reason} ${tabId} ms=${Date.now() - startedAt}`);
                    return value;
                }
                catch (err) {
                    noteFailure(label, err);
                    return undefined;
                }
            };
            const historyStartedAt = Date.now();
            let projection = skipHistory
                ? undefined
                : await loadTimed("history", () => 
                // Windowed slice load through the transcript store. A resident
                // session (LRU hit) projects synchronously with no backend round
                // trip; reset loads always re-fetch so a rebound session can never
                // serve the previous session's rows.
                getTranscriptStore().loadLatest(tabId, sessionPath, {
                    turns: HISTORY_PAGE_TURNS,
                    preferResident: !reset,
                    expectedRevision: sessionRevision,
                    expectedDigest: sessionDigest,
                }));
            if (!stillCurrent())
                return;
            if (!skipHistory && projection === undefined) {
                const errText = t("history.failedLoadHistory");
                dispatchTo(tabId, { type: "hydrate_error", reason, error: errText });
                dispatchTo(tabId, { type: "local_notice", level: "warn", text: errText });
                addBreadcrumb("tab.hydrate", `history failed ${tabId} ms=${Date.now() - historyStartedAt}`);
                return;
            }
            if (!skipHistory && projection !== undefined && !foregroundTurnActive()) {
                if (deferResetUntilHistory && stillCurrent())
                    dispatchTo(tabId, { type: "reset" });
                dispatchTo(tabId, {
                    type: "history_replace",
                    items: projection.items,
                    startTurn: projection.startTurn,
                    totalTurns: projection.totalTurns,
                    hasOlder: projection.hasOlder,
                    revision: projection.revisionKnown ? projection.revision : undefined,
                    digest: projection.digest || undefined,
                });
                addBreadcrumb("tab.hydrate", `history page ${tabId} items=${projection.items.length} turns=${projection.startTurn}-${projection.endTurn}/${projection.totalTurns} ms=${Date.now() - historyStartedAt}`);
                if (reason === "switch-tab") {
                    addBreadcrumb("tab.switch", `history-done ${tabId} items=${projection.items.length} turns=${projection.startTurn}-${projection.endTurn}/${projection.totalTurns} ms=${Date.now() - historyStartedAt}`);
                }
            }
            else if (skipHistory) {
                const skipReason = options.skipHistory ? "cached-live-turn" : "cached-transcript";
                addBreadcrumb("tab.hydrate", `history skipped ${tabId} reason=${skipReason}`);
                if (reason === "switch-tab") {
                    addBreadcrumb("tab.switch", `history-done ${tabId} skipped ms=${Date.now() - historyStartedAt}`);
                }
            }
            if (!stillCurrent())
                return;
            dispatchTo(tabId, { type: "hydrate_done" });
            addBreadcrumb("tab.hydrate", `done ${reason} ${tabId} ms=${Date.now() - hydrateStartedAt}`);
            // Phase 2: local ancillary data. It stays inside the same in-flight
            // promise so duplicate ready/startup hydrations coalesce, but it runs
            // after hydrate_done so slow Wails calls don't keep the visible transcript
            // in a loading state.
            await new Promise<void>((resolve) => window.setTimeout(resolve, 0));
            if (!stillCurrent())
                return;
            if (!stillVisible()) {
                addBreadcrumb("tab.hydrate", `ancillary skipped inactive ${reason} ${tabId}`);
                return;
            }
            let meta = await loadTimed("meta", () => loadMetaForTab(tabId));
            if (!stillCurrent())
                return;
            if (!stillVisible()) {
                addBreadcrumb("tab.hydrate", `meta ignored inactive ${reason} ${tabId}`);
                return;
            }
            if (meta !== undefined)
                dispatchTo(tabId, { type: "meta", meta });
            if (meta !== undefined && projection !== undefined && !skipHistory &&
                !foregroundTurnActive() && !historyFingerprintMatchesMeta(projection, meta)) {
                // The transcript and metadata are persisted in separate files. A save
                // can advance between those reads, so an exact-content page may be
                // older than the metadata sampled just afterwards. Re-read both as a
                // bounded pair; only replace the visible history when their canonical
                // fingerprints agree. A continuously changing session keeps the first
                // exact page and remains non-reusable because its digest differs.
                for (let attempt = 0; attempt < 2; attempt += 1) {
                    const reconciledProjection = await loadTimed("history reconcile", () => getTranscriptStore().loadLatest(tabId, sessionPath, {
                        turns: HISTORY_PAGE_TURNS,
                        preferResident: false,
                        expectedRevision: meta?.sessionRevision,
                        expectedDigest: meta?.sessionDigest,
                    }));
                    if (!stillCurrent() || !stillVisible() || reconciledProjection === undefined)
                        return;
                    const reconciledMeta = await loadTimed("meta reconcile", () => loadMetaForTab(tabId));
                    if (!stillCurrent() || !stillVisible() || reconciledMeta === undefined)
                        return;
                    meta = reconciledMeta;
                    dispatchTo(tabId, { type: "meta", meta });
                    if (!foregroundTurnActive() && historyFingerprintMatchesMeta(reconciledProjection, meta)) {
                        projection = reconciledProjection;
                        dispatchTo(tabId, {
                            type: "history_replace",
                            items: projection.items,
                            startTurn: projection.startTurn,
                            totalTurns: projection.totalTurns,
                            hasOlder: projection.hasOlder,
                            revision: projection.revisionKnown ? projection.revision : undefined,
                            digest: projection.digest || undefined,
                        });
                        break;
                    }
                    if (foregroundTurnActive())
                        break;
                }
            }
            const ancillaryStartedAt = Date.now();
            const loadAncillary = async <T,>(label: string, load: () => Promise<T>): Promise<T | undefined> => {
                return loadTimed(`ancillary ${label}`, load);
            };
            const [effort, jobs, context] = await Promise.all([
                loadAncillary("effort", () => app.EffortForTab(tabId)),
                loadAncillary("jobs", () => app.JobsForTab(tabId)),
                loadAncillary("context", () => app.ContextUsageForTab(tabId)),
            ]);
            if (!stillCurrent())
                return;
            if (effort !== undefined)
                dispatchTo(tabId, { type: "effort", effort });
            if (jobs !== undefined)
                dispatchTo(tabId, { type: "jobs", jobs: asArray(jobs) });
            if (context !== undefined)
                dispatchTo(tabId, { type: "context", context });
            // Signal ContextPanel to re-fetch now that ancillary data (context,
            // effort, jobs) has landed. Without this, the right-side panel keeps
            // stale RequestCount / ElapsedMs / SessionCost from before a session
            // rebind because its refreshKey (dockRefreshKey) only bumps on turn_done.
            dispatchTo(tabId, { type: "context_panel_refresh" });
            await new Promise<void>((resolve) => window.setTimeout(resolve, 0));
            if (!stillCurrent())
                return;
            if (!stillVisible()) {
                addBreadcrumb("tab.hydrate", `checkpoints skipped inactive ${reason} ${tabId}`);
                return;
            }
            const checkpoints = await loadAncillary("checkpoints", () => app.CheckpointsForTab(tabId));
            if (!stillCurrent())
                return;
            if (!stillVisible()) {
                addBreadcrumb("tab.hydrate", `checkpoints ignored inactive ${reason} ${tabId}`);
                return;
            }
            if (checkpoints !== undefined)
                dispatchTo(tabId, { type: "checkpoints", checkpoints: asArray(checkpoints) });
            addBreadcrumb("tab.hydrate", `ancillary ${reason} ${tabId} ms=${Date.now() - ancillaryStartedAt}`);
            void refreshBalanceForTab(tabId, {
                apply: () => sessionLoadCurrent(tabId, seq) && stillVisible(),
            });
        })();
        if (shouldTrackInFlight) {
            sessionLoadInFlight.current.set(tabId, { sessionPath, revision: sessionRevision, digest: sessionDigest, promise });
        }
        try {
            await promise;
        }
        finally {
            if (sessionLoadInFlight.current.get(tabId)?.promise === promise) {
                sessionLoadInFlight.current.delete(tabId);
            }
        }
    }, [bumpSessionLoadSeq, cancelHydrateCurrent, dispatchTo, loadMetaForTab, refreshBalanceForTab, sessionLoadCurrent]);
    // On-demand full content for a ref-replaced history field (entries carrying
    // refs[] ship a ≤4KiB preview inline). Resolves through the transcript
    // store, which patches the projected items by stable id on completion. The
    // rendering layer calls this when a truncated entry scrolls into view.
    const requestHistoryFullContent = useCallback(async (entryId: string, field: string): Promise<string | undefined> => {
        const tabId = activeTabIdRef.current;
        if (!tabId)
            return undefined;
        ensureTranscriptSubscription(tabId);
        return getTranscriptStore().requestFullContent(tabId, entryId, field);
    }, [ensureTranscriptSubscription]);
    const loadOlderHistory = useCallback(async (tabId?: string): Promise<void> => {
        const targetTabId = tabId || activeTabIdRef.current;
        if (!targetTabId)
            return;
        const state = statesRef.current.get(targetTabId);
        if (!state?.historyHasOlder || state.historyOlderLoading || state.running)
            return;
        const sessionPath = state.meta?.sessionPath ?? "";
        const sessionRevision = state.meta?.sessionRevision ?? state.historyRevision;
        const sessionDigest = state.meta?.sessionDigest ?? state.historyDigest;
        ensureTranscriptSubscription(targetTabId);
        dispatchTo(targetTabId, { type: "history_older_start" });
        const startedAt = Date.now();
        try {
            const result = await getTranscriptStore().loadOlder(targetTabId, sessionPath, { turns: HISTORY_PAGE_TURNS });
            const current = statesRef.current.get(targetTabId);
            const currentRevision = current?.meta?.sessionRevision ?? current?.historyRevision;
            const currentDigest = current?.meta?.sessionDigest ?? current?.historyDigest;
            const fingerprintMatches = (expected: number | undefined, actual: number | undefined) => expected === undefined || expected <= 0 ? true : actual === expected;
            const digestMatches = (expected: string | undefined, actual: string | undefined) => !expected || actual === expected;
            // A replace-level hydrate while the page was in flight clears
            // historyOlderLoading; a metadata or canonical-identity change also
            // makes the page belong to a different transcript generation.
            if (!current || !current.historyOlderLoading || (current.meta?.sessionPath ?? "") !== sessionPath ||
                !fingerprintMatches(sessionRevision, currentRevision) || !digestMatches(sessionDigest, currentDigest) ||
                (result !== undefined && (!fingerprintMatches(sessionRevision, result.revisionKnown ? result.revision : undefined) ||
                    !digestMatches(sessionDigest, result.digest)))) {
                dispatchTo(targetTabId, { type: "history_older_error" });
                return;
            }
            if (!result) {
                // Superseded (generation moved) or nothing older left.
                dispatchTo(targetTabId, { type: "history_older_error" });
                return;
            }
            if (result.kind === "reload") {
                // The cursor went stale (session rewritten): the store reloaded the
                // latest page; replace instead of prepend.
                dispatchTo(targetTabId, {
                    type: "history_replace",
                    items: result.items,
                    startTurn: result.startTurn,
                    totalTurns: result.totalTurns,
                    hasOlder: result.hasOlder,
                    revision: result.revisionKnown ? result.revision : undefined,
                    digest: result.digest || undefined,
                });
            }
            else {
                dispatchTo(targetTabId, {
                    type: "history_prepend",
                    items: result.prependItems,
                    removeIds: result.removeIds,
                    startTurn: result.startTurn,
                    totalTurns: result.totalTurns,
                    hasOlder: result.hasOlder,
                    revision: result.revisionKnown ? result.revision : undefined,
                    digest: result.digest || undefined,
                });
            }
            addBreadcrumb("tab.hydrate", `history older ${targetTabId} kind=${result.kind} items=${result.kind === "prepend" ? result.prependItems.length : result.items.length} turns=${result.startTurn}-${result.endTurn}/${result.totalTurns} ms=${Date.now() - startedAt}`);
        }
        catch (err) {
            dispatchTo(targetTabId, { type: "history_older_error" });
            addBreadcrumb("tab.hydrate", `history older failed ${targetTabId}: ${errorMessage(err)}`);
        }
    }, [dispatchTo, ensureTranscriptSubscription]);
    const activeTabFromBackend = useCallback(async (): Promise<TabMeta | undefined> => {
        const tabs = asArray(await app.ListTabs().catch(() => [] as TabMeta[]));
        return tabs.find((tab) => tab.active) ?? tabs[0];
    }, []);
    // snapshotAt is the promptEventClock() reading taken immediately before
    // initiating the backend call that produced `tab`. The reducer uses it to
    // ignore snapshots that predate a live approval/ask event (#6429).
    const dispatchRuntimeStatusForTab = useCallback((tabId: string, tab: RuntimeMetaSnapshot, snapshotAt?: number) => {
        const foregroundRunning = foregroundRunningFromRuntimeMeta(tab);
        // Will the reducer reject this as a snapshot that predates the live prompt?
        // Computed on pre-dispatch state so we can schedule an authoritative
        // refetch when a stale idle snapshot is ignored.
        const rejectedStaleIdle = !tab.pendingPrompt && runtimeSnapshotPredatesPrompt(statesRef.current.get(tabId), snapshotAt);
        dispatchTo(tabId, {
            type: "backend_status",
            running: foregroundRunning,
            pendingPrompt: Boolean(tab.pendingPrompt),
            backgroundJobs: tab.backgroundJobs ?? 0,
            cancelRequested: Boolean(tab.cancelRequested),
            cancellable: foregroundRunning,
            snapshotAt,
        });
        // backend_status reconciliation can clear a live prompt from frontend state.
        // If the backend is still blocked, ask it to replay the approval/ask event.
        if (tab.pendingPrompt)
            replayPendingPromptsForActiveTab(tabId);
        // A stale idle snapshot the reducer ignored cannot be trusted to have kept a
        // GENUINE prompt: navigation can drop the prompt anchor, so a delayed replay
        // of an already-answered prompt looks like a fresh prompt and re-anchors,
        // making this authoritative idle look stale. Refetch backend truth once so a
        // resolved prompt is cleared instead of surviving as a zombie (#6432).
        if (rejectedStaleIdle)
            scheduleStalePromptReconcileRef.current(tabId);
        // A prompt that survived reconciliation (fresh pendingPrompt=true meta, or
        // a stale snapshot the reducer ignored) keeps the tab blocked on the user.
        // Report it as foreground-running so callers do not treat the snapshot as
        // a missed turn_done and reset the session out from under the prompt.
        const local = statesRef.current.get(tabId);
        if (local?.approval || local?.ask)
            return true;
        return foregroundRunning;
    }, [dispatchTo]);
    const waitForTabReady = useCallback(async (tabId: string): Promise<void> => {
        for (let attempt = 0; attempt < 60; attempt += 1) {
            const tabs = asArray(await app.ListTabs().catch(() => [] as TabMeta[]));
            const tab = tabs.find((candidate) => candidate.id === tabId);
            if (!tab || tab.ready || tab.startupErr)
                return;
            await new Promise((resolve) => window.setTimeout(resolve, 100));
        }
    }, []);
    const syncActiveTabFromBackend = useCallback(async (reset = false, guard = false, options: SyncActiveTabOptions = {}): Promise<string | undefined> => {
        const snapshotAt = promptEventClock();
        const active = await activeTabFromBackend();
        if (!active)
            return undefined;
        // When guard is true, skip if the frontend already settled on a
        // different tab while we were fetching — this prevents fire-and-forget
        // calls from mount/onReady from overwriting a user-initiated tab switch
        // (e.g. handleNewTab → ensureBlankSurface / switchTab).
        if (guard && activeTabIdRef.current && activeTabIdRef.current !== active.id) {
            return active.id;
        }
        if (activeTabIdRef.current !== active.id)
            beginActiveNavigation();
        setActiveTabId(active.id);
        activeTabIdRef.current = active.id;
        confirmBackendActiveTab(active.id);
        if (active.runtime?.epoch)
            runtimeEpochByTabRef.current.set(active.id, active.runtime.epoch);
        dispatchTo(active.id, { type: "optimistic_meta", meta: metaFromTab(active, statesRef.current.get(active.id)?.meta) });
        const preserveCachedHistory = options.preserveCachedHistory ?? !reset;
        if (!reset)
            dispatchRuntimeStatusForTab(active.id, active, snapshotAt);
        await loadSessionDataForTab(active.id, reset, "startup", {
            preserveCachedHistory,
            sessionPath: active.sessionPath,
            sessionRevision: active.sessionRevision,
            sessionDigest: active.sessionDigest,
        });
        if (reset)
            dispatchRuntimeStatusForTab(active.id, active, snapshotAt);
        return active.id;
    }, [activeTabFromBackend, beginActiveNavigation, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, loadSessionDataForTab]);
    const reconcileTabRuntime = useCallback(async (tabId: string, options: {
        hydrateSessionData?: boolean;
    } = {}): Promise<TabMeta[] | undefined> => {
        const hydrateSessionData = options.hydrateSessionData ?? true;
        const snapshotAt = promptEventClock();
        const tabs = asArray(await app.ListTabs().catch(() => [] as TabMeta[]));
        const tab = tabs.find((candidate) => candidate.id === tabId);
        if (!tab)
            return undefined;
        if (tab.runtime?.epoch)
            runtimeEpochByTabRef.current.set(tabId, tab.runtime.epoch);
        const local = statesRef.current.get(tabId);
        const needsInitialLoad = !local?.meta;
        const foregroundRunning = dispatchRuntimeStatusForTab(tabId, tab, snapshotAt);
        const missedTurnDone = Boolean(local?.running && !foregroundRunning);
        if (hydrateSessionData && (needsInitialLoad || missedTurnDone)) {
            await loadSessionDataForTab(tabId, missedTurnDone, "startup", {
                sessionPath: tab.sessionPath,
                sessionRevision: tab.sessionRevision,
                sessionDigest: tab.sessionDigest,
            });
            return tabs;
        }
        const [jobs, effort] = await Promise.all([
            app.JobsForTab(tabId).catch(() => undefined),
            app.EffortForTab(tabId).catch(() => undefined),
        ]);
        if (jobs)
            dispatchTo(tabId, { type: "jobs", jobs: asArray(jobs) });
        if (effort)
            dispatchTo(tabId, { type: "effort", effort });
        await refreshBalanceForTab(tabId);
        return tabs;
    }, [dispatchRuntimeStatusForTab, loadSessionDataForTab, refreshBalanceForTab]);
    // Authoritative backstop for the prompt-freshness heuristic: after the reducer
    // rejects a stale idle snapshot, refetch backend state once. If the backend
    // resolved the prompt, the fresh snapshot (fetched after any in-flight replay)
    // is newer than the anchor and reconciles the zombie away; if the prompt is
    // genuinely pending, the fresh snapshot keeps it. Debounced per tab so a burst
    // of stale snapshots schedules at most one refetch (#6432).
    const scheduleStalePromptReconcile = useCallback((tabId: string) => {
        if (stalePromptReconcileTimers.current.has(tabId))
            return;
        const timer = window.setTimeout(() => {
            stalePromptReconcileTimers.current.delete(tabId);
            void reconcileTabRuntime(tabId, { hydrateSessionData: false }).catch(() => { });
        }, STALE_PROMPT_RECONCILE_MS);
        stalePromptReconcileTimers.current.set(tabId, timer);
    }, [reconcileTabRuntime]);
    scheduleStalePromptReconcileRef.current = scheduleStalePromptReconcile;
    const clearCancelReconcileTimer = useCallback((tabId: string) => {
        const timer = cancelReconcileTimers.current.get(tabId);
        if (timer === undefined)
            return;
        window.clearTimeout(timer);
        cancelReconcileTimers.current.delete(tabId);
    }, []);
    const scheduleCancelReconcile = useCallback((tabId: string, attempt = 0, cancelHydrateGeneration?: number) => {
        clearCancelReconcileTimer(tabId);
        const delay = CANCEL_RECONCILE_DELAYS_MS[Math.min(attempt, CANCEL_RECONCILE_DELAYS_MS.length - 1)];
        const timer = window.setTimeout(() => {
            cancelReconcileTimers.current.delete(tabId);
            void reconcileTabRuntime(tabId, { hydrateSessionData: false }).then((tabs) => {
                const tab = tabs?.find((candidate) => candidate.id === tabId);
                if (!tab)
                    return;
                const stillReconciling = foregroundRunningFromRuntimeMeta(tab) || Boolean(tab.cancelRequested);
                if (stillReconciling && attempt + 1 < CANCEL_RECONCILE_DELAYS_MS.length) {
                    scheduleCancelReconcile(tabId, attempt + 1, cancelHydrateGeneration);
                    return;
                }
                // Cancel can race the optimistic user bubble before turn_started. The
                // backend still persists that visible prompt (and its checkpoint), so
                // hydrate the authoritative transcript once teardown is idle instead
                // of leaving the UI with a discarded bubble and no edit/rewind target.
                if (!stillReconciling) {
                    void loadSessionDataForTab(tabId, true, "rewind", {
                        cancelHydrateGeneration,
                        deferResetUntilHistory: true,
                    }).catch(() => { });
                    void refreshCheckpoints(tabId);
                }
            }).catch(() => { });
        }, delay);
        cancelReconcileTimers.current.set(tabId, timer);
    }, [clearCancelReconcileTimer, loadSessionDataForTab, reconcileTabRuntime, refreshCheckpoints]);
    // Topic-activation lifecycle events drive the ticketed activation flow: the
    // visible surface already switched when StartTopicActivation returned; the
    // history hydrate waits for the terminal "ready" of the LATEST request.
    // Events for superseded requestIds (including their "cancelled") are
    // dropped; agent:ready/agent:event handling is untouched and still covers
    // every non-ticketed flow (rebind, recovery, restore, SetActiveTab).
    const handleTopicActivationEvent = useCallback((event: TopicActivationEvent) => {
        const pending = pendingTopicActivationRef.current;
        if (!pending || event.requestId !== pending.requestId)
            return;
        if (event.phase === "starting") {
            noteActivationStarted(event.requestId, event.tabId);
            return;
        }
        if (!pending.tabId) {
            // The ticket has not resolved yet; replay once activateTopic applies it.
            pending.terminal = event;
            return;
        }
        if (event.phase === "cancelled") {
            noteActivationSettled(event.requestId, "cancelled");
            if (pendingTopicActivationRef.current === pending)
                pendingTopicActivationRef.current = undefined;
            return;
        }
        pendingTopicActivationRef.current = undefined;
        if (!isNavigationIntentCurrent(pending.navigationSeq))
            return;
        const tabId = pending.tabId;
        if (activeTabIdRef.current !== tabId)
            return;
        if (event.phase === "failed") {
            noteActivationSettled(event.requestId, "failed", event.error);
            dispatchTo(tabId, {
                type: "hydrate_error",
                reason: "open-topic",
                error: event.error?.trim() || "session is not ready",
            });
            return;
        }
        noteActivationSettled(event.requestId, "ready");
        ensureTranscriptSubscription(tabId);
        void loadSessionDataForTab(tabId, true, "open-topic", { placeholderItems: pending.placeholderItems })
            .then(() => reconcileTabRuntime(tabId, { hydrateSessionData: false }))
            .catch(() => { });
    }, [dispatchTo, ensureTranscriptSubscription, isNavigationIntentCurrent, loadSessionDataForTab, reconcileTabRuntime]);
    useEffect(() => {
        const textBatch = createRafBatch<StreamDeltaEntry>((batch) => {
            uiPerfTracker.onStreamDispatch();
            for (const b of coalesceStreamDeltas(batch))
                dispatchTo(b.tabId, { type: "stream_batch", segments: b.segments });
        });
        const off = onEvent((e) => {
            // Untagged compatibility events belong to the tab that the backend has
            // actually activated, not the frontend's optimistic selection. During a
            // slow SetActiveTab these can differ, and routing to the optimistic tab
            // leaks the previous session's approval/ask gate into the new composer.
            const targetTabId = e.tabId || backendActiveTabIdRef.current || activeTabIdRef.current;
            if (!targetTabId)
                return;
            const acceptedEpoch = runtimeEpochByTabRef.current.get(targetTabId);
            if (e.runtimeEpoch) {
                if (!acceptsRuntimeEventEpoch(acceptedEpoch, e.runtimeEpoch))
                    return;
                if (!acceptedEpoch)
                    runtimeEpochByTabRef.current.set(targetTabId, e.runtimeEpoch);
            }
            uiPerfTracker.onWireEvent(targetTabId, e.kind);
            if (TURN_ACTIVITY_KINDS.has(e.kind))
                lastTurnActivityAtByTab.current.set(targetTabId, Date.now());
            if (e.kind === "text" || e.kind === "reasoning") {
                if (e.submissionId)
                    dispatchTo(targetTabId, { type: "send_confirmed", submissionId: e.submissionId });
                textBatch.push({ tabId: targetTabId, e });
            }
            else {
                textBatch.drain();
                dispatchTo(targetTabId, { type: "event", e });
            }
            if (e.kind === "turn_done" || e.kind === "context_maintenance") {
                void app.ContextUsageForTab(targetTabId).then((context) => dispatchTo(targetTabId, { type: "context", context })).catch(() => { });
            }
            if (e.kind === "turn_done") {
                invalidateSharedQuery("BalanceForTab", [targetTabId]);
                void refreshBalanceForTab(targetTabId);
                app.EffortForTab(targetTabId).then((effort) => dispatchTo(targetTabId, { type: "effort", effort })).catch(() => { });
                void refreshCheckpoints(targetTabId);
                invalidateSharedQuery("MetaForTab", [targetTabId]);
                void refreshMetaForTab(targetTabId);
            }
            if (e.kind === "turn_done" || e.kind === "notice") {
                app.JobsForTab(targetTabId).then((jobs) => dispatchTo(targetTabId, { type: "jobs", jobs: asArray(jobs) })).catch(() => { });
            }
        });
        const offReady = onReady((readyTabId) => {
            const activeId = activeTabIdRef.current;
            if (readyTabId && activeId && readyTabId !== activeId) {
                addBreadcrumb("tab.hydrate", `ready ignored ${readyTabId}`);
                return;
            }
            // A ready event can race the initial hydrate. Refresh the tab metadata
            // first so a stale ready=false snapshot does not keep the composer locked.
            void syncActiveTabFromBackend(false, true, { preserveCachedHistory: true });
        });
        // A rebuilt controller reissues approval/ask ids from "1" (see sound.ts).
        // Drop this tab's id-anchored prompt bookkeeping so a genuinely new
        // prompt from the new controller is never misread as a stale replay of
        // one the old controller already resolved (#6432 round 3). A tab-less
        // rebuild (settings-wide) affects every known tab.
        const offRebuilt = onRuntimeRebuilt((rebuiltTabId, runtimeEpoch) => {
            if (rebuiltTabId) {
                invalidateSharedQuery("MetaForTab", [rebuiltTabId]);
                if (runtimeEpoch)
                    runtimeEpochByTabRef.current.set(rebuiltTabId, runtimeEpoch);
                dispatchTo(rebuiltTabId, { type: "controller_rebuilt" });
            }
            else {
                if (runtimeEpoch) {
                    for (const id of Array.from(statesRef.current.keys()))
                        runtimeEpochByTabRef.current.set(id, runtimeEpoch);
                }
                for (const id of Array.from(statesRef.current.keys())) {
                    invalidateSharedQuery("MetaForTab", [id]);
                    dispatchTo(id, { type: "controller_rebuilt" });
                }
            }
        });
        const offTopicActivation = onTopicActivation(handleTopicActivationEvent);
        // tab:meta carries a full refreshed Meta after the backend's background
        // refresh of the expensive fields (git branch, image-input capability) —
        // those arrive empty in the first MetaForTab response now. Merge it like a
        // MetaForTab result, fenced to the session the tab is currently bound to.
        const offTabMeta = onTabMeta(({ tabId, meta }) => {
            if (!tabId || !meta)
                return;
            const current = statesRef.current.get(tabId);
            if (!current?.meta)
                return;
            if (meta.sessionPath !== undefined &&
                current.meta.sessionPath !== undefined &&
                meta.sessionPath !== current.meta.sessionPath) {
                return;
            }
            dispatchTo(tabId, { type: "meta", meta });
        });
        void syncActiveTabFromBackend(false, true);
        // The event subscription is live now, so ask the backend to re-emit any
        // approval/ask prompt that was already blocking a tab before this load —
        // otherwise a session left mid-confirmation shows "waiting" with no modal
        // and no way to stop (#3844).
        void app.ReplayPendingPrompts().catch(() => { });
        return () => {
            textBatch.drain();
            for (const timer of cancelReconcileTimers.current.values()) {
                window.clearTimeout(timer);
            }
            cancelReconcileTimers.current.clear();
            for (const timer of stalePromptReconcileTimers.current.values()) {
                window.clearTimeout(timer);
            }
            stalePromptReconcileTimers.current.clear();
            off();
            offReady();
            offRebuilt();
            offTopicActivation();
            offTabMeta();
        };
    }, [dispatchTo, handleTopicActivationEvent, loadSessionDataForTab, refreshBalanceForTab, refreshCheckpoints, refreshMetaForTab, syncActiveTabFromBackend]);
    // Track the visible tab in the transcript store: the active tab is pinned
    // out of LRU eviction. (In-flight loads of background tabs still complete
    // into their own per-tab state; store generations move on session switch,
    // evict, and unload — not on visible-tab changes.)
    const previousStoreActiveTabRef = useRef<string | undefined>(undefined);
    useEffect(() => {
        getTranscriptStore().noteActiveTab(activeTabId, previousStoreActiveTabRef.current);
        previousStoreActiveTabRef.current = activeTabId;
    }, [activeTabId]);
    // Keep shared all-source telemetry live between turn boundaries. Delivery
    // mode can complete dozens of provider requests inside one UI turn, while
    // the status bar reads state.context and would otherwise stay pinned to the
    // previous turn_done snapshot. A usage event is emitted after the backend
    // has recorded that request, so refresh the authoritative tab aggregate here.
    // The usage sequence and active-tab checks make this latest-request-wins:
    // slower snapshots cannot overwrite a newer usage event or a tab switch.
    useEffect(() => {
        const tabId = activeTabId;
        const usageSeq = activeState.usageSeq;
        if (!tabId || usageSeq <= 0 || !activeState.turnActive)
            return;
        let cancelled = false;
        void app.ContextUsageForTab(tabId).then((context) => {
            if (cancelled || activeTabIdRef.current !== tabId)
                return;
            if (statesRef.current.get(tabId)?.usageSeq !== usageSeq)
                return;
            dispatchTo(tabId, { type: "context", context });
        }).catch(() => { });
        return () => {
            cancelled = true;
        };
    }, [activeTabId, activeState.turnActive, activeState.usageSeq, dispatchTo]);
    // If the startup ready event is missed, keep the composer lock in sync with
    // the active tab's backend metadata without kicking off tab activation work.
    useEffect(() => {
        const tabId = activeTabId;
        const meta = activeState.meta;
        if (!tabId || !meta || meta.ready || meta.startupErr || activeState.backendActivationPending) {
            readyMetaReconcileSeq.current += 1;
            readyMetaReconcileActive.current = undefined;
            return;
        }
        let cancelled = false;
        let timer: number | undefined;
        const seq = readyMetaReconcileSeq.current + 1;
        readyMetaReconcileSeq.current = seq;
        readyMetaReconcileActive.current = { tabId, seq };
        const stillCurrent = () => {
            const active = readyMetaReconcileActive.current;
            return !cancelled && active?.tabId === tabId && active.seq === seq && activeTabIdRef.current === tabId;
        };
        const schedule = (attempt: number) => {
            timer = window.setTimeout(() => {
                void tick(attempt);
            }, STARTUP_READY_META_RECONCILE_MS);
        };
        const tick = async (attempt: number) => {
            if (!stillCurrent())
                return;
            const current = statesRef.current.get(tabId);
            if (!current?.meta || current.meta.ready || current.meta.startupErr || current.backendActivationPending)
                return;
            const nextMeta = await refreshMetaOnlyForTab(tabId);
            if (!stillCurrent())
                return;
            if (nextMeta?.ready || nextMeta?.startupErr || attempt + 1 >= STARTUP_READY_META_RECONCILE_ATTEMPTS)
                return;
            schedule(attempt + 1);
        };
        schedule(0);
        return () => {
            cancelled = true;
            if (timer !== undefined)
                window.clearTimeout(timer);
        };
    }, [activeTabId, activeState.meta?.ready, activeState.meta?.startupErr, activeState.backendActivationPending, refreshMetaOnlyForTab]);
    // Stale-turn watchdog: if the frontend thinks the agent is running but the
    // turn stream has gone quiet, reconcile with the backend. This catches cases
    // where the Wails event channel silently drops turn_done after the final
    // message or synthetic todo update has already closed the live stream.
    useEffect(() => {
        if (!activeTabId)
            return;
        const s = statesRef.current.get(activeTabId);
        const now = Date.now();
        const lastTurnActivityAt = lastTurnActivityAtByTab.current.get(activeTabId) ?? 0;
        if (!s?.running || !s.turnActive || lastTurnActivityAt <= 0)
            return;
        const since = Math.max(0, now - lastTurnActivityAt);
        if (shouldReconcileStaleTurn(s, lastTurnActivityAt, now)) {
            void reconcileTabRuntime(activeTabId);
            return;
        }
        const timer = window.setTimeout(() => {
            const cur = statesRef.current.get(activeTabId);
            const lastActivity = lastTurnActivityAtByTab.current.get(activeTabId) ?? 0;
            if (shouldReconcileStaleTurn(cur, lastActivity)) {
                void reconcileTabRuntime(activeTabId);
            }
        }, STALE_TURN_RECONCILE_MS - since);
        return () => window.clearTimeout(timer);
    }, [activeTabId, reconcileTabRuntime, activeState.running, activeState.turnActive]);
    // Replay any pending approval/ask prompts when switching tabs, so a
    // plan-mode session left awaiting confirmation rebuilds its modal (#4275).
    useEffect(() => {
        replayPendingPromptsForActiveTab(activeTabId);
    }, [activeTabId]);
    const sendToTab = useCallback(async (tabId: string, displayText: string, submitText = displayText, originalText?: string, structured?: import("./invocationDisplay").StructuredInvocationSubmit, initialGoal?: {
        goal: string;
        collaborationMode: CollaborationMode;
        toolApprovalMode: ToolApprovalMode;
    }) => {
        if (!tabId)
            throw new Error(t("composer.workspaceStarting"));
        const currentState = getOrCreateState(statesRef.current, tabId);
        const runtime = currentState.meta?.runtime;
        if (currentState.meta && !runtimeReadyForSubmit(currentState.meta)) {
            throw new Error(runtime?.issue?.message || currentState.meta.startupErr || t("composer.workspaceStarting"));
        }
        const seq = currentState.seq;
        const submissionId = createTurnSubmissionId(tabId, currentState.sessionGen, seq, runtimeEpochByTabRef.current.get(tabId) ?? runtime?.epoch);
        const promptEpoch = currentState.promptEpoch;
        const { display, submit } = normalizeTurnSubmit(displayText, submitText);
        const original = originalText?.trim() ?? "";
        bumpCancelHydrateSeq(tabId);
        if (currentState.hydrateReason === "rewind")
            dispatchTo(tabId, { type: "hydrate_done" });
        dispatchTo(tabId, { type: "user", text: displayText, submitText: display !== submit ? submit : undefined, seq, submissionId });
        invalidateCache();
        try {
            const submitPromise = initialGoal
                ? app.SubmitInitialGoalToTabWithID(tabId, initialGoal.goal, structured?.display.trim() || display, structured?.input.trim() || submit, structured?.invocations ?? [], initialGoal.collaborationMode, initialGoal.toolApprovalMode, submissionId)
                : structured
                    ? app.SubmitInvocationsToTabWithID(tabId, structured.display.trim(), structured.input.trim(), structured.invocations, submissionId)
                    : original
                        ? app.SubmitEditedDisplayToTabWithID(tabId, display, submit, original, submissionId)
                        : display !== submit ? app.SubmitDisplayToTabWithID(tabId, display, submit, submissionId) : app.SubmitToTabWithID(tabId, submit, submissionId);
            if (initialGoal) {
                const drained = await submitPromise;
                dispatchTo(tabId, { type: "send_confirmed", submissionId });
                const ids = Array.isArray(drained) ? drained : [];
                if (ids.length)
                    dispatchTo(tabId, { type: "approval_drained", ids, epoch: promptEpoch });
                return;
            }
            void submitPromise.then(() => dispatchTo(tabId, { type: "send_confirmed", submissionId }), (error) => dispatchTo(tabId, { type: "send_failed", submissionId, error: `Send failed: ${error instanceof Error ? error.message : String(error)}` }));
        }
        catch (error) {
            dispatchTo(tabId, { type: "send_failed", submissionId, error: `Send failed: ${error instanceof Error ? error.message : String(error)}` });
            throw error;
        }
    }, [bumpCancelHydrateSeq, dispatchTo]);
    const submitDeliveryTurnToTab = useCallback(async (tabId: string, displayText: string, submitText: string, submit: (tabId: string, display: string, input: string, submissionId: string) => Promise<void>) => {
        if (!tabId)
            throw new Error(t("composer.workspaceStarting"));
        const currentState = getOrCreateState(statesRef.current, tabId);
        if (currentState.meta && !runtimeReadyForSubmit(currentState.meta)) {
            throw new Error(currentState.meta?.runtime?.issue?.message || currentState.meta.startupErr || t("composer.workspaceStarting"));
        }
        const submissionId = createTurnSubmissionId(tabId, currentState.sessionGen, currentState.seq, runtimeEpochByTabRef.current.get(tabId) ?? currentState.meta?.runtime?.epoch);
        const display = displayText.trim(), trimmedSubmit = submitText.trim();
        dispatchTo(tabId, { type: "user", text: displayText, submitText: display !== trimmedSubmit ? trimmedSubmit : undefined, seq: currentState.seq, submissionId, deliveryRecovery: true });
        invalidateCache();
        try {
            void submit(tabId, display, trimmedSubmit, submissionId).then(() => dispatchTo(tabId, { type: "send_confirmed", submissionId }), (error) => dispatchTo(tabId, { type: "send_failed", submissionId, error: `Send failed: ${error instanceof Error ? error.message : String(error)}` }));
        }
        catch (error) {
            dispatchTo(tabId, { type: "send_failed", submissionId, error: `Send failed: ${error instanceof Error ? error.message : String(error)}` });
            throw error;
        }
    }, [dispatchTo]);
    const recoverDeliveryToTab = useCallback((tabId: string, displayText: string, submitText = displayText) => submitDeliveryTurnToTab(tabId, displayText, submitText, (id, d, s, sid) => app.SubmitDeliveryRecoveryToTabWithID(id, d, s, sid)), [submitDeliveryTurnToTab]);
    const waiveDeliveryToTab = useCallback((tabId: string, displayText: string, submitText = displayText) => submitDeliveryTurnToTab(tabId, displayText, submitText, (id, d, s, sid) => app.SubmitDeliveryWaiverToTabWithID(id, d, s, sid)), [submitDeliveryTurnToTab]);
    const send = useCallback((displayText: string, submitText = displayText) => {
        const tabId = activeTabIdRef.current ?? activeTabId;
        if (tabId) {
            return sendToTab(tabId, displayText, submitText);
        }
        const snapshotAt = promptEventClock();
        return activeTabFromBackend().then((active) => {
            if (!active?.id)
                throw new Error(t("composer.workspaceStarting"));
            setActiveTabId(active.id);
            activeTabIdRef.current = active.id;
            confirmBackendActiveTab(active.id);
            dispatchRuntimeStatusForTab(active.id, active, snapshotAt);
            return sendToTab(active.id, displayText, submitText);
        });
    }, [activeTabFromBackend, activeTabId, confirmBackendActiveTab, dispatchRuntimeStatusForTab, sendToTab]);
    const runShellForTab = useCallback(async (tabId: string, command: string) => {
        if (!tabId)
            throw new Error(t("composer.workspaceStarting"));
        const currentState = getOrCreateState(statesRef.current, tabId);
        const submissionId = createTurnSubmissionId(tabId, currentState.sessionGen, currentState.seq, runtimeEpochByTabRef.current.get(tabId) ?? currentState.meta?.runtime?.epoch);
        dispatchTo(tabId, { type: "user", text: `!${command}`, seq: currentState.seq, submissionId });
        try {
            await app.RunShellForTab(tabId, command);
            dispatchTo(tabId, { type: "send_confirmed", submissionId });
        }
        catch (error) {
            dispatchTo(tabId, { type: "send_failed", submissionId, error: `Command failed: ${error instanceof Error ? error.message : String(error)}` });
            throw error;
        }
    }, [dispatchTo]);
    const runShell = useCallback(async (command: string) => {
        if (!activeTabId)
            throw new Error(t("composer.workspaceStarting"));
        await runShellForTab(activeTabId, command);
    }, [activeTabId, runShellForTab]);
    const steerForTab = useCallback(async (tabId: string, text: string) => {
        if (!tabId)
            throw new Error(t("composer.workspaceStarting"));
        // Durable steer first: body is on disk before admission. Rejected steers
        // become follow-ups automatically (disposition queued_followup).
        const receipt = await app.EnqueueInboxSteer(tabId, text, text, "");
        if (receipt?.error)
            throw new Error(receipt.error);
        if (receipt?.disposition && receipt.disposition !== "steer_accepted") {
            throw new Error(inboxSteerQueuedMessage(getLocale()));
        }
    }, []);
    const steer = useCallback(async (text: string) => {
        if (!activeTabId)
            throw new Error(t("composer.workspaceStarting"));
        await steerForTab(activeTabId, text);
    }, [activeTabId, steerForTab]);
    const notice = useCallback((text: string, level: "info" | "warn" = "info") => {
        if (!activeTabId)
            return;
        dispatchTo(activeTabId, { type: "local_notice", level, text });
    }, [activeTabId, dispatchTo]);
    // Extension form dismissed/submitted locally: hide the surface. The backend
    // round-trip (SubmitExtensionForm) lives in App.tsx, which owns the toast
    // context used for error reporting.
    const dismissExtensionForm = useCallback(() => {
        if (!activeTabId)
            return;
        dispatchTo(activeTabId, { type: "clearExtensionForm" });
    }, [activeTabId, dispatchTo]);
    // The App drained the queued extension notifications into the toast system.
    const drainExtensionNotifications = useCallback(() => {
        if (!activeTabId)
            return;
        dispatchTo(activeTabId, { type: "extension_notifications_drained" });
    }, [activeTabId, dispatchTo]);
    const cancelTab = useCallback((tabId: string, inboxItemIDs: string[] = []) => {
        const cancelHydrateGeneration = bumpCancelHydrateSeq(tabId);
        const cancelRequest = inboxItemIDs.length > 0
            ? app.CancelTabWithInboxItems(tabId, inboxItemIDs)
            : app.CancelTab(tabId);
        cancelRequest
            .then(() => scheduleCancelReconcile(tabId, 0, cancelHydrateGeneration))
            .catch((error) => {
            dispatchTo(tabId, { type: "local_notice", level: "warn", text: formatInboxCancelError(error, getLocale()) });
        });
    }, [bumpCancelHydrateSeq, dispatchTo, scheduleCancelReconcile]);
    const cancel = useCallback((inboxItemIDs: string[] = []): string | undefined => {
        const cur = stateRef.current;
        const tabId = activeTabId;
        if (cur.running && cur.pendingUser !== undefined) {
            const text = cur.pendingUser;
            if (tabId) {
                dispatchTo(tabId, { type: "unsend" });
                cancelTab(tabId, inboxItemIDs);
            }
            return text;
        }
        if (tabId) {
            dispatchTo(tabId, { type: "cancel_requested" });
            cancelTab(tabId, inboxItemIDs);
        }
        return undefined;
    }, [activeTabId, cancelTab, dispatchTo]);
    const approve = useCallback((id: string, allow: boolean, session: boolean, persist: boolean) => {
        if (!activeTabId)
            return;
        const tabId = activeTabId;
        // Pin the failure callback to the prompt-id epoch the RPC was issued in:
        // if a controller rebuild lands while the call is in flight, a late
        // failure must not undo bookkeeping the NEW controller wrote for the same
        // numeric id (#6432 round 4).
        const epoch = statesRef.current.get(tabId)?.promptEpoch ?? 0;
        dispatchTo(tabId, { type: "clearApproval" });
        app.ApproveTab(tabId, id, allow, session, persist).catch(() => {
            // The backend never actually resolved this prompt — undo the optimistic
            // tombstone and ask it to replay, so the approval card can come back
            // instead of being silently lost forever (#6432 round 3).
            dispatchTo(tabId, { type: "submit_prompt_failed", id, epoch });
            replayPendingPromptsForActiveTab(tabId);
        });
    }, [activeTabId, dispatchTo]);
    const resolvePlanDecision = useCallback((id: string, action: "start_execution" | "revise_plan" | "exit_plan") => {
        if (!activeTabId)
            return;
        const tabId = activeTabId;
        const epoch = statesRef.current.get(tabId)?.promptEpoch ?? 0;
        dispatchTo(tabId, { type: "clearApproval" });
        const request = typeof app.ResolvePlanDecisionTab === "function"
            ? app.ResolvePlanDecisionTab(tabId, id, action)
            : app.ApproveTab(tabId, id, action === "start_execution", false, false);
        request.catch(() => {
            dispatchTo(tabId, { type: "submit_prompt_failed", id, epoch });
            replayPendingPromptsForActiveTab(tabId);
        });
    }, [activeTabId, dispatchTo]);
    const resolveRecovery = useCallback((id: string, action: "continue" | "continue_task" | "revise" | "stop", feedback = "") => {
        if (!activeTabId)
            return;
        const tabId = activeTabId;
        const epoch = statesRef.current.get(tabId)?.promptEpoch ?? 0;
        dispatchTo(tabId, { type: "clearApproval" });
        app.ResolveRecoveryTab(tabId, id, action, feedback).catch(() => {
            dispatchTo(tabId, { type: "submit_prompt_failed", id, epoch });
            replayPendingPromptsForActiveTab(tabId);
        });
    }, [activeTabId, dispatchTo]);
    const answerQuestion = useCallback((id: string, answers: QuestionAnswer[]) => {
        if (!activeTabId)
            return;
        const tabId = activeTabId;
        const epoch = statesRef.current.get(tabId)?.promptEpoch ?? 0;
        dispatchTo(tabId, { type: "clearAsk" });
        app.AnswerQuestionForTab(tabId, id, answers).catch(() => {
            dispatchTo(tabId, { type: "submit_prompt_failed", id, epoch });
            replayPendingPromptsForActiveTab(tabId);
        });
    }, [activeTabId, dispatchTo]);
    const setControllerMode = useCallback((mode: Mode): Promise<void> => {
        if (!activeTabId)
            return Promise.resolve();
        const tabId = activeTabId;
        const epoch = statesRef.current.get(tabId)?.promptEpoch ?? 0;
        return app.SetModeForTab(tabId, mode).then((drained) => {
            // Only dismiss the approvals the backend reports it actually
            // auto-allowed. Fresh prompts (plan/memory/sandbox escape) survive a
            // yolo switch backend-side and must stay visible (#6432 round 4).
            const ids = Array.isArray(drained) ? drained : [];
            if (ids.length)
                dispatchTo(tabId, { type: "approval_drained", ids, epoch });
        }).catch(() => { });
    }, [activeTabId, dispatchTo]);
    const setCollaborationModeForTab = useCallback(async (tabId: string, mode: CollaborationMode): Promise<void> => {
        if (!tabId)
            return;
        await app.SetCollaborationModeForTab(tabId, mode).catch(() => { });
        await refreshMetaForTab(tabId);
    }, [refreshMetaForTab]);
    const setCollaborationMode = useCallback(async (mode: CollaborationMode): Promise<void> => {
        if (!activeTabId)
            return;
        await setCollaborationModeForTab(activeTabId, mode);
    }, [activeTabId, setCollaborationModeForTab]);
    const setToolApprovalModeForTab = useCallback(async (tabId: string, mode: ToolApprovalMode): Promise<void> => {
        if (!tabId)
            return;
        const epoch = statesRef.current.get(tabId)?.promptEpoch ?? 0;
        // Same contract as setControllerMode: the backend reports which pending
        // approvals the new posture auto-allowed; anything else is still pending
        // there (fresh prompts; ask-rule approvals under auto) and stays visible.
        const drained = await app.SetToolApprovalModeForTab(tabId, mode).catch(() => undefined);
        const ids = Array.isArray(drained) ? drained : [];
        if (ids.length)
            dispatchTo(tabId, { type: "approval_drained", ids, epoch });
        await refreshMetaForTab(tabId);
    }, [dispatchTo, refreshMetaForTab]);
    const setToolApprovalMode = useCallback(async (mode: ToolApprovalMode): Promise<void> => {
        if (!activeTabId)
            return;
        await setToolApprovalModeForTab(activeTabId, mode);
    }, [activeTabId, setToolApprovalModeForTab]);
    const setComposerProfileForTab = useCallback(async (tabId: string, collaborationMode: CollaborationMode, toolApprovalMode: ToolApprovalMode, goal: string, options?: {
        propagateError?: boolean;
    }): Promise<boolean> => {
        if (!tabId)
            return false;
        const state = statesRef.current.get(tabId);
        const promptEpoch = state?.promptEpoch ?? 0;
        const key = composerProfileApplicationKey(runtimeEpochByTabRef.current.get(tabId) ?? state?.meta?.runtime?.epoch, collaborationMode, toolApprovalMode, goal);
        if (appliedComposerProfileByTabRef.current.get(tabId) === key)
            return true;
        const existing = composerProfileInFlightByTabRef.current.get(tabId);
        if (existing?.key === key)
            return existing.promise;
        const lifecycle = composerProfileLifecycleByTabRef.current.get(tabId) ?? 0;
        const previous = composerProfileQueueByTabRef.current.get(tabId) ?? Promise.resolve();
        const promise = previous.then(async () => {
            if ((composerProfileLifecycleByTabRef.current.get(tabId) ?? 0) !== lifecycle)
                return false;
            if (appliedComposerProfileByTabRef.current.get(tabId) === key)
                return true;
            let drained: string[] | void;
            try {
                drained = await app.SetComposerProfileForTab(tabId, collaborationMode, toolApprovalMode, goal);
            }
            catch (error) {
                if ((composerProfileLifecycleByTabRef.current.get(tabId) ?? 0) === lifecycle) {
                    await refreshMetaForTab(tabId);
                }
                if (options?.propagateError)
                    throw error;
                return false;
            }
            if ((composerProfileLifecycleByTabRef.current.get(tabId) ?? 0) !== lifecycle)
                return false;
            appliedComposerProfileByTabRef.current.set(tabId, key);
            const ids = Array.isArray(drained) ? drained : [];
            if (ids.length)
                dispatchTo(tabId, { type: "approval_drained", ids, epoch: promptEpoch });
            await refreshMetaForTab(tabId);
            return true;
        });
        const tail = promise.then(() => { }, () => { });
        composerProfileQueueByTabRef.current.set(tabId, tail);
        composerProfileInFlightByTabRef.current.set(tabId, { key, promise });
        try {
            return await promise;
        }
        finally {
            const current = composerProfileInFlightByTabRef.current.get(tabId);
            if (current?.promise === promise)
                composerProfileInFlightByTabRef.current.delete(tabId);
            if (composerProfileQueueByTabRef.current.get(tabId) === tail) {
                composerProfileQueueByTabRef.current.delete(tabId);
            }
        }
    }, [dispatchTo, refreshMetaForTab]);
    const setGoalForTab = useCallback(async (tabId: string, goal: string): Promise<void> => {
        if (!tabId)
            return;
        // Propagate activation failures so the first Goal turn (especially structured
        // Skill submit) can abort instead of executing without an active Goal.
        try {
            await app.SetGoalForTab(tabId, goal);
        }
        finally {
            await refreshMetaForTab(tabId);
        }
    }, [refreshMetaForTab]);
    const setGoal = useCallback(async (goal: string): Promise<void> => {
        if (!activeTabId)
            return;
        await setGoalForTab(activeTabId, goal);
    }, [activeTabId, setGoalForTab]);
    const clearGoalForTab = useCallback(async (tabId: string): Promise<void> => {
        if (!tabId)
            return;
        try {
            await app.ClearGoalForTab(tabId);
        }
        finally {
            await refreshMetaForTab(tabId);
        }
    }, [refreshMetaForTab]);
    const clearGoal = useCallback(async (): Promise<void> => {
        if (!activeTabId)
            return;
        await clearGoalForTab(activeTabId);
    }, [activeTabId, clearGoalForTab]);
    const resumeGoalForTab = useCallback(async (tabId: string): Promise<boolean> => {
        if (!tabId)
            return false;
        try {
            const resumed = await app.ResumeGoalForTab(tabId);
            await refreshMetaForTab(tabId);
            return resumed;
        }
        catch {
            return false;
        }
    }, [refreshMetaForTab]);
    const resumeGoal = useCallback(async (): Promise<boolean> => {
        if (!activeTabId)
            return false;
        return resumeGoalForTab(activeTabId);
    }, [activeTabId, resumeGoalForTab]);
    const pauseGoalForTab = useCallback(async (tabId: string): Promise<boolean> => {
        if (!tabId)
            return false;
        try {
            const paused = await app.PauseGoalForTab(tabId);
            await refreshMetaForTab(tabId);
            return paused;
        }
        catch {
            return false;
        }
    }, [refreshMetaForTab]);
    const pauseGoal = useCallback(async (): Promise<boolean> => {
        if (!activeTabId)
            return false;
        return pauseGoalForTab(activeTabId);
    }, [activeTabId, pauseGoalForTab]);
    const newSession = useCallback(async () => {
        const tabId = activeTabId;
        if (tabId)
            await waitForTabReady(tabId);
        if (tabId) {
            addBreadcrumb("session.new", `click ${tabId}`);
            bumpCheckpointRefreshSeq(tabId);
            bumpSessionLoadSeq(tabId);
            dispatchTo(tabId, { type: "reset" });
            dispatchTo(tabId, { type: "hydrate_start", reason: "new-session" });
            addBreadcrumb("session.new", `visible-reset ${tabId}`);
        }
        try {
            if (tabId)
                await app.NewSessionForTab(tabId);
            else
                await app.NewSession();
            addBreadcrumb("session.new", `backend-done ${tabId ?? ""}`);
        }
        catch (err) {
            if (tabId) {
                dispatchTo(tabId, { type: "hydrate_error", reason: "new-session", error: errorMessage(err) });
                void loadSessionDataForTab(tabId, true, "new-session").then(() => {
                    dispatchTo(tabId, { type: "local_notice", level: "warn", text: `New session failed: ${errorMessage(err)}` });
                });
            }
            return; // backend refused (workspace starting / failed) — keep the transcript
        }
        invalidateCache();
        if (tabId) {
            dispatchTo(tabId, { type: "history", messages: [] });
            dispatchTo(tabId, { type: "hydrate_done" });
            void refreshMetaForTab(tabId);
            app.ContextUsageForTab(tabId).then((context) => dispatchTo(tabId, { type: "context", context })).catch(() => { });
            void refreshCheckpoints(tabId);
        }
    }, [activeTabId, bumpCheckpointRefreshSeq, bumpSessionLoadSeq, dispatchTo, loadSessionDataForTab, refreshCheckpoints, refreshMetaForTab, waitForTabReady]);
    const clearSession = useCallback(async () => {
        const tabId = activeTabId;
        if (tabId)
            await waitForTabReady(tabId);
        if (tabId) {
            bumpCheckpointRefreshSeq(tabId);
            bumpSessionLoadSeq(tabId);
            sessionLoadInFlight.current.delete(tabId);
        }
        let cleared: {
            sessionPath: string;
            sessionRevision?: number;
            sessionDigest?: string;
            sessionGeneration: number;
        };
        try {
            cleared = tabId ? await app.ClearSessionForTab(tabId) : await app.ClearSession();
        }
        catch {
            if (tabId)
                void loadSessionDataForTab(tabId);
            return;
        }
        if (tabId)
            bumpSessionLoadSeq(tabId);
        invalidateCache();
        if (tabId) {
            // Retire every resident projection for this tab so a mode switch cannot
            // preferResident-serve the destroyed transcript.
            getTranscriptStore().evictTab(tabId);
            const existing = statesRef.current.get(tabId)?.meta;
            const nextMeta = {
                ...(existing ?? { label: "", ready: true, eventChannel: "agent:event", cwd: "" }),
                sessionPath: cleared.sessionPath || "",
                sessionRevision: cleared.sessionRevision,
                sessionDigest: cleared.sessionDigest,
                sessionGeneration: cleared.sessionGeneration,
            };
            // Meta first so reset preserves the replacement identity.
            dispatchTo(tabId, { type: "optimistic_meta", meta: nextMeta });
            dispatchTo(tabId, { type: "reset" });
            dispatchTo(tabId, { type: "history", messages: [] });
        }
    }, [activeTabId, bumpCheckpointRefreshSeq, bumpSessionLoadSeq, dispatchTo, loadSessionDataForTab, waitForTabReady]);
    const listSessions = useCallback(async (): Promise<SessionMeta[]> => {
        const page = await app.ListHistorySessions({ scope: "all", workspaceRoot: "", status: "all", timeFilter: "all", query: "", cursor: "", limit: 200 });
        if (!page)
            throw new Error(t("history.failedLoadHistory"));
        return asArray<SessionMeta>(page.items);
    }, []);
    const listTrashedSessions = useCallback(async (): Promise<SessionMeta[]> => asArray<SessionMeta>(await app.ListTrashedSessions().catch(() => [])), []);
    const retrySessionHistory = useCallback(async (tabId?: string) => {
        const id = tabId || activeTabIdRef.current;
        if (!id)
            return;
        const m = statesRef.current.get(id)?.meta;
        await loadSessionDataForTab(id, false, "startup", { sessionPath: m?.sessionPath, sessionRevision: m?.sessionRevision, sessionDigest: m?.sessionDigest, preserveCachedHistory: false });
    }, [loadSessionDataForTab]);
    const resumeSession = useCallback(async (path: string, tabId?: string, navigationIntentSeq?: number) => {
        const targetTabId = tabId || activeTabId;
        if (!targetTabId)
            return;
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        const existingMeta = statesRef.current.get(targetTabId)?.meta;
        if (existingMeta)
            dispatchTo(targetTabId, { type: "optimistic_meta", meta: { ...existingMeta, sessionPath: path } });
        void loadSessionDataForTab(targetTabId, false, "resume-session", { sessionPath: path, preserveCachedHistory: true });
        if (tabId)
            await waitForTabReady(tabId);
        else if (!(await waitForBackendActiveTab(targetTabId)))
            return;
        if (!navigationCompletionCurrent(navigationSeq, "session.resume", targetTabId))
            return;
        const seq = bumpSessionLoadSeq(targetTabId);
        dispatchTo(targetTabId, { type: "hydrate_start", reason: "resume-session" });
        let page: HistoryPage;
        try {
            page = tabId
                ? await app.ResumeSessionPageForTab(tabId, path, HISTORY_PAGE_TURNS)
                : await app.ResumeSessionPage(path, HISTORY_PAGE_TURNS);
        }
        catch (err) {
            if (isNavigationIntentCurrent(navigationSeq) && sessionLoadCurrent(targetTabId, seq)) {
                dispatchTo(targetTabId, { type: "hydrate_error", reason: "resume-session", error: errorMessage(err) });
                dispatchTo(targetTabId, { type: "local_notice", level: "warn", text: `${t("history.failedOpenSession")}: ${errorMessage(err)}` });
            }
            return;
        }
        if (!navigationCompletionCurrent(navigationSeq, "session.resume", targetTabId) || !sessionLoadCurrent(targetTabId, seq))
            return;
        dispatchTo(targetTabId, { type: "reset" });
        dispatchTo(targetTabId, { type: "history_page", page, mode: "replace" });
        dispatchTo(targetTabId, { type: "hydrate_done" });
        await refreshMetaOnlyForTab(targetTabId);
        if (!isNavigationIntentCurrent(navigationSeq) || !sessionLoadCurrent(targetTabId, seq))
            return;
        app.ContextUsageForTab(targetTabId).then((context) => dispatchTo(targetTabId, { type: "context", context })).catch(() => { });
        void refreshCheckpoints(targetTabId);
    }, [activeTabId, beginActiveNavigation, bumpSessionLoadSeq, dispatchTo, isNavigationIntentCurrent, loadSessionDataForTab, navigationCompletionCurrent, refreshCheckpoints, refreshMetaOnlyForTab, sessionLoadCurrent, waitForBackendActiveTab, waitForTabReady]);
    const openChannelSession = useCallback(async (path: string, tabId: string, navigationIntentSeq?: number) => {
        if (!tabId)
            return;
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        await waitForTabReady(tabId);
        if (!navigationCompletionCurrent(navigationSeq, "session.channel", tabId))
            return;
        const seq = bumpSessionLoadSeq(tabId);
        dispatchTo(tabId, { type: "hydrate_start", reason: "resume-session" });
        let page: HistoryPage;
        try {
            page = await app.OpenChannelSessionPageForTab(tabId, path, HISTORY_PAGE_TURNS);
        }
        catch (err) {
            if (isNavigationIntentCurrent(navigationSeq) && sessionLoadCurrent(tabId, seq)) {
                dispatchTo(tabId, { type: "hydrate_error", reason: "resume-session", error: errorMessage(err) });
                dispatchTo(tabId, { type: "local_notice", level: "warn", text: `${t("history.failedOpenSession")}: ${errorMessage(err)}` });
            }
            return;
        }
        if (!navigationCompletionCurrent(navigationSeq, "session.channel", tabId) || !sessionLoadCurrent(tabId, seq))
            return;
        dispatchTo(tabId, { type: "reset" });
        dispatchTo(tabId, { type: "history_page", page, mode: "replace" });
        dispatchTo(tabId, { type: "hydrate_done" });
        await refreshMetaOnlyForTab(tabId);
        if (!isNavigationIntentCurrent(navigationSeq) || !sessionLoadCurrent(tabId, seq))
            return;
        app.ContextUsageForTab(tabId).then((context) => dispatchTo(tabId, { type: "context", context })).catch(() => { });
        void refreshCheckpoints(tabId);
    }, [beginActiveNavigation, bumpSessionLoadSeq, dispatchTo, isNavigationIntentCurrent, navigationCompletionCurrent, refreshCheckpoints, refreshMetaOnlyForTab, sessionLoadCurrent, waitForTabReady]);
    const previewSession = useCallback(async (path: string): Promise<HistoryMessage[]> => asArray<HistoryMessage>(await app.PreviewSession(path).catch(() => [])), []);
    const deleteSession = useCallback((path: string) => app.DeleteSession(path).finally(() => invalidateCache()), []);
    const restoreSession = useCallback((path: string) => app.RestoreSession(path).catch(() => { }).finally(() => invalidateCache()), []);
    const purgeTrashedSession = useCallback((path: string) => app.PurgeTrashedSession(path).catch(() => { }).finally(() => invalidateCache()), []);
    const renameSession = useCallback((path: string, title: string) => app.RenameSession(path, title).catch(() => { }).finally(() => invalidateCache()), []);
    const refreshMeta = useCallback(async () => {
        if (!activeTabId)
            return;
        invalidateSharedQuery("MetaForTab", [activeTabId]);
        await refreshMetaForTab(activeTabId);
    }, [activeTabId, refreshMetaForTab]);
    const refreshWorkspaceState = useCallback(async (path: string): Promise<string> => {
        if (path)
            await syncActiveTabFromBackend(true);
        return path;
    }, [syncActiveTabFromBackend]);
    const pickWorkspace = useCallback(async (): Promise<string> => {
        beginActiveNavigation();
        const path = await app.PickWorkspace().catch(() => "");
        return refreshWorkspaceState(path);
    }, [beginActiveNavigation, refreshWorkspaceState]);
    const switchWorkspace = useCallback(async (path: string): Promise<string> => {
        beginActiveNavigation();
        const next = await app.SwitchWorkspace(path).catch(() => "");
        return refreshWorkspaceState(next);
    }, [beginActiveNavigation, refreshWorkspaceState]);
    const compact = useCallback(() => {
        const tabId = activeTabIdRef.current;
        if (!tabId)
            return;
        void waitForTabReady(tabId).then(() => app.CompactForTab(tabId).catch(() => { }));
    }, [waitForTabReady]);
    const enqueueModelSwitch = useCallback((tabId: string, name: string, fallbackBalance?: BalanceInfo) => {
        let queue = modelSwitchQueueByTab.current.get(tabId);
        if (!queue) {
            queue = { running: false, fallbackBalance };
            modelSwitchQueueByTab.current.set(tabId, queue);
        }
        const queueState = queue;
        return new Promise<ModelSwitchQueueResult>((resolve, reject) => {
            const request: ModelSwitchQueueRequest = { name, resolve, reject };
            const run = (next: ModelSwitchQueueRequest) => {
                queueState.running = true;
                void Promise.resolve()
                    .then(() => app.SetModelForTab(tabId, next.name))
                    .then(() => next.resolve("applied"), (err) => next.reject(err))
                    .finally(() => {
                    if (modelSwitchQueueByTab.current.get(tabId) !== queueState)
                        return;
                    const pending = queueState.pending;
                    queueState.pending = undefined;
                    if (pending) {
                        run(pending);
                        return;
                    }
                    queueState.running = false;
                    modelSwitchQueueByTab.current.delete(tabId);
                });
            };
            if (queueState.running) {
                queueState.pending?.resolve("superseded");
                queueState.pending = request;
                return;
            }
            run(request);
        });
    }, []);
    const setModel = useCallback(async (name: string) => {
        if (!activeTabId)
            return false;
        const tabId = activeTabId;
        const switchSeq = (modelSwitchSeqByTab.current.get(tabId) ?? 0) + 1;
        const successVersion = modelSwitchSuccessVersionByTab.current.get(tabId) ?? 0;
        const existingQueue = modelSwitchQueueByTab.current.get(tabId);
        // Every attempt in one queued burst shares the balance that was visible
        // before the first switch cleared it. Otherwise a later queued failure
        // captures the placeholder and cannot restore the outgoing provider.
        const fallbackBalance = existingQueue
            ? existingQueue.fallbackBalance
            : statesRef.current.get(tabId)?.balance;
        modelSwitchSeqByTab.current.set(tabId, switchSeq);
        // Hide the outgoing provider's wallet as soon as the user starts a hot
        // switch. If the rebuild fails, the catch path re-queries the still-active
        // provider and restores its balance.
        clearBalanceForTab(tabId);
        try {
            const result = await enqueueModelSwitch(tabId, name, fallbackBalance);
            if (result === "superseded")
                return false;
            modelSwitchSuccessVersionByTab.current.set(tabId, (modelSwitchSuccessVersionByTab.current.get(tabId) ?? 0) + 1);
        }
        catch (err) {
            if (modelSwitchSeqByTab.current.get(tabId) !== switchSeq)
                return false;
            dispatchTo(tabId, { type: "local_notice", level: "warn", text: modelSwitchNoticeText(err) });
            const olderSwitchSucceeded = (modelSwitchSuccessVersionByTab.current.get(tabId) ?? 0) !== successVersion;
            // Restore the known balance only when no older overlapping switch
            // completed after this attempt began. Otherwise the backend now owns a
            // different provider and the refresh below must establish its balance.
            if (fallbackBalance && !olderSwitchSucceeded) {
                dispatchTo(tabId, { type: "balance", balance: fallbackBalance });
            }
            void refreshBalanceForTab(tabId);
            // A superseded success deliberately skips its own UI reconciliation.
            // If this latest queued switch then fails, reconcile the model metadata
            // to the provider that actually became active in the backend.
            if (olderSwitchSucceeded)
                await refreshMetaForTab(tabId);
            return false;
        }
        if (modelSwitchSeqByTab.current.get(tabId) !== switchSeq)
            return false;
        void refreshBalanceForTab(tabId);
        await refreshMetaForTab(tabId);
        return modelSwitchSeqByTab.current.get(tabId) === switchSeq;
    }, [activeTabId, clearBalanceForTab, dispatchTo, enqueueModelSwitch, refreshBalanceForTab, refreshMetaForTab]);
    const setEffort = useCallback(async (level: string) => {
        if (!activeTabId)
            return;
        try {
            await app.SetEffortForTab(activeTabId, level);
        }
        catch (err) {
            dispatchTo(activeTabId, { type: "local_notice", level: "warn", text: effortSwitchNoticeText(err) });
            return;
        }
        await refreshMetaForTab(activeTabId);
    }, [activeTabId, dispatchTo, refreshMetaForTab]);
    const setTokenMode = useCallback(async (mode: TokenMode): Promise<boolean> => {
        if (!activeTabId)
            return false;
        try {
            await app.SetTokenModeForTab(activeTabId, mode);
        }
        catch (err) {
            dispatchTo(activeTabId, { type: "local_notice", level: "warn", text: tokenModeSwitchNoticeText(err) });
            return false;
        }
        await refreshMetaForTab(activeTabId);
        return true;
    }, [activeTabId, dispatchTo, refreshMetaForTab]);
    const cancelJob = useCallback(async (jobID: string): Promise<boolean> => {
        const tabId = activeTabId;
        if (!tabId || !jobID.trim())
            return false;
        try {
            const cancelled = await app.CancelJobForTab(tabId, jobID);
            const jobs = asArray(await app.JobsForTab(tabId));
            dispatchTo(tabId, { type: "jobs", jobs });
            await refreshMetaForTab(tabId);
            return cancelled;
        }
        catch {
            dispatchTo(tabId, { type: "local_notice", level: "warn", text: t("status.jobStopFailed") });
            return false;
        }
    }, [activeTabId, dispatchTo, refreshMetaForTab]);
    const fetchMemory = useCallback((): Promise<MemoryView> => app.Memory().catch(() => ({
        docs: [], facts: [], archives: [], scopes: [], instructionDiagnostics: [], conflicts: [],
        lastRecall: { query: "", hits: [], omitted: 0, charBudget: 0, usedChars: 0 },
        storeDir: "", available: false,
    })), []);
    const remember = useCallback(async (scope: string, note: string) => { await app.Remember(scope, note).catch(() => { }); }, []);
    const forget = useCallback(async (name: string) => { await app.Forget(name).catch(() => { }); }, []);
    const saveDoc = useCallback(async (path: string, body: string) => { await app.SaveDoc(path, body).catch(() => { }); }, []);
    type RewindOutcome = {
        ok: boolean;
        transactionId?: string;
        undoAvailable?: boolean;
        written?: string[];
        deleted?: string[];
    };
    const rewindForTabDetailed = useCallback(async (sourceTabId: string, turn: number, scope: string): Promise<RewindOutcome> => {
        if (!sourceTabId)
            return { ok: false };
        const forkNavigationSeq = activeNavigationSeqRef.current;
        await waitForTabReady(sourceTabId);
        const actionScope = (["fork", "summ-from", "summ-upto", "conversation", "code", "both"].includes(scope) ? scope : "both") as MessageActionScope;
        dispatchTo(sourceTabId, { type: "message_action_start", action: { turn, scope: actionScope } });
        dispatchTo(sourceTabId, { type: "local_notice", level: "info", text: messageActionBusyText(actionScope) });
        try {
            if (actionScope === "fork") {
                const snapshotAt = promptEventClock();
                const tab = await app.ForkForTab(sourceTabId, turn);
                if (tab?.id) {
                    const navigationUnchanged = activeNavigationSeqRef.current === forkNavigationSeq;
                    const activateFork = tab.active && navigationUnchanged && activeTabIdRef.current === sourceTabId;
                    if (!activateFork) {
                        dispatchTo(tab.id, { type: "optimistic_meta", meta: metaFromTab(tab, statesRef.current.get(tab.id)?.meta) });
                        dispatchRuntimeStatusForTab(tab.id, tab, snapshotAt);
                        const currentTabId = activeTabIdRef.current;
                        if (tab.active) {
                            await reassertVisibleTabAfterStaleNavigation("tab.fork", tab.id);
                        }
                        else if (!tab.active && navigationUnchanged && currentTabId === sourceTabId) {
                            await syncActiveTabFromBackend(false, true);
                        }
                        addBreadcrumb("tab.fork", `stale completion ${tab.id} current=${currentTabId ?? ""}`);
                        return { ok: true };
                    }
                    beginActiveNavigation();
                    setActiveTabId(tab.id);
                    activeTabIdRef.current = tab.id;
                    confirmBackendActiveTab(tab.id);
                    dispatchRuntimeStatusForTab(tab.id, tab, snapshotAt);
                    await waitForTabReady(tab.id);
                    await loadSessionDataForTab(tab.id, true);
                    await reconcileTabRuntime(tab.id, { hydrateSessionData: false });
                }
                else {
                    await syncActiveTabFromBackend(true);
                }
                return { ok: true };
            }
            let outcome: RewindOutcome = { ok: true };
            if (actionScope === "summ-from")
                await app.SummarizeFromForTab(sourceTabId, turn);
            else if (actionScope === "summ-upto")
                await app.SummarizeUpToForTab(sourceTabId, turn);
            else {
                const { commitRewindWithPreview } = await import("./rewindCommit");
                const result = await commitRewindWithPreview(sourceTabId, turn, actionScope);
                if (!result?.ok) {
                    const detail = result?.error
                        || (result?.conflicts?.length ? result.conflicts.join("; ") : "")
                        || "rewind failed";
                    dispatchTo(sourceTabId, { type: "local_notice", level: "warn", text: detail });
                    return { ok: false, written: result?.written, deleted: result?.deleted };
                }
                outcome = result as RewindOutcome;
            }
            await loadSessionDataForTab(sourceTabId, true, "rewind");
            return outcome;
        }
        catch {
            return { ok: false };
        }
        finally {
            dispatchTo(sourceTabId, { type: "message_action_done" });
        }
    }, [beginActiveNavigation, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, loadSessionDataForTab, reassertVisibleTabAfterStaleNavigation, reconcileTabRuntime, syncActiveTabFromBackend, waitForTabReady]);
    const rewindForTab = useCallback(async (sourceTabId: string, turn: number, scope: string): Promise<boolean> => {
        return (await rewindForTabDetailed(sourceTabId, turn, scope)).ok;
    }, [rewindForTabDetailed]);
    const rewind = useCallback(async (turn: number, scope: string): Promise<boolean> => {
        if (!activeTabId)
            return false;
        return rewindForTab(activeTabId, turn, scope);
    }, [activeTabId, rewindForTab]);
    const undoRewindForTab = useCallback(async (sourceTabId: string, transactionId: string): Promise<boolean> => {
        if (!sourceTabId || !transactionId)
            return false;
        try {
            const { undoCommittedRewind } = await import("./rewindCommit");
            const result = await undoCommittedRewind(sourceTabId, transactionId);
            if (!result?.ok) {
                const detail = result?.error || "undo rewind failed";
                dispatchTo(sourceTabId, { type: "local_notice", level: "warn", text: detail });
                return false;
            }
            await loadSessionDataForTab(sourceTabId, true, "rewind");
            return true;
        }
        catch (err) {
            dispatchTo(sourceTabId, {
                type: "local_notice",
                level: "warn",
                text: err instanceof Error ? err.message : String(err),
            });
            return false;
        }
    }, [dispatchTo, loadSessionDataForTab]);
    // Tab management: switch preserves per-tab state; open creates it.
    const switchTab = useCallback(async (tabId: string, optimisticTab?: TabMeta, navigationIntentSeq?: number): Promise<TabMeta[] | undefined> => {
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        if (!navigationCompletionCurrent(navigationSeq, "tab.switch", tabId))
            return undefined;
        const startedAt = Date.now();
        topicActivationSeqRef.current += 1;
        const switchRequestId = `fe-switch-${Date.now()}-${topicActivationSeqRef.current}`;
        noteActivationRequested(switchRequestId);
        const previousTabId = activeTabIdRef.current;
        const targetSessionPath = optimisticTab?.sessionPath ?? statesRef.current.get(tabId)?.meta?.sessionPath;
        const targetSessionRevision = optimisticTab?.sessionRevision ?? statesRef.current.get(tabId)?.meta?.sessionRevision;
        const targetSessionDigest = optimisticTab?.sessionDigest ?? statesRef.current.get(tabId)?.meta?.sessionDigest;
        const preserveCachedHistory = hasReusableCachedTranscript(statesRef.current.get(tabId), targetSessionPath, targetSessionRevision, targetSessionDigest);
        addBreadcrumb("tab.switch", `click ${tabId}`);
        setActiveTabId(tabId);
        activeTabIdRef.current = tabId;
        dispatchTo(tabId, { type: "backend_activation_start" });
        noteActivationStarted(switchRequestId, tabId);
        if (optimisticTab) {
            dispatchTo(tabId, { type: "optimistic_meta", meta: metaFromTab(optimisticTab, statesRef.current.get(tabId)?.meta) });
            const optimisticStatus = backendStatusFromRuntimeMeta(optimisticTab);
            if (optimisticStatus.running)
                dispatchTo(tabId, optimisticStatus);
        }
        dispatchTo(tabId, { type: "hydrate_start", reason: "switch-tab" });
        addBreadcrumb("tab.switch", `active-rendered ${tabId} ms=${Date.now() - startedAt}`);
        const backendActivation = app.SetActiveTab(tabId)
            .then(async () => {
            const navigationCurrent = isNavigationIntentCurrent(navigationSeq);
            if (!navigationCurrent || activeTabIdRef.current !== tabId) {
                const currentTabId = activeTabIdRef.current;
                noteActivationSettled(switchRequestId, "cancelled");
                await reassertVisibleTabAfterStaleNavigation("tab.switch", tabId);
                addBreadcrumb("tab.switch", `set-active-stale ${tabId} seq=${navigationSeq} current=${currentTabId ?? ""} ms=${Date.now() - startedAt}`);
                return false;
            }
            confirmBackendActiveTab(tabId);
            addBreadcrumb("tab.switch", `set-active-done ${tabId} ms=${Date.now() - startedAt}`);
            return true;
        })
            .catch((err) => {
            noteActivationSettled(switchRequestId, "failed", errorMessage(err));
            if (!isNavigationIntentCurrent(navigationSeq))
                return false;
            dispatchTo(tabId, { type: "backend_activation_done" });
            dispatchTo(tabId, { type: "hydrate_error", reason: "switch-tab", error: errorMessage(err) });
            if (previousTabId && activeTabIdRef.current === tabId) {
                setActiveTabId(previousTabId);
                activeTabIdRef.current = previousTabId;
                addBreadcrumb("tab.switch", `set-active-failed-reverted ${tabId} -> ${previousTabId} ms=${Date.now() - startedAt}`);
            }
            return false;
        });
        trackBackendActivation(tabId, backendActivation);
        const backendSwitch = backendActivation
            .then(async (activated) => {
            if (!activated || !isNavigationIntentCurrent(navigationSeq)) {
                if (!activated)
                    noteActivationSettled(switchRequestId, "failed", "backend activation did not complete");
                return undefined;
            }
            const tabs = await reconcileTabRuntime(tabId, { hydrateSessionData: false });
            if (!isNavigationIntentCurrent(navigationSeq))
                return tabs;
            void loadSessionDataForTab(tabId, false, "switch-tab", {
                skipHistory: hasCachedLiveTurn(statesRef.current.get(tabId)),
                preserveCachedHistory,
                sessionPath: targetSessionPath,
                sessionRevision: targetSessionRevision,
                sessionDigest: targetSessionDigest,
            });
            noteActivationSettled(switchRequestId, "ready");
            return tabs;
        })
            .catch((err) => {
            noteActivationSettled(switchRequestId, "failed", errorMessage(err));
            if (isNavigationIntentCurrent(navigationSeq)) {
                dispatchTo(tabId, { type: "hydrate_error", reason: "switch-tab", error: errorMessage(err) });
            }
            return undefined;
        });
        return backendSwitch;
    }, [beginActiveNavigation, confirmBackendActiveTab, dispatchTo, isNavigationIntentCurrent, loadSessionDataForTab, navigationCompletionCurrent, reassertVisibleTabAfterStaleNavigation, reconcileTabRuntime, trackBackendActivation]);
    const openProjectTab = useCallback(async (workspaceRoot: string, topicId: string, navigationIntentSeq?: number): Promise<TabMeta> => {
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        const snapshotAt = promptEventClock();
        const meta = await app.OpenProjectTab(workspaceRoot, topicId);
        if (!navigationCompletionCurrent(navigationSeq, "tab.open-project", meta.id)) {
            await reassertVisibleTabAfterStaleNavigation("tab.open-project", meta.id);
            return meta;
        }
        const prevItems = activeTabIdRef.current ? statesRef.current.get(activeTabIdRef.current)?.items : undefined;
        const prevState = statesRef.current.get(meta.id);
        const isNewTab = !prevState;
        const preserveCachedHistory = hasReusableCachedTranscript(prevState, meta.sessionPath, meta.sessionRevision, meta.sessionDigest);
        setActiveTabId(meta.id);
        activeTabIdRef.current = meta.id;
        confirmBackendActiveTab(meta.id);
        dispatchTo(meta.id, { type: "optimistic_meta", meta: metaFromTab(meta, statesRef.current.get(meta.id)?.meta) });
        dispatchRuntimeStatusForTab(meta.id, meta, snapshotAt);
        const load = loadSessionDataForTab(meta.id, isNewTab, "open-topic", {
            placeholderItems: isNewTab ? prevItems : undefined,
            preserveCachedHistory,
            sessionPath: meta.sessionPath,
            sessionRevision: meta.sessionRevision,
            sessionDigest: meta.sessionDigest,
        });
        if (isNewTab)
            void load.then(() => reconcileTabRuntime(meta.id, { hydrateSessionData: false })).catch(() => { });
        else
            void load;
        return meta;
    }, [beginActiveNavigation, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, loadSessionDataForTab, navigationCompletionCurrent, reassertVisibleTabAfterStaleNavigation, reconcileTabRuntime]);
    const openGlobalTab = useCallback(async (topicId: string, navigationIntentSeq?: number): Promise<TabMeta> => {
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        const snapshotAt = promptEventClock();
        const meta = await app.OpenGlobalTab(topicId);
        if (!navigationCompletionCurrent(navigationSeq, "tab.open-global", meta.id)) {
            await reassertVisibleTabAfterStaleNavigation("tab.open-global", meta.id);
            return meta;
        }
        const prevItems = activeTabIdRef.current ? statesRef.current.get(activeTabIdRef.current)?.items : undefined;
        const prevState = statesRef.current.get(meta.id);
        const isNewTab = !prevState;
        const preserveCachedHistory = hasReusableCachedTranscript(prevState, meta.sessionPath, meta.sessionRevision, meta.sessionDigest);
        setActiveTabId(meta.id);
        activeTabIdRef.current = meta.id;
        confirmBackendActiveTab(meta.id);
        dispatchTo(meta.id, { type: "optimistic_meta", meta: metaFromTab(meta, statesRef.current.get(meta.id)?.meta) });
        dispatchRuntimeStatusForTab(meta.id, meta, snapshotAt);
        const load = loadSessionDataForTab(meta.id, isNewTab, "open-topic", {
            placeholderItems: isNewTab ? prevItems : undefined,
            preserveCachedHistory,
            sessionPath: meta.sessionPath,
            sessionRevision: meta.sessionRevision,
            sessionDigest: meta.sessionDigest,
        });
        if (isNewTab)
            void load.then(() => reconcileTabRuntime(meta.id, { hydrateSessionData: false })).catch(() => { });
        else
            void load;
        return meta;
    }, [beginActiveNavigation, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, loadSessionDataForTab, navigationCompletionCurrent, reassertVisibleTabAfterStaleNavigation, reconcileTabRuntime]);
    const openTopicSession = useCallback(async (scope: string, workspaceRoot: string, topicId: string, sessionPath: string, navigationIntentSeq?: number): Promise<TabMeta> => {
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        const snapshotAt = promptEventClock();
        const meta = await app.OpenTopicSession(scope, workspaceRoot, topicId, sessionPath);
        if (!navigationCompletionCurrent(navigationSeq, "tab.open-session", meta.id)) {
            await reassertVisibleTabAfterStaleNavigation("tab.open-session", meta.id);
            return meta;
        }
        const prevItems = activeTabIdRef.current ? statesRef.current.get(activeTabIdRef.current)?.items : undefined;
        const prevState = statesRef.current.get(meta.id);
        const isNewTab = !prevState;
        const preserveCachedHistory = hasReusableCachedTranscript(prevState, meta.sessionPath, meta.sessionRevision, meta.sessionDigest);
        setActiveTabId(meta.id);
        activeTabIdRef.current = meta.id;
        confirmBackendActiveTab(meta.id);
        dispatchTo(meta.id, { type: "optimistic_meta", meta: metaFromTab(meta, statesRef.current.get(meta.id)?.meta) });
        dispatchRuntimeStatusForTab(meta.id, meta, snapshotAt);
        const load = loadSessionDataForTab(meta.id, isNewTab, "open-topic", {
            placeholderItems: isNewTab ? prevItems : undefined,
            preserveCachedHistory,
            sessionPath: meta.sessionPath,
            sessionRevision: meta.sessionRevision,
            sessionDigest: meta.sessionDigest,
        });
        if (isNewTab)
            void load.then(() => reconcileTabRuntime(meta.id, { hydrateSessionData: false })).catch(() => { });
        else
            void load;
        return meta;
    }, [beginActiveNavigation, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, loadSessionDataForTab, navigationCompletionCurrent, reassertVisibleTabAfterStaleNavigation, reconcileTabRuntime]);
    const activateTopic = useCallback(async (scope: string, workspaceRoot: string, topicId: string, sessionPath = "", navigationIntentSeq?: number): Promise<TabMeta> => {
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        const snapshotAt = promptEventClock();
        // Ticketed two-phase activation: the backend switches the visible surface
        // before returning the ticket; the controller build and tab prune finish
        // in the background and report through "topic:activation". Register the
        // pending ticket before the call so synchronously-emitted events match.
        topicActivationSeqRef.current += 1;
        const pending: PendingTopicActivation = {
            requestId: `fe-act-${Date.now()}-${topicActivationSeqRef.current}`,
            navigationSeq,
        };
        pendingTopicActivationRef.current = pending;
        noteActivationRequested(pending.requestId);
        const ticket = await app.StartTopicActivation({
            scope,
            workspaceRoot,
            topicId,
            sessionPath,
            requestId: pending.requestId,
        });
        const meta = ticket.meta;
        pending.tabId = ticket.tabId;
        if (pendingTopicActivationRef.current === pending && ticket.requestId) {
            if (ticket.requestId !== pending.requestId)
                aliasActivationRequest(pending.requestId, ticket.requestId);
            pending.requestId = ticket.requestId;
        }
        if (!navigationCompletionCurrent(navigationSeq, "topic.activate", meta.id)) {
            // A newer navigation started while the backend processed this
            // activation. Applying the stale result would flip the visible tab
            // away from the user's last click and — worse — the single-surface
            // prune below deletes every other tab's cached state, blanking the
            // surface the user is actually looking at. Last click wins: hand the
            // meta back for bookkeeping and leave the visible state to the newer
            // navigation. The backend supersedes this ticket (its terminal event
            // is ignored above: the pending slot belongs to the newer request).
            await reassertVisibleTabAfterStaleNavigation("topic.activate", meta.id);
            return meta;
        }
        // Save previous tab's items so the new tab can use them as a placeholder
        // during loading, avoiding a blank/Welcome flash before history arrives.
        const prevItems = activeTabIdRef.current ? statesRef.current.get(activeTabIdRef.current)?.items : undefined;
        pending.placeholderItems = prevItems;
        for (const id of Array.from(statesRef.current.keys())) {
            if (id !== meta.id) {
                invalidateProviderStateForTab(id);
                disposeComposerProfileState(id);
                statesRef.current.delete(id);
                releaseTranscriptState(id);
            }
        }
        setActiveTabId(meta.id);
        activeTabIdRef.current = meta.id;
        confirmBackendActiveTab(meta.id);
        noteActivationStarted(pending.requestId, meta.id);
        dispatchTo(meta.id, { type: "optimistic_meta", meta: metaFromTab(meta, statesRef.current.get(meta.id)?.meta) });
        dispatchRuntimeStatusForTab(meta.id, meta, snapshotAt);
        // The hydrate is driven by the activation's terminal "ready" event; until
        // then keep the loading surface up with the previous tab's items as the
        // placeholder (same no-flash behavior the immediate hydrate had).
        dispatchTo(meta.id, { type: "hydrate_start", reason: "open-topic", placeholderItems: prevItems });
        if (pending.terminal && pendingTopicActivationRef.current === pending) {
            // The terminal event beat the ticket resolution; process it now.
            handleTopicActivationEvent(pending.terminal);
        }
        return meta;
    }, [beginActiveNavigation, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, disposeComposerProfileState, handleTopicActivationEvent, invalidateProviderStateForTab, navigationCompletionCurrent, reassertVisibleTabAfterStaleNavigation, releaseTranscriptState]);
    // Ensure a blank tab exists for the given scope — reuses an existing one
    // or creates a new tab, then loads its session data.
    const ensureBlankTab = useCallback(async (scope: string, workspaceRoot: string, navigationIntentSeq?: number): Promise<TabMeta> => {
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        const snapshotAt = promptEventClock();
        const meta = await app.EnsureBlankTab(scope, workspaceRoot);
        if (!navigationCompletionCurrent(navigationSeq, "tab.ensure-blank", meta.id)) {
            await reassertVisibleTabAfterStaleNavigation("tab.ensure-blank", meta.id);
            return meta;
        }
        // EnsureBlankTab may return a tab id already present in local state.
        // Invalidate its old hydration and force a fresh history read, otherwise a
        // late request can restore orphaned tool cards from the prior session.
        bumpCheckpointRefreshSeq(meta.id);
        const isNewTab = !statesRef.current.has(meta.id);
        setActiveTabId(meta.id);
        activeTabIdRef.current = meta.id;
        confirmBackendActiveTab(meta.id);
        dispatchTo(meta.id, { type: "optimistic_meta", meta: metaFromTab(meta, statesRef.current.get(meta.id)?.meta) });
        dispatchRuntimeStatusForTab(meta.id, meta, snapshotAt);
        const load = loadSessionDataForTab(meta.id, true, "new-session", { sessionPath: meta.sessionPath });
        if (isNewTab)
            void load.then(() => reconcileTabRuntime(meta.id, { hydrateSessionData: false })).catch(() => { });
        else
            void load;
        return meta;
    }, [beginActiveNavigation, bumpCheckpointRefreshSeq, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, loadSessionDataForTab, navigationCompletionCurrent, reassertVisibleTabAfterStaleNavigation, reconcileTabRuntime]);
    const ensureBlankSurface = useCallback(async (scope: string, workspaceRoot: string, navigationIntentSeq?: number): Promise<TabMeta> => {
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        const snapshotAt = promptEventClock();
        const meta = await app.EnsureBlankSurface(scope, workspaceRoot);
        if (!navigationCompletionCurrent(navigationSeq, "surface.ensure-blank", meta.id)) {
            await reassertVisibleTabAfterStaleNavigation("surface.ensure-blank", meta.id);
            return meta;
        }
        for (const id of Array.from(statesRef.current.keys())) {
            if (id !== meta.id) {
                invalidateProviderStateForTab(id);
                disposeComposerProfileState(id);
                statesRef.current.delete(id);
                releaseTranscriptState(id);
            }
        }
        setActiveTabId(meta.id);
        activeTabIdRef.current = meta.id;
        confirmBackendActiveTab(meta.id);
        dispatchTo(meta.id, { type: "optimistic_meta", meta: metaFromTab(meta, statesRef.current.get(meta.id)?.meta) });
        dispatchRuntimeStatusForTab(meta.id, meta, snapshotAt);
        void loadSessionDataForTab(meta.id, true, "open-topic")
            .then(() => reconcileTabRuntime(meta.id, { hydrateSessionData: false }))
            .catch(() => { });
        return meta;
    }, [beginActiveNavigation, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, disposeComposerProfileState, invalidateProviderStateForTab, loadSessionDataForTab, navigationCompletionCurrent, reassertVisibleTabAfterStaleNavigation, reconcileTabRuntime, releaseTranscriptState]);
    const createDeliveryWorktree = useCallback(async (workspaceRoot: string, navigationIntentSeq?: number): Promise<DeliveryWorktreeOpenResult> => {
        const navigationSeq = navigationIntentSeq ?? beginActiveNavigation();
        const snapshotAt = promptEventClock();
        const result = await app.CreateDeliveryWorktree(workspaceRoot);
        const meta = result.tab;
        if (!navigationCompletionCurrent(navigationSeq, "tab.delivery-worktree", meta.id)) {
            await reassertVisibleTabAfterStaleNavigation("tab.delivery-worktree", meta.id);
            return result;
        }
        const isNewTab = !statesRef.current.has(meta.id);
        setActiveTabId(meta.id);
        activeTabIdRef.current = meta.id;
        confirmBackendActiveTab(meta.id);
        dispatchTo(meta.id, { type: "optimistic_meta", meta: metaFromTab(meta, statesRef.current.get(meta.id)?.meta) });
        dispatchRuntimeStatusForTab(meta.id, meta, snapshotAt);
        const load = loadSessionDataForTab(meta.id, isNewTab, "open-topic");
        if (isNewTab)
            void load.then(() => reconcileTabRuntime(meta.id, { hydrateSessionData: false })).catch(() => { });
        else
            void load;
        return result;
    }, [beginActiveNavigation, confirmBackendActiveTab, dispatchRuntimeStatusForTab, dispatchTo, loadSessionDataForTab, navigationCompletionCurrent, reassertVisibleTabAfterStaleNavigation, reconcileTabRuntime]);
    const closeTab = useCallback(async (tabId: string, policy: "keep_running" | "stop_and_close" = "keep_running"): Promise<boolean> => {
        if (tabId === activeTabIdRef.current)
            beginActiveNavigation();
        try {
            await app.CloseTabWithPolicy(tabId, policy);
            invalidateProviderStateForTab(tabId);
            disposeComposerProfileState(tabId);
            statesRef.current.delete(tabId);
            releaseTranscriptState(tabId);
            notifyLiveListeners(tabId);
            bump();
            if (tabId === activeTabId)
                await syncActiveTabFromBackend(false);
            return true;
        }
        catch {
            return false;
        }
    }, [activeTabId, beginActiveNavigation, bump, disposeComposerProfileState, invalidateProviderStateForTab, notifyLiveListeners, releaseTranscriptState, syncActiveTabFromBackend]);
    const reorderTabs = useCallback(async (tabIds: string[]) => {
        try {
            await app.ReorderTabs(tabIds);
        }
        catch { /* ignore */ }
    }, []);
    return {
        state: activeState,
        liveStore,
        activeTabId,
        send, sendToTab, recoverDeliveryToTab, waiveDeliveryToTab, runShell, runShellForTab, steer, steerForTab, notice, cancel, approve, resolvePlanDecision, resolveRecovery, answerQuestion, setControllerMode,
        dismissExtensionForm, drainExtensionNotifications,
        setCollaborationMode, setCollaborationModeForTab, setToolApprovalMode, setToolApprovalModeForTab, setComposerProfileForTab, setGoal, setGoalForTab, clearGoal, clearGoalForTab, resumeGoal, resumeGoalForTab, pauseGoal, pauseGoalForTab,
        newSession, clearSession, listSessions, listTrashedSessions, retrySessionHistory, resumeSession, openChannelSession, previewSession, deleteSession, restoreSession, purgeTrashedSession, renameSession,
        loadOlderHistory,
        requestHistoryFullContent,
        refreshMeta, pickWorkspace, switchWorkspace, compact, rewind, rewindForTab, rewindForTabDetailed, undoRewindForTab, setModel, setEffort, setTokenMode, cancelJob,
        fetchMemory, remember, forget, saveDoc,
        switchTab, openProjectTab, openGlobalTab, openTopicSession, ensureBlankTab, activateTopic, ensureBlankSurface, createDeliveryWorktree, closeTab, reorderTabs,
        // Invalidate in-flight navigation completions (activateTopic's stale
        // guard) from outside the hook. The App-level navigation queue must call
        // this at ENQUEUE time: a queued click does not run — and so does not
        // advance this epoch — until the running request finishes, which would
        // let the running stale activation pass the guard and prune the state of
        // the surface the user just clicked.
        noteNavigationIntent: beginActiveNavigation,
        isNavigationIntentCurrent,
        syncActiveTab: syncActiveTabFromBackend,
    };
}
export type { ToolStatus as ToolStatus } from "./controller_subagent";
export { SUBAGENT_PROGRESS_STATUS as SUBAGENT_PROGRESS_STATUS } from "./controller_subagent";
export { SUBAGENT_PROGRESS_REASONING as SUBAGENT_PROGRESS_REASONING } from "./controller_subagent";
export { SUBAGENT_PROGRESS_TEXT as SUBAGENT_PROGRESS_TEXT } from "./controller_subagent";
export { SUBAGENT_PROGRESS_NOTICE as SUBAGENT_PROGRESS_NOTICE } from "./controller_subagent";
export type { SubagentPhase as SubagentPhase } from "./controller_subagent";
export type { SubagentProgress as SubagentProgress } from "./controller_subagent";
export { isSubagentProgressName as isSubagentProgressName } from "./controller_subagent";
export { isTerminalSubagentPhase as isTerminalSubagentPhase } from "./controller_subagent";
export { TURN_ACTIVITY_KINDS as TURN_ACTIVITY_KINDS } from "./controller_subagent";
export { SUBAGENT_PROGRESS_TOOLS as SUBAGENT_PROGRESS_TOOLS } from "./controller_subagent";
export { isGroupSubagentTool as isGroupSubagentTool } from "./controller_subagent";
export { terminalStatusOf as terminalStatusOf } from "./controller_subagent";
export { freshSubagentProgress as freshSubagentProgress } from "./controller_subagent";
export { applySubagentProgress as applySubagentProgress } from "./controller_subagent";
export { touchSubagentParent as touchSubagentParent } from "./controller_subagent";
export type { LiveStream as LiveStream } from "./controller_state";
export type { ControllerLiveStore as ControllerLiveStore } from "./controller_state";
export type { MessageActionScope as MessageActionScope } from "./controller_state";
export type { MessageActionState as MessageActionState } from "./controller_state";
export type { HydrateReason as HydrateReason } from "./controller_state";
export type { TurnPhaseName as TurnPhaseName } from "./controller_state";
export type { Item as Item } from "./controller_state";
export type { ExtensionItem as ExtensionItem } from "./controller_state";
export type { ExtensionStatusEntry as ExtensionStatusEntry } from "./controller_state";
export type { ExtensionFormState as ExtensionFormState } from "./controller_state";
export type { ExtensionNotificationEntry as ExtensionNotificationEntry } from "./controller_state";
export { extensionSurfaceKey as extensionSurfaceKey } from "./controller_state";
export { acceptsExtensionGeneration as acceptsExtensionGeneration } from "./controller_state";
export { STEER_NOTICE_PREFIX as STEER_NOTICE_PREFIX } from "./controller_state";
export { isSteerNoticeText as isSteerNoticeText } from "./controller_state";
export { initialState as initialState } from "./controller_state";
export type { SyncActiveTabOptions as SyncActiveTabOptions } from "./controller_state";
export type { PendingTopicActivation as PendingTopicActivation } from "./controller_state";
export type { ModelSwitchQueueResult as ModelSwitchQueueResult } from "./controller_state";
export type { ModelSwitchQueueRequest as ModelSwitchQueueRequest } from "./controller_state";
export type { ModelSwitchQueueState as ModelSwitchQueueState } from "./controller_state";
export { HISTORY_PAGE_TURNS as HISTORY_PAGE_TURNS } from "./controller_state";
export type { ToolItem as ToolItem } from "./controller_state";
export type { State as State } from "./controller_state";
export { foregroundRunningFromRuntimeMeta as foregroundRunningFromRuntimeMeta } from "./controller_meta";
export { promptEventClock as promptEventClock } from "./controller_meta";
export { runtimeSnapshotPredatesPrompt as runtimeSnapshotPredatesPrompt } from "./controller_meta";
export { metaFromTab as metaFromTab } from "./controller_meta";
export { sameMeta as sameMeta } from "./controller_meta";
export { runtimeReadyForSubmit as runtimeReadyForSubmit } from "./controller_meta";
export { normalizeTurnSubmit as normalizeTurnSubmit } from "./controller_meta";
export { createTurnSubmissionId as createTurnSubmissionId } from "./controller_meta";
export { acceptsRuntimeEventEpoch as acceptsRuntimeEventEpoch } from "./controller_meta";
export { composerProfileApplicationKey as composerProfileApplicationKey } from "./controller_meta";
export { shouldReconcileStaleTurn as shouldReconcileStaleTurn } from "./controller_meta";
export { isReadOnlyTool as isReadOnlyTool } from "./controller_meta";
export { usageTotalTokens as usageTotalTokens } from "./controller_meta";
export type { RuntimeMetaSnapshot as RuntimeMetaSnapshot } from "./controller_meta";
export { runtimeSnapshotPredatesRetry as runtimeSnapshotPredatesRetry } from "./controller_meta";
export { updatesContextGauge as updatesContextGauge } from "./controller_meta";
export { countsTowardCurrentTurn as countsTowardCurrentTurn } from "./controller_meta";
export { metaWithoutCanonicalTodos as metaWithoutCanonicalTodos } from "./controller_meta";
export { STALE_TURN_RECONCILE_MS as STALE_TURN_RECONCILE_MS } from "./controller_meta";
export { CANCEL_RECONCILE_DELAYS_MS as CANCEL_RECONCILE_DELAYS_MS } from "./controller_meta";
export { STALE_PROMPT_RECONCILE_MS as STALE_PROMPT_RECONCILE_MS } from "./controller_meta";
export { STARTUP_READY_META_RECONCILE_MS as STARTUP_READY_META_RECONCILE_MS } from "./controller_meta";
export { STARTUP_READY_META_RECONCILE_ATTEMPTS as STARTUP_READY_META_RECONCILE_ATTEMPTS } from "./controller_meta";
export { hasCachedLiveTurn as hasCachedLiveTurn } from "./controller_meta";
export { hasReusableCachedTranscript as hasReusableCachedTranscript } from "./controller_meta";
export { historyFingerprintMatchesMeta as historyFingerprintMatchesMeta } from "./controller_meta";
export { historyMessagesToItems as historyMessagesToItems } from "./controller_history";
export { historyToolError as historyToolError } from "./controller_history";
export { currentTurnWaitMs as currentTurnWaitMs } from "./controller_history";
export { compactArchivedToolItems as compactArchivedToolItems } from "./controller_history";
export type { Action as Action } from "./controller_history";
export { backendStatusFromRuntimeMeta as backendStatusFromRuntimeMeta } from "./controller_history";
export { applyTurnCheckpoint as applyTurnCheckpoint } from "./controller_history";
export { historyPageItems as historyPageItems } from "./controller_history";
export { ensureAssistant as ensureAssistant } from "./controller_history";
export { liveReasoningDurationMs as liveReasoningDurationMs } from "./controller_history";
export { applyDeltaSegments as applyDeltaSegments } from "./controller_history";
export { applyStreamBatch as applyStreamBatch } from "./controller_history";
export { currentTurnDurationMs as currentTurnDurationMs } from "./controller_history";
export { reducer as reducer } from "./controller_reducer";
export { beginTurnModelActivity as beginTurnModelActivity } from "./controller_reducer";
export { effortSwitchNoticeText as effortSwitchNoticeText } from "./controller_notice";
export { modelSwitchNoticeText as modelSwitchNoticeText } from "./controller_notice";
export { tokenModeSwitchNoticeText as tokenModeSwitchNoticeText } from "./controller_notice";
export { localizedNoticeText as localizedNoticeText } from "./controller_notice";
export { deliveryReadinessDetail as deliveryReadinessDetail } from "./controller_notice";
export { localizedBackendNoticeText as localizedBackendNoticeText } from "./controller_notice";
export { quietTranscriptNoticeKey as quietTranscriptNoticeKey } from "./controller_notice";
export type { TabStates as TabStates } from "./controller_notice";
export { getOrCreateState as getOrCreateState } from "./controller_notice";
export { messageActionBusyText as messageActionBusyText } from "./controller_notice";
export { errorMessage as errorMessage } from "./controller_notice";
export { appendNoticeItem as appendNoticeItem } from "./controller_notice";
export { appendNoticeToState as appendNoticeToState } from "./controller_notice";

