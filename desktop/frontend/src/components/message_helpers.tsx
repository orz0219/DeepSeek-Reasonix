import { createContext, useState } from "react";
import { ChevronRight, FileText, Folder, Image } from "lucide-react";
import type { DisplayAttachment } from "../lib/attachmentDisplay";
import { useT } from "../lib/i18n";
import { type InvocationMetadataMap } from "../lib/invocationDisplay";
import type { Item } from "../lib/useController";
import { formatSelectionLabels, parseSelectedTextContext } from "../lib/selectedTextContext";
export type AssistantItem = Extract<Item, {
    kind: "assistant";
}>;
export type TurnActionMenu = "summary" | "rewind";
export const InvocationMetadataContext = createContext<InvocationMetadataMap>({});
type ImSourceMessage = {
    provider: string;
    label: string;
    sender: string;
    chat: string;
    text: string;
};
const IM_SOURCE_START = "[[reasonix-im]]";
const IM_SOURCE_END = "[[/reasonix-im]]";
export function parseImSourceMessage(text: string): ImSourceMessage | null {
    // Display-only metadata: keep IM sender/chat details out of model prompts.
    if (!text.startsWith(IM_SOURCE_START))
        return null;
    const end = text.indexOf(IM_SOURCE_END);
    if (end < 0)
        return null;
    const metaBlock = text.slice(IM_SOURCE_START.length, end).trim();
    const body = text.slice(end + IM_SOURCE_END.length).replace(/^\r?\n/, "");
    const meta: Record<string, string> = {};
    for (const line of metaBlock.split(/\r?\n/)) {
        const index = line.indexOf("=");
        if (index <= 0)
            continue;
        const key = line.slice(0, index).trim().toLowerCase();
        const value = line.slice(index + 1).trim();
        if (key)
            meta[key] = value;
    }
    return {
        provider: meta.provider || "",
        label: meta.label || "",
        sender: meta.sender || meta.senderid || "",
        chat: meta.chat || meta.chat_type || "",
        text: body,
    };
}
export function imSourceLabel(source: ImSourceMessage, t: ReturnType<typeof useT>): string {
    if (source.label.trim())
        return source.label.trim();
    const provider = source.provider.trim().toLowerCase();
    if (provider === "lark")
        return "Lark";
    if (provider === "weixin" || provider === "wechat")
        return t("settings.botWeixin");
    return t("settings.botFeishu");
}
export function attachmentIcon(kind: "image" | "file" | "folder") {
    if (kind === "image")
        return <Image size={15}/>;
    if (kind === "folder")
        return <Folder size={15}/>;
    return <FileText size={15}/>;
}
export function mergeDisplayAttachments(existing: DisplayAttachment[], incoming: DisplayAttachment[]): DisplayAttachment[] {
    if (incoming.length === 0)
        return existing;
    const seen = new Set(existing.map((attachment) => attachment.path));
    const merged = [...existing];
    for (const attachment of incoming) {
        if (seen.has(attachment.path))
            continue;
        seen.add(attachment.path);
        merged.push(attachment);
    }
    return merged;
}
export type PastedBlockInfo = {
    label: string;
    content: string;
};
const PASTE_LABEL_RE = /\[(?:已粘贴文本|已貼上文字|Pasted text) #\d+ · \d+ (?:行|lines)\]/g;
export function parsePastedBlocks(text: string, submitText?: string): PastedBlockInfo[] {
    const labels = text.match(PASTE_LABEL_RE);
    if (!labels || labels.length === 0 || !submitText)
        return [];
    const unique = [...new Set(labels)];
    const blocks: PastedBlockInfo[] = [];
    for (const label of unique) {
        const beginMarker = `--- Begin ${label} ---`;
        const endMarker = `--- End ${label} ---`;
        const beginIdx = submitText.indexOf(beginMarker);
        const endIdx = submitText.indexOf(endMarker);
        if (beginIdx < 0 || endIdx <= beginIdx)
            continue;
        const contentStart = beginIdx + beginMarker.length;
        const content = submitText.slice(contentStart, endIdx).replace(/^\r?\n/, "");
        blocks.push({ label, content });
    }
    return blocks;
}
export type SelectedTextBlockInfo = {
    label: string;
    content: string;
    path?: string;
    start: number;
    end: number;
    kind: "chat" | "code";
};
export function parseSelectedTextBlocks(text: string, submitText?: string): SelectedTextBlockInfo[] {
    const entries = parseSelectedTextContext(submitText);
    if (entries.length === 0)
        return [];
    const suffix = formatSelectionLabels(entries);
    if (!suffix || !text.endsWith(suffix))
        return [];
    // Composer owns the exact trailing label suffix. Deriving it from the JSON
    // entries avoids consuming label-shaped or unterminated authored prose.
    let start = text.length - suffix.length;
    return entries.map((entry) => {
        const label = formatSelectionLabels([entry]);
        const kind = entry.path ? "code" : "chat";
        const block = {
            label,
            content: entry.text,
            path: entry.path,
            start,
            end: start + label.length,
            kind,
        } satisfies SelectedTextBlockInfo;
        start = block.end + 1;
        return block;
    });
}

