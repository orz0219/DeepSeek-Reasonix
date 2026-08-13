import { asArray } from "./array";
import { t, type DictKey } from "./i18n";
import type { WireDecisionReceipt, WireFinalReadiness } from "./types";
import { State, initialState, MessageActionScope, Item } from "./controller_state";
// ---- per-tab state map ----
export type TabStates = Map<string, State>;
export function getOrCreateState(states: TabStates, tabId: string): State {
    if (!states.has(tabId))
        states.set(tabId, { ...initialState });
    return states.get(tabId)!;
}
export function messageActionBusyText(scope: MessageActionScope): string {
    switch (scope) {
        case "fork":
            return t("rewind.busyFork");
        case "summ-from":
            return t("rewind.busySummFrom");
        case "summ-upto":
            return t("rewind.busySummUpto");
        case "conversation":
            return t("rewind.busyConversation");
        case "code":
            return t("rewind.busyCode");
        default:
            return t("rewind.busyBoth");
    }
}
export function errorMessage(err: unknown): string {
    if (err instanceof Error)
        return err.message;
    if (typeof err === "string")
        return err;
    return String(err || "");
}
export function effortSwitchNoticeText(err: unknown): string {
    return settingSwitchNoticeText(err, "effort", {
        busy: "status.effortSwitchBusy",
        busyRunning: "status.effortSwitchBusyRunning",
        busyPrompt: "status.effortSwitchBusyPrompt",
        busyJobs: "status.effortSwitchBusyJobs",
        leaseHeld: "status.effortSwitchLeaseHeld",
        starting: "status.effortSwitchStarting",
        startupFailed: "status.effortSwitchStartupFailed",
        retry: "status.effortSwitchRetry",
        failed: "status.effortSwitchFailed",
    });
}
export function modelSwitchNoticeText(err: unknown): string {
    const msg = errorMessage(err).trim() || "unknown error";
    const unknownModel = /^unknown model (.+)$/i.exec(msg);
    if (unknownModel) {
        return t("status.modelSwitchUnknown", { model: unknownModel[1] });
    }
    const unavailable = /^model (.+) is not available because provider (.+) is not added$/i.exec(msg);
    if (unavailable) {
        return t("status.modelSwitchProviderUnavailable", { model: unavailable[1], provider: unavailable[2] });
    }
    return settingSwitchNoticeText(msg, "model", {
        busy: "status.modelSwitchBusy",
        busyRunning: "status.modelSwitchBusyRunning",
        busyPrompt: "status.modelSwitchBusyPrompt",
        busyJobs: "status.modelSwitchBusyJobs",
        leaseHeld: "status.modelSwitchLeaseHeld",
        starting: "status.modelSwitchStarting",
        startupFailed: "status.modelSwitchStartupFailed",
        retry: "status.modelSwitchRetry",
        failed: "status.modelSwitchFailed",
    });
}
export function tokenModeSwitchNoticeText(err: unknown): string {
    return settingSwitchNoticeText(err, "token mode", {
        busy: "status.tokenModeSwitchBusy",
        busyRunning: "status.tokenModeSwitchBusyRunning",
        busyPrompt: "status.tokenModeSwitchBusyPrompt",
        busyJobs: "status.tokenModeSwitchBusyJobs",
        leaseHeld: "status.tokenModeSwitchLeaseHeld",
        starting: "status.tokenModeSwitchStarting",
        startupFailed: "status.tokenModeSwitchStartupFailed",
        retry: "status.tokenModeSwitchRetry",
        failed: "status.tokenModeSwitchFailed",
    });
}
// noticeCodeKeys maps the backend's stable notice codes (event.NoticeCode*) to
// dictionary keys. Codes survive backend copy edits, unlike the exact-text
// matching in backendNoticeKey, which stays only as the fallback for events
// and replayed histories that carry no code.
const noticeCodeKeys: Record<string, DictKey> = {
    final_readiness: "notice.finalReadiness",
    empty_final: "notice.emptyFinal",
    executor_handoff: "notice.executorHandoff",
    tool_budget: "notice.toolBudget",
    prompt_queued: "notice.promptQueued",
    loop_guard: "notice.loopGuard",
    workspace_lease: "notice.workspaceLease",
    cancelled_turn_display: "notice.cancelledTurnDisplay",
    session_recovery_forked: "recovery.noticeSavedCopy",
    session_recovery_adopted: "recovery.noticeAdopted",
    session_recovery_adopted_covered: "recovery.noticeAdoptedCovered",
    session_recovery_depth_cap: "recovery.noticeKeptCurrent",
    session_shutdown_recovery_forked: "recovery.noticeSavedCopy",
    decision_receipt: "notice.decisionReceiptTitle",
    context_editing_fallback: "notice.contextEditingFallback",
};
// localizedNoticeText localizes a notice's main copy by its stable code first,
// then falls back to English-text matching for codeless payloads.
export function localizedNoticeText(text: string, code?: string): string {
    if (code === "unapplied_steer") {
        const separator = text.indexOf("\n");
        const guidance = separator >= 0 ? text.slice(separator + 1) : text;
        return t("notice.unappliedSteer", { guidance });
    }
    const key = code ? noticeCodeKeys[code] : undefined;
    if (key)
        return t(key);
    return localizedBackendNoticeText(text);
}
const deliveryRequirementKeys: Record<string, DictKey> = {
    project_check: "notice.deliveryRequirementProjectCheck",
    todo: "notice.deliveryRequirementTodo",
    criteria: "notice.deliveryRequirementCriteria",
    verification: "notice.deliveryRequirementVerification",
    review: "notice.deliveryRequirementReview",
    signoff: "notice.deliveryRequirementSignoff",
    action: "notice.deliveryRequirementAction",
    mutation: "notice.deliveryRequirementMutation",
    capability: "notice.deliveryRequirementCapability",
};
export function deliveryReadinessDetail(readiness: WireFinalReadiness | undefined, fallback = ""): string {
    const labels = asArray(readiness?.missing)
        .map((id) => deliveryRequirementKeys[id])
        .filter((key): key is DictKey => Boolean(key))
        .map((key) => t(key));
    if (labels.length === 0)
        return fallback;
    return t("notice.deliveryIncompleteMissing", { items: labels.join(t("notice.deliveryRequirementSeparator")) });
}
export function localizedBackendNoticeText(text: string): string {
    const msg = text.trim();
    const autosave = /^Session autosave failed: (.+)$/s.exec(msg);
    if (autosave) {
        return t("status.sessionAutosaveFailed", { err: autosave[1] });
    }
    const saveBefore = /^Session save failed before (.+?): (.+)$/s.exec(msg);
    if (saveBefore) {
        return t("status.sessionSaveFailedBefore", { action: localizedSessionAction(saveBefore[1]), err: saveBefore[2] });
    }
    const modelFallback = /^model (.+) is no longer available; switched to (.+)$/s.exec(msg);
    if (modelFallback) {
        return t("status.modelFallbackSwitched", { model: modelFallback[1], fallback: modelFallback[2] });
    }
    const backgroundJob = /^background (.+) failed: needs attention$/s.exec(msg);
    if (backgroundJob) {
        return t("notice.backgroundJobFailed", { kind: backgroundJob[1] });
    }
    const canonicalNoticeKey = backendNoticeKey(msg);
    if (canonicalNoticeKey) {
        return t(canonicalNoticeKey);
    }
    if (/^session changed on disk; unsaved local transcript was saved as a conflict copy$/i.test(msg) ||
        /^session changed on disk; unsaved local transcript was saved as recovery branch\b/i.test(msg)) {
        return t("recovery.noticeSavedCopy");
    }
    if (/^repeated save conflicts were detected; saved the current conflict copy in place$/i.test(msg) ||
        /^repeated save conflicts were detected; saved the current conflict copy in an isolated recovery branch$/i.test(msg) ||
        /^session conflicts kept recurring; kept the transcript on the current recovery branch$/i.test(msg)) {
        return t("recovery.noticeKeptCurrent");
    }
    if (/^session changed on disk; adopted the newer transcript \(local changes already covered\)$/i.test(msg)) {
        return t("recovery.noticeAdoptedCovered");
    }
    if (/^session changed on disk; adopted the newer transcript$/i.test(msg)) {
        return t("recovery.noticeAdopted");
    }
    return msg;
}
function backendNoticeKey(msg: string): DictKey | "" {
    switch (msg) {
        case "Task status needs one more check; asking the assistant to finish or explain what is blocking it.":
            return "notice.finalReadiness";
        case "No visible answer was produced; asking the assistant to respond again.":
            return "notice.emptyFinal";
        case "The assistant answered before taking action; asking it to use the required tools.":
            return "notice.executorHandoff";
        case "Tool round limit reached; asking the assistant to summarize progress.":
            return "notice.toolBudget";
        case "The assistant is stuck retrying a blocked action; asking it to change approach.":
            return "notice.loopGuard";
        case "Context is getting large; preserving cache until cleanup is needed.":
            return "notice.contextLarge";
        case "Context cleanup skipped for now.":
            return "notice.contextCleanupSkipped";
        case "Automatic context cleanup paused because the context window is too small.":
            return "notice.contextCleanupPaused";
        case "Context was compacted without a generated summary.":
            return "notice.compactionNoSummary";
        case "Goal is not ready to complete yet; continuing the remaining work.":
            return "notice.goalNotReady";
        case "Goal still has unfinished task state; continuing the remaining work.":
            return "notice.goalUnfinished";
        case "Job artifact migration failed.":
            return "notice.jobArtifactMigrationFailed";
        case "Background job teardown timed out.":
            return "notice.jobTeardownTimeout";
        case "Some plan-mode tool settings were ignored.":
            return "notice.planModeToolSettingsIgnored";
        case "Some plan-mode command settings were ignored.":
            return "notice.planModeCommandSettingsIgnored";
        case "Config migration did not complete.":
            return "notice.configMigrationIncomplete";
        case "Selected model is missing its API key.":
            return "notice.modelMissingApiKey";
        case "An MCP server failed to start.":
            return "notice.mcpServerFailed";
        case "Some MCP servers failed to start; run /mcp for details.":
            return "notice.mcpServersFailed";
        case "Guardian was disabled because its model was not found.":
            return "notice.guardianModelMissing";
        case "Guardian was disabled because it could not start.":
            return "notice.guardianStartFailed";
        default:
            return "";
    }
}
function recoveryNoticeDedupeKey(text: string, code?: string): string {
    switch (code) {
        case "session_recovery_forked":
        case "session_shutdown_recovery_forked":
            return "recovery:saved-copy";
        case "session_recovery_depth_cap":
            return "recovery:kept-current";
        case "session_recovery_adopted_covered":
            return "recovery:adopted-covered";
        case "session_recovery_adopted":
            return "recovery:adopted";
    }
    const msg = text.trim();
    if (/^session changed on disk; unsaved local transcript was saved as a conflict copy$/i.test(msg) ||
        /^session changed on disk; unsaved local transcript was saved as recovery branch\b/i.test(msg) ||
        msg === t("recovery.noticeSavedCopy")) {
        return "recovery:saved-copy";
    }
    if (/^repeated save conflicts were detected; saved the current conflict copy in place$/i.test(msg) ||
        /^repeated save conflicts were detected; saved the current conflict copy in an isolated recovery branch$/i.test(msg) ||
        /^session conflicts kept recurring; kept the transcript on the current recovery branch$/i.test(msg) ||
        msg === t("recovery.noticeKeptCurrent")) {
        return "recovery:kept-current";
    }
    if (/^session changed on disk; adopted the newer transcript \(local changes already covered\)$/i.test(msg) ||
        msg === t("recovery.noticeAdoptedCovered")) {
        return "recovery:adopted-covered";
    }
    if (/^session changed on disk; adopted the newer transcript$/i.test(msg) ||
        msg === t("recovery.noticeAdopted")) {
        return "recovery:adopted";
    }
    return "";
}
export function quietTranscriptNoticeKey(text: string, code?: string): string {
    const recovery = recoveryNoticeDedupeKey(text, code);
    if (recovery)
        return recovery;
    const msg = text.trim();
    if (/^guardian enabled · model=.+$/i.test(msg)) {
        return "startup:guardian-enabled";
    }
    if (/^\d+ MCP server\(s\) failed to start: .+ \u2014 run \/mcp for details$/i.test(msg)) {
        return "startup:mcp-failures";
    }
    const directMCPFailure = /^mcp\s+([A-Za-z0-9._-]+):\s+.+$/i.exec(msg);
    if (directMCPFailure) {
        const name = directMCPFailure[1].toLowerCase();
        if (!["add", "auth", "config", "connect", "import", "mode", "remove"].includes(name)) {
            return "startup:mcp-failure";
        }
    }
    if (/^plugin ".+" has been slow \d+ startups in a row \(last \d+ms, budget \d+ms\); demoting to background startup this session$/i.test(msg)) {
        return "startup:plugin-demote";
    }
    if (/^.+ applied: session refreshed after the lease was released$/i.test(msg)) {
        return "settings:deferred-refresh-applied";
    }
    return "";
}
export function appendNoticeItem(items: Item[], seq: number, id: string, level: "info" | "warn", rawText: string, detail?: string, code?: string, decisionReceipt?: WireDecisionReceipt): {
    items: Item[];
    seq: number;
} {
    if (quietTranscriptNoticeKey(rawText, code)) {
        return { items, seq };
    }
    const text = localizedNoticeText(rawText, code);
    if (quietTranscriptNoticeKey(text, code)) {
        return { items, seq };
    }
    const trimmedDetail = detail?.trim();
    return { items: [...items, { kind: "notice", id, level, text, ...(trimmedDetail ? { detail: trimmedDetail } : {}), ...(decisionReceipt ? { decisionReceipt } : {}) }], seq: seq + 1 };
}
export function appendNoticeToState(s: State, level: "info" | "warn", text: string, detail?: string, code?: string, decisionReceipt?: WireDecisionReceipt): State {
    const next = appendNoticeItem(s.items, s.seq, `n${s.seq}`, level, text, detail, code, decisionReceipt);
    return { ...s, running: s.turnActive ? s.running : false, seq: next.seq, items: next.items };
}
function localizedSessionAction(action: string): string {
    switch (action.trim()) {
        case "changing model":
            return t("status.actionChangingModel");
        case "changing effort":
            return t("status.actionChangingEffort");
        case "changing token mode":
            return t("status.actionChangingTokenMode");
        case "rebuilding settings":
            return t("status.actionRebuildingSettings");
        case "switching sessions":
            return t("status.actionSwitchingSessions");
        case "switching tabs":
            return t("status.actionSwitchingTabs");
        case "autosave":
            return t("status.actionAutosave");
        default:
            return action.trim() || t("status.actionCurrentSession");
    }
}
function settingSwitchNoticeText(err: unknown, setting: "effort" | "model" | "token mode", keys: {
    busy: DictKey;
    busyRunning: DictKey;
    busyPrompt: DictKey;
    busyJobs: DictKey;
    leaseHeld: DictKey;
    starting: DictKey;
    startupFailed: DictKey;
    retry: DictKey;
    failed: DictKey;
}): string {
    const msg = errorMessage(err).trim() || "unknown error";
    const lower = msg.toLowerCase();
    if (lower.includes("finish or cancel") && lower.includes(`before changing ${setting}`)) {
        const detail = /running=(true|false);\s*pending_prompt=(true|false);\s*background_jobs=(\d+)/i.exec(msg);
        if (detail?.[2] === "true")
            return t(keys.busyPrompt);
        if (detail?.[1] === "true")
            return t(keys.busyRunning);
        const jobs = Number(detail?.[3] ?? 0);
        if (jobs > 0)
            return t(keys.busyJobs, { n: jobs });
        return t(keys.busy);
    }
    if (lower.includes("already open in another reasonix window") || lower.includes("session lease held")) {
        return t(keys.leaseHeld);
    }
    if (lower.includes("workspace is still starting")) {
        return t(keys.starting);
    }
    if (lower.startsWith("workspace failed to start")) {
        return t(keys.startupFailed, { err: msg });
    }
    if (lower.includes(`changed while switching ${setting}`) || (lower.includes("tab ") && lower.includes("not found"))) {
        return t(keys.retry);
    }
    return t(keys.failed, { err: msg });
}

