// transcriptStore is the per-session record store behind the transcript view
// (Phase C of the session-switch/history refactor). It replaces the old
// "convert a whole HistoryPage on every call" flow with windowed paging over
// HistorySliceForTab:
//
//   - Records keyed by stable backend entryId, kept sorted by (order, entryId)
//     and merged a page at a time (replace / prepend / append) — no full
//     re-sort on each op; pages are contiguous suffixes/prefixes.
//   - Item projection derives Item ids from entryIds, so ids stay stable
//     across page merges (the old h<startTurn>-<seq> scheme renumbered every
//     item on prepend). Cross-page tool call/result pairs merge exactly like
//     the single-shot historyMessagesToItems conversion: a result row that
//     paged in before its call converts standalone first and is folded into
//     the call's tool item (same item id: the toolCallId) when the call's
//     page arrives.
//   - Weighted LRU: at most maxResidentSessions sessions keep records
//     resident; history body bytes and the parsed-markdown cache each have a
//     byte budget. Sessions whose tab is active, running, or mid-turn are
//     pinned out of eviction. Eviction only releases memory — records are
//     re-fetchable from the backend via HistorySliceForTab.
//   - Generation binding: every in-flight slice/content request carries the
//     session generation it started under. Switching away, evicting, or
//     starting a newer load bumps the generation; late responses are
//     discarded (Wails calls are not abortable).
//   - Lazy content: entries carrying refs[] keep preview text inline;
//     requestFullContent fetches and assembles HistoryContentForTab chunks on
//     demand (and automatically for refs in the newest page). A stale chunk
//     marks the ref stale and keeps the preview.
//
// Rendering consumes the store through TranscriptProjection (items + paging
// state); useController dispatches projections into per-tab reducer state.
import { asArray } from "./array";
import { fileDiffFromWire, summarizeFileDiff } from "./tools";
import { historyToolError, isReadOnlyTool, localizedNoticeText, quietTranscriptNoticeKey, type Item } from "./useController";
import type { HistoryContentChunk, HistoryContentRef, HistoryEntry, HistoryMessage, HistorySlice, HistorySliceRequest } from "./types";
export interface TranscriptBackend {
    HistorySliceForTab(tabID: string, req: HistorySliceRequest): Promise<HistorySlice>;
    HistoryContentForTab(tabID: string, ref: HistoryContentRef, chunkIndex: number): Promise<HistoryContentChunk>;
}
export interface TranscriptStoreOptions {
    /** Resident sessions with records (unpinned). Default 3. */
    maxResidentSessions?: number;
    /** Total inline history body bytes across resident sessions. Default 32MiB. */
    historyBodyBudgetBytes?: number;
    /** Parsed-markdown cache budget. Default 16MiB. */
    markdownBudgetBytes?: number;
}
export interface TranscriptProjection {
    items: Item[];
    startTurn: number;
    endTurn: number;
    totalTurns: number;
    hasOlder: boolean;
    revision: number;
    revisionKnown: boolean;
    digest: string;
}
export interface LoadOlderResult extends TranscriptProjection {
    /** "prepend": page older items; "reload": cursor went stale, full latest replace. */
    kind: "prepend" | "reload";
    /** Items contributed by the older page (kind === "prepend"). */
    prependItems: Item[];
    /** Existing item ids superseded by cross-page tool merges (kind === "prepend"). */
    removeIds: string[];
    /** Backend re-bound the cursor to the current identity of an append-only
     * session: identity checks may adopt this page instead of discarding it. */
    appendOnly?: boolean;
}
export interface TranscriptContentChange {
    tabId: string;
    /** Re-converted items keyed by their stable item id. */
    patches: Record<string, Item>;
}
export interface TranscriptRecord {
    entryId: string;
    turn: number;
    order: number;
    message: HistoryMessage;
    refs: HistoryContentRef[];
    /** field -> full content once fetched; also applied into message. */
    resolved?: Record<string, string>;
    /** field -> true when the backend reported the ref stale; preview is kept. */
    staleRefs?: Record<string, true>;
    bytes: number;
}
export interface RecordConversion {
    items: Item[];
    /** Result records this record's tool calls consumed (entryIds). */
    claims: string[];
    /** ID-based calls converted without a result (toolCallIds). */
    unresolvedIds: string[];
    /** Positional (id-less) call indexes still unmatched. */
    pendingPositional: number[];
    /** callIndex -> result record entryId for matched calls (re-conversion input). */
    matches: Map<number, string>;
}
export interface SessionTranscript {
    key: string;
    tabId: string;
    sessionPath: string;
    records: TranscriptRecord[];
    byId: Map<string, TranscriptRecord>;
    /** toolCallId -> result record entryId (first record wins, like resultByID). */
    toolResultOwners: Map<string, string>;
    /** entryId -> projected items of that record ([] when consumed). */
    contributions: Map<string, Item[]>;
    /** Result record entryIds folded into a call's tool item. */
    consumed: Set<string>;
    /** result entryId -> claimer (assistant) entryId. */
    consumedBy: Map<string, string>;
    /** toolCallId -> assistant record entryId whose call still lacks a result. */
    unresolvedCalls: Map<string, string>;
    /** assistant entryId -> unmatched positional call indexes. */
    pendingPositional: Map<string, number[]>;
    /** assistant entryId -> callIndex -> result entryId (for re-conversion). */
    matchTables: Map<string, Map<number, string>>;
    itemsCache: Item[] | null;
    nextCursor: string;
    hasOlder: boolean;
    totalTurns: number;
    startTurn: number;
    endTurn: number;
    revision: number;
    revisionKnown: boolean;
    digest: string;
    generation: number;
    bodyBytes: number;
    olderInFlight: boolean;
    pendingContent: Map<string, {
        generation: number;
        promise: Promise<string | undefined>;
    }>;
}
export const DEFAULT_MAX_RESIDENT_SESSIONS = 3;
export const DEFAULT_HISTORY_BODY_BUDGET = 32 << 20;
export const DEFAULT_MARKDOWN_BUDGET = 16 << 20;
export function sessionKeyFor(tabId: string, sessionPath: string): string {
    return `${tabId}\n${sessionPath}`;
}
export function sliceRevisionKnown(slice: Pick<HistorySlice, "revision" | "revisionKnown">): boolean {
    // Compatibility with the first HistorySlice contract: positive revisions
    // were already canonical, but revisionKnown was not exposed yet.
    return slice.revisionKnown ?? (slice.revision ?? 0) > 0;
}
export function compareRecords(a: Pick<TranscriptRecord, "order" | "entryId">, b: Pick<TranscriptRecord, "order" | "entryId">): number {
    if (a.order !== b.order)
        return a.order - b.order;
    return a.entryId < b.entryId ? -1 : a.entryId > b.entryId ? 1 : 0;
}
// recordBytes approximates the retained UTF-16 size of a record's inline text
// (the same fields the Go side counts for its slice byte budget).
export function recordBytes(m: HistoryMessage): number {
    let chars = (m.content?.length ?? 0) +
        (m.reasoning?.length ?? 0) +
        (m.submitText?.length ?? 0) +
        (m.detail?.length ?? 0) +
        (m.code?.length ?? 0) +
        (m.summary?.length ?? 0) +
        (m.archive?.length ?? 0) +
        (m.toolResultError?.length ?? 0) +
        (m.toolCallId?.length ?? 0) +
        (m.toolName?.length ?? 0) +
        (m.role?.length ?? 0);
    for (const tc of m.toolCalls ?? []) {
        chars +=
            (tc.arguments?.length ?? 0) +
                (tc.subject?.length ?? 0) +
                (tc.summary?.length ?? 0) +
                (tc.diff?.length ?? 0) +
                (tc.id?.length ?? 0) +
                (tc.name?.length ?? 0);
    }
    return chars * 2;
}
export function entryToRecord(entry: HistoryEntry): TranscriptRecord {
    return {
        entryId: entry.entryId,
        turn: entry.turn,
        order: entry.order,
        message: entry.message,
        refs: asArray<HistoryContentRef>(entry.refs),
        bytes: recordBytes(entry.message),
    };
}
// itemIdForToolCall mirrors the single-shot conversion: id-addressed tool
// items take the toolCallId whether they come from the call or from a
// standalone result row, so a late-merging pair keeps one stable item id.
function itemIdForToolCall(tcId: string, fallback: string): string {
    return tcId || fallback;
}
// Convert one record into its items. Mirrors historyMessagesToItems per role,
// with tool results resolved through the session-wide view instead of a
// page-local map. priorMatches/priorClaims carry a re-conversion's earlier
// positional assignments so they reproduce exactly.
export function convertRecord(rec: TranscriptRecord, view: {
    records: TranscriptRecord[];
    indexOf: Map<string, number>;
    toolResultOwners: Map<string, string>;
}, consumed: Set<string>, priorMatches?: Map<number, string>): RecordConversion {
    const items: Item[] = [];
    const claims: string[] = [];
    const unresolvedIds: string[] = [];
    const pendingPositional: number[] = [];
    const matches = new Map<number, string>(priorMatches);
    const m = rec.message;
    const id = `he:${rec.entryId}`;
    if (m.role === "system")
        return { items, claims, unresolvedIds, pendingPositional, matches };
    if (m.role === "phase") {
        if (m.content.trim() !== "")
            items.push({ kind: "phase", id, text: m.content });
        return { items, claims, unresolvedIds, pendingPositional, matches };
    }
    if (m.role === "notice") {
        if (m.content.trim() !== "" || m.decisionReceipt) {
            if (!quietTranscriptNoticeKey(m.content, m.code)) {
                const text = localizedNoticeText(m.content, m.code);
                if (!quietTranscriptNoticeKey(text, m.code)) {
                    const trimmedDetail = m.detail?.trim();
                    items.push({
                        kind: "notice",
                        id,
                        level: m.level === "warn" ? "warn" : "info",
                        text,
                        ...(trimmedDetail ? { detail: trimmedDetail } : {}),
                        ...(m.decisionReceipt ? { decisionReceipt: m.decisionReceipt } : {}),
                    });
                }
            }
        }
        return { items, claims, unresolvedIds, pendingPositional, matches };
    }
    if (m.role === "compaction") {
        items.push({
            kind: "compaction",
            id,
            pending: Boolean(m.pending),
            trigger: m.trigger ?? "",
            messages: m.messages ?? 0,
            summary: m.summary ?? "",
            archive: m.archive ?? "",
        });
        return { items, claims, unresolvedIds, pendingPositional, matches };
    }
    if (m.role === "user") {
        if (m.content.trim() !== "") {
            items.push({ kind: "user", id, text: m.content, submitText: m.submitText, createdAt: m.createdAt, checkpointTurn: m.checkpointTurn });
        }
        return { items, claims, unresolvedIds, pendingPositional, matches };
    }
    if (m.role === "assistant") {
        const hasText = m.content.trim() !== "" || (m.reasoning ?? "").trim() !== "";
        if (hasText) {
            items.push({
                kind: "assistant",
                id,
                text: m.content,
                reasoning: m.reasoning ?? "",
                streaming: false,
                workDurationMs: m.workDurationMs,
            });
        }
        const toolCalls = m.toolCalls ?? [];
        // Positional scan cursor: id-less calls consume the following unconsumed
        // id-less tool rows in order, stopping at the first non-tool record —
        // the same run positionalToolResults walks in the single-shot pass.
        let scan = (view.indexOf.get(rec.entryId) ?? -1) + 1;
        for (let callIndex = 0; callIndex < toolCalls.length; callIndex += 1) {
            const tc = toolCalls[callIndex];
            let result: HistoryMessage | undefined;
            let resultEntryId: string | undefined;
            const prior = matches.get(callIndex);
            if (prior) {
                resultEntryId = prior;
                result = view.records[view.indexOf.get(prior) ?? -1]?.message;
            }
            else if (tc.id) {
                const owner = view.toolResultOwners.get(tc.id);
                if (owner) {
                    resultEntryId = owner;
                    result = view.records[view.indexOf.get(owner) ?? -1]?.message;
                }
                else {
                    unresolvedIds.push(tc.id);
                }
            }
            else {
                while (scan < view.records.length) {
                    const candidate = view.records[scan];
                    if (candidate.message.role !== "tool")
                        break;
                    scan += 1;
                    if (candidate.message.toolCallId || consumed.has(candidate.entryId))
                        continue;
                    resultEntryId = candidate.entryId;
                    result = candidate.message;
                    break;
                }
                if (!resultEntryId)
                    pendingPositional.push(callIndex);
            }
            if (resultEntryId) {
                matches.set(callIndex, resultEntryId);
                claims.push(resultEntryId);
                consumed.add(resultEntryId);
            }
            const archived = Boolean(tc.argumentsArchived || result?.toolResultArchived);
            const output = result?.toolResultArchived ? undefined : result?.content ?? "";
            const error = result?.toolResultError || (output ? historyToolError(output) : undefined);
            const fileDiff = fileDiffFromWire(tc);
            items.push({
                kind: "tool",
                id: itemIdForToolCall(tc.id, `he:${rec.entryId}:tc${callIndex}`),
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
        }
        return { items, claims, unresolvedIds, pendingPositional, matches };
    }
    if (m.role === "tool") {
        if (consumed.has(rec.entryId))
            return { items, claims, unresolvedIds, pendingPositional, matches };
        const output = m.toolResultArchived ? undefined : m.content;
        const error = m.toolResultError || (output ? historyToolError(output) : undefined);
        items.push({
            kind: "tool",
            id: itemIdForToolCall(m.toolCallId ?? "", id),
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
        return { items, claims, unresolvedIds, pendingPositional, matches };
    }
    return { items, claims, unresolvedIds, pendingPositional, matches };
}
export function applyResolvedField(rec: TranscriptRecord, ref: HistoryContentRef, data: string): boolean {
    const m = rec.message;
    switch (ref.field) {
        case "content":
            rec.message = { ...m, content: data };
            return true;
        case "reasoning":
            rec.message = { ...m, reasoning: data };
            return true;
        case "submitText":
            rec.message = { ...m, submitText: data };
            return true;
        case "detail":
            rec.message = { ...m, detail: data };
            return true;
        case "code":
            rec.message = { ...m, code: data };
            return true;
        case "summary":
            rec.message = { ...m, summary: data };
            return true;
        case "archive":
            rec.message = { ...m, archive: data };
            return true;
        case "toolResultError":
            rec.message = { ...m, toolResultError: data };
            return true;
        case "toolArguments":
        case "toolSubject":
        case "toolSummary":
        case "toolDiff": {
            const toolCalls = (m.toolCalls ?? []).map((tc) => {
                if (tc.id !== ref.toolCallId)
                    return tc;
                if (ref.field === "toolArguments")
                    return { ...tc, arguments: data };
                if (ref.field === "toolSubject")
                    return { ...tc, subject: data };
                if (ref.field === "toolSummary")
                    return { ...tc, summary: data };
                return { ...tc, diff: data };
            });
            rec.message = { ...m, toolCalls };
            return true;
        }
        default:
            return false;
    }
}

