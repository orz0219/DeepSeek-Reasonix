import { type Locale, type Translator } from "../lib/i18n";
import type { DictKey } from "../locales/en";
import type { BalanceInfo, ContextInfo, ContextPanelInfo, WireUsage } from "../lib/types";
export interface ContextPanelProps {
    tabId?: string;
    context?: ContextInfo;
    usage?: WireUsage;
    sessionTokens?: number;
    sessionCost?: number;
    sessionCurrency?: string;
    sessionTurns?: number;
    turnTokens?: number;
    turnCost?: number;
    balance?: BalanceInfo;
    sessionGen?: number;
    refreshKey?: number;
    // Monotonic counter bumped by EVERY usage event (executor and subagent).
    // The executor-gated `usage` prop freezes during sub-agent runs, which used
    // to pin 会话指标/用量分析 for minutes; this keeps the snapshot ticking.
    usageSeq?: number;
}
export function fmtDuration(ms: number, t: Translator): string {
    if (ms <= 0)
        return "-";
    const totalSeconds = Math.max(1, Math.round(ms / 1000));
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    if (minutes <= 0)
        return t("context.durationSeconds", { seconds });
    return t("context.durationMinutesSeconds", { minutes, seconds });
}
interface MetricTokenDisplay {
    display: string;
    exact: string;
}
function numberLocale(locale: Locale | string): string {
    if (locale === "zh")
        return "zh-CN";
    if (locale === "zh-TW")
        return "zh-TW";
    return "en";
}
export function formatMetricTokens(tokens: number | undefined, locale: Locale | string): MetricTokenDisplay {
    if (typeof tokens !== "number" || tokens <= 0) {
        return { display: "-", exact: "-" };
    }
    const tag = numberLocale(locale);
    const exact = tokens.toLocaleString(tag);
    return { display: exact, exact };
}
export function fmtUsageCacheRate(usage?: WireUsage): string {
    if (!usage)
        return "-";
    const denom = usage.cacheHitTokens + usage.cacheMissTokens;
    if (denom <= 0)
        return "-";
    return `${((usage.cacheHitTokens / denom) * 100).toFixed(2)}%`;
}
export function formatCacheHitRate(hitTokens: number, missTokens: number): string {
    const denom = hitTokens + missTokens;
    if (denom <= 0)
        return "-";
    return `${((hitTokens / denom) * 100).toFixed(2)}%`;
}
export type MetricTone = "accent" | "good" | "notice" | "warn";
export type UsageAnalysisView = "source" | "type";
type ContextUsageRefreshFields = Pick<WireUsage, "totalTokens" | "promptTokens" | "completionTokens" | "reasoningTokens" | "sessionCacheHitTokens" | "sessionCacheMissTokens">;
export function contextUsageRefreshKey(usage?: ContextUsageRefreshFields): string {
    if (!usage)
        return "";
    return [
        usage.totalTokens ?? 0,
        usage.promptTokens ?? 0,
        usage.completionTokens ?? 0,
        usage.reasoningTokens ?? 0,
        usage.sessionCacheHitTokens ?? 0,
        usage.sessionCacheMissTokens ?? 0,
    ].join(":");
}
export function cacheHitTone(hitTokens: number, missTokens: number): MetricTone | undefined {
    const denom = hitTokens + missTokens;
    if (denom <= 0)
        return undefined;
    const pct = (hitTokens / denom) * 100;
    if (pct >= 80)
        return "good";
    if (pct >= 60)
        return "notice";
    return "warn";
}
export function formatSharePercent(value: number, total: number): string {
    if (total <= 0 || value <= 0)
        return "-";
    const pct = (value / total) * 100;
    if (pct > 0 && pct < 1)
        return "<1%";
    return `${Math.round(pct)}%`;
}
export interface ContextWindowStatus {
    tone: "good" | "notice" | "warn";
    key: DictKey;
}
export function contextCostDisplay({ info, sessionCost, sessionCurrency, usage, }: {
    info?: Pick<ContextPanelInfo, "sessionCost" | "sessionCurrency" | "sessionCostUsd" | "sessionCostComplete" | "sessionCostEstimated" | "sessionBillingMode" | "sessionCostQuote"> | null;
    sessionCost?: number;
    sessionCurrency?: string;
    usage?: Pick<WireUsage, "cost" | "costUsd" | "currency" | "currencyCode" | "costQuote">;
}): {
    amount: number;
    currency?: string;
    estimated?: boolean;
    complete?: boolean;
    billingMode?: string;
    labelKind?: "estimated" | "payg_equivalent" | "fallback" | "bucketed" | "unavailable";
} {
    // Prefer structured session quote, then per-usage quote.
    const quote = info?.sessionCostQuote || usage?.costQuote;
    if (quote?.displayStatus === "bucketed" || quote?.aggregateMode === "currency_buckets") {
        return {
            amount: 0,
            currency: undefined,
            estimated: true,
            complete: false,
            labelKind: "bucketed",
        };
    }
    const fallbackOriginal = quote?.displayStatus === "fallback_original";
    if (!fallbackOriginal && (info?.sessionCostComplete === false || quote?.displayStatus === "unavailable" || quote?.costComplete === false)) {
        return {
            amount: 0,
            currency: info?.sessionCurrency || sessionCurrency || usage?.currencyCode || usage?.currency,
            estimated: true,
            complete: false,
            labelKind: "unavailable",
        };
    }
    const selected = quote?.selected;
    if (selected?.amount) {
        const n = Number(selected.amount);
        if (Number.isFinite(n) && n > 0) {
            const mode = quote?.billingMode || info?.sessionBillingMode;
            return {
                amount: n,
                currency: selected.currency || usage?.currencyCode || usage?.currency || info?.sessionCurrency,
                estimated: quote?.estimated !== false,
                complete: quote?.displayComplete !== false,
                billingMode: mode,
                labelKind: fallbackOriginal ? "fallback" : mode === "subscription_equivalent" ? "payg_equivalent" : "estimated",
            };
        }
    }
    // Session-scoped scalar fallbacks (legacy telemetry).
    if (info?.sessionCost && info.sessionCost > 0) {
        return {
            amount: info.sessionCost,
            currency: info.sessionCurrency || sessionCurrency || usage?.currencyCode || usage?.currency,
            estimated: true,
            complete: true,
            labelKind: "estimated",
        };
    }
    if (sessionCost && sessionCost > 0) {
        return {
            amount: sessionCost,
            currency: sessionCurrency || info?.sessionCurrency || usage?.currencyCode || usage?.currency,
            estimated: true,
            complete: true,
            labelKind: "estimated",
        };
    }
    if (info?.sessionCostUsd && info.sessionCostUsd > 0) {
        return {
            amount: info.sessionCostUsd,
            currency: info.sessionCurrency || sessionCurrency || usage?.currencyCode || usage?.currency,
            estimated: true,
            complete: true,
            labelKind: "estimated",
        };
    }
    return {
        amount: 0,
        currency: info?.sessionCurrency || sessionCurrency || usage?.currencyCode || usage?.currency,
        estimated: true,
        complete: false,
        labelKind: "unavailable",
    };
}
// contextSessionCache picks the session-cumulative cache hit/miss pair for the
// panel's session average. The shared ContextInfo is refreshed after every
// usage event and also drives StatusBar, so prefer it over the panel's
// independently throttled snapshot. Panel telemetry remains the all-sources
// fallback for callers without live context; executor-only wire counters only
// bridge the pre-refresh gap. The pair always comes from one source so the
// computed rate never mixes scopes.
export function contextSessionCache(info?: Pick<ContextPanelInfo, "sessionCacheHitTokens" | "sessionCacheMissTokens"> | null, context?: Pick<ContextInfo, "cacheHitTokens" | "cacheMissTokens">, usage?: Pick<WireUsage, "sessionCacheHitTokens" | "sessionCacheMissTokens">): {
    hit: number;
    miss: number;
} {
    const ctxHit = context?.cacheHitTokens ?? 0;
    const ctxMiss = context?.cacheMissTokens ?? 0;
    if (ctxHit + ctxMiss > 0)
        return { hit: ctxHit, miss: ctxMiss };
    const infoHit = info?.sessionCacheHitTokens ?? 0;
    const infoMiss = info?.sessionCacheMissTokens ?? 0;
    if (infoHit + infoMiss > 0)
        return { hit: infoHit, miss: infoMiss };
    return { hit: usage?.sessionCacheHitTokens ?? 0, miss: usage?.sessionCacheMissTokens ?? 0 };
}
export interface ContextBreakdown {
    promptTokens: number;
    completionTokens: number;
    reasoningTokens: number;
    otherTokens: number;
    promptPct: number;
    completionPct: number;
    reasoningPct: number;
    otherPct: number;
}
export function nonNegativeTokenCount(value: number): number {
    return Number.isFinite(value) ? Math.max(0, value) : 0;
}

