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
import { app } from "./bridge";
import { noteHistoryPage, registerTranscriptCacheDiagnostics } from "./sessionDiagnostics";
import { TranscriptMarkdownCache, type ParsedMarkdownValue } from "./transcriptMarkdownCache";
import { type Item } from "./useController";
import type { HistoryEntry, HistorySlice, HistorySliceRequest } from "./types";
import { TranscriptBackend, TranscriptStoreOptions, TranscriptProjection, LoadOlderResult, TranscriptContentChange, TranscriptRecord, RecordConversion, SessionTranscript, DEFAULT_MAX_RESIDENT_SESSIONS, DEFAULT_HISTORY_BODY_BUDGET, DEFAULT_MARKDOWN_BUDGET, sessionKeyFor, sliceRevisionKnown, compareRecords, recordBytes, entryToRecord, convertRecord, applyResolvedField } from "./transcript_types";
export type { ParsedMarkdownValue } from "./transcriptMarkdownCache";
// Diagnostics provider: cache weights flow to the crash/perf context without
// crash.ts importing this module's bridge-backed graph.
registerTranscriptCacheDiagnostics(() => singleton?.stats() ?? {
    residentSessions: 0,
    maxResidentSessions: DEFAULT_MAX_RESIDENT_SESSIONS,
    bodyBytes: 0,
    bodyBudgetBytes: DEFAULT_HISTORY_BODY_BUDGET,
    markdownBytes: 0,
    markdownBudgetBytes: DEFAULT_MARKDOWN_BUDGET,
    historyEvictions: 0,
    markdownEvictions: 0,
});
export class TranscriptStore {
    private readonly backend: TranscriptBackend;
    private readonly maxResidentSessions: number;
    private readonly historyBodyBudgetBytes: number;
    /** Insertion-ordered (oldest first); touch re-inserts at the end. */
    private readonly sessions = new Map<string, SessionTranscript>();
    private readonly tabPins = new Map<string, {
        live: boolean;
        active: boolean;
    }>();
    private readonly listeners = new Map<string, Set<(change: TranscriptContentChange) => void>>();
    private readonly markdown: TranscriptMarkdownCache;
    private historyEvictions = 0;
    constructor(backend: TranscriptBackend, options: TranscriptStoreOptions = {}) {
        this.backend = backend;
        this.maxResidentSessions = Math.max(1, options.maxResidentSessions ?? DEFAULT_MAX_RESIDENT_SESSIONS);
        this.historyBodyBudgetBytes = Math.max(0, options.historyBodyBudgetBytes ?? DEFAULT_HISTORY_BODY_BUDGET);
        this.markdown = new TranscriptMarkdownCache(Math.max(0, options.markdownBudgetBytes ?? DEFAULT_MARKDOWN_BUDGET));
    }
    // ── session identity / LRU ────────────────────────────────────────────────
    private newSession(key: string, tabId: string, sessionPath: string): SessionTranscript {
        return {
            key,
            tabId,
            sessionPath,
            records: [],
            byId: new Map(),
            toolResultOwners: new Map(),
            contributions: new Map(),
            consumed: new Set(),
            consumedBy: new Map(),
            unresolvedCalls: new Map(),
            pendingPositional: new Map(),
            matchTables: new Map(),
            itemsCache: null,
            nextCursor: "",
            hasOlder: false,
            totalTurns: 0,
            startTurn: 0,
            endTurn: 0,
            revision: 0,
            revisionKnown: false,
            digest: "",
            generation: 0,
            bodyBytes: 0,
            olderInFlight: false,
            pendingContent: new Map(),
        };
    }
    private touch(session: SessionTranscript): void {
        this.sessions.delete(session.key);
        this.sessions.set(session.key, session);
    }
    private isPinned(session: SessionTranscript): boolean {
        const pins = this.tabPins.get(session.tabId);
        return Boolean(pins?.live || pins?.active);
    }
    /** Pin/unpin a tab with live or in-flight turn state out of the LRU. */
    setPinned(tabId: string, pinned: boolean): void {
        const pins = this.tabPins.get(tabId) ?? { live: false, active: false };
        if (pins.live === pinned)
            return;
        this.tabPins.set(tabId, { ...pins, live: pinned });
    }
    /**
     * The visible tab changed: pin the new active tab out of eviction and drop
     * the previous tab's active pin. Generations are NOT bumped here — a
     * background tab's in-flight load still completes into its own state (and
     * the store); generations move on session switch (fresh loadLatest), evict,
     * and unload only.
     */
    noteActiveTab(tabId: string | undefined, previousTabId?: string): void {
        if (previousTabId && previousTabId !== tabId) {
            const pins = this.tabPins.get(previousTabId) ?? { live: false, active: false };
            if (pins.active)
                this.tabPins.set(previousTabId, { ...pins, active: false });
        }
        if (tabId) {
            const pins = this.tabPins.get(tabId) ?? { live: false, active: false };
            if (!pins.active)
                this.tabPins.set(tabId, { ...pins, active: true });
        }
    }
    /** Drop all sessions of a tab (pruned surface, closed tab). */
    evictTab(tabId: string): void {
        for (const [key, session] of Array.from(this.sessions.entries())) {
            if (session.tabId === tabId)
                this.sessions.delete(key);
        }
        this.tabPins.delete(tabId);
    }
    private evictSession(session: SessionTranscript): void {
        session.generation += 1; // in-flight responses discard against a missing/stale session
        this.sessions.delete(session.key);
        this.historyEvictions += 1;
    }
    private enforceBudgets(): void {
        const evictable = (): SessionTranscript[] => Array.from(this.sessions.values()).filter((s) => s.records.length > 0 && !this.isPinned(s));
        let candidates = evictable();
        let resident = candidates.length;
        while (resident > this.maxResidentSessions && candidates.length > 0) {
            const victim = candidates.shift();
            if (!victim)
                break;
            this.evictSession(victim);
            resident -= 1;
        }
        let total = 0;
        for (const session of this.sessions.values())
            total += session.bodyBytes;
        candidates = evictable();
        while (total > this.historyBodyBudgetBytes && candidates.length > 0) {
            const victim = candidates.shift();
            if (!victim)
                break;
            total -= victim.bodyBytes;
            this.evictSession(victim);
        }
    }
    // ── projection ────────────────────────────────────────────────────────────
    private rebuildProjection(session: SessionTranscript): Item[] {
        const items: Item[] = [];
        for (const rec of session.records) {
            const contribution = session.contributions.get(rec.entryId);
            if (contribution)
                items.push(...contribution);
        }
        session.itemsCache = items;
        return items;
    }
    private projectionOf(session: SessionTranscript): TranscriptProjection {
        return {
            items: session.itemsCache ?? this.rebuildProjection(session),
            startTurn: session.startTurn,
            endTurn: session.endTurn,
            totalTurns: session.totalTurns,
            hasOlder: session.hasOlder,
            revision: session.revision,
            revisionKnown: session.revisionKnown,
            digest: session.digest,
        };
    }
    /** Synchronous projection for an LRU-resident session; undefined on a miss. */
    peek(tabId: string, sessionPath: string): TranscriptProjection | undefined {
        const session = this.sessions.get(sessionKeyFor(tabId, sessionPath));
        if (!session || session.records.length === 0)
            return undefined;
        this.touch(session);
        return this.projectionOf(session);
    }
    /** Test/diagnostic introspection. */
    isResident(tabId: string, sessionPath: string): boolean {
        const session = this.sessions.get(sessionKeyFor(tabId, sessionPath));
        return Boolean(session && session.records.length > 0);
    }
    residentSessionCount(): number {
        let count = 0;
        for (const session of this.sessions.values())
            if (session.records.length > 0)
                count += 1;
        return count;
    }
    totalBodyBytes(): number {
        let total = 0;
        for (const session of this.sessions.values())
            total += session.bodyBytes;
        return total;
    }
    /** Cache-weight snapshot for diagnostics (sessionDiagnostics/crash context). */
    stats() {
        return {
            residentSessions: this.residentSessionCount(),
            maxResidentSessions: this.maxResidentSessions,
            bodyBytes: this.totalBodyBytes(),
            bodyBudgetBytes: this.historyBodyBudgetBytes,
            markdownBytes: this.markdown.bytes,
            markdownBudgetBytes: this.markdown.budgetBytes,
            historyEvictions: this.historyEvictions,
            markdownEvictions: this.markdown.evictions,
        };
    }
    // fetchSlice times one backend page request and records the content-free
    // page stats (entries, inline bytes, duration, stale, read-path source).
    private async fetchSlice(tabId: string, req: HistorySliceRequest): Promise<HistorySlice> {
        const startedAt = typeof performance !== "undefined" ? performance.now() : Date.now();
        const slice = await this.backend.HistorySliceForTab(tabId, req);
        const endedAt = typeof performance !== "undefined" ? performance.now() : Date.now();
        if (slice.error?.trim())
            throw new Error(slice.error.trim());
        const entries = asArray<HistoryEntry>(slice.entries);
        let inlineBytes = 0;
        for (const entry of entries)
            inlineBytes += recordBytes(entry.message);
        noteHistoryPage({
            entries: entries.length,
            inlineBytes,
            durationMs: Math.max(0, endedAt - startedAt),
            stale: Boolean(slice.stale),
            source: slice.source ?? "",
        });
        return slice;
    }
    generationOf(tabId: string, sessionPath: string): number | undefined {
        return this.sessions.get(sessionKeyFor(tabId, sessionPath))?.generation;
    }
    // ── record merge ops ──────────────────────────────────────────────────────
    private viewOf(records: TranscriptRecord[]): {
        records: TranscriptRecord[];
        indexOf: Map<string, number>;
        toolResultOwners: Map<string, string>;
    } {
        const indexOf = new Map<string, number>();
        const toolResultOwners = new Map<string, string>();
        records.forEach((rec, index) => {
            indexOf.set(rec.entryId, index);
            const toolCallId = rec.message.role === "tool" ? rec.message.toolCallId : undefined;
            if (toolCallId && !toolResultOwners.has(toolCallId))
                toolResultOwners.set(toolCallId, rec.entryId);
        });
        return { records, indexOf, toolResultOwners };
    }
    private trackConversion(session: SessionTranscript, rec: TranscriptRecord, conversion: RecordConversion): void {
        session.contributions.set(rec.entryId, conversion.items);
        session.matchTables.set(rec.entryId, conversion.matches);
        for (const claimed of conversion.claims)
            session.consumedBy.set(claimed, rec.entryId);
        for (const toolCallId of conversion.unresolvedIds)
            session.unresolvedCalls.set(toolCallId, rec.entryId);
        if (conversion.pendingPositional.length > 0)
            session.pendingPositional.set(rec.entryId, conversion.pendingPositional);
        else
            session.pendingPositional.delete(rec.entryId);
    }
    private replaceRecords(session: SessionTranscript, entries: HistoryEntry[]): void {
        const records = entries.map(entryToRecord);
        const view = this.viewOf(records);
        const consumed = new Set<string>();
        session.records = records;
        session.byId = new Map(records.map((rec) => [rec.entryId, rec]));
        session.toolResultOwners = view.toolResultOwners;
        session.contributions = new Map();
        session.consumed = consumed;
        session.consumedBy = new Map();
        session.unresolvedCalls = new Map();
        session.pendingPositional = new Map();
        session.matchTables = new Map();
        session.bodyBytes = 0;
        for (const rec of records) {
            session.bodyBytes += rec.bytes;
            const conversion = convertRecord(rec, view, consumed);
            this.trackConversion(session, rec, conversion);
        }
        session.itemsCache = null;
        this.rebuildProjection(session);
    }
    /**
     * Prepend an older page. Returns the page's contributed items plus the ids
     * of existing standalone tool items now folded into a call from this page.
     */
    private prependRecords(session: SessionTranscript, entries: HistoryEntry[]): {
        items: Item[];
        removeIds: string[];
    } {
        const fresh: TranscriptRecord[] = [];
        for (const entry of entries) {
            if (session.byId.has(entry.entryId))
                continue; // contract guard: never duplicate
            fresh.push(entryToRecord(entry));
        }
        if (fresh.length === 0)
            return { items: [], removeIds: [] };
        const combined = [...fresh, ...session.records];
        if (session.records.length > 0 && compareRecords(fresh[fresh.length - 1], session.records[0]) > 0) {
            // Backend contract violation (pages must be contiguous prefixes): fall
            // back to one full sort rather than corrupting the order.
            combined.sort(compareRecords);
        }
        const view = this.viewOf(combined);
        const consumed = new Set(session.consumed);
        const removeIds: string[] = [];
        const prependItems: Item[] = [];
        for (const rec of fresh) {
            const before = new Set(consumed);
            const conversion = convertRecord(rec, view, consumed);
            for (const claimed of conversion.claims) {
                if (before.has(claimed))
                    continue;
                const existing = session.contributions.get(claimed);
                if (existing && existing.length > 0) {
                    // An existing standalone tool row is now folded into this call.
                    for (const item of existing)
                        removeIds.push(item.id);
                    session.contributions.set(claimed, []);
                }
            }
            this.trackConversion(session, rec, conversion);
            prependItems.push(...conversion.items);
            session.bodyBytes += rec.bytes;
        }
        session.records = combined;
        session.byId = new Map(combined.map((rec) => [rec.entryId, rec]));
        session.toolResultOwners = view.toolResultOwners;
        session.consumed = consumed;
        const removeSet = new Set(removeIds);
        const base = session.itemsCache ?? this.rebuildProjection(session);
        session.itemsCache = removeSet.size > 0 ? [...prependItems, ...base.filter((item) => !removeSet.has(item.id))] : [...prependItems, ...base];
        return { items: prependItems, removeIds };
    }
    /**
     * Append newer entries (live tail / fresh suffix). Results arriving for
     * calls that paged in unresolved fold into the call's tool item. Returns the
     * appended records' contributed items. Rendering/virtual-list phases can use
     * this to stream new rows into a resident session without a full reload.
     */
    appendEntries(tabId: string, sessionPath: string, entries: HistoryEntry[]): Item[] {
        const session = this.sessions.get(sessionKeyFor(tabId, sessionPath));
        if (!session || session.records.length === 0)
            return [];
        const items = this.appendRecords(session, entries);
        this.enforceBudgets();
        return items;
    }
    /** Append newer entries (live tail / fresh suffix). */
    private appendRecords(session: SessionTranscript, entries: HistoryEntry[]): Item[] {
        const fresh: TranscriptRecord[] = [];
        for (const entry of entries) {
            if (session.byId.has(entry.entryId))
                continue;
            fresh.push(entryToRecord(entry));
        }
        if (fresh.length === 0)
            return [];
        const combined = [...session.records, ...fresh];
        if (session.records.length > 0 && compareRecords(session.records[session.records.length - 1], fresh[0]) > 0) {
            combined.sort(compareRecords);
        }
        const view = this.viewOf(combined);
        const consumed = new Set(session.consumed);
        const appendedItems: Item[] = [];
        let dirty = false;
        session.records = combined;
        session.byId = new Map(combined.map((rec) => [rec.entryId, rec]));
        session.toolResultOwners = view.toolResultOwners;
        // Resolve existing calls whose results only arrive now (a page cut between
        // a call and its result, or a live tail landing after the call).
        for (const rec of fresh) {
            const toolCallId = rec.message.role === "tool" ? rec.message.toolCallId : undefined;
            if (!toolCallId)
                continue;
            const owner = session.unresolvedCalls.get(toolCallId);
            if (!owner)
                continue;
            const ownerRec = session.byId.get(owner);
            if (!ownerRec)
                continue;
            session.unresolvedCalls.delete(toolCallId);
            const reconverted = convertRecord(ownerRec, view, consumed, session.matchTables.get(owner));
            this.trackConversion(session, ownerRec, reconverted);
            dirty = true;
        }
        for (const rec of fresh) {
            const conversion = convertRecord(rec, view, consumed);
            this.trackConversion(session, rec, conversion);
            appendedItems.push(...conversion.items);
            session.bodyBytes += rec.bytes;
        }
        session.consumed = consumed;
        if (dirty || session.itemsCache === null) {
            this.rebuildProjection(session);
        }
        else {
            session.itemsCache = [...session.itemsCache, ...appendedItems];
        }
        return appendedItems;
    }
    // ── paging API ────────────────────────────────────────────────────────────
    /**
     * Load the newest page. preferResident serves an LRU-resident session
     * synchronously-equivalent projection without a backend round trip.
     * Returns undefined when the load was superseded/evicted mid-flight.
     */
    async loadLatest(tabId: string, sessionPath: string, options: {
        turns?: number;
        preferResident?: boolean;
        expectedRevision?: number;
        expectedDigest?: string;
    } = {}): Promise<TranscriptProjection | undefined> {
        const key = sessionKeyFor(tabId, sessionPath);
        const existing = this.sessions.get(key);
        if (options.preferResident && existing && existing.records.length > 0 &&
            this.matchesExpectedFingerprint(existing, options.expectedRevision, options.expectedDigest)) {
            this.touch(existing);
            return this.projectionOf(existing);
        }
        const session = existing ?? this.newSession(key, tabId, sessionPath);
        session.tabId = tabId;
        session.sessionPath = sessionPath;
        // A fresh load supersedes every in-flight request of the previous load.
        session.generation += 1;
        const generation = session.generation;
        this.sessions.set(key, session);
        this.touch(session);
        const turns = options.turns;
        let slice = await this.fetchSlice(tabId, { cursor: "", turns });
        if (this.sessions.get(key) !== session || session.generation !== generation)
            return undefined;
        if (slice.stale) {
            // cursor "" cannot bind a stale identity, but a concurrent rewrite may
            // still report one — retry once against the settled revision.
            slice = await this.fetchSlice(tabId, { cursor: "", turns });
            if (this.sessions.get(key) !== session || session.generation !== generation)
                return undefined;
        }
        this.replaceRecords(session, asArray<HistoryEntry>(slice.entries));
        session.nextCursor = slice.nextCursor ?? "";
        session.hasOlder = Boolean(slice.hasOlder);
        session.totalTurns = slice.totalTurns ?? 0;
        session.startTurn = slice.startTurn ?? 0;
        session.endTurn = slice.endTurn ?? 0;
        session.revision = slice.revision ?? 0;
        session.revisionKnown = sliceRevisionKnown(slice);
        session.digest = slice.digest ?? "";
        this.autoFetchRefs(session);
        this.enforceBudgets();
        if (this.sessions.get(key) !== session)
            return undefined; // evicted by the budget
        return this.projectionOf(session);
    }
    /**
     * Page toward older history. On a stale cursor the records are dropped and
     * the latest page is reloaded (kind "reload": callers replace, not prepend).
     */
    async loadOlder(tabId: string, sessionPath: string, options: {
        turns?: number;
    } = {}): Promise<LoadOlderResult | undefined> {
        const key = sessionKeyFor(tabId, sessionPath);
        const session = this.sessions.get(key);
        if (!session || session.records.length === 0) {
            // Evicted or never loaded: re-prime from the newest page; callers must
            // replace rather than prepend.
            const projection = await this.loadLatest(tabId, sessionPath, options);
            return projection ? { ...projection, kind: "reload", prependItems: [], removeIds: [] } : undefined;
        }
        if (!session.hasOlder || !session.nextCursor || session.olderInFlight)
            return undefined;
        session.olderInFlight = true;
        const generation = session.generation;
        try {
            const slice = await this.fetchSlice(tabId, { cursor: session.nextCursor, turns: options.turns });
            if (this.sessions.get(key) !== session || session.generation !== generation)
                return undefined;
            if (slice.stale) {
                const projection = await this.loadLatest(tabId, sessionPath, options);
                return projection ? { ...projection, kind: "reload", prependItems: [], removeIds: [] } : undefined;
            }
            if (!this.sameFingerprint(session, slice)) {
                if (slice.appendOnly) {
                    // The backend re-bound the cursor to the current identity of
                    // an append-only live session; the message prefix is intact,
                    // so adopt the new identity and prepend instead of reloading
                    // the same newest page (which would hide the earlier turns
                    // again).
                    session.revision = slice.revision ?? session.revision;
                    session.revisionKnown = sliceRevisionKnown(slice);
                    session.digest = slice.digest ?? session.digest;
                } else {
                    // A backend that raced a rewrite may return a fresh page
                    // instead of a stale marker. Never prepend rows from a
                    // different canonical state.
                    const projection = await this.loadLatest(tabId, sessionPath, options);
                    return projection ? { ...projection, kind: "reload", prependItems: [], removeIds: [] } : undefined;
                }
            }
            const { items, removeIds } = this.prependRecords(session, asArray<HistoryEntry>(slice.entries));
            session.nextCursor = slice.nextCursor ?? "";
            session.hasOlder = Boolean(slice.hasOlder);
            session.totalTurns = slice.totalTurns ?? session.totalTurns;
            session.startTurn = slice.startTurn ?? session.startTurn;
            session.revision = slice.revision ?? session.revision;
            session.revisionKnown = sliceRevisionKnown(slice);
            session.digest = slice.digest ?? session.digest;
            this.enforceBudgets();
            if (this.sessions.get(key) !== session)
                return undefined;
            return { ...this.projectionOf(session), kind: "prepend", prependItems: items, removeIds, appendOnly: slice.appendOnly };
        }
        finally {
            session.olderInFlight = false;
        }
    }
    private matchesExpectedFingerprint(session: SessionTranscript, expectedRevision?: number, expectedDigest?: string): boolean {
        const digest = (expectedDigest ?? "").trim();
        const revisionKnown = typeof expectedRevision === "number" && expectedRevision > 0;
        if (digest !== "" && session.digest !== digest)
            return false;
        if (revisionKnown && (!session.revisionKnown || session.revision !== expectedRevision))
            return false;
        if (!revisionKnown && digest === "") {
            // Metadata identity temporarily missing cannot prove a known resident
            // projection is current. A backend round trip is the safe fallback.
            return !session.revisionKnown && session.digest === "";
        }
        return true;
    }
    private sameFingerprint(session: SessionTranscript, slice: HistorySlice): boolean {
        return session.revision === (slice.revision ?? 0) &&
            session.revisionKnown === sliceRevisionKnown(slice) &&
            session.digest === (slice.digest ?? "");
    }
    // ── lazy content ──────────────────────────────────────────────────────────
    private sessionForEntry(tabId: string, entryId: string): SessionTranscript | undefined {
        for (const session of this.sessions.values()) {
            if (session.tabId === tabId && session.byId.has(entryId))
                return session;
        }
        return undefined;
    }
    /**
     * Fetch the full value of a ref-replaced field, chunk by chunk, and fold it
     * into the record. Late (generation-stale) responses are discarded; a stale
     * chunk marks the ref stale and keeps the inline preview.
     */
    async requestFullContent(tabId: string, entryId: string, field: string): Promise<string | undefined> {
        const session = this.sessionForEntry(tabId, entryId);
        const rec = session?.byId.get(entryId);
        if (!session || !rec)
            return undefined;
        if (rec.resolved?.[field])
            return rec.resolved[field];
        const ref = rec.refs.find((candidate) => candidate.field === field);
        if (!ref)
            return undefined;
        const pendingKey = `${entryId}${field}`;
        // Dedupe only within the same generation: a request started before a
        // session switch/evict is doomed to discard, never join it.
        const pending = session.pendingContent.get(pendingKey);
        if (pending && pending.generation === session.generation)
            return pending.promise;
        const generation = session.generation;
        const request = (async (): Promise<string | undefined> => {
            let data = "";
            const chunks = Math.max(1, ref.chunks);
            for (let index = 0; index < chunks; index += 1) {
                const chunk = await this.backend.HistoryContentForTab(tabId, ref, index);
                if (this.sessions.get(session.key) !== session || session.generation !== generation)
                    return undefined;
                if (chunk.stale) {
                    rec.staleRefs = { ...rec.staleRefs, [field]: true };
                    return undefined;
                }
                data += chunk.data ?? "";
                if (chunk.done)
                    break;
            }
            if (!applyResolvedField(rec, ref, data))
                return undefined;
            const previousBytes = rec.bytes;
            rec.bytes = recordBytes(rec.message);
            session.bodyBytes += rec.bytes - previousBytes;
            rec.resolved = { ...rec.resolved, [field]: data };
            this.reconvertAndNotify(session, rec);
            this.enforceBudgets();
            return data;
        })();
        const entry = { generation, promise: request };
        request.finally(() => {
            if (session.pendingContent.get(pendingKey) === entry)
                session.pendingContent.delete(pendingKey);
        });
        session.pendingContent.set(pendingKey, entry);
        return request;
    }
    /**
     * Resolve every unresolved ref field of an entry. The rendering layer calls
     * this when a history-backed row mounts (mount implies near-viewport with
     * the virtual list's overscan); entries without refs no-op.
     */
    requestEntryFullContent(tabId: string | undefined, entryId: string): void {
        if (!tabId)
            return;
        const session = this.sessionForEntry(tabId, entryId);
        const rec = session?.byId.get(entryId);
        if (!session || !rec)
            return;
        for (const ref of rec.refs) {
            if (rec.resolved?.[ref.field] || rec.staleRefs?.[ref.field])
                continue;
            void this.requestFullContent(session.tabId, entryId, ref.field).catch(() => { });
        }
    }
    private reconvertAndNotify(session: SessionTranscript, rec: TranscriptRecord): void {
        // A resolved field on a CONSUMED tool-result row shows up in the claiming
        // call's tool item, so re-convert the claimer instead of the row.
        const targetId = session.consumedBy.get(rec.entryId) ?? rec.entryId;
        const target = session.byId.get(targetId);
        if (!target)
            return;
        // Re-convert with the record's established claims so tool results stay put.
        const view = this.viewOf(session.records);
        const consumed = new Set(session.consumed);
        for (const claimed of session.matchTables.get(targetId)?.values() ?? [])
            consumed.delete(claimed);
        const conversion = convertRecord(target, view, consumed, session.matchTables.get(targetId));
        session.consumed = consumed;
        this.trackConversion(session, target, conversion);
        session.itemsCache = null;
        this.rebuildProjection(session);
        const listeners = this.listeners.get(session.tabId);
        if (!listeners || listeners.size === 0)
            return;
        const patches: Record<string, Item> = {};
        for (const item of conversion.items)
            patches[item.id] = item;
        const change: TranscriptContentChange = { tabId: session.tabId, patches };
        for (const listener of listeners)
            listener(change);
    }
    /** Newest-page refs resolve eagerly so the visible transcript is complete. */
    private autoFetchRefs(session: SessionTranscript): void {
        for (const rec of session.records) {
            for (const ref of rec.refs) {
                void this.requestFullContent(session.tabId, rec.entryId, ref.field).catch(() => { });
            }
        }
    }
    // ── markdown cache (populated by the rendering/worker phase) ──────────────
    getMarkdown(entryId: string, revision: number): ParsedMarkdownValue | undefined {
        return this.markdown.get(entryId, revision);
    }
    setMarkdown(entryId: string, revision: number, value: ParsedMarkdownValue): void {
        this.markdown.set(entryId, revision, value);
    }
    pinMarkdown(entryId: string, revision: number): () => void {
        return this.markdown.pin(entryId, revision);
    }
    markdownCacheSize(): number {
        return this.markdown.size();
    }
    // ── subscriptions ─────────────────────────────────────────────────────────
    /** Notified when a record's projected items change (content resolution). */
    subscribe(tabId: string, listener: (change: TranscriptContentChange) => void): () => void {
        let set = this.listeners.get(tabId);
        if (!set) {
            set = new Set();
            this.listeners.set(tabId, set);
        }
        set.add(listener);
        return () => {
            set.delete(listener);
            if (set.size === 0)
                this.listeners.delete(tabId);
        };
    }
}
// Bridge-backed singleton: resolves window.go.main.App at call time through
// the app proxy, so test/dev mocks install whenever they appear.
let singleton: TranscriptStore | undefined;
export function getTranscriptStore(): TranscriptStore {
    if (!singleton) {
        singleton = new TranscriptStore({
            HistorySliceForTab: (tabID, req) => app.HistorySliceForTab(tabID, req),
            HistoryContentForTab: (tabID, ref, chunkIndex) => app.HistoryContentForTab(tabID, ref, chunkIndex),
        });
    }
    return singleton;
}
export type { TranscriptBackend as TranscriptBackend } from "./transcript_types";
export type { TranscriptStoreOptions as TranscriptStoreOptions } from "./transcript_types";
export type { TranscriptProjection as TranscriptProjection } from "./transcript_types";
export type { LoadOlderResult as LoadOlderResult } from "./transcript_types";
export type { TranscriptContentChange as TranscriptContentChange } from "./transcript_types";
export type { TranscriptRecord as TranscriptRecord } from "./transcript_types";
export type { RecordConversion as RecordConversion } from "./transcript_types";
export type { SessionTranscript as SessionTranscript } from "./transcript_types";
export { DEFAULT_MAX_RESIDENT_SESSIONS as DEFAULT_MAX_RESIDENT_SESSIONS } from "./transcript_types";
export { DEFAULT_HISTORY_BODY_BUDGET as DEFAULT_HISTORY_BODY_BUDGET } from "./transcript_types";
export { DEFAULT_MARKDOWN_BUDGET as DEFAULT_MARKDOWN_BUDGET } from "./transcript_types";
export { sessionKeyFor as sessionKeyFor } from "./transcript_types";
export { sliceRevisionKnown as sliceRevisionKnown } from "./transcript_types";
export { compareRecords as compareRecords } from "./transcript_types";
export { recordBytes as recordBytes } from "./transcript_types";
export { entryToRecord as entryToRecord } from "./transcript_types";
export { convertRecord as convertRecord } from "./transcript_types";
export { applyResolvedField as applyResolvedField } from "./transcript_types";

