import { esc, page } from "./shell";
import { type User, userNav } from "./auth";
import { i18n, i18nHTML, statusPill, Daily, MetricRow, BarRow, OverviewCounts, StatsModule, lastDays, AGENT_METRIC_SIGNALS, cacheHitRate, ratioPer100, sumMetric, healthLevel, pct, dailyChart, listBars, settingsDashboard, agentHealth, metricsCards } from "./stats";
type CrashRow = {
    fingerprint: string;
    kind: string;
    count: number;
    first_version: string;
    last_version: string;
    seen: string;
    status: string;
    title: string;
    source: string;
    label: string;
    error_type: string;
    top_frame: string;
    severity: string;
    last_os: string;
    last_arch: string;
    last_channel: string;
    regressed_at: string;
    development?: boolean;
    affected_installs?: number;
    window_events?: number;
    identified_events?: number;
    identity_coverage?: number;
    dimension_coverage?: number;
    impact_rate?: number | null;
};
export function clip(s: string, n: number): string {
    return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}
function filterTab(label: string, zhLabel: string, href: string, active: boolean): string {
    return `<a class="filter-tab${active ? " active" : ""}" href="${esc(href)}">${i18n(label, zhLabel)}</a>`;
}
function facetChip(row: {
    label: string;
    users: number;
}, active: string, hrefFor: (label: string) => string): string {
    const label = row.label || "legacy";
    return `<a class="facet-chip${active === row.label ? " active" : ""}" href="${esc(hrefFor(row.label))}" title="${esc(label)}"><span class="facet-label">${esc(label)}</span><b>${row.users}</b></a>`;
}
function facetChips(rows: {
    label: string;
    users: number;
}[], active: string, hrefFor: (label: string) => string, limit = 5): string {
    if (!rows.length)
        return `<span class="filter-empty">${i18n("none", "暂无")}</span>`;
    const visible = rows.slice(0, limit);
    const activeRow = active ? rows.find((r) => r.label === active) : undefined;
    if (activeRow && !visible.some((r) => r.label === activeRow.label))
        visible.push(activeRow);
    const visibleKeys = new Set(visible.map((r) => r.label));
    const hidden = rows.filter((r) => !visibleKeys.has(r.label));
    const chips = visible.map((r) => facetChip(r, active, hrefFor)).join("");
    if (!hidden.length)
        return chips;
    return `${chips}<details class="facet-more"><summary>${i18nHTML(`More ${hidden.length}`, `更多 ${hidden.length}`)}</summary><div class="facet-more-list">${hidden
        .map((r) => facetChip(r, active, hrefFor))
        .join("")}</div></details>`;
}
function statCard(label: {
    en: string;
    zh: string;
}, value: string, note: string, href: string, tone = ""): string {
    return `<a class="overview-card ${tone}" href="${esc(href)}"><span>${i18n(label.en, label.zh)}</span><strong>${esc(value)}</strong><small>${note}</small></a>`;
}
function latestVersionShare(adoptionPct: number | null): string {
    return adoptionPct === null ? "n/a" : `${Math.round(adoptionPct)}%`;
}
function topSeverityTone(openReports: number, regressedReports: number, criticalOpenReports: number): string {
    if (criticalOpenReports || regressedReports)
        return "bad";
    if (openReports)
        return "warn";
    return "good";
}
function navLink(href: string, label: {
    en: string;
    zh: string;
}, active = false): string {
    return `<a${active ? ` class="active" aria-current="page"` : ""} href="${esc(href)}">${i18n(label.en, label.zh)}</a>`;
}
function preferencePanel(title: string, body: string, active: boolean): string {
    return `<section class="module-panel preference-panel${active ? " active" : ""}"${active ? ` aria-current="true"` : ""}>
<h3>${title}</h3>${body}</section>`;
}
function reportGroups(rows: CrashRow[], compact = false): string {
    if (!rows.length)
        return `<div class="empty">${i18n("No diagnostic reports yet — that's the good kind of empty", "还没有诊断报告，这是好消息")}</div>`;
    return `<div class="crash-list${compact ? " compact" : ""}"><div class="crash-head"><span>${i18n("summary", "摘要")}</span><span>${i18n("scope", "范围")}</span><span>${i18n("health", "状态")}</span><span title="${i18n("Window events and affected installs; lifetime count remains visible for historical context", "窗口事件数和受影响安装数；同时保留全生命周期累计次数")}">${i18n("window / lifetime", "窗口 / 累计")}</span></div>${rows
        .map((c) => {
        const platform = [c.last_os, c.last_arch].filter(Boolean).join("/");
        const versions = `${c.first_version || "?"} → ${c.last_version || "?"}`;
        const title = c.title || c.error_type || c.top_frame || c.fingerprint;
        return `<a class="crash-item" href="/stats/group/${esc(c.fingerprint)}" title="${esc(title)}">
<span class="crash-summary"><span>${c.title ? esc(clip(c.title, compact ? 88 : 120)) : `<span class="muted">${i18n("No summary captured", "暂无摘要")}</span>`}</span><small>${esc(c.fingerprint.slice(0, 8))} · ${esc(c.seen)}</small>${c.regressed_at ? `<em>${i18nHTML(`regressed ${esc(c.regressed_at.slice(0, 10))}`, `回归 ${esc(c.regressed_at.slice(0, 10))}`)}</em>` : ""}</span>
<span class="crash-scope"><small>${esc(c.source || "legacy")}</small><small>${esc(versions)}</small><small>${platform ? esc(platform) : "unknown platform"}</small>${c.last_channel && c.last_channel !== "stable" ? `<small>${esc(c.last_channel)}</small>` : ""}</span>
<span class="crash-health"><span class="pill">${esc(c.severity || "medium")}</span><span class="pill ${c.kind === "crash" ? "crash" : ""}">${esc(c.kind)}</span>${statusPill(c.status)}</span>
<span class="crash-count"><b>${Number(c.affected_installs ?? 0)} ${i18n("installs", "安装")}</b><small>${Number(c.window_events ?? 0)} ${i18n("events", "事件")} · ${Number(c.identity_coverage ?? 0) >= 0.9 && Number(c.dimension_coverage ?? 1) >= 0.9 ? `${Math.round(Number(c.identity_coverage) * 100)}% ${i18n("identified", "已关联")}${c.impact_rate !== null && c.impact_rate !== undefined ? ` · ${(c.impact_rate * 100).toFixed(1)}% ${i18n("impact", "影响率")}` : ""}` : i18n("sample incomplete", "样本不完整")}</small><small>${c.count} ${i18n("lifetime", "累计")}</small></span>
</a>`;
    })
        .join("")}</div>`;
}
export function renderStats(data: {
    daily: Daily[];
    versions: {
        label: string;
        users: number;
    }[];
    platforms: {
        label: string;
        users: number;
    }[];
    crashes: CrashRow[];
    metrics: MetricRow[];
    previousMetrics: MetricRow[];
    metricUsers: MetricRow[];
    metricUsersUnavailable: boolean;
    /** Oldest computed_at in the rollup; empty when the window was queried live. */
    metricUsersComputedAt: string;
    sources: {
        label: string;
        users: number;
    }[];
    diagnosticFacets?: {
        osBuilds: BarRow[];
        osRevisions: BarRow[];
        distros: BarRow[];
        distroVersions: BarRow[];
        kernels: BarRow[];
        sessions: BarRow[];
        architectures: BarRow[];
        channels: BarRow[];
        runtimes: BarRow[];
        runtimeEngines: BarRow[];
        failureKinds: BarRow[];
        failureReasons: BarRow[];
        exitCodes: BarRow[];
        recoveries: BarRow[];
        gpuStates: BarRow[];
    };
    installationLinkedSince?: string;
    overview: OverviewCounts;
    latestVersion: string;
    filters: {
        surface: "desktop" | "cli";
        status: string;
        source: string;
        version: string;
        os: string;
        platform: string;
        osBuild?: string;
        osRevision?: string;
        distroId?: string;
        distroVersion?: string;
        kernelVersion?: string;
        sessionType?: string;
        arch?: string;
        channel?: string;
        runtimeVersion?: string;
        runtimeEngine?: string;
        failureKind?: string;
        failureReason?: string;
        exitCode?: string;
        recovery?: string;
        gpu?: string;
        newLatest: boolean;
        regressed: boolean;
        windowDays: 7 | 30;
        preferenceMode: "users" | "opens";
    };
}, user: User, activeModule: StatsModule = "usage"): string {
    const days = lastDays(data.daily, data.filters.windowDays);
    const range = data.filters.windowDays;
    const rangeText = `${range}d`;
    const diagnosticFacets = data.diagnosticFacets ?? {
        osBuilds: [], osRevisions: [], distros: [], distroVersions: [], kernels: [], sessions: [],
        architectures: [], channels: [], runtimes: [], runtimeEngines: [],
        failureKinds: [], failureReasons: [], exitCodes: [], recoveries: [], gpuStates: [],
    };
    const totalUsers = days.at(-1)?.users ?? 0;
    const anyPing = days.some((d) => d.opens > 0);
    const agentMetrics = data.metrics.filter((r) => AGENT_METRIC_SIGNALS.includes(r.signal));
    const previousAgentMetrics = data.previousMetrics.filter((r) => AGENT_METRIC_SIGNALS.includes(r.signal));
    const agentMetricUsers = data.metricUsers.filter((r) => AGENT_METRIC_SIGNALS.includes(r.signal));
    const isSettingsSignal = (signal: string) => signal === "client_surface" || signal === "client_version" || signal.startsWith("settings_") ||
        ["cli_mode", "cli_profile", "cli_permission_mode", "cli_session_mode"].includes(signal);
    const settingsMetrics = data.metrics.filter((r) => isSettingsSignal(r.signal));
    const settingsMetricUsers = data.metricUsers.filter((r) => isSettingsSignal(r.signal));
    const cache = cacheHitRate(agentMetrics);
    const providerRate = ratioPer100(agentMetrics, "provider_error");
    const toolRate = ratioPer100(agentMetrics, "tool_error");
    const desktopHangs = sumMetric(agentMetrics, "desktop_hang");
    const abnormalExits = agentMetrics
        .filter((r) => r.signal === "desktop_exit" && r.bucket === "abnormal")
        .reduce((sum, r) => sum + r.total, 0);
    const webViewFailures = sumMetric(agentMetrics, "desktop_web_runtime_failure") + sumMetric(agentMetrics, "desktop_webview2_failure");
    const healthWatchCount = [healthLevel("cache", cache), healthLevel("rate", providerRate), healthLevel("rate", toolRate)].filter((v) => v === "warn" || v === "bad").length +
        (desktopHangs > 0 ? 1 : 0) +
        (abnormalExits > 0 ? 1 : 0) +
        (webViewFailures > 0 ? 1 : 0);
    const modulePath = (module: StatsModule) => (module === "usage" ? "/stats" : `/stats/${module}`);
    const filterQS = (patch: Record<string, string>, module: StatsModule = activeModule) => {
        const params = new URLSearchParams();
        const put = (k: string, v: string) => {
            if (v)
                params.set(k, v);
        };
        put("status", data.filters.status);
        put("source", data.filters.source);
        put("version", data.filters.version);
        put("os", data.filters.os);
        put("platform", data.filters.platform);
        put("osBuild", data.filters.osBuild ?? "");
        put("osRevision", data.filters.osRevision ?? "");
        put("distro", data.filters.distroId ?? "");
        put("distroVersion", data.filters.distroVersion ?? "");
        put("kernel", data.filters.kernelVersion ?? "");
        put("session", data.filters.sessionType ?? "");
        put("arch", data.filters.arch ?? "");
        put("channel", data.filters.channel ?? "");
        put("runtime", data.filters.runtimeVersion ?? "");
        put("engine", data.filters.runtimeEngine ?? "");
        put("failureKind", data.filters.failureKind ?? "");
        put("reason", data.filters.failureReason ?? "");
        put("exitCode", data.filters.exitCode ?? "");
        put("recovery", data.filters.recovery ?? "");
        put("gpu", data.filters.gpu ?? "");
        put("surface", data.filters.surface === "cli" ? "cli" : "");
        if (data.filters.newLatest)
            params.set("new", "latest");
        if (data.filters.regressed)
            params.set("regressed", "1");
        if (data.filters.windowDays === 7)
            params.set("window", "7d");
        if (module === "preferences" && data.filters.preferenceMode === "opens")
            params.set("prefs", "opens");
        for (const [k, v] of Object.entries(patch)) {
            if (v)
                params.set(k, v);
            else
                params.delete(k);
        }
        const qs = params.toString();
        const path = modulePath(module);
        return qs ? `${path}?${qs}` : path;
    };
    const clearFiltersHref = filterQS({ status: "", source: "", version: "", os: "", platform: "", osBuild: "", osRevision: "", distro: "", distroVersion: "", kernel: "", session: "", arch: "", channel: "", engine: "", runtime: "", failureKind: "", reason: "", exitCode: "", recovery: "", gpu: "", new: "", regressed: "" });
    const hasFilters = Boolean(data.filters.status || data.filters.source || data.filters.version || data.filters.os || data.filters.platform || data.filters.osBuild || data.filters.osRevision || data.filters.distroId || data.filters.distroVersion || data.filters.kernelVersion || data.filters.sessionType || data.filters.arch || data.filters.channel || data.filters.runtimeEngine || data.filters.runtimeVersion || data.filters.failureKind || data.filters.failureReason || data.filters.exitCode || data.filters.recovery || data.filters.gpu || data.filters.newLatest || data.filters.regressed);
    const windowControls = `<div class="segmented" aria-label="Time window">
<a class="${range === 7 ? "active" : ""}"${range === 7 ? ` aria-current="true"` : ""} href="${esc(filterQS({ window: "7d" }))}">7d</a>
<a class="${range === 30 ? "active" : ""}"${range === 30 ? ` aria-current="true"` : ""} href="${esc(filterQS({ window: "" }))}">30d</a>
</div>`;
    const surfaceControls = `<div class="segmented" aria-label="Client surface">
<a class="${data.filters.surface === "desktop" ? "active" : ""}"${data.filters.surface === "desktop" ? ` aria-current="true"` : ""} href="${esc(filterQS({ surface: "" }))}">${i18n("Desktop", "桌面端")}</a>
<a class="${data.filters.surface === "cli" ? "active" : ""}"${data.filters.surface === "cli" ? ` aria-current="true"` : ""} href="${esc(filterQS({ surface: "cli" }))}">CLI</a>
</div>`;
    const preferenceControls = `<div class="segmented" aria-label="Preference metric mode">
<a class="${data.filters.preferenceMode === "users" ? "active" : ""}"${data.filters.preferenceMode === "users" ? ` aria-current="true"` : ""} href="${esc(filterQS({ prefs: "" }, "preferences"))}">${i18n("Installs", "按安装")}</a>
<a class="${data.filters.preferenceMode === "opens" ? "active" : ""}"${data.filters.preferenceMode === "opens" ? ` aria-current="true"` : ""} href="${esc(filterQS({ prefs: "opens" }, "preferences"))}">${i18n("Opens", "按启动")}</a>
</div>`;
    const overviewTone = topSeverityTone(data.overview.openReports, data.overview.regressedReports, data.overview.criticalOpenReports);
    const isDevelopmentDiagnostic = (row: CrashRow) => row.development ?? row.fingerprint.startsWith("dev:");
    const releaseCrashes = data.crashes.filter((row) => row.kind !== "performance" && row.severity !== "low" && !isDevelopmentDiagnostic(row));
    const performanceDiagnostics = data.crashes.filter((row) => row.kind === "performance" && !isDevelopmentDiagnostic(row));
    const developmentDiagnostics = data.crashes.filter(isDevelopmentDiagnostic);
    const overview = `<section class="overview-grid">
${statCard({ en: "Active today", zh: "今日活跃" }, String(totalUsers), i18n("anonymous installs", "匿名安装"), filterQS({}, "usage"))}
${statCard({ en: "Latest adoption", zh: "最新版本占比" }, latestVersionShare(data.overview.latestAdoptionPct), i18nHTML(`latest ${esc(data.latestVersion || "n/a")}`, `最新 ${esc(data.latestVersion || "n/a")}`), filterQS({}, "usage"))}
${statCard({ en: "Open reports", zh: "未处理报告" }, String(data.overview.openReports), i18n("needs triage", "需要分诊"), filterQS({}, "diagnostics"), overviewTone)}
${statCard({ en: "New in latest", zh: "最新新增" }, String(data.overview.newLatestReports), i18n("first seen on latest", "首次出现在最新版"), filterQS({}, "diagnostics"), data.overview.newLatestReports ? "warn" : "good")}
${statCard({ en: "Regressions", zh: "回归问题" }, String(data.overview.regressedReports), i18n("previously resolved", "曾经解决后复现"), filterQS({}, "diagnostics"), data.overview.regressedReports ? "bad" : "good")}
${statCard({ en: "Agent health", zh: "运行健康" }, healthWatchCount ? String(healthWatchCount) : "OK", i18nHTML(`${pct(cache)} cache · ${providerRate === null ? "n/a" : Number(providerRate.toFixed(1))}/100 provider · ${desktopHangs} hangs`, `${pct(cache)} 缓存 · ${providerRate === null ? "n/a" : Number(providerRate.toFixed(1))}/100 Provider · ${desktopHangs} 次卡死`), filterQS({}, "health"), healthWatchCount ? "warn" : "good")}
</section>`;
    const pageOverview = activeModule === "usage" ? overview : "";
    const dashboardNav = `<nav class="site-nav" aria-label="Stats navigation">
${navLink(filterQS({}, "usage"), { en: "Home", zh: "主页" }, activeModule === "usage")}
${navLink(filterQS({}, "diagnostics"), { en: "Diagnostics", zh: "诊断分诊" }, activeModule === "diagnostics")}
${navLink(filterQS({}, "preferences"), { en: "Preferences", zh: "设置偏好" }, activeModule === "preferences")}
${navLink(filterQS({}, "health"), { en: "Agent Health", zh: "运行健康" }, activeModule === "health")}
</nav>`;
    const linkedSince = data.installationLinkedSince
        ? `<p class="muted">${i18n("Installation-linked data available since", "可关联安装数据起始于")} ${esc(data.installationLinkedSince)}</p>`
        : "";
    const filters = `<div class="filter-card"><div class="filter-head"><h2>${i18n("Report filters", "诊断筛选")}</h2><span>${i18nHTML(`latest ${esc(data.latestVersion || "n/a")}`, `最新 ${esc(data.latestVersion || "n/a")}`)}</span></div>${linkedSince}
<div class="filter-tabs">
${filterTab("All", "全部", clearFiltersHref, !hasFilters)}
${filterTab("Open", "未处理", filterQS({ status: "open" }), data.filters.status === "open")}
${filterTab("Resolved", "已解决", filterQS({ status: "resolved" }), data.filters.status === "resolved")}
${filterTab("Ignored", "已忽略", filterQS({ status: "ignored" }), data.filters.status === "ignored")}
${filterTab("New in latest", "最新新增", filterQS({ new: data.filters.newLatest ? "" : "latest" }), data.filters.newLatest)}
${filterTab("Regressed", "回归", filterQS({ regressed: data.filters.regressed ? "" : "1" }), data.filters.regressed)}
</div>
<div class="facet-grid">
<section><h3>${i18n("Source", "来源")}</h3><div class="facet-list">${facetChips(data.sources, data.filters.source, (label) => filterQS({ source: label }), 4)}</div></section>
<section><h3>${i18n("Version", "版本")}</h3><div class="facet-list">${facetChips(data.versions, data.filters.version, (label) => filterQS({ version: label }), 5)}</div></section>
<section><h3>${i18n("Platform", "平台")}</h3><div class="facet-list">${facetChips(data.platforms, data.filters.platform, (label) => filterQS({ platform: label }), 4)}</div></section>
<section><h3>${i18n("Windows build / revision", "Windows build / revision")}</h3><div class="facet-list">${facetChips(diagnosticFacets.osBuilds, data.filters.osBuild ?? "", (label) => filterQS({ osBuild: label }), 6)}${facetChips(diagnosticFacets.osRevisions, data.filters.osRevision ?? "", (label) => filterQS({ osRevision: label }), 4)}${data.filters.osBuild !== "17763" ? `<a class="facet-chip" href="${esc(filterQS({ osBuild: "17763" }))}"><span class="facet-label">LTSC 2019 · 17763</span></a>` : ""}</div></section>
<section><h3>${i18n("Linux distribution / session", "Linux 发行版 / 会话")}</h3><div class="facet-list">${facetChips(diagnosticFacets.distros, data.filters.distroId ?? "", (label) => filterQS({ distro: label }), 5)}${facetChips(diagnosticFacets.distroVersions, data.filters.distroVersion ?? "", (label) => filterQS({ distroVersion: label }), 4)}${facetChips(diagnosticFacets.kernels, data.filters.kernelVersion ?? "", (label) => filterQS({ kernel: label }), 4)}${facetChips(diagnosticFacets.sessions, data.filters.sessionType ?? "", (label) => filterQS({ session: label }), 4)}</div></section>
<section><h3>${i18n("Architecture / channel", "架构 / 渠道")}</h3><div class="facet-list">${facetChips(diagnosticFacets.architectures, data.filters.arch ?? "", (label) => filterQS({ arch: label }), 4)}${facetChips(diagnosticFacets.channels, data.filters.channel ?? "", (label) => filterQS({ channel: label }), 4)}</div></section>
<section><h3>Web Runtime</h3><div class="facet-list">${facetChips(diagnosticFacets.runtimeEngines, data.filters.runtimeEngine ?? "", (label) => filterQS({ engine: label }), 3)}${facetChips(diagnosticFacets.runtimes, data.filters.runtimeVersion ?? "", (label) => filterQS({ runtime: label }), 5)}</div></section>
<section><h3>${i18n("Failure kind / reason / exit", "故障类型 / 原因 / 退出码")}</h3><div class="facet-list">${facetChips(diagnosticFacets.failureKinds, data.filters.failureKind ?? "", (label) => filterQS({ failureKind: label }), 5)}${facetChips(diagnosticFacets.failureReasons, data.filters.failureReason ?? "", (label) => filterQS({ reason: label }), 5)}${facetChips(diagnosticFacets.exitCodes, data.filters.exitCode ?? "", (label) => filterQS({ exitCode: label }), 4)}</div></section>
<section><h3>${i18n("Recovery / GPU", "恢复 / GPU")}</h3><div class="facet-list">${facetChips(diagnosticFacets.recoveries, data.filters.recovery ?? "", (label) => filterQS({ recovery: label }), 4)}${facetChips(diagnosticFacets.gpuStates, data.filters.gpu ?? "", (label) => filterQS({ gpu: label }), 3)}</div></section>
</div></div>`;
    const usageModule = `<section id="usage" class="card full module-card"><div class="module-head"><div><span>${i18n("Module", "模块")}</span><h2>${i18n("Usage distribution", "使用分布")}</h2></div></div>
<div class="module-panel wide"><h3>${i18nHTML(`Daily active installs <b>— ${rangeText}</b> (solid: users, faded: opens)`, `每日活跃 <b>— ${rangeText}</b>（实线：用户，淡色：打开次数）`)}</h3>
${anyPing ? dailyChart(days) : `<div class="empty">${i18n("No pings yet — data starts flowing once a telemetry-enabled build ships", "暂无启动 ping — 等带统计的版本发布后这里开始有数据")}</div>`}</div>
<div class="module-split">
<section class="module-panel"><h3>${i18nHTML(`Versions <b>— ${rangeText}</b>`, `版本分布 <b>— ${rangeText}</b>`)}</h3>${listBars(data.versions)}</section>
<section class="module-panel"><h3>${i18nHTML(`Platforms <b>— ${rangeText}</b>`, `平台分布 <b>— ${rangeText}</b>`)}</h3>${listBars(data.platforms)}</section>
</div></section>`;
    const diagnosticsModule = `<section id="diagnostics" class="card full module-card"><div class="module-head"><div><span>${i18n("Module", "模块")}</span><h2>${i18n("Diagnostic triage", "诊断分诊")}</h2></div><a class="module-action" href="#top">${i18n("Back to overview", "回到概览")}</a></div>
<p class="sub">${i18n("Installation-linked data is available only from the diagnostics-v2 deployment date; historical device counts are not backfilled.", "可关联安装的数据仅从 diagnostics-v2 部署日起提供；历史设备数不回填。")}</p>
<section class="module-panel"><h3>${i18nHTML("Needs attention <b>— top 10 release crashes and exceptions</b>", "优先处理 <b>— 正式版崩溃与异常 Top 10</b>")}</h3>${reportGroups(releaseCrashes.slice(0, 10), true)}</section>
${performanceDiagnostics.length ? `<section class="module-panel"><h3>${i18nHTML("Performance signals <b>— tracked separately from crashes</b>", "性能信号 <b>— 与崩溃分开统计</b>")}</h3>${reportGroups(performanceDiagnostics.slice(0, 5), true)}</section>` : ""}
${developmentDiagnostics.length ? `<section class="module-panel"><h3>${i18nHTML("Development diagnostics <b>— excluded from release priority</b>", "开发版诊断 <b>— 不计入正式版优先级</b>")}</h3>${reportGroups(developmentDiagnostics.slice(0, 5), true)}</section>` : ""}
${filters}
<section class="module-panel"><h3>${i18nHTML("All report groups <b>— open, regression, severity, count, recency</b>", "全部诊断分组 <b>— 未处理、回归、严重性、次数和最近出现</b>")}</h3>${reportGroups(data.crashes)}</section>
</section>`;
    const sevenDayHref = esc(filterQS({ window: "7d" }, "preferences"));
    const unavailableNotice = range === 30
        ? i18nHTML(`The 30-day deduplication is precomputed hourly and has not reached every signal yet. <a href="${sevenDayHref}">Use 7d</a> meanwhile.`, `30 天去重统计由后台每小时预聚合，目前还没覆盖到全部信号。<a href="${sevenDayHref}">先看 7 天</a>。`)
        : i18nHTML(`The ${rangeText} deduplication did not finish.`, `${rangeText} 的去重统计没能跑完。`);
    // A precomputed window can silently go stale if the rollup cron stops, so the
    // heading carries how old the least recently recomputed signal is.
    const computedAt = data.metricUsersComputedAt
        ? ` <b>${esc(data.metricUsersComputedAt.slice(0, 16).replace("T", " "))}Z</b>`
        : "";
    const healthComputedAt = data.metricUsersComputedAt
        ? ` ${esc(data.metricUsersComputedAt.slice(0, 16).replace("T", " "))}Z`
        : "";
    const installsPanel = preferencePanel(i18nHTML(`Deduplicated installs <b>— ${rangeText}</b>${computedAt ? ` computed${computedAt}` : ""}`, `按安装去重 <b>— ${rangeText}</b>${computedAt ? ` 统计于${computedAt}` : ""}`), data.metricUsersUnavailable
        ? `<div class="empty">${unavailableNotice}</div>`
        : settingsDashboard(settingsMetricUsers, { collapseSections: true }), data.filters.preferenceMode === "users");
    const opensPanel = preferencePanel(i18nHTML(`Launch/open snapshots <b>— ${rangeText}</b>`, `启动/开启快照 <b>— ${rangeText}</b>`), settingsDashboard(settingsMetrics, { collapseSections: true }), data.filters.preferenceMode === "opens");
    const preferencePanels = data.filters.preferenceMode === "opens" ? `${opensPanel}${installsPanel}` : `${installsPanel}${opensPanel}`;
    const preferencesModule = `<section id="preferences" class="card full module-card"><div class="module-head"><div><span>${i18n("Module", "模块")}</span><h2>${i18n("Settings preferences", "设置偏好")}</h2></div><div class="module-actions">${preferenceControls}</div></div>
<div class="preference-compare">${preferencePanels}</div></section>`;
    const healthModule = `<section id="health" class="card full module-card"><div class="module-head"><div><span>${i18n("Module", "模块")}</span><h2>${i18n("Agent health", "运行健康")}</h2></div><div class="module-actions"><a class="module-action" href="${esc(filterQS({}, "preferences"))}">${i18n("Preferences", "设置偏好")}</a></div></div>
<section class="module-panel"><h3>${i18nHTML(`Health summary <b>— ${rangeText}, compared with previous window</b>`, `健康摘要 <b>— ${rangeText}，对比上一窗口</b>`)}</h3>${agentHealth(agentMetrics, previousAgentMetrics)}</section>
<section class="module-panel"><h3>${i18nHTML(`Affected installs <b>— ${rangeText}, deduplicated${healthComputedAt ? `, computed ${healthComputedAt}` : ""}</b>`, `受影响安装 <b>— ${rangeText}，按安装去重${healthComputedAt ? `，统计于 ${healthComputedAt}` : ""}</b>`)}</h3>${data.metricUsersUnavailable
        ? `<div class="empty">${range === 30
            ? i18nHTML(`The 30-day deduplication is not ready. <a href="${esc(filterQS({ window: "7d" }, "health"))}">Use 7d</a> meanwhile.`, `30 天去重统计尚未就绪。<a href="${esc(filterQS({ window: "7d" }, "health"))}">先看 7 天</a>。`)
            : i18n(`The ${rangeText} deduplication did not finish.`, `${rangeText} 的去重统计没能跑完。`)}</div>`
        : metricsCards(agentMetricUsers, ["desktop_hang", "desktop_hang_age", "desktop_web_runtime_failure", "desktop_webview2_failure", "desktop_restore", "desktop_exit"])}</section>
<section class="module-panel"><h3>${i18nHTML(`Signal distributions <b>— ${rangeText}, opt-in aggregate</b>`, `信号分布 <b>— ${rangeText}，opt-in 汇总</b>`)}</h3>${metricsCards(agentMetrics)}</section>
</section>`;
    const activeModuleHTML: Record<StatsModule, string> = {
        diagnostics: diagnosticsModule,
        usage: usageModule,
        preferences: preferencesModule,
        health: healthModule,
    };
    return page("Reasonix · Crash & Telemetry", "health", `${dashboardNav}
<div id="top" class="hero-line"><div><h1>${i18n("Crash & Telemetry", "客户端健康看板")}</h1><p class="sub">${i18nHTML(`${rangeText} window · anonymous launch pings, opt-in aggregate metrics, and user-sent diagnostic reports only`, `${rangeText} 时间窗口 · 仅包含匿名启动 ping、opt-in 汇总指标和用户发送的诊断报告`)}</p></div><div class="module-actions">${surfaceControls}${windowControls}</div></div>
${pageOverview}
<div class="grid">
${activeModuleHTML[activeModule]}
</div>`, userNav(user));
}

