import { useEffect, useState } from "react";
import type { CheckpointMeta } from "../lib/types";
import type { InvocationMetadataMap } from "../lib/invocationDisplay";
import { useT } from "../lib/i18n";
import { type AssistantItem } from "../lib/transcriptRows";
export type OpenTurnAction = {
    turn: number;
    menu: "summary" | "rewind";
};
export const QUESTION_NAV_MIN_COUNT = 2;
export type AssistantReasoningDisplay = "normal" | "hide";
export const EMPTY_CHECKPOINTS: CheckpointMeta[] = [];
export const EMPTY_INVOCATION_METADATA: InvocationMetadataMap = {};
export const VIRTUAL_OVERSCAN_ROWS = 8;
export type TranscriptVirtuosoContext = {
    tabId?: string;
    scrollElement: HTMLDivElement | null;
    nativeScrollbarDragging: boolean;
    overlayRevision: string;
    olderHistory: null | {
        loading: boolean;
        label: string;
        onLoad?: () => void;
    };
};
// ── Helpers ───────────────────────────────────────────────────────────────────
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
function formatWorkDuration(durationMs: number, t: ReturnType<typeof useT>): string {
    if (!Number.isFinite(durationMs) || durationMs <= 0)
        return "";
    const totalSeconds = Math.max(1, Math.round(durationMs / 1000));
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    if (minutes <= 0)
        return t("transcript.durationSeconds", { s: totalSeconds });
    if (seconds <= 0)
        return t("transcript.durationMinutes", { m: minutes });
    return t("transcript.durationMinutesSeconds", { m: minutes, s: seconds });
}
export function workStatusLabel(durationMs: number, running: boolean, t: ReturnType<typeof useT>): string {
    const duration = formatWorkDuration(durationMs, t);
    if (running) {
        return duration ? t("transcript.workingDuration", { duration }) : t("transcript.working");
    }
    return duration ? t("transcript.workedDuration", { duration }) : t("transcript.worked");
}
export function assistantAnswerOnly(item: AssistantItem): AssistantItem {
    return { ...item, reasoning: "", reasoningComplete: true, reasoningDurationMs: undefined };
}

