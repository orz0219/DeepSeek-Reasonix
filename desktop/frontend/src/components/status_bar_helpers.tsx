import { type ReactNode } from "react";
import { type Translator } from "../lib/i18n";
import { type ContextInfo, type UsageSourceStats, type WireUsage } from "../lib/types";
export type StatusBarLabelStyle = "icon" | "text";
function formatRate(hit: number, denom: number): string | null {
    return denom > 0 ? ((hit / denom) * 100).toFixed(2) : null;
}
// nowRate is the SINGLE-TURN prompt cache-hit % (latest turn) — the higher,
// steeper number on a non-compacting DeepSeek session. null when nothing yet.
export function nowRate(u?: WireUsage): string | null {
    if (!u)
        return null;
    const denom = u.cacheHitTokens + u.cacheMissTokens;
    return formatRate(u.cacheHitTokens, denom);
}
// avgRate is the SESSION-AGGREGATE cache-hit % — Σhit/Σ(hit+miss) across every
// turn — but scoped to the EXECUTOR agent only: the wire session counters come
// from the main agent and exclude subagent/planner/auxiliary requests. It is
// only the pre-first-refresh fallback; the authoritative all-sources number is
// contextAvgRate below, so the "session average" label reports one scope.
export function avgRate(u?: WireUsage): string | null {
    if (!u)
        return null;
    const denom = u.sessionCacheHitTokens + u.sessionCacheMissTokens;
    return formatRate(u.sessionCacheHitTokens, denom);
}
// contextAvgRate computes the session-aggregate cache-hit % from ContextInfo
// cache tokens — the tab telemetry that accumulates ALL request sources
// (executor, subagents, planner, auxiliary calls), refreshed at turn
// boundaries. Preferred over avgRate: it matches the 会话费用 tooltip's
// "includes main model, subagents and auxiliary calls" scope.
export function contextAvgRate(ctx: ContextInfo): string | null {
    const hit = ctx.cacheHitTokens ?? 0;
    const miss = ctx.cacheMissTokens ?? 0;
    return formatRate(hit, hit + miss);
}
export function rateValueClass(rate: string | null): string {
    if (rate === null)
        return "stat__value--empty";
    const pct = Number.parseFloat(rate);
    if (!Number.isFinite(pct))
        return "";
    if (pct >= 80)
        return "statusbar__rate-value--good";
    if (pct >= 50)
        return "statusbar__rate-value--notice";
    return "statusbar__rate-value--critical";
}
export function formatTokenCount(tokens?: number): string {
    if (typeof tokens !== "number" || tokens <= 0)
        return "-";
    return tokens.toLocaleString();
}
export function formatTurnCount(turns: number | undefined, t: Translator): string {
    if (typeof turns !== "number" || turns < 0)
        return "-";
    return t(turns === 1 ? "history.turnOne" : "history.turnOther", { n: turns });
}
export function formatTps(tps?: number | null, estimated = false): string | null {
    if (!tps || tps <= 0)
        return null;
    const prefix = estimated ? "≈" : "";
    if (tps < 1)
        return `${prefix}<1 t/s`;
    return `${prefix}${Math.round(tps)} t/s`;
}
const STATUS_SOURCE_ORDER = ["executor", "planner", "subagent", "compaction", "classifier", "title"];
function sourceLabel(source: string, t: Translator): string {
    switch (source) {
        case "executor": return t("context.sourceExecutor");
        case "planner": return t("context.sourcePlanner");
        case "subagent": return t("context.sourceSubagent");
        case "compaction": return t("context.sourceCompaction");
        case "classifier": return t("context.sourceClassifier");
        case "title": return t("context.sourceTitle");
        default: return source;
    }
}
function sourceRows(sources?: Record<string, UsageSourceStats>): Array<{
    source: string;
    stats: UsageSourceStats;
}> {
    return Object.entries(sources ?? {})
        .filter(([, stats]) => (stats.requestCount ?? 0) > 0 ||
        (stats.promptTokens ?? 0) > 0 ||
        (stats.completionTokens ?? 0) > 0 ||
        (stats.cacheHitTokens ?? 0) > 0 ||
        (stats.cacheMissTokens ?? 0) > 0)
        .sort(([a], [b]) => {
        const ia = STATUS_SOURCE_ORDER.indexOf(a);
        const ib = STATUS_SOURCE_ORDER.indexOf(b);
        if (ia >= 0 || ib >= 0)
            return (ia >= 0 ? ia : STATUS_SOURCE_ORDER.length) - (ib >= 0 ? ib : STATUS_SOURCE_ORDER.length);
        return a.localeCompare(b);
    })
        .map(([source, stats]) => ({ source, stats }));
}
export function sourceCacheTooltip(t: Translator, title: string, context: ContextInfo): ReactNode {
    const rows = sourceRows(context.sources);
    if (rows.length === 0)
        return title;
    return (<span className="statusbar__tooltip-stack">
      <span>{title}</span>
      {rows.map(({ source, stats }) => {
            const denom = stats.cacheHitTokens + stats.cacheMissTokens;
            const rate = denom > 0 ? `${formatRate(stats.cacheHitTokens, denom)}%` : t("context.cacheNotReported");
            return (<span key={source}>
            {sourceLabel(source, t)}: {rate} · {t("context.sourceInput")} {formatTokenCount(stats.promptTokens)}
            {" · "}{t("context.sourceOutput")} {formatTokenCount(stats.completionTokens)}
            {" · "}{t("context.sourceRequests", { count: stats.requestCount ?? 0 })}
          </span>);
        })}
    </span>);
}
export function MetricLabel({ style, icon, label }: {
    style: StatusBarLabelStyle;
    icon: ReactNode;
    label: string;
}) {
    return (<span className={`stat__label stat__label--${style}`} aria-hidden={style === "icon" ? "true" : undefined}>
      {style === "icon" ? icon : label}
    </span>);
}
export function compactPath(path?: string, fallback?: string): string {
    const value = (path || fallback || "").trim();
    if (!value)
        return "";
    const normalized = value.replace(/\\/g, "/");
    const homeMatch = normalized.match(/^~\/?(.+)?$/);
    const parts = (homeMatch ? homeMatch[1] ?? "" : normalized).split("/").filter(Boolean);
    if (parts.length === 0)
        return normalized;
    if (parts.length === 1)
        return parts[0];
    return `…/${parts.slice(-2).join("/")}`;
}
export function workspaceTooltip(t: Translator, displayPath: string, workspacePath?: string, gitBranch?: string) {
    const workspace = (workspacePath || displayPath).trim();
    const branch = (gitBranch || "").trim();
    if (branch) {
        return (<span className="statusbar__tooltip-stack">
        {workspace && <span>{t("status.workspaceTitle")}: {workspace}</span>}
        {branch && <span>{t("status.gitBranchTitle")}: {branch}</span>}
      </span>);
    }
    return `${t("status.workspaceTitle")}: ${workspace}`;
}

