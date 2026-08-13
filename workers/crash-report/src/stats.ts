import { esc, page } from "./shell";
export type Daily = {
    date: string;
    users: number;
    opens: number;
};
export type MetricRow = {
    signal: string;
    bucket: string;
    total: number;
};
export type BarRow = {
    label: string;
    users: number;
};
type BarListOptions = {
    limit?: number;
    className?: string;
    labelFormatter?: (label: string) => string;
};
export type OverviewCounts = {
    latestAdoptionPct: number | null;
    openReports: number;
    newLatestReports: number;
    regressedReports: number;
    criticalOpenReports: number;
};
export type StatsModule = "diagnostics" | "usage" | "preferences" | "health";
export function lastDays(rows: Daily[], count: 7 | 30): Daily[] {
    const byDate = new Map(rows.map((r) => [r.date, r]));
    const out: Daily[] = [];
    for (let i = count - 1; i >= 0; i--) {
        const date = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10);
        out.push(byDate.get(date) ?? { date, users: 0, opens: 0 });
    }
    return out;
}
function chartTickStep(max: number, targetTicks = 4): number {
    if (max <= targetTicks)
        return 1;
    const raw = Math.max(1, max) / targetTicks;
    const pow = 10 ** Math.floor(Math.log10(raw));
    const fraction = raw / pow;
    if (fraction <= 1)
        return pow;
    if (fraction <= 2)
        return 2 * pow;
    if (fraction <= 5)
        return 5 * pow;
    return 10 * pow;
}
function chartTickLabel(n: number): string {
    if (n >= 1000000)
        return `${Number((n / 1000000).toFixed(n % 1000000 === 0 ? 0 : 1))}m`;
    if (n >= 1000)
        return `${Number((n / 1000).toFixed(n % 1000 === 0 ? 0 : 1))}k`;
    return String(Math.round(n));
}
export function i18n(en: string, zh: string): string {
    return `<span data-i18n="en">${esc(en)}</span><span data-i18n="zh">${esc(zh)}</span>`;
}
export function i18nHTML(en: string, zh: string): string {
    return `<span data-i18n="en">${en}</span><span data-i18n="zh">${zh}</span>`;
}
export function dailyChart(days: Daily[]): string {
    const W = 960;
    const H = 220;
    const plotLeft = 50;
    const plotRight = 8;
    const plotTop = 16;
    const baseY = H - 26;
    const plotH = baseY - plotTop;
    const slot = (W - plotLeft - plotRight) / days.length;
    const max = Math.max(1, ...days.map((d) => d.opens));
    const step = chartTickStep(max);
    const chartMax = Math.max(step, Math.ceil(max / step) * step);
    const h = (v: number) => (v / chartMax) * plotH;
    const ticks: number[] = [];
    for (let v = 0; v <= chartMax; v += step)
        ticks.push(v);
    const grid = ticks
        .map((v) => {
        const y = baseY - h(v);
        return `<g><line x1="${plotLeft}" y1="${y}" x2="${W - plotRight}" y2="${y}" class="gridline"/><text x="${plotLeft - 8}" y="${y + 4}" text-anchor="end" class="ay">${chartTickLabel(v)}</text></g>`;
    })
        .join("");
    const bars = days
        .map((d, i) => {
        const x = plotLeft + i * slot;
        const label = i % 5 === 4 ? `<text x="${x + slot / 2}" y="${H - 8}" text-anchor="middle" class="ax">${d.date.slice(5)}</text>` : "";
        return `<g><title>${esc(`${d.date} — ${d.users} users · ${d.opens} opens`)}</title>
<rect x="${x}" y="${plotTop}" width="${slot}" height="${plotH}" fill="transparent" pointer-events="all"/>
<rect x="${x + slot * 0.18}" y="${baseY - h(d.opens)}" width="${slot * 0.64}" height="${h(d.opens)}" rx="3" fill="var(--accent)" opacity="0.22"/>
<rect x="${x + slot * 0.3}" y="${baseY - h(d.users)}" width="${slot * 0.4}" height="${h(d.users)}" rx="3" fill="var(--accent)"/>
${label}</g>`;
    })
        .join("");
    return `<svg class="chart" viewBox="0 0 ${W} ${H}" role="img" aria-label="Daily active installs chart"><style>.ax,.ay{font:11px var(--mono);fill:var(--ink-3)}.gridline{stroke:var(--line);stroke-width:1}</style>
${grid}${bars}</svg>`;
}
function bucketDisplayLabel(signal: string, bucket: string): string {
    if (signal.includes("_model") && bucket.startsWith("custom_")) {
        const model = bucket.slice("custom_".length).replace(/_/g, " ");
        return `<span class="bucket-prefix">custom</span><span class="bucket-main">${esc(model)}</span>`;
    }
    return esc(bucket);
}
function barRow(r: BarRow, max: number, labelFormatter?: (label: string) => string): string {
    const label = labelFormatter ? labelFormatter(r.label) : esc(r.label);
    return `<div class="row" title="${esc(r.label)}"><span class="row-label">${label}</span><div class="row-bar"><div class="bar" style="width:${Math.max(3, Math.round((r.users / max) * 100))}%"></div></div><span class="n">${r.users}</span></div>`;
}
export function listBars(rows: BarRow[], options: BarListOptions = {}): string {
    if (!rows.length)
        return `<div class="empty">${i18n("No data in this window", "当前时间窗口暂无数据")}</div>`;
    const max = Math.max(1, ...rows.map((r) => r.users));
    const limit = options.limit ?? 5;
    const visible = limit > 0 ? rows.slice(0, limit) : rows;
    const hidden = limit > 0 ? rows.slice(limit) : [];
    const className = options.className ? ` ${esc(options.className)}` : "";
    const visibleRows = visible.map((r) => barRow(r, max, options.labelFormatter)).join("");
    if (!hidden.length)
        return `<div class="bars-list${className}">${visibleRows}</div>`;
    return `<div class="bars-list${className}">${visibleRows}<details class="bars-more"><summary><span class="more-closed">${i18nHTML(`Show ${hidden.length} more`, `展开 ${hidden.length} 项`)}</span><span class="more-open">${i18nHTML(`Hide ${hidden.length}`, `收起 ${hidden.length} 项`)}</span></summary><div class="bars-more-list">${hidden
        .map((r) => barRow(r, max, options.labelFormatter))
        .join("")}</div></details></div>`;
}
function labelizeBucket(bucket: string): string {
    return bucket.replace(/^n_/, "").replace(/_/g, " ");
}
export function sumMetric(rows: MetricRow[], signal: string): number {
    return rows.filter((r) => r.signal === signal).reduce((sum, r) => sum + r.total, 0);
}
function topMetricBucket(rows: MetricRow[], signal: string): string {
    const row = rows.filter((r) => r.signal === signal).sort((a, b) => b.total - a.total)[0];
    return row ? `${labelizeBucket(row.bucket)} · ${row.total}` : "none";
}
export function cacheHitRate(rows: MetricRow[]): number | null {
    const cacheRows = rows.filter((r) => r.signal === "cache_hit");
    const total = cacheRows.reduce((sum, r) => sum + r.total, 0);
    if (!total)
        return null;
    const weighted = cacheRows.reduce((sum, r) => {
        const m = r.bucket.match(/^(\d+)_(\d+)$/);
        const midpoint = m ? (Number(m[1]) + Number(m[2])) / 2 : 0;
        return sum + midpoint * r.total;
    }, 0);
    return weighted / total;
}
export function pct(n: number | null): string {
    if (n === null || !Number.isFinite(n))
        return "n/a";
    return `${Math.round(n)}%`;
}
export function ratioPer100(rows: MetricRow[], signal: string): number | null {
    const turns = sumMetric(rows, "turns");
    if (!turns)
        return null;
    return (sumMetric(rows, signal) / turns) * 100;
}
function deltaLabel(current: number | null, previous: number | null, suffix = ""): string {
    if (current === null || previous === null)
        return "new";
    const delta = current - previous;
    if (Math.abs(delta) < 0.05)
        return "flat";
    const sign = delta > 0 ? "+" : "";
    const rounded = Math.abs(delta) >= 10 ? Math.round(delta) : Number(delta.toFixed(1));
    return `${sign}${rounded}${suffix}`;
}
const METRIC_SIGNAL_LABELS: Record<string, {
    en: string;
    zh: string;
}> = {
    finish_reason: { en: "Finish reason", zh: "结束原因" },
    empty_final: { en: "Empty final guard", zh: "空回复拦截" },
    provider_error: { en: "Provider errors", zh: "Provider 错误" },
    cache_hit: { en: "Cache hit rate", zh: "缓存命中率" },
    tool_error: { en: "Tool errors", zh: "工具错误" },
    updater_error: { en: "Updater errors", zh: "更新器错误" },
    updater_event: { en: "Updater events", zh: "更新器事件" },
    compaction: { en: "Compactions", zh: "压缩" },
    turns: { en: "Turns", zh: "轮次" },
    desktop_hang: { en: "Desktop hangs", zh: "桌面卡死" },
    desktop_hang_age: { en: "Desktop hang age", zh: "桌面卡死时长" },
    desktop_exit: { en: "Desktop exits", zh: "桌面退出" },
    desktop_exit_phase: { en: "Abnormal exit phase", zh: "异常退出阶段" },
    desktop_uptime: { en: "Uptime before exit", zh: "退出前运行时长" },
    desktop_install: { en: "Install profile", zh: "安装方式" },
    desktop_update_transition: { en: "Update transition", zh: "升级阶段" },
    desktop_restore: { en: "Window restore", zh: "窗口恢复" },
    desktop_webview2_failure: { en: "WebView2 failures", zh: "WebView2 故障" },
    desktop_web_runtime_failure: { en: "Web runtime failures", zh: "Web Runtime 故障" },
    desktop_web_runtime_outcome: { en: "Web runtime outcomes", zh: "Web Runtime 结果" },
    recovery_failure: { en: "Recovery failures", zh: "恢复失败" },
    recovery_rule_continue: { en: "Rule recovery continues", zh: "规则恢复继续" },
    recovery_review_continue: { en: "Review recovery continues", zh: "复核恢复继续" },
    recovery_human_prompt: { en: "Recovery prompts", zh: "恢复询问" },
    recovery_human_continue: { en: "Human recovery continues", zh: "人工恢复继续" },
    recovery_human_revise: { en: "Human recovery revisions", zh: "人工恢复修订" },
    recovery_review_error: { en: "Recovery review errors", zh: "恢复复核错误" },
    recovery_repeat_prompt: { en: "Repeated recovery prompts", zh: "重复恢复询问" },
    recovery_review_latency: { en: "Recovery review latency", zh: "恢复复核耗时" },
    client_surface: { en: "Client surface", zh: "客户端形态" },
    client_version: { en: "Client version", zh: "客户端版本" },
    settings_language: { en: "Settings: language", zh: "设置：语言" },
    settings_desktop_layout: { en: "Settings: desktop style", zh: "设置：桌面风格" },
    settings_theme: { en: "Settings: light/dark", zh: "设置：深浅模式" },
    settings_theme_style: { en: "Settings: theme style", zh: "设置：主题" },
    settings_close_behavior: { en: "Settings: close behavior", zh: "设置：关闭行为" },
    settings_display_mode: { en: "Settings: transcript mode", zh: "设置：会话展示" },
    settings_status_bar_style: { en: "Settings: status bar style", zh: "设置：信息栏样式" },
    settings_status_bar_items_count: { en: "Settings: status bar items", zh: "设置：信息栏项数" },
    settings_check_updates: { en: "Settings: update checks", zh: "设置：更新检查" },
    settings_default_model: { en: "Settings: default model", zh: "设置：默认模型" },
    settings_planner_model: { en: "Settings: planner model", zh: "设置：规划模型" },
    settings_subagent_model: { en: "Settings: subagent model", zh: "设置：子代理模型" },
    settings_subagent_effort: { en: "Settings: subagent effort", zh: "设置：子代理 effort" },
    settings_reasoning_language: { en: "Settings: reasoning language", zh: "设置：推理语言" },
    settings_provider_count: { en: "Settings: provider count", zh: "设置：Provider 数量" },
    settings_provider_access_count: { en: "Settings: enabled providers", zh: "设置：启用 Provider 数量" },
    settings_provider_access: { en: "Settings: provider access", zh: "设置：Provider 选择" },
    settings_bot_enabled: { en: "Bot: enabled", zh: "机器人：总开关" },
    settings_bot_model: { en: "Bot: default model", zh: "机器人：默认模型" },
    settings_bot_tool_approval: { en: "Bot: tool approval", zh: "机器人：工具审批" },
    settings_bot_allowlist: { en: "Bot: allowlist", zh: "机器人：白名单" },
    settings_bot_allow_all: { en: "Bot: allow all", zh: "机器人：允许所有人" },
    settings_bot_qq_enabled: { en: "Bot: QQ legacy", zh: "机器人：QQ 旧配置" },
    settings_bot_feishu_enabled: { en: "Bot: Feishu legacy", zh: "机器人：飞书旧配置" },
    settings_bot_weixin_enabled: { en: "Bot: Weixin legacy", zh: "机器人：微信旧配置" },
    settings_bot_connection_count: { en: "Bot: connection count", zh: "机器人：连接数量" },
    settings_bot_connection_provider: { en: "Bot: connection provider", zh: "机器人：连接渠道" },
    settings_bot_connection_enabled: { en: "Bot: connection enabled", zh: "机器人：连接开关" },
    settings_bot_connection_status: { en: "Bot: connection status", zh: "机器人：连接状态" },
    settings_bot_connection_model: { en: "Bot: connection model", zh: "机器人：连接模型" },
    settings_bot_connection_approval: { en: "Bot: connection approval", zh: "机器人：连接审批" },
    cli_mode: { en: "CLI mode", zh: "CLI 模式" },
    cli_profile: { en: "CLI profile", zh: "CLI 配置档" },
    cli_permission_mode: { en: "CLI permission mode", zh: "CLI 权限模式" },
    cli_session_mode: { en: "CLI session mode", zh: "CLI 会话模式" },
    cli_turn_latency: { en: "CLI turn latency", zh: "CLI turn 延迟" },
    cli_exit: { en: "CLI turn outcome", zh: "CLI turn 结果" },
};
export const AGENT_METRIC_SIGNALS = [
    "finish_reason",
    "empty_final",
    "provider_error",
    "cache_hit",
    "tool_error",
    "updater_error",
    "updater_event",
    "compaction",
    "turns",
    "desktop_hang",
    "desktop_hang_age",
    "desktop_exit",
    "desktop_exit_phase",
    "desktop_uptime",
    "desktop_install",
    "desktop_update_transition",
    "desktop_restore",
    "desktop_webview2_failure",
    "desktop_web_runtime_failure",
    "desktop_web_runtime_outcome",
    "cli_turn_latency",
    "cli_exit",
    "recovery_failure",
    "recovery_rule_continue",
    "recovery_review_continue",
    "recovery_human_prompt",
    "recovery_human_continue",
    "recovery_human_revise",
    "recovery_review_error",
    "recovery_repeat_prompt",
    "recovery_review_latency",
];
const DEFAULT_OPEN_SETTING_GROUPS = new Set(["Client", "Models", "Providers"]);
const SETTINGS_METRIC_GROUPS: {
    en: string;
    zh: string;
    signals: string[];
}[] = [
    {
        en: "Client",
        zh: "客户端",
        signals: ["client_surface", "client_version", "settings_language", "cli_mode", "cli_profile", "cli_permission_mode", "cli_session_mode"],
    },
    {
        en: "Appearance and layout",
        zh: "外观与布局",
        signals: [
            "settings_desktop_layout",
            "settings_theme",
            "settings_theme_style",
            "settings_display_mode",
            "settings_status_bar_style",
            "settings_status_bar_items_count",
        ],
    },
    {
        en: "Models",
        zh: "模型",
        signals: [
            "settings_default_model",
            "settings_planner_model",
            "settings_subagent_model",
            "settings_subagent_effort",
            "settings_reasoning_language",
        ],
    },
    {
        en: "Providers",
        zh: "Provider",
        signals: ["settings_provider_count", "settings_provider_access_count", "settings_provider_access"],
    },
    {
        en: "Behavior toggles",
        zh: "行为开关",
        signals: ["settings_close_behavior", "settings_check_updates"],
    },
    {
        en: "Bots",
        zh: "机器人",
        signals: [
            "settings_bot_enabled",
            "settings_bot_model",
            "settings_bot_tool_approval",
            "settings_bot_allowlist",
            "settings_bot_allow_all",
            "settings_bot_qq_enabled",
            "settings_bot_feishu_enabled",
            "settings_bot_weixin_enabled",
            "settings_bot_connection_count",
            "settings_bot_connection_provider",
            "settings_bot_connection_enabled",
            "settings_bot_connection_status",
            "settings_bot_connection_model",
            "settings_bot_connection_approval",
        ],
    },
];
function metricSignalLabel(signal: string): string {
    const label = METRIC_SIGNAL_LABELS[signal];
    return label ? i18n(label.en, label.zh) : esc(signal);
}
function metricsBySignal(rows: MetricRow[]): Map<string, {
    label: string;
    users: number;
}[]> {
    const bySignal = new Map<string, {
        label: string;
        users: number;
    }[]>();
    for (const r of rows) {
        const list = bySignal.get(r.signal) ?? [];
        list.push({ label: r.bucket, users: r.total });
        bySignal.set(r.signal, list);
    }
    return bySignal;
}
function metricBlocks(bySignal: Map<string, BarRow[]>, signals: string[], options: {
    barLimit?: number;
} = {}): string {
    return signals
        .filter((signal) => bySignal.has(signal))
        .map((signal) => {
        const rows = bySignal.get(signal) ?? [];
        return `<div class="metric-block"><h3>${metricSignalLabel(signal)}<span>${rows.length}</span></h3>${listBars(rows, {
            limit: options.barLimit ?? 5,
            className: "metric-bars",
            labelFormatter: (label) => bucketDisplayLabel(signal, label),
        })}</div>`;
    })
        .join("");
}
export function metricsCards(rows: MetricRow[], signals = AGENT_METRIC_SIGNALS): string {
    if (!rows.length)
        return `<div class="empty">${i18n("No metrics yet — flows in once an opt-in build ships", "暂无运行指标 — 等 opt-in 版本发布后有数据")}</div>`;
    const bySignal = metricsBySignal(rows);
    const blocks = metricBlocks(bySignal, signals);
    return blocks ? `<div class="metrics">${blocks}</div>` : `<div class="empty">${i18n("No data in this window", "当前时间窗口暂无数据")}</div>`;
}
export function settingsDashboard(rows: MetricRow[], options: {
    collapseSections?: boolean;
} = {}): string {
    const bySignal = metricsBySignal(rows);
    const sections = SETTINGS_METRIC_GROUPS.map((group) => {
        const availableSignals = group.signals.filter((signal) => bySignal.has(signal));
        const blocks = metricBlocks(bySignal, group.signals);
        if (!blocks)
            return "";
        const heading = `<h3>${i18n(group.en, group.zh)}<span>${i18nHTML(`${availableSignals.length} metrics`, `${availableSignals.length} 项指标`)}</span></h3>`;
        if (options.collapseSections && !DEFAULT_OPEN_SETTING_GROUPS.has(group.en)) {
            return `<details class="pref-section pref-section-collapsed"><summary>${heading}</summary><div class="metrics pref-metrics">${blocks}</div></details>`;
        }
        return `<section class="pref-section">${heading}<div class="metrics pref-metrics">${blocks}</div></section>`;
    })
        .filter(Boolean)
        .join("");
    if (!sections)
        return `<div class="empty">${i18n("No settings preference metrics yet", "暂无设置偏好指标")}</div>`;
    return `<div class="preference-dashboard">${sections}</div>`;
}
export function healthLevel(kind: "cache" | "rate", value: number | null): "good" | "warn" | "bad" | "unknown" {
    if (value === null)
        return "unknown";
    if (kind === "cache") {
        if (value >= 80)
            return "good";
        if (value >= 50)
            return "warn";
        return "bad";
    }
    if (value <= 1)
        return "good";
    if (value <= 5)
        return "warn";
    return "bad";
}
function countHealthLevel(value: number): "good" | "warn" | "bad" {
    if (value <= 0)
        return "good";
    if (value <= 2)
        return "warn";
    return "bad";
}
function levelText(level: "good" | "warn" | "bad" | "unknown"): string {
    if (level === "good")
        return i18n("Good", "健康");
    if (level === "warn")
        return i18n("Watch", "关注");
    if (level === "bad")
        return i18n("Risk", "风险");
    return i18n("No data", "暂无数据");
}
function healthCard(label: {
    en: string;
    zh: string;
}, value: string, level: "good" | "warn" | "bad" | "unknown", deltaHTML: string, detailHTML: string): string {
    return `<div class="health-card ${level}"><div class="health-top"><span>${i18n(label.en, label.zh)}</span><b>${levelText(level)}</b></div>
<strong>${esc(value)}</strong><small>${deltaHTML}</small><p>${detailHTML}</p></div>`;
}
function healthDeltaHTML(value: string): string {
    return i18nHTML(`${esc(value)} vs previous window`, `${esc(value)} 较上一窗口`);
}
function healthDetailHTML(rows: MetricRow[], signal: string): string {
    return i18nHTML(`${esc(topMetricBucket(rows, signal))} top bucket`, `主要分桶：${esc(topMetricBucket(rows, signal))}`);
}
export function agentHealth(rows: MetricRow[], previousRows: MetricRow[]): string {
    if (!rows.length)
        return `<div class="empty">${i18n("No agent health metrics yet", "暂无运行健康指标")}</div>`;
    const cache = cacheHitRate(rows);
    const prevCache = cacheHitRate(previousRows);
    const desktopHangs = sumMetric(rows, "desktop_hang");
    const prevDesktopHangs = sumMetric(previousRows, "desktop_hang");
    const abnormalExits = rows.filter((r) => r.signal === "desktop_exit" && r.bucket === "abnormal").reduce((sum, r) => sum + r.total, 0);
    const prevAbnormalExits = previousRows.filter((r) => r.signal === "desktop_exit" && r.bucket === "abnormal").reduce((sum, r) => sum + r.total, 0);
    const webRuntimeFailures = sumMetric(rows, "desktop_web_runtime_failure") + sumMetric(rows, "desktop_webview2_failure");
    const prevWebRuntimeFailures = sumMetric(previousRows, "desktop_web_runtime_failure") + sumMetric(previousRows, "desktop_webview2_failure");
    const rateCard = (signal: string, en: string, zh: string) => {
        const value = ratioPer100(rows, signal);
        const prev = ratioPer100(previousRows, signal);
        return healthCard({ en, zh }, value === null ? "n/a" : `${Number(value.toFixed(value < 10 ? 1 : 0))}/100`, healthLevel("rate", value), healthDeltaHTML(deltaLabel(value, prev, "/100")), healthDetailHTML(rows, signal));
    };
    return `<div class="health-grid">
${healthCard({ en: "Cache hit rate", zh: "缓存命中率" }, pct(cache), healthLevel("cache", cache), healthDeltaHTML(deltaLabel(cache, prevCache, "pp")), healthDetailHTML(rows, "cache_hit"))}
${rateCard("provider_error", "Provider errors", "Provider 错误")}
${rateCard("tool_error", "Tool errors", "工具错误")}
${rateCard("empty_final", "Empty final guard", "空回复拦截")}
${rateCard("compaction", "Compactions", "压缩")}
${healthCard({ en: "Desktop hangs", zh: "桌面卡死" }, String(desktopHangs), countHealthLevel(desktopHangs), healthDeltaHTML(deltaLabel(desktopHangs, prevDesktopHangs)), healthDetailHTML(rows, "desktop_hang_age"))}
${healthCard({ en: "Abnormal desktop exits", zh: "桌面异常退出" }, String(abnormalExits), countHealthLevel(abnormalExits), healthDeltaHTML(deltaLabel(abnormalExits, prevAbnormalExits)), healthDetailHTML(rows, "desktop_exit_phase"))}
${healthCard({ en: "Web runtime process failures", zh: "Web Runtime 进程故障" }, String(webRuntimeFailures), countHealthLevel(webRuntimeFailures), healthDeltaHTML(deltaLabel(webRuntimeFailures, prevWebRuntimeFailures)), healthDetailHTML(rows, sumMetric(rows, "desktop_web_runtime_failure") ? "desktop_web_runtime_failure" : "desktop_webview2_failure"))}
</div>`;
}
export function statusPill(status: string): string {
    if (status === "resolved")
        return `<span class="pill resolved">resolved</span>`;
    if (status === "ignored")
        return `<span class="pill ignored">ignored</span>`;
    return "";
}
export { clip as clip } from "./stats_crash";
export { renderStats as renderStats } from "./stats_crash";

