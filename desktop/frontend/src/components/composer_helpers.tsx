import { useEffect, useState } from "react";
import type { KeyboardEvent } from "react";
import { DedupIndex } from "../lib/attachDedup";
import { type Translator } from "../lib/i18n";
import { type ComposerInvocation } from "../lib/invocationDisplay";
import { loadOptionalLayoutSize } from "../lib/layoutPreferences";
import { type DirEntry, type HistoryMessage, type SessionMeta, type SessionReference } from "../lib/types";
import type { PendingGuidance } from "./ComposerGuidanceShelf";
import { type RichComposerSelection } from "./RichComposerInput";
import { dirEntrySubmitPath } from "./FileReferenceMenu";
import { escapeRefPath } from "../lib/refToken";
import { type SelectedTextReference } from "../lib/selectedTextContext";
export interface Attachment {
    path: string;
    previewUrl?: string;
    displayName?: string;
}
export interface AttachmentDedupKey {
    hash: string;
    source: string;
}
export interface WorkspaceReference {
    path: string;
    isDir?: boolean;
    displayPath?: string;
}
const LONG_PASTE_MIN_CHARS = 2000;
const LONG_PASTE_MIN_LINES = 20;
export const COMPOSER_MIN_HEIGHT = 104;
const COMPOSER_MAX_HEIGHT = 360;
// Height reserved for the in-card run strip while a turn runs; applied via a
// CSS calc so --composer-height always stays in "logical height" space.
export const COMPOSER_RUN_STRIP_RESERVED = 30;
const COMPOSER_MAX_VIEWPORT_RATIO = 0.4;
export const COMPOSER_AUTO_RESERVED_HEIGHT = 58;
export const PROMPT_HISTORY_PREFETCH_REMAINING = 3;
export const FILE_REF_SEARCH_CACHE_TTL_MS = 5000;
export type PastedBlock = {
    label: string;
    text: string;
};
export type FileRefSearchCacheEntry = {
    entries: DirEntry[];
    cachedAt: number;
};
export type ComposerDraft = {
    text: string;
    invocations: ComposerInvocation[];
    attachments: Attachment[];
    workspaceRefs: WorkspaceReference[];
    pastedBlocks: PastedBlock[];
    openPastedLabels: string[];
    sessionRefs: SessionReference[];
    selectedTextRefs: SelectedTextReference[];
    attachmentDedupKeys: Record<string, AttachmentDedupKey>;
    nextPasteId: number;
    historyIndex: number;
    savedText: string;
    pendingGuidance: PendingGuidance[];
    guidanceExpanded: boolean;
    guidanceSendingId: string | null;
    pendingPaste: number;
    submitting: boolean;
};
export type ComposerEditSnapshot = {
    text: string;
    invocations: ComposerInvocation[];
    pastedBlocks: PastedBlock[];
    openPastedLabels: string[];
    nextPasteId: number;
    selection: RichComposerSelection;
};
type ComposerEditTransaction = {
    before: ComposerEditSnapshot;
    after: ComposerEditSnapshot;
    nativeBarrierBefore: boolean;
    nativeBarrierAfter: boolean;
};
export type ComposerEditHistory = {
    undo: ComposerEditTransaction[];
    redo: ComposerEditTransaction[];
    undoNativeBarrier: boolean;
    redoNativeBarrier: boolean;
};
export type WebkitFileEntry = {
    isDirectory?: boolean;
};
export const DEFAULT_COMPOSER_DRAFT_KEY = "__default_composer_draft__";
export const MAX_COMPOSER_EDIT_HISTORY = 50;
export function lineCount(s: string): number {
    if (s === "")
        return 0;
    return s.split(/\r\n|\r|\n/).length;
}
export function shouldFoldPaste(s: string): boolean {
    return s.length >= LONG_PASTE_MIN_CHARS || lineCount(s) >= LONG_PASTE_MIN_LINES;
}
export function renderPastedBlock(block: PastedBlock): string {
    return `${block.label}\n\n--- Begin ${block.label} ---\n${block.text}\n--- End ${block.label} ---`;
}
export function baseName(path: string): string {
    const clean = path.replace(/[\\/]+$/, "");
    return clean.split(/[\\/]/).filter(Boolean).pop() ?? path;
}
export function attachmentName(attachment: Attachment): string {
    return (attachment.displayName || baseName(attachment.path) || "attachment").trim();
}
export function attachmentExt(name: string): string {
    const dot = name.lastIndexOf(".");
    return dot >= 0 ? name.slice(dot + 1).toUpperCase() : "";
}
export function hasImageAttachments(items: Attachment[]): boolean {
    return items.some((attachment) => Boolean(attachment.previewUrl));
}
function displayRefName(name: string): string {
    return name.replace(/[\[\]\(\)\r\n]+/g, " ").replace(/\s+/g, " ").trim() || "attachment";
}
export function formatAttachmentDisplayReference(attachment: Attachment): string {
    return `@[${displayRefName(attachmentName(attachment))}](${attachment.path})`;
}
export function sortComposerAttachments(items: Attachment[]): Attachment[] {
    return [...items].sort((a, b) => {
        const ai = a.previewUrl ? 0 : 1;
        const bi = b.previewUrl ? 0 : 1;
        return ai - bi;
    });
}
export function workspaceReferenceKey(ref: WorkspaceReference): string {
    return `${ref.isDir ? "dir" : "file"}:${ref.path}`;
}
type PastChatToken = {
    from: number;
    query: string;
};
export function activePastChatToken(text: string): PastChatToken | null {
    const queryText = text.replace(/[\r\n]+$/u, "");
    const match = /(?:^|\s)#([^\s#]*)$/u.exec(queryText);
    if (!match)
        return null;
    return { from: match.index, query: match[1] };
}
export function composerPickFileEntry(text: string, atRaw: string | null, atDir: string, entry: DirEntry): {
    text: string;
    workspaceRef?: WorkspaceReference;
} {
    const queryText = text.replace(/[\r\n]+$/u, "");
    const atPos = queryText.length - (atRaw?.length ?? 0) - 1; // index of '@'
    const prefix = queryText.slice(0, Math.max(0, atPos));
    const refPath = dirEntrySubmitPath(entry, atDir);
    if (entry.path || entry.displayPath) {
        return { text: prefix, workspaceRef: { path: refPath, isDir: entry.isDir, displayPath: entry.displayPath } };
    }
    // Inline fallback: escape whitespace so the ref survives @-token parsing.
    return { text: prefix + "@" + escapeRefPath(refPath) + (entry.isDir ? "/" : " ") };
}
export function emptyComposerDraft(): ComposerDraft {
    return {
        text: "",
        invocations: [],
        attachments: [],
        workspaceRefs: [],
        pastedBlocks: [],
        openPastedLabels: [],
        sessionRefs: [],
        selectedTextRefs: [],
        attachmentDedupKeys: {},
        nextPasteId: 1,
        historyIndex: -1,
        savedText: "",
        pendingGuidance: [],
        guidanceExpanded: false,
        guidanceSendingId: null,
        pendingPaste: 0,
        submitting: false,
    };
}
export function cloneComposerDraft(draft: ComposerDraft): ComposerDraft {
    return {
        text: draft.text,
        invocations: draft.invocations.map((invocation) => ({ ...invocation, command: { ...invocation.command } })),
        attachments: [...draft.attachments],
        workspaceRefs: [...draft.workspaceRefs],
        pastedBlocks: [...draft.pastedBlocks],
        openPastedLabels: [...draft.openPastedLabels],
        sessionRefs: [...draft.sessionRefs],
        selectedTextRefs: draft.selectedTextRefs.map((reference) => ({ ...reference })),
        attachmentDedupKeys: { ...draft.attachmentDedupKeys },
        nextPasteId: draft.nextPasteId,
        historyIndex: draft.historyIndex,
        savedText: draft.savedText,
        pendingGuidance: draft.pendingGuidance.map((item) => ({ ...item })),
        guidanceExpanded: draft.guidanceExpanded,
        guidanceSendingId: draft.guidanceSendingId,
        pendingPaste: draft.pendingPaste,
        submitting: draft.submitting,
    };
}
export function attachmentDedupFromKeys(keys: Record<string, AttachmentDedupKey>): DedupIndex {
    const index = new DedupIndex();
    for (const key of Object.values(keys)) {
        index.add(key.hash, key.source);
    }
    return index;
}
export function draftHasAttachmentDedupKey(draft: ComposerDraft, key: AttachmentDedupKey): boolean {
    return Object.values(draft.attachmentDedupKeys).some((existing) => existing.hash === key.hash && existing.source === key.source);
}
function fileKey(file: File): string {
    return `${file.name}:${file.type}:${file.size}:${file.lastModified}`;
}
export function clipboardFiles(data: DataTransfer): File[] {
    const files = Array.from(data.files);
    const seen = new Set(files.map(fileKey));
    for (const item of Array.from(data.items)) {
        if (item.kind !== "file")
            continue;
        const file = item.getAsFile();
        if (!file)
            continue;
        const key = fileKey(file);
        if (seen.has(key))
            continue;
        seen.add(key);
        files.push(file);
    }
    return files;
}
export function clipboardHasImageHint(data: DataTransfer): boolean {
    const imageType = (value: string) => {
        const type = value.toLowerCase();
        return type.startsWith("image/") || type.includes("png") || type.includes("jpeg") || type.includes("jpg") || type.includes("tiff");
    };
    return Array.from(data.items).some((item) => imageType(item.type)) || Array.from(data.types).some(imageType);
}
export function isPasteShortcut(e: KeyboardEvent<HTMLElement>): boolean {
    return e.key.toLowerCase() === "v" && (e.metaKey || e.ctrlKey) && !e.altKey;
}
export function composerMaxHeight(): number {
    if (typeof window === "undefined")
        return COMPOSER_MAX_HEIGHT;
    return Math.max(COMPOSER_MIN_HEIGHT, Math.min(COMPOSER_MAX_HEIGHT, Math.floor(window.innerHeight * COMPOSER_MAX_VIEWPORT_RATIO)));
}
// The rendered card includes the run strip while a turn runs; subtract it to
// recover the user's logical height when measuring from the DOM.
export function composerLogicalHeight(card: HTMLElement): number {
    const strip = card.querySelector(".composer-run-strip");
    const stripHeight = strip ? strip.getBoundingClientRect().height : 0;
    return card.getBoundingClientRect().height - stripHeight;
}
export function clampComposerHeight(height: number): number {
    return Math.min(Math.max(Math.round(height), COMPOSER_MIN_HEIGHT), composerMaxHeight());
}
export function composerAutoInputMaxHeight(extraReservedHeight = 0): number {
    return Math.max(32, composerMaxHeight() - COMPOSER_AUTO_RESERVED_HEIGHT - extraReservedHeight);
}
export function loadComposerHeight(): number | null {
    return loadOptionalLayoutSize("composerHeight", clampComposerHeight);
}
export function fmtElapsed(ms: number): string {
    const s = Math.floor(ms / 1000);
    if (s < 60)
        return `${s}s`;
    return `${Math.floor(s / 60)}m ${s % 60}s`;
}
// --- past:chats hover preview helpers (PR-C2) ---
// Pure formatting helpers used by the past:chats list tooltip. They never read
// from disk, never call PreviewSession — they only shape the data that already
// lives in the SessionMeta snapshot we fetched on entry.
const PAST_CHAT_PREVIEW_MAX = 200;
export function truncatePreview(value?: string, max = PAST_CHAT_PREVIEW_MAX): string {
    const text = (value || "").trim();
    if (text.length <= max)
        return text;
    return `${text.slice(0, max)}...`;
}
export function fmtSessionTime(value?: number): string {
    if (!value)
        return "";
    const d = new Date(value);
    if (Number.isNaN(d.getTime()))
        return "";
    const yyyy = d.getFullYear();
    const mm = String(d.getMonth() + 1).padStart(2, "0");
    const dd = String(d.getDate()).padStart(2, "0");
    const hh = String(d.getHours()).padStart(2, "0");
    const mi = String(d.getMinutes()).padStart(2, "0");
    return `${yyyy}-${mm}-${dd} ${hh}:${mi}`;
}
export function pastChatTitle(session: SessionMeta): string {
    return session.title || session.topicTitle || session.preview || "Untitled";
}
export function useTick(on: boolean): number {
    const [, setN] = useState(0);
    useEffect(() => {
        if (!on)
            return;
        const id = window.setInterval(() => setN((n) => n + 1), 1000);
        return () => window.clearInterval(id);
    }, [on]);
    return Date.now();
}
// --- past:chats session reference → prompt context (PR-B) ---
// Send-side helpers for "@past:chats" session references. PR-A wired the menu and
// the composer-context card; this layer reads each referenced session through the
// existing PreviewSession API and prepends a compact user/assistant transcript to
// submitText so the model sees the referenced chat as background context.
const SESSION_REF_MAX_MESSAGES = 30;
const SESSION_REF_MAX_CHARS = 20000;
export const PAST_CHATS_MENU_ITEM = "past:chats";
// limitSessionMessages keeps the most recent useful messages within a char budget.
// Walks from the end so the truncation is always "drop the oldest", which matches
// the intuition that the latest turns are the relevant ones for follow-up.
export function limitSessionMessages(messages: HistoryMessage[], maxMessages = SESSION_REF_MAX_MESSAGES, maxChars = SESSION_REF_MAX_CHARS): {
    messages: HistoryMessage[];
    truncated: boolean;
} {
    const useful = messages
        .filter((m) => (m.role === "user" || m.role === "assistant") &&
        typeof m.content === "string" &&
        m.content.trim().length > 0)
        .slice(-maxMessages);
    const result: HistoryMessage[] = [];
    let total = 0;
    let truncated = useful.length >= maxMessages;
    for (let i = useful.length - 1; i >= 0; i--) {
        const msg = useful[i];
        const content = msg.content.trim();
        if (total + content.length > maxChars) {
            truncated = true;
            break;
        }
        result.unshift({ ...msg, content });
        total += content.length;
    }
    if (result.length < useful.length)
        truncated = true;
    return { messages: result, truncated };
}
// formatSessionContext renders one referenced session as a labelled transcript.
// Falls back to a "no usable messages" note when filtering empties the list so
// the model still sees that something was referenced.
export function formatSessionContext(ref: SessionReference, messages: HistoryMessage[], truncated: boolean, t: Translator): string {
    const body = messages
        .map((m) => `${m.role === "user" ? t("composer.sessionContextUser") : t("composer.sessionContextAssistant")}: ${m.content.trim()}`)
        .join("\n\n");
    return [
        `[${t("composer.sessionContextSession", { title: ref.title })}]`,
        truncated ? t("composer.sessionContextTruncated") : "",
        body || t("composer.sessionContextEmpty"),
    ]
        .filter(Boolean)
        .join("\n");
}

