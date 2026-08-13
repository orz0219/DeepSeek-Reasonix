import { type ComposerInvocation } from "../lib/invocationDisplay";
import type { CommandInfo } from "../lib/types";
export type RichComposerSelection = {
    start: number;
    end: number;
    afterInvocationId?: string;
};
export type RichComposerChangeOrigin = {
    source: "browser" | "programmatic";
    inputType?: string;
    beforeSelection: RichComposerSelection;
    afterSelection: RichComposerSelection;
};
export type RichSlashQuery = {
    from: number;
    to: number;
    query: string;
};
export type RichComposerInputHandle = {
    focus: () => void;
    getSelection: () => RichComposerSelection;
    setSelectionRange: (start: number, end?: number, afterInvocationId?: string) => void;
    replaceRange: (value: string, start: number, end: number) => void;
    insertInvocation: (command: CommandInfo, query: RichSlashQuery) => void;
    scrollHeight: () => number;
};
export type DomSelectionRead = {
    ok: true;
    selection: RichComposerSelection;
} | {
    ok: false;
};
export type PendingSelection = RichComposerSelection | null;
export type ComposerModel = {
    text: string;
    invocations: ComposerInvocation[];
};
export type RenderedComposerModel = ComposerModel & {
    version: number;
};
export type DomPoint = {
    node: Node;
    offset: number;
};
export type EditSnapshot = {
    text: string;
    selection: RichComposerSelection;
    inputType: string;
    data: string | null;
};
export const CARET_SENTINEL = "\u00A0";
export function sameComposerModel(left: ComposerModel, right: ComposerModel | null): boolean {
    if (!right || left.text !== right.text || left.invocations.length !== right.invocations.length)
        return false;
    return left.invocations.every((item, index) => {
        const candidate = right.invocations[index];
        return item.id === candidate.id && item.offset === candidate.offset && item.command === candidate.command;
    });
}
function isHTMLElement(node: Node): node is HTMLElement {
    return node instanceof HTMLElement;
}
function isInvocationToken(node: Node): node is HTMLElement {
    return isHTMLElement(node) && Boolean(node.dataset.invocationId);
}
function isCaretAnchor(node: Node): node is HTMLElement {
    return isHTMLElement(node) && Boolean(node.dataset.composerCaretAnchor);
}
function isBreak(node: Node): node is HTMLElement {
    return isHTMLElement(node) && node.tagName === "BR";
}
/**
 * Shared DOM walk for model text, selection reads, and selection restores.
 *
 * Logical length rules:
 * - invocation tokens are zero-length atoms (children ignored)
 * - the first CARET_SENTINEL inside a caret anchor is zero-length
 * - remaining user text in the anchor counts
 * - <br> is one newline
 * - ordinary / nested text uses JavaScript UTF-16 offsets
 */
export function walkComposerDom(root: Node, visitor: {
    onInvocation?: (id: string, element: HTMLElement) => boolean | void;
    onText?: (node: Text, start: number, end: number) => boolean | void;
    onBreak?: (element: HTMLElement) => boolean | void;
}): void {
    const visit = (node: Node, inAnchor: boolean, anchorState: {
        skippedSentinel: boolean;
    }): boolean => {
        if (isInvocationToken(node)) {
            const id = node.dataset.invocationId;
            if (id && visitor.onInvocation?.(id, node))
                return true;
            return false;
        }
        if (isCaretAnchor(node)) {
            const state = { skippedSentinel: false };
            for (const child of Array.from(node.childNodes)) {
                if (visit(child, true, state))
                    return true;
            }
            return false;
        }
        if (isBreak(node)) {
            return Boolean(visitor.onBreak?.(node));
        }
        if (node.nodeType === Node.TEXT_NODE) {
            const textNode = node as Text;
            const value = textNode.textContent ?? "";
            if (!value)
                return false;
            if (!inAnchor) {
                return Boolean(visitor.onText?.(textNode, 0, value.length));
            }
            let start = 0;
            if (!anchorState.skippedSentinel) {
                const sentinelAt = value.indexOf(CARET_SENTINEL);
                if (sentinelAt === 0) {
                    anchorState.skippedSentinel = true;
                    start = 1;
                }
                else if (sentinelAt > 0) {
                    // Count text before the first sentinel, then skip the sentinel once.
                    if (visitor.onText?.(textNode, 0, sentinelAt))
                        return true;
                    anchorState.skippedSentinel = true;
                    start = sentinelAt + 1;
                }
            }
            if (start < value.length) {
                return Boolean(visitor.onText?.(textNode, start, value.length));
            }
            return false;
        }
        if (isHTMLElement(node) || node.nodeType === Node.DOCUMENT_FRAGMENT_NODE) {
            for (const child of Array.from(node.childNodes)) {
                if (visit(child, inAnchor, anchorState))
                    return true;
            }
        }
        return false;
    };
    visit(root, false, { skippedSentinel: false });
}
export function normalizeModelText(text: string): string {
    return text.replace(/\u00a0/g, " ");
}

