// Last-resort crash surface: a React render error with no boundary unmounts the
// whole tree (blank window), and global errors/rejections leave no trace either.
import { dumpBreadcrumbs } from "./breadcrumbs";
import { sessionPipelineDiagnostics } from "./sessionDiagnostics";
import { clip, formatProfilerFrame } from "./crash";
// __BUILD_COMMIT__ is injected by vite's define (vite.config.ts), not a module
// export, so declare it locally like crash.ts does.
declare const __BUILD_COMMIT__: string;
import { CrashKind, CrashPayload, NormalizedError, PerformanceSnapshot, BrowserPerformanceMemory, longTasks, LONG_TASK_WINDOW_MS, lagSamples, BrowserNavigator, VISIBILITY_RESUME_GRACE_MS, LONG_TASK_PROMPT_MS, LONG_TASK_TOTAL_PROMPT_MS, EVENT_LOOP_LAG_CONSECUTIVE_SAMPLES, EVENT_LOOP_LAG_PROMPT_MS, ProfilerTrace } from "./crash_types";
const PERF_REPORTED_STORAGE_KEY = "reasonix:perf-reported";
// Idempotent per pressure label: once a category is reported (persisted per build) or
// dismissed (session only), stop re-surfacing it so a steady slowdown can't spam prompts.
export const dismissedPerfLabels = new Set<string>();
let reportedPerfLabels: Set<string> | null = null;
export function currentBuildCommit(): string {
    return typeof __BUILD_COMMIT__ === "string" ? __BUILD_COMMIT__ : "dev";
}
export function parseReportedPerf(raw: string | null, build: string): Set<string> {
    if (!raw)
        return new Set();
    try {
        const parsed = JSON.parse(raw) as {
            build?: string;
            labels?: unknown;
        };
        if (parsed.build !== build || !Array.isArray(parsed.labels))
            return new Set();
        return new Set(parsed.labels.filter((label): label is string => typeof label === "string"));
    }
    catch {
        return new Set();
    }
}
export function serializeReportedPerf(labels: ReadonlySet<string>, build: string): string {
    return JSON.stringify({ build, labels: [...labels] });
}
export function getReportedPerfLabels(): Set<string> {
    if (reportedPerfLabels)
        return reportedPerfLabels;
    let raw: string | null = null;
    try {
        raw = typeof localStorage !== "undefined" ? localStorage.getItem(PERF_REPORTED_STORAGE_KEY) : null;
    }
    catch {
        raw = null;
    }
    reportedPerfLabels = parseReportedPerf(raw, currentBuildCommit());
    return reportedPerfLabels;
}
export function markPerfReported(label: string): void {
    const set = getReportedPerfLabels();
    if (set.has(label))
        return;
    set.add(label);
    try {
        if (typeof localStorage !== "undefined") {
            localStorage.setItem(PERF_REPORTED_STORAGE_KEY, serializeReportedPerf(set, currentBuildCommit()));
        }
    }
    catch {
        // localStorage can throw (private mode / quota); the session-level set still dedups.
    }
}
export function currentView(): string {
    if (typeof window === "undefined")
        return "";
    const { protocol, host, pathname, hash } = window.location;
    const safeHash = hash && hash.length < 80 ? hash : "";
    return clip(`${protocol}//${host}${pathname}${safeHash}`, 180);
}
export function kindForLabel(label: string): CrashKind {
    return label === "unhandledrejection" ? "exception" : "crash";
}
export function sourceForLabel(label: string): CrashPayload["source"] {
    if (label === "react")
        return "frontend.react";
    if (label === "window.error" || label === "unhandledrejection")
        return "frontend.global";
    return "frontend";
}
export function formatText(label: string, normalized: NormalizedError, extra?: string): string {
    const detail = normalized.stack || normalized.errorMessage;
    const crumbs = dumpBreadcrumbs();
    const buildCommit = typeof __BUILD_COMMIT__ === "string" ? __BUILD_COMMIT__ : "dev";
    return [`[${label}]`, detail, extra?.trim(), crumbs && `--- breadcrumbs ---\n${crumbs}`, `build ${buildCommit}`]
        .filter(Boolean)
        .join("\n\n");
}
export function fmtNumber(n: number, digits = 0): string {
    return Number.isFinite(n) ? n.toFixed(digits) : "0";
}
function fmtMb(n: number): string {
    return `${fmtNumber(n, 1)} MB`;
}
export function readHeapSnapshot(): PerformanceSnapshot["jsHeap"] | undefined {
    if (typeof performance === "undefined")
        return undefined;
    const memory = (performance as Performance & {
        memory?: BrowserPerformanceMemory;
    }).memory;
    if (!memory?.usedJSHeapSize || !memory.totalJSHeapSize || !memory.jsHeapSizeLimit)
        return undefined;
    const usedMb = memory.usedJSHeapSize / 1024 / 1024;
    const totalMb = memory.totalJSHeapSize / 1024 / 1024;
    const limitMb = memory.jsHeapSizeLimit / 1024 / 1024;
    return {
        usedMb,
        totalMb,
        limitMb,
        usagePercent: limitMb > 0 ? (usedMb / limitMb) * 100 : undefined,
    };
}
export function pruneLongTasks(now = performance.now()): void {
    while (longTasks.length && now - longTasks[0].startMs > LONG_TASK_WINDOW_MS)
        longTasks.shift();
}
export function longTaskSummary(now = performance.now()): PerformanceSnapshot["longTasks"] {
    pruneLongTasks(now);
    if (!longTasks.length)
        return undefined;
    const totalMs = longTasks.reduce((sum, t) => sum + t.durationMs, 0);
    const maxMs = Math.max(...longTasks.map((t) => t.durationMs));
    return {
        count: longTasks.length,
        totalMs,
        maxMs,
        recent: longTasks.slice(-5),
    };
}
function eventLoopLagSummary(currentMs = 0): PerformanceSnapshot["eventLoopLag"] {
    const samples = lagSamples.filter((n) => n > 0);
    if (!samples.length && currentMs <= 0)
        return undefined;
    const all = currentMs > 0 ? [...samples, currentMs] : samples;
    const total = all.reduce((sum, n) => sum + n, 0);
    return {
        currentMs,
        maxMs: Math.max(...all),
        avgMs: total / all.length,
        samples: all.length,
    };
}
function networkSnapshot(): PerformanceSnapshot["connection"] {
    if (typeof navigator === "undefined")
        return undefined;
    const connection = (navigator as BrowserNavigator).connection;
    if (!connection)
        return undefined;
    return {
        effectiveType: connection.effectiveType,
        downlinkMbps: connection.downlink,
        rttMs: connection.rtt,
        saveData: connection.saveData,
    };
}
export function performanceSnapshot(reason: string, currentLagMs = 0): PerformanceSnapshot {
    const nav = typeof navigator === "undefined" ? undefined : (navigator as BrowserNavigator);
    const doc = typeof document === "undefined" ? undefined : document;
    const pipeline = sessionPipelineDiagnostics();
    return {
        reason,
        uptimeMs: typeof performance !== "undefined" ? performance.now() : 0,
        visibility: doc?.visibilityState ?? "",
        focused: doc?.hasFocus?.() ?? false,
        online: nav?.onLine ?? true,
        hardwareConcurrency: nav?.hardwareConcurrency ?? 0,
        deviceMemoryGb: nav?.deviceMemory,
        jsHeap: readHeapSnapshot(),
        eventLoopLag: eventLoopLagSummary(currentLagMs),
        longTasks: typeof performance !== "undefined" ? longTaskSummary() : undefined,
        connection: networkSnapshot(),
        sessionPipeline: Object.keys(pipeline).length > 0 ? pipeline : undefined,
    };
}
export function formatPerformanceContext(snapshot: PerformanceSnapshot): string {
    const lines = [
        `reason: ${snapshot.reason}`,
        `uptime: ${fmtNumber(snapshot.uptimeMs / 1000, 1)}s`,
        `visibility: ${snapshot.visibility || "unknown"}`,
        `focused: ${snapshot.focused ? "true" : "false"}`,
        `online: ${snapshot.online ? "true" : "false"}`,
        `hardware concurrency: ${snapshot.hardwareConcurrency || "unknown"}`,
    ];
    if (snapshot.deviceMemoryGb)
        lines.push(`device memory: ${snapshot.deviceMemoryGb} GB`);
    if (snapshot.jsHeap) {
        const pct = snapshot.jsHeap.usagePercent !== undefined ? `, ${fmtNumber(snapshot.jsHeap.usagePercent)}% of limit` : "";
        lines.push(`js heap: ${fmtMb(snapshot.jsHeap.usedMb)} used, ${fmtMb(snapshot.jsHeap.totalMb)} allocated, ${fmtMb(snapshot.jsHeap.limitMb)} limit${pct}`);
    }
    if (snapshot.eventLoopLag) {
        lines.push(`event loop lag: current ${fmtNumber(snapshot.eventLoopLag.currentMs)}ms, max ${fmtNumber(snapshot.eventLoopLag.maxMs)}ms, avg ${fmtNumber(snapshot.eventLoopLag.avgMs)}ms over ${snapshot.eventLoopLag.samples} samples`);
    }
    if (snapshot.longTasks) {
        const recent = snapshot.longTasks.recent
            .map((t) => `${fmtNumber(t.durationMs)}ms @ ${fmtNumber(t.startMs / 1000, 1)}s${t.attribution ? ` (${t.attribution})` : ""}`)
            .join("; ");
        lines.push(`long tasks: ${snapshot.longTasks.count} in the last 60s, max ${fmtNumber(snapshot.longTasks.maxMs)}ms, total ${fmtNumber(snapshot.longTasks.totalMs)}ms`);
        if (recent)
            lines.push(`recent long tasks: ${recent}`);
    }
    if (snapshot.longTaskFrames?.length) {
        lines.push("long task top frames (sampled):");
        for (const frame of snapshot.longTaskFrames)
            lines.push(`  ${frame.samples}x ${frame.label}`);
    }
    if (snapshot.connection) {
        const parts = [
            snapshot.connection.effectiveType,
            snapshot.connection.rttMs !== undefined ? `${snapshot.connection.rttMs}ms rtt` : "",
            snapshot.connection.downlinkMbps !== undefined ? `${snapshot.connection.downlinkMbps} Mbps` : "",
            snapshot.connection.saveData !== undefined ? `saveData ${snapshot.connection.saveData ? "true" : "false"}` : "",
        ].filter(Boolean);
        if (parts.length)
            lines.push(`connection: ${parts.join(", ")}`);
    }
    const pipeline = snapshot.sessionPipeline;
    if (pipeline?.activation) {
        const a = pipeline.activation;
        const parts = [`request ${a.requestId}`];
        if (a.tabId)
            parts.push(`tab ${a.tabId}`);
        if (a.ticketToStartingMs !== undefined)
            parts.push(`ticket→starting ${fmtNumber(a.ticketToStartingMs)}ms`);
        if (a.startingToReadyMs !== undefined)
            parts.push(`starting→ready ${fmtNumber(a.startingToReadyMs)}ms`);
        if (a.totalMs !== undefined)
            parts.push(`total ${fmtNumber(a.totalMs)}ms`);
        if (a.outcome)
            parts.push(`outcome ${a.outcome}`);
        if (a.failureClass)
            parts.push(`failure ${a.failureClass}`);
        lines.push(`activation: ${parts.join(", ")}`);
    }
    if (pipeline?.history) {
        const h = pipeline.history;
        lines.push(`history page: ${h.entries} entries, ${fmtNumber(h.inlineBytes / 1024, 1)} KiB inline, ${fmtNumber(h.durationMs)}ms, source ${h.source || "unknown"}${h.stale ? ", stale" : ""} ` +
            `(pages ${h.pages}, stale ${h.staleCount}, index hits ${h.indexHits}, misses ${h.indexMisses})`);
    }
    if (pipeline?.mountedRows) {
        lines.push(`mounted rows: ${pipeline.mountedRows.mounted} of ${pipeline.mountedRows.total}`);
    }
    if (pipeline?.markdownWorker) {
        const w = pipeline.markdownWorker;
        lines.push(`markdown worker: ${w.pending} pending, ${w.completed} parsed, avg ${fmtNumber(w.avgParseMs, 1)}ms, max ${fmtNumber(w.maxParseMs)}ms` +
            `${w.fallbackActive ? ", fallback active" : ""}${w.workerFailures > 0 ? `, ${w.workerFailures} worker failures` : ""}`);
    }
    if (pipeline?.transcriptCache) {
        const c = pipeline.transcriptCache;
        lines.push(`transcript cache: ${c.residentSessions}/${c.maxResidentSessions} resident sessions, ` +
            `bodies ${fmtMb(c.bodyBytes / 1048576)} of ${fmtMb(c.bodyBudgetBytes / 1048576)}, ` +
            `markdown ${fmtMb(c.markdownBytes / 1048576)} of ${fmtMb(c.markdownBudgetBytes / 1048576)}, ` +
            `evictions ${c.historyEvictions} history + ${c.markdownEvictions} markdown`);
    }
    return lines.join("\n");
}
export function performanceLabelForReason(reason: string): string {
    const normalized = reason.trim().toLowerCase();
    if (normalized.startsWith("event loop lag"))
        return "performance.lag";
    if (normalized.startsWith("long task"))
        return "performance.longtask";
    if (normalized.startsWith("js heap"))
        return "performance.heap";
    return "performance.pressure";
}
export function performanceFingerprintHintForReason(reason: string): string | undefined {
    const normalized = reason.trim().toLowerCase();
    if (!normalized.startsWith("js heap"))
        return undefined;
    const match = normalized.match(/(\d+(?:\.\d+)?)%/);
    const percent = match ? Number(match[1]) : Number.NaN;
    if (!Number.isFinite(percent))
        return "frontend.performance.heap.unknown";
    return percent >= 95
        ? "frontend.performance.heap.critical"
        : "frontend.performance.heap.high";
}
export function shouldRecordLongTaskSample(startMs: number, durationMs: number, graceUntilMs: number, visibilityHidden = false, visibleSinceMs = 0, focused = true): boolean {
    if (!focused)
        return false;
    if (visibilityHidden)
        return false;
    return durationMs >= 50 && startMs >= graceUntilMs && startMs - visibleSinceMs >= VISIBILITY_RESUME_GRACE_MS;
}
export function shouldPromptForLongTasks(summary: {
    count: number;
    totalMs: number;
    maxMs: number;
}): boolean {
    return summary.maxMs >= LONG_TASK_PROMPT_MS || (summary.count >= 3 && summary.totalMs >= LONG_TASK_TOTAL_PROMPT_MS);
}
export function shouldPromptForEventLoopLag(samples: readonly number[], longTask?: {
    count: number;
    totalMs: number;
    maxMs: number;
}): boolean {
    const recent = samples.slice(-EVENT_LOOP_LAG_CONSECUTIVE_SAMPLES);
    const sustained = recent.length === EVENT_LOOP_LAG_CONSECUTIVE_SAMPLES &&
        recent.every((sample) => sample >= EVENT_LOOP_LAG_PROMPT_MS);
    const current = samples.length ? samples[samples.length - 1] : 0;
    const corroborated = current >= EVENT_LOOP_LAG_PROMPT_MS && Boolean(longTask && shouldPromptForLongTasks(longTask));
    return sustained || corroborated;
}
export type TaskAttributionLike = {
    containerType?: string;
    containerName?: string;
    containerId?: string;
    containerSrc?: string;
};
// Longtask entries carry no stacks, only a culprit descriptor ("self", "same-origin",
// iframe container, ...). "self" and "unknown" are the expected no-signal cases, so
// only anomalies (cross-context culprits, named containers) make it into the report.
export function formatLongTaskAttribution(entryName?: string, attribution?: TaskAttributionLike[]): string {
    const parts: string[] = [];
    if (entryName && entryName !== "unknown" && entryName !== "self")
        parts.push(entryName);
    const culprit = attribution?.[0];
    if (culprit) {
        const container = culprit.containerName || culprit.containerId || culprit.containerSrc || "";
        const containerType = culprit.containerType && culprit.containerType !== "window" ? culprit.containerType : "";
        const detail = [containerType, container].filter(Boolean).join(":");
        if (detail)
            parts.push(detail);
    }
    return parts.join(" ");
}
// Self-time view of a self-profiling trace: count each sample that landed inside a
// long-task window against its leaf frame, so the report names the code that was
// actually on-CPU while the UI was blocked.
export function aggregateLongTaskProfile(trace: ProfilerTrace, windows: {
    startMs: number;
    durationMs: number;
}[], maxFrames = 8): {
    label: string;
    samples: number;
}[] {
    if (!windows.length)
        return [];
    const counts = new Map<number, number>();
    for (const sample of trace.samples ?? []) {
        if (sample.stackId === undefined)
            continue;
        const inWindow = windows.some((w) => sample.timestamp >= w.startMs && sample.timestamp <= w.startMs + w.durationMs);
        if (!inWindow)
            continue;
        const stack = trace.stacks?.[sample.stackId];
        if (!stack)
            continue;
        counts.set(stack.frameId, (counts.get(stack.frameId) ?? 0) + 1);
    }
    return [...counts.entries()]
        .sort((a, b) => b[1] - a[1])
        .slice(0, maxFrames)
        .map(([frameId, samples]) => ({ label: formatProfilerFrame(trace, frameId), samples }));
}

