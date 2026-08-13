import { forwardRef, memo, useContext, useEffect, useMemo } from "react";
import { type Components, type ItemProps, type ListProps } from "react-virtuoso";
import { AssistantMessage } from "./Message";
import { historyEntryIdForRow, type AssistantItem, type TranscriptRow } from "../lib/transcriptRows";
import { getTranscriptStore } from "../lib/transcriptStore";
import { LiveStreamContext } from "./LiveStreamContext";
import { TranscriptSelectionOverlay } from "./TranscriptSelectionOverlay";
import { AssistantReasoningDisplay, TranscriptVirtuosoContext } from "./transcript_helpers";
export const LiveAssistantMessage = memo(function LiveAssistantMessage({ item, defaultExpanded = false, expandWhileStreaming = false, truncateStreamingReasoning = false, creationMode = false, reasoningDisplay = "normal", }: {
    item: AssistantItem;
    defaultExpanded?: boolean;
    expandWhileStreaming?: boolean;
    truncateStreamingReasoning?: boolean;
    creationMode?: boolean;
    reasoningDisplay?: AssistantReasoningDisplay;
}) {
    const live = useContext(LiveStreamContext);
    const shown = useMemo(() => {
        const merged = live && live.id === item.id
            ? {
                ...item,
                text: live.text,
                reasoning: live.reasoning,
                streaming: true,
                reasoningComplete: live.reasoningComplete,
                reasoningDurationMs: live.reasoningStartedAt && live.reasoningCompletedAt && live.reasoningCompletedAt >= live.reasoningStartedAt
                    ? live.reasoningCompletedAt - live.reasoningStartedAt
                    : item.reasoningDurationMs,
            }
            : item;
        if (reasoningDisplay === "hide") {
            return { ...merged, reasoning: "", reasoningComplete: true, reasoningDurationMs: undefined };
        }
        return merged;
    }, [item, live?.id, live?.text, live?.reasoning, live?.reasoningComplete, live?.reasoningStartedAt, live?.reasoningCompletedAt, reasoningDisplay]);
    return (<AssistantMessage item={shown} defaultExpanded={defaultExpanded} expandWhileStreaming={expandWhileStreaming} truncateStreamingReasoning={truncateStreamingReasoning} creationMode={creationMode}/>);
});
const TranscriptVirtuosoItem = forwardRef<HTMLDivElement, ItemProps<TranscriptRow> & {
    context: TranscriptVirtuosoContext;
}>(function TranscriptVirtuosoItem({ item, context, children, style, ...props }, ref) {
    const entryId = historyEntryIdForRow(item);
    useEffect(() => {
        if (entryId)
            getTranscriptStore().requestEntryFullContent(context.tabId, entryId);
    }, [context.tabId, entryId]);
    const knownSize = Number.parseFloat(String(props["data-known-size"] ?? ""));
    const frozenStyle = context.nativeScrollbarDragging && Number.isFinite(knownSize) && knownSize > 0
        ? { ...style, boxSizing: "border-box" as const, height: knownSize, overflow: "hidden" as const }
        : style;
    return (<div {...props} ref={ref} style={frozenStyle} data-row-key={String(item.key)} className="transcript__row">
        {children}
      </div>);
});
const TranscriptVirtuosoList = forwardRef<HTMLDivElement, ListProps & {
    context: TranscriptVirtuosoContext;
}>(function TranscriptVirtuosoList({ context, children, ...props }, ref) {
    return (<div {...props} ref={ref} className="transcript__virtual-sizer">
        <TranscriptSelectionOverlay tabId={context.tabId ?? ""} scrollElement={context.scrollElement} virtualRevision={context.overlayRevision}/>
        {children}
      </div>);
});
function TranscriptVirtuosoHeader({ context }: {
    context: TranscriptVirtuosoContext;
}) {
    if (!context.olderHistory)
        return null;
    return (<div className="transcript__header">
      <button type="button" className="warm-collapse transcript__older" onClick={context.olderHistory.onLoad} disabled={context.olderHistory.loading}>
        {context.olderHistory.label}
      </button>
    </div>);
}
export const TRANSCRIPT_VIRTUOSO_COMPONENTS: Components<TranscriptRow, TranscriptVirtuosoContext> = {
    Item: TranscriptVirtuosoItem,
    List: TranscriptVirtuosoList,
};
export const TRANSCRIPT_VIRTUOSO_COMPONENTS_WITH_HEADER: Components<TranscriptRow, TranscriptVirtuosoContext> = {
    ...TRANSCRIPT_VIRTUOSO_COMPONENTS,
    Header: TranscriptVirtuosoHeader,
};

