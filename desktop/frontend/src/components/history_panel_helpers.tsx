import { t, useT } from "../lib/i18n";
import { sessionActivityTime } from "../lib/session";
import type { HistoryMessage, SessionMeta } from "../lib/types";
import { historyMessagesToItems, type Item } from "../lib/useController";
import { HistoryDateFilter } from "./HistoryPanel";
// dayLabel buckets a timestamp into "Today", "Yesterday", or a locale date. It's
// module-level (not a component), so it uses the non-reactive translator; the
// panel re-renders on a locale switch via its parent, picking up the new strings.
export function dayLabel(ms: number): string {
    const startOfDay = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
    const days = Math.round((startOfDay(new Date()) - startOfDay(new Date(ms))) / 86400000);
    if (days <= 0)
        return t("history.today");
    if (days === 1)
        return t("history.yesterday");
    return new Date(ms).toLocaleDateString();
}
export function timeLabel(ms: number): string {
    return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
export function dateBucket(ms: number): Exclude<HistoryDateFilter, "all"> {
    const startOfDay = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
    const days = Math.round((startOfDay(new Date()) - startOfDay(new Date(ms))) / 86400000);
    if (days <= 0)
        return "today";
    if (days === 1)
        return "yesterday";
    return "older";
}
export function sessionTimeForGrouping(s: SessionMeta, isTrash: boolean): number {
    return isTrash ? s.deletedAt || sessionActivityTime(s) : sessionActivityTime(s);
}
export function sessionScope(s: SessionMeta): "project" | "global" {
    return s.scope === "project" ? "project" : "global";
}
export function isChannelSession(s: SessionMeta): boolean {
    return s.kind === "channel" || s.sessionSource === "auto";
}
export function sessionLocation(s: SessionMeta, tr: ReturnType<typeof useT>): string {
    if (isChannelSession(s)) {
        return s.channelLabel || s.channel || tr("history.channel");
    }
    if (s.workspaceRoot) {
        const parts = s.workspaceRoot.split(/[\\/]/).filter(Boolean);
        return parts[parts.length - 1] || s.workspaceRoot;
    }
    return sessionScope(s) === "project" ? tr("history.filterProject") : tr("history.filterGlobal");
}
export function sessionMetaLine(s: SessionMeta, tr: ReturnType<typeof useT>, isTrash = false): string {
    const time = timeLabel(isTrash ? s.deletedAt || sessionActivityTime(s) : sessionActivityTime(s));
    const suffix = isTrash && s.deletedAt ? ` · ${tr("history.deleted")}` : "";
    const prefix = isChannelSession(s) ? `${tr("history.channelReadOnly")} · ` : "";
    const turns = s.turnsState === "unknown"
        ? tr("history.indexing")
        : tr(s.turns === 1 ? "history.turnOne" : "history.turnOther", { n: s.turns });
    return `${prefix}${turns} · ${time}${suffix}`;
}
export function previewMessagesToItems(messages: HistoryMessage[]): Item[] {
    return historyMessagesToItems(messages, "hp").items;
}

