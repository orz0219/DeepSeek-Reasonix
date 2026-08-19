import { asArray } from "./array";
import { applyLiveSegments, type StreamSegment } from "./streamDeltaBatch";
import { fileDiffFromWire, summarizeFileDiff } from "./tools";
import type { BalanceInfo, CheckpointMeta, ContextInfo, EffortInfo, HistoryMessage, HistoryPage, JobView, Meta, WireEvent } from "./types";
import { ToolItem, Item, HydrateReason, MessageActionState, State, LiveStream } from "./controller_state";
import { RuntimeMetaSnapshot, foregroundRunningFromRuntimeMeta, isReadOnlyTool } from "./controller_meta";
import { appendNoticeItem } from "./controller_notice";
import { beginTurnModelActivity } from "./controller_reducer";
const ARCHIVED_TOOL_ARG_LIMIT = 200;
function archivedToolArgs(_name: string, args: string): string {
    return args && args.length > ARCHIVED_TOOL_ARG_LIMIT ? args.slice(0, ARCHIVED_TOOL_ARG_LIMIT) + "…" : args;
}
function isCanonicalTodoTool(tool: ToolItem): boolean {
    return tool.name === "todo_write" && !tool.parentId && tool.status === "done" && !tool.error;
}
function latestCanonicalTodoToolIndex(items: Item[]): number {
    for (let i = items.length - 1; i >= 0; i -= 1) {
        const item = items[i];
        if (item.kind === "tool" && isCanonicalTodoTool(item))
            return i;
    }
    return -1;
}
export function compactArchivedToolItems(items: Item[]): Item[] {
    const canonicalTodoIndex = latestCanonicalTodoToolIndex(items);
    return items.map((item, index) => {
        if (item.kind !== "tool" || item.status === "running")
            return item;
        const preserveArgs = index === canonicalTodoIndex;
        const nextArgs = preserveArgs ? item.args : archivedToolArgs(item.name, item.args);
        if (nextArgs === item.args && item.output === undefined && item.dataArchived === true)
            return item;
        return {
            ...item,
            args: nextArgs,
            output: undefined,
            dataArchived: true,
        };
    });
}
export type Action = {
    type: "event";
    e: WireEvent;
} | {
    type: "stream_batch";
    segments: StreamSegment[];
} | {
    type: "user";
    text: string;
    submitText?: string;
    seq: number;
    submissionId: string;
    deliveryRecovery?: boolean;
} | {
    type: "unsend";
} | {
    type: "send_confirmed";
    submissionId: string;
} | {
    type: "send_failed";
    submissionId: string;
    error: string;
} | {
    type: "backend_status";
    running: boolean;
    pendingPrompt?: boolean;
    backgroundJobs?: number;
    cancelRequested?: boolean;
    cancellable?: boolean;
    snapshotAt?: number;
} | {
    type: "cancel_requested";
} | {
    type: "meta";
    meta: Meta;
} | {
    type: "optimistic_meta";
    meta: Meta;
} | {
    type: "context";
    context: ContextInfo;
} | {
    type: "balance";
    balance: BalanceInfo;
} | {
    type: "effort";
    effort: EffortInfo;
} | {
    type: "jobs";
    jobs: JobView[];
} | {
    type: "checkpoints";
    checkpoints: CheckpointMeta[];
} | {
    type: "hydrate_start";
    reason: HydrateReason;
    placeholderItems?: Item[];
} | {
    type: "hydrate_done";
} | {
    type: "hydrate_error";
    reason: HydrateReason;
    error: string;
} | {
    type: "backend_activation_start";
} | {
    type: "backend_activation_done";
} | {
    type: "message_action_start";
    action: MessageActionState;
} | {
    type: "message_action_done";
} | {
    type: "history";
    messages: HistoryMessage[];
} | {
    type: "history_page";
    page: HistoryPage;
    mode: "replace" | "prepend";
}
// TranscriptStore-driven history actions (windowed HistorySliceForTab flow).
// Items carry stable entryId-derived ids; prepend also lists existing item
// ids superseded by cross-page tool call/result merges.
 | {
    type: "history_replace";
    items: Item[];
    startTurn: number;
    totalTurns: number;
    hasOlder: boolean;
    revision?: number;
    digest?: string;
} | {
    type: "history_prepend";
    items: Item[];
    removeIds: string[];
    startTurn: number;
    totalTurns: number;
    hasOlder: boolean;
    revision?: number;
    digest?: string;
} | {
    type: "history_items_patch";
    patches: Record<string, Item>;
} | {
    type: "history_older_start";
} | {
    type: "history_older_error";
} | {
    type: "local_notice";
    level: "info" | "warn";
    text: string;
} | {
    type: "clearApproval";
} | {
    type: "clearAsk";
} | {
    type: "clearExtensionForm";
} | {
    type: "extension_notifications_drained";
} | {
    type: "approval_drained";
    ids: string[];
    epoch: number;
} | {
    type: "submit_prompt_failed";
    id: string;
    epoch: number;
} | {
    type: "controller_rebuilt";
} | {
    type: "reset";
} | {
    type: "context_panel_refresh";
};
export function backendStatusFromRuntimeMeta(meta: RuntimeMetaSnapshot): Extract<Action, {
    type: "backend_status";
}> {
    const foregroundRunning = foregroundRunningFromRuntimeMeta(meta);
    return {
        type: "backend_status",
        running: foregroundRunning,
        pendingPrompt: Boolean(meta.pendingPrompt),
        backgroundJobs: meta.backgroundJobs ?? 0,
        cancelRequested: Boolean(meta.cancelRequested),
        cancellable: foregroundRunning,
    };
}
// ---- reducer helpers (unchanged logic) ----
export function historyMessagesToItems(messages: HistoryMessage[], idPrefix: string, startSeq = 0): {
    items: Item[];
    seq: number;
} {
    const resultByID = new Map<string, HistoryMessage>();
    for (const m of messages) {
        if (m.role === "tool" && m.toolCallId && !resultByID.has(m.toolCallId)) {
            resultByID.set(m.toolCallId, m);
        }
    }
    const positionalResults = positionalToolResults(messages);
    const consumedPositionalToolIndexes = new Set(Array.from(positionalResults.values(), (result) => result.index));
    let items: Item[] = [];
    let seq = startSeq;
    const consumedToolIDs = new Set<string>();
    for (let messageIndex = 0; messageIndex < messages.length; messageIndex += 1) {
        const m = messages[messageIndex];
        if (m.role === "system")
            continue;
        if (m.role === "phase") {
            if (m.content.trim() !== "") {
                items.push({ kind: "phase", id: `${idPrefix}${seq}`, text: m.content });
                seq++;
            }
            continue;
        }
        if (m.role === "notice") {
            if (m.content.trim() !== "" || m.decisionReceipt) {
                const next = appendNoticeItem(items, seq, `${idPrefix}${seq}`, m.level === "warn" ? "warn" : "info", m.content, m.detail, m.code, m.decisionReceipt);
                items = next.items;
                seq = next.seq;
            }
            continue;
        }
        if (m.role === "compaction") {
            items.push({
                kind: "compaction",
                id: `${idPrefix}${seq}`,
                pending: Boolean(m.pending),
                trigger: m.trigger ?? "",
                messages: m.messages ?? 0,
                summary: m.summary ?? "",
                archive: m.archive ?? "",
            });
            seq++;
            continue;
        }
        if (m.role === "user") {
            if (m.content.trim() === "")
                continue;
            items.push({ kind: "user", id: `${idPrefix}${seq}`, text: m.content, submitText: m.submitText, createdAt: m.createdAt, checkpointTurn: m.checkpointTurn });
            seq++;
            continue;
        }
        if (m.role === "assistant") {
            const hasText = m.content.trim() !== "" || (m.reasoning ?? "").trim() !== "";
            if (hasText) {
                items.push({
                    kind: "assistant",
                    id: `${idPrefix}${seq}`,
                    text: m.content,
                    reasoning: m.reasoning ?? "",
                    streaming: false,
                    workDurationMs: m.workDurationMs,
                });
                seq++;
            }
            const toolCalls = m.toolCalls ?? [];
            for (let callIndex = 0; callIndex < toolCalls.length; callIndex += 1) {
                const tc = toolCalls[callIndex];
                const positionalResult = tc.id ? undefined : positionalResults.get(positionalToolResultKey(messageIndex, callIndex));
                const result = tc.id ? resultByID.get(tc.id) : positionalResult?.message;
                if (tc.id)
                    consumedToolIDs.add(tc.id);
                const archived = Boolean(tc.argumentsArchived || result?.toolResultArchived);
                const output = result?.toolResultArchived ? undefined : result?.content ?? "";
                const error = result?.toolResultError || (output ? historyToolError(output) : undefined);
                const fileDiff = fileDiffFromWire(tc);
                items.push({
                    kind: "tool",
                    id: tc.id || `${idPrefix}tool${seq}`,
                    name: tc.name,
                    args: tc.arguments ?? "",
                    readOnly: typeof tc.resolvedReadOnly === "boolean" ? tc.resolvedReadOnly : isReadOnlyTool(tc.name),
                    resolvedName: tc.resolvedName,
                    capabilityId: tc.capabilityId,
                    status: result ? (error ? "error" : "done") : "stopped",
                    output,
                    error,
                    dataArchived: archived || undefined,
                    subject: tc.subject,
                    summary: summarizeFileDiff(fileDiff) || tc.summary,
                    fileDiff,
                    isShell: tc.name === "bash" || (tc.id || "").startsWith("shell-"),
                    execution: result?.execution,
                });
                seq++;
            }
            continue;
        }
        if (m.role === "tool") {
            if ((m.toolCallId && consumedToolIDs.has(m.toolCallId)) || consumedPositionalToolIndexes.has(messageIndex))
                continue;
            const output = m.toolResultArchived ? undefined : m.content;
            const error = m.toolResultError || (output ? historyToolError(output) : undefined);
            items.push({
                kind: "tool",
                id: m.toolCallId || `${idPrefix}tool${seq}`,
                name: m.toolName || "tool",
                args: "",
                readOnly: isReadOnlyTool(m.toolName || "tool"),
                status: error ? "error" : "done",
                output,
                error,
                dataArchived: m.toolResultArchived || undefined,
                isShell: (m.toolName || "") === "bash" || (m.toolCallId || "").startsWith("shell-"),
                execution: m.execution,
            });
            seq++;
            continue;
        }
    }
    return { items, seq };
}
export function applyTurnCheckpoint(items: Item[], submissionId: string | undefined, turn: number | undefined): Item[] {
    if (!submissionId)
        return items;
    const validTurn = turn !== undefined && Number.isInteger(turn) && turn >= 0;
    let changed = false;
    const next = items.map((item) => {
        if (item.kind !== "user" || item.submissionId !== submissionId)
            return item;
        changed = true;
        return { ...item, submissionId: undefined, checkpointTurn: item.checkpointTurn ?? (validTurn ? turn : undefined) };
    });
    return changed ? next : items;
}
export function historyPageItems(page: HistoryPage): {
    items: Item[];
    seq: number;
} {
    return historyMessagesToItems(asArray(page.messages), `h${page.startTurn}-`, 0);
}
function positionalToolResults(messages: HistoryMessage[]): Map<string, {
    message: HistoryMessage;
    index: number;
}> {
    const out = new Map<string, {
        message: HistoryMessage;
        index: number;
    }>();
    const consumed = new Set<number>();
    for (let messageIndex = 0; messageIndex < messages.length; messageIndex += 1) {
        const message = messages[messageIndex];
        const toolCalls = message.role === "assistant" ? message.toolCalls ?? [] : [];
        if (toolCalls.length === 0)
            continue;
        let resultIndex = messageIndex + 1;
        for (let callIndex = 0; callIndex < toolCalls.length; callIndex += 1) {
            if (toolCalls[callIndex].id)
                continue;
            let matched = false;
            while (resultIndex < messages.length) {
                const candidate = messages[resultIndex];
                if (candidate.role !== "tool")
                    break;
                const candidateIndex = resultIndex;
                resultIndex += 1;
                if (candidate.toolCallId || consumed.has(candidateIndex))
                    continue;
                consumed.add(candidateIndex);
                out.set(positionalToolResultKey(messageIndex, callIndex), { message: candidate, index: candidateIndex });
                matched = true;
                break;
            }
            if (!matched)
                break;
        }
    }
    return out;
}
function positionalToolResultKey(messageIndex: number, callIndex: number): string {
    return `${messageIndex}:${callIndex}`;
}
export function historyToolError(output: string): string | undefined {
    const trimmed = output.trimStart();
    if (trimmed.startsWith("[error") ||
        trimmed.startsWith("Error:") ||
        trimmed.startsWith("error:") ||
        trimmed.startsWith("blocked:")) {
        return output;
    }
    return undefined;
}
export function ensureAssistant(s: State): {
    items: Item[];
    id: string;
    seq: number;
} {
    if (s.currentAssistant) {
        const exists = s.items.some((it) => it.id === s.currentAssistant && it.kind === "assistant");
        if (exists)
            return { items: s.items, id: s.currentAssistant, seq: s.seq };
    }
    const id = `a${s.seq}`;
    const item: Item = { kind: "assistant", id, text: "", reasoning: "", streaming: true };
    return { items: [...s.items, item], id, seq: s.seq + 1 };
}
export function liveReasoningDurationMs(live?: LiveStream): number | undefined {
    if (!live?.reasoningStartedAt || !live.reasoning)
        return undefined;
    const completedAt = live.reasoningCompletedAt;
    if (!completedAt || completedAt < live.reasoningStartedAt)
        return undefined;
    return completedAt - live.reasoningStartedAt;
}
// applyDeltaSegments folds ordered stream segments into the assistant's live
// stream in one state transition. Assumes applyEvent's preamble already ran.
export function applyDeltaSegments(s: State, segments: StreamSegment[]): State {
    const { items, id, seq } = ensureAssistant(s);
    const base = s.live?.id === id ? s.live : { id, text: "", reasoning: "", reasoningComplete: false };
    const now = Date.now();
    const deltaChars = segments.reduce((total, segment) => total + segment.delta.length, 0);
    const next = { ...s, items, live: applyLiveSegments(base, segments, now), currentAssistant: id, seq, turnOutputChars: s.turnOutputChars + deltaChars };
    return deltaChars > 0 ? beginTurnModelActivity(next, now) : next;
}
// applyStreamBatch is the stream_batch action: one frame's deltas, one reducer
// pass, one notification. Mirrors applyEvent's preamble for delta events.
export function applyStreamBatch(s: State, segments: StreamSegment[]): State {
    if (s.discardTurn)
        return s;
    if (s.retry)
        s = { ...s, retry: undefined };
    return applyDeltaSegments(s, segments);
}
/** Closed + open user-wait ms for the active turn (approval/ask). */
export function currentTurnWaitMs(s: Pick<State, "turnWaitAccumMs" | "promptWaitStartedAt">, now = Date.now()): number {
    const closed = Math.max(0, s.turnWaitAccumMs || 0);
    const open = s.promptWaitStartedAt && s.promptWaitStartedAt > 0
        ? Math.max(0, now - s.promptWaitStartedAt)
        : 0;
    return closed + open;
}
export function currentTurnDurationMs(s: Pick<State, "turnStartAt" | "turnWaitAccumMs" | "promptWaitStartedAt">, now = Date.now()): number | undefined {
    if (!Number.isFinite(s.turnStartAt) || s.turnStartAt <= 0 || now < s.turnStartAt)
        return undefined;
    return Math.max(1, now - s.turnStartAt - currentTurnWaitMs(s, now));
}

