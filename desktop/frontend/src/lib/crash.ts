// Last-resort crash surface: a React render error with no boundary unmounts the
// whole tree (blank window), and global errors/rejections leave no trace either.
import { addBreadcrumb, dumpBreadcrumbs, snapshotBreadcrumbs } from "./breadcrumbs";
import { writeClipboardText } from "./clipboard";
import { t } from "./i18n";
import { PerformanceSnapshot, CrashPayload, ProfilerTrace, NormalizedError, STARTUP_GRACE_MS, PROMPT_COOLDOWN_MS, MAX_LAG_SAMPLES, VISIBILITY_RESUME_GRACE_MS, longTasks, lagSamples, performanceMonitorInstalled, lastPerformancePromptAt, activeProfiler, startLongTaskProfiler, setActiveProfiler, setLastPerformancePromptAt, setPerformanceMonitorInstalled } from "./crash_types";
import { formatPerformanceContext, performanceLabelForReason, performanceFingerprintHintForReason, shouldRecordLongTaskSample, shouldPromptForLongTasks, shouldPromptForEventLoopLag, formatLongTaskAttribution, aggregateLongTaskProfile, dismissedPerfLabels, currentBuildCommit, getReportedPerfLabels, markPerfReported, currentView, kindForLabel, sourceForLabel, formatText, fmtNumber, readHeapSnapshot, pruneLongTasks, longTaskSummary, performanceSnapshot, TaskAttributionLike } from "./crash_perf";
declare const __BUILD_COMMIT__: string;
declare const __BUILD_CHANNEL__: string;
async function collectLongTaskFrames(windows: {
    startMs: number;
    durationMs: number;
}[]): Promise<{
    label: string;
    samples: number;
}[]> {
    const profiler = activeProfiler;
    if (!profiler)
        return [];
    setActiveProfiler(null);
    try {
        const trace = await profiler.stop();
        return aggregateLongTaskProfile(trace, windows);
    }
    catch {
        return [];
    }
    finally {
        startLongTaskProfiler();
    }
}
export function clip(s: string, n: number): string {
    return s.length > n ? s.slice(0, n) : s;
}
function safeStringify(value: unknown): string {
    try {
        return JSON.stringify(value);
    }
    catch {
        return String(value);
    }
}
export function normalizeCrashError(err: unknown): NormalizedError {
    if (err instanceof Error) {
        return {
            errorType: err.name || "Error",
            errorMessage: err.message || String(err),
            stack: err.stack,
        };
    }
    if (typeof err === "string") {
        return { errorType: "string", errorMessage: err };
    }
    if (err && typeof err === "object") {
        const obj = err as {
            name?: unknown;
            message?: unknown;
            stack?: unknown;
            constructor?: {
                name?: string;
            };
        };
        const errorType = typeof obj.name === "string" && obj.name ? obj.name : obj.constructor?.name || "object";
        const errorMessage = typeof obj.message === "string" && obj.message ? obj.message : clip(safeStringify(err), 1000);
        return {
            errorType,
            errorMessage,
            stack: typeof obj.stack === "string" ? obj.stack : undefined,
        };
    }
    return { errorType: typeof err, errorMessage: String(err) };
}
export function topFrameFromStack(stack?: string): string {
    if (!stack)
        return "";
    const lines = stack
        .split("\n")
        .map((l) => l.trim())
        .filter(Boolean);
    return lines.find((l) => /\b(src|assets|wails|frontend)\b|\.tsx?:|\.jsx?:/.test(l)) ?? lines[1] ?? lines[0] ?? "";
}
export function formatProfilerFrame(trace: ProfilerTrace, frameId: number): string {
    const frame = trace.frames?.[frameId];
    if (!frame)
        return `frame#${frameId}`;
    const name = frame.name || "(anonymous)";
    const resource = frame.resourceId !== undefined ? trace.resources?.[frame.resourceId] : undefined;
    if (!resource)
        return name;
    const line = frame.line !== undefined ? `:${frame.line}${frame.column !== undefined ? `:${frame.column}` : ""}` : "";
    return `${name} (${resource}${line})`;
}
export function shouldRecordEventLoopLagSample(visibilityHidden: boolean, msSinceVisible: number, focused = true, msSinceFocused = msSinceVisible): boolean {
    if (!focused)
        return false;
    if (visibilityHidden)
        return false;
    return msSinceVisible >= VISIBILITY_RESUME_GRACE_MS && msSinceFocused >= VISIBILITY_RESUME_GRACE_MS;
}
export function buildPerformancePayload(snapshot: PerformanceSnapshot): CrashPayload {
    const buildCommit = typeof __BUILD_COMMIT__ === "string" ? __BUILD_COMMIT__ : "dev";
    const context = formatPerformanceContext(snapshot);
    const crumbs = dumpBreadcrumbs();
    const label = performanceLabelForReason(snapshot.reason);
    const errorMessage = "UI responsiveness degraded because the app observed long tasks, event-loop lag, or high JS heap pressure.";
    return {
        schemaVersion: 2,
        source: "frontend.performance",
        kind: "performance",
        label,
        message: [
            `[${label}]`,
            errorMessage,
            `--- performance context ---\n${context}`,
            crumbs && `--- breadcrumbs ---\n${crumbs}`,
            `build ${buildCommit}`,
        ]
            .filter(Boolean)
            .join("\n\n"),
        errorType: "PerformancePressure",
        errorMessage,
        topFrame: "frontend.performance",
        fingerprintHint: performanceFingerprintHintForReason(snapshot.reason),
        buildCommit,
        channel: typeof __BUILD_CHANNEL__ === "string" ? __BUILD_CHANNEL__ : "",
        language: typeof navigator !== "undefined" ? navigator.language || "" : "",
        view: currentView(),
        breadcrumbs: snapshotBreadcrumbs(),
        occurredAt: new Date().toISOString(),
    };
}
export function buildCrashPayload(label: string, err: unknown, extra?: string): CrashPayload {
    const normalized = normalizeCrashError(err);
    const buildCommit = typeof __BUILD_COMMIT__ === "string" ? __BUILD_COMMIT__ : "dev";
    return {
        schemaVersion: 2,
        source: sourceForLabel(label),
        kind: kindForLabel(label),
        label,
        message: formatText(label, normalized, extra),
        errorType: normalized.errorType,
        errorMessage: normalized.errorMessage,
        stack: normalized.stack,
        componentStack: extra?.trim() || undefined,
        topFrame: topFrameFromStack(normalized.stack || extra),
        buildCommit,
        channel: typeof __BUILD_CHANNEL__ === "string" ? __BUILD_CHANNEL__ : "",
        language: typeof navigator !== "undefined" ? navigator.language || "" : "",
        view: currentView(),
        breadcrumbs: snapshotBreadcrumbs(),
        occurredAt: new Date().toISOString(),
    };
}
export function opaqueScriptFingerprintHint(rawView = currentView(), breadcrumbs = snapshotBreadcrumbs(), buildCommit = currentBuildCommit()): string {
    const view = rawView
        .replace(/[?#].*$/, "")
        .replace(/\b[0-9a-f]{8,}\b/gi, "_")
        .replace(/\/\d+(?=\/|$)/g, "/_");
    const categories = breadcrumbs
        .slice(-8)
        .map((crumb) => crumb.cat?.trim().toLowerCase().replace(/[^a-z0-9_.-]+/g, "_") ?? "")
        .filter(Boolean)
        .join(">");
    return clip(`build:${buildCommit.slice(0, 16)}|view:${view}|cats:${categories || "none"}`, 300);
}
function sendButton(payload: CrashPayload, className = "crash-overlay__send", onSent?: () => void): HTMLButtonElement | null {
    // Resolved at click time via window.go, not the bridge module: this overlay must
    // stay usable even when the rest of the app (and its imports) is broken.
    const report = window.go?.main?.App?.ReportCrash;
    if (!report)
        return null;
    const send = document.createElement("button");
    send.className = className;
    send.textContent = t("crash.send");
    send.onclick = async () => {
        send.disabled = true;
        send.textContent = t("crash.sending");
        try {
            await report(payload.kind, JSON.stringify(payload));
            send.textContent = t("crash.sent");
            onSent?.();
        }
        catch (err) {
            send.textContent = t("crash.sendFailed");
            send.title = err instanceof Error ? err.message : String(err);
            send.disabled = false;
        }
    };
    return send;
}
const COPY_FEEDBACK_MS = 2000;
function copyButton(text: string, className: string): HTMLButtonElement {
    const copy = document.createElement("button");
    copy.className = className;
    copy.textContent = t("crash.copy");
    copy.onclick = async () => {
        copy.disabled = true;
        let copied = false;
        // The crash overlay is the last-resort surface, so the button must re-enable
        // even if the clipboard path throws unexpectedly — a stuck disabled Copy is
        // exactly the #6388 unresponsive symptom. Catch so a rejection can't escape as
        // an unhandledrejection into the global crash handler either.
        try {
            copied = await writeClipboardText(text);
        }
        catch {
            copied = false;
        }
        finally {
            copy.textContent = copied ? t("crash.copied") : t("crash.copyFailed");
            copy.disabled = false;
            window.setTimeout(() => {
                copy.textContent = t("crash.copy");
            }, COPY_FEEDBACK_MS);
        }
    };
    return copy;
}
function paintPerformancePrompt(payload: CrashPayload, snapshot: PerformanceSnapshot) {
    if (typeof document === "undefined")
        return;
    let host = document.getElementById("performance-report-prompt");
    if (!host) {
        host = document.createElement("div");
        host.id = "performance-report-prompt";
        document.body.appendChild(host);
    }
    const title = document.createElement("div");
    title.className = "performance-report__title";
    title.textContent = t("performanceReport.title");
    const body = document.createElement("pre");
    body.className = "performance-report__body";
    body.textContent = formatPerformanceContext(snapshot);
    const actions = document.createElement("div");
    actions.className = "performance-report__actions";
    const send = sendButton(payload, "performance-report__send", () => markPerfReported(payload.label));
    const copy = copyButton(payload.message, "performance-report__copy");
    const dismiss = document.createElement("button");
    dismiss.className = "performance-report__dismiss";
    dismiss.textContent = t("performanceReport.dismiss");
    dismiss.onclick = () => {
        dismissedPerfLabels.add(payload.label);
        host?.remove();
    };
    if (send)
        actions.append(send);
    actions.append(copy, dismiss);
    const note = document.createElement("div");
    note.className = "performance-report__note";
    note.textContent = t("performanceReport.privacyNote");
    host.replaceChildren(title, body, actions, note);
}
function paint(payload: CrashPayload) {
    let host = document.getElementById("crash-overlay");
    if (!host) {
        host = document.createElement("div");
        host.id = "crash-overlay";
        document.body.appendChild(host);
    }
    const title = document.createElement("div");
    title.className = "crash-overlay__title";
    title.textContent = t("crash.title");
    const body = document.createElement("pre");
    body.className = "crash-overlay__body";
    body.textContent = payload.message;
    const copy = copyButton(payload.message, "crash-overlay__copy");
    const actions = document.createElement("div");
    actions.className = "crash-overlay__actions";
    const send = sendButton(payload);
    if (send)
        actions.append(send);
    actions.append(copy);
    const note = document.createElement("div");
    note.className = "crash-overlay__note";
    note.textContent = t("crash.privacyNote");
    host.replaceChildren(title, body, actions, ...(send ? [note] : []));
}
export function reportCrash(label: string, err: unknown, extra?: string) {
    paint(buildCrashPayload(label, err, extra));
}
type GlobalCrashEventLike = Pick<Event, "defaultPrevented"> & {
    message?: unknown;
    error?: unknown;
    filename?: unknown;
    lineno?: unknown;
    colno?: unknown;
};
const RESIZE_OBSERVER_LOOP_MESSAGE_RE = /^ResizeObserver loop (?:limit exceeded|completed with undelivered notifications\.?)$/;
const OPAQUE_SCRIPT_ERROR_MESSAGE = "Script error.";
function globalCrashEventMessages(e: GlobalCrashEventLike): string[] {
    const messages: string[] = [];
    const pushMessage = (message: string) => {
        const trimmed = message.trim();
        if (trimmed)
            messages.push(trimmed);
    };
    if (typeof e.message === "string")
        pushMessage(e.message);
    const error = e.error;
    if (typeof error === "string")
        pushMessage(error);
    if (error && typeof error === "object" && "message" in error) {
        const msg = (error as {
            message?: unknown;
        }).message;
        if (typeof msg === "string")
            pushMessage(msg);
    }
    return messages;
}
export function shouldReportGlobalCrashEvent(e: GlobalCrashEventLike): boolean {
    if (e.defaultPrevented)
        return false;
    if (globalCrashEventMessages(e).some((message) => RESIZE_OBSERVER_LOOP_MESSAGE_RE.test(message)))
        return false;
    if (globalCrashEventMessages(e).some((message) => /Minified React error #520\b/.test(message)))
        return false;
    return true;
}
export function isOpaqueScriptErrorEvent(e: GlobalCrashEventLike): boolean {
    return ((e.error === undefined || e.error === null) &&
        typeof e.message === "string" &&
        e.message.trim() === OPAQUE_SCRIPT_ERROR_MESSAGE &&
        globalScriptErrorLocation(e) === "");
}
function globalScriptErrorLocation(e: GlobalCrashEventLike): string {
    const parts: string[] = [];
    if (typeof e.filename === "string" && e.filename.trim())
        parts.push(`filename=${e.filename.trim()}`);
    if (typeof e.lineno === "number" && Number.isFinite(e.lineno) && e.lineno > 0)
        parts.push(`lineno=${e.lineno}`);
    if (typeof e.colno === "number" && Number.isFinite(e.colno) && e.colno > 0)
        parts.push(`colno=${e.colno}`);
    return parts.join(" ");
}
export function globalCrashReportReason(e: GlobalCrashEventLike): unknown {
    if (e.error !== undefined && e.error !== null)
        return e.error;
    const message = typeof e.message === "string" ? e.message.trim() : e.message;
    if (message === OPAQUE_SCRIPT_ERROR_MESSAGE) {
        const location = globalScriptErrorLocation(e);
        if (location)
            return `${OPAQUE_SCRIPT_ERROR_MESSAGE}\n${location}`;
    }
    return e.message;
}
export function shouldPromptForPerformanceLabel(alreadyHandled: boolean, msSinceLastPrompt: number, visibilityHidden: boolean, focused = true): boolean {
    if (alreadyHandled)
        return false;
    if (msSinceLastPrompt < PROMPT_COOLDOWN_MS)
        return false;
    if (visibilityHidden)
        return false;
    if (!focused)
        return false;
    return true;
}
function isPerfLabelHandled(label: string): boolean {
    return dismissedPerfLabels.has(label) || getReportedPerfLabels().has(label);
}
function shouldPromptForPerformance(now: number, label: string): boolean {
    const hidden = typeof document !== "undefined" && document.visibilityState === "hidden";
    const focused = typeof document === "undefined" || document.hasFocus?.() !== false;
    return shouldPromptForPerformanceLabel(isPerfLabelHandled(label), now - lastPerformancePromptAt, hidden, focused);
}
function promptPerformanceReport(reason: string, currentLagMs = 0): void {
    const now = Date.now();
    const label = performanceLabelForReason(reason);
    if (!shouldPromptForPerformance(now, label))
        return;
    setLastPerformancePromptAt(now);
    addBreadcrumb("performance", reason);
    const snapshot = performanceSnapshot(reason, currentLagMs);
    if (!activeProfiler) {
        paintPerformancePrompt(buildPerformancePayload(snapshot), snapshot);
        return;
    }
    // Attribute samples to the blocked spans: every recorded long task, plus the lag
    // spike itself for event-loop reports (profiler timestamps share performance.now()'s origin).
    const windows = [...longTasks];
    if (currentLagMs > 0) {
        const nowMs = performance.now();
        windows.push({ startMs: Math.max(0, nowMs - currentLagMs), durationMs: currentLagMs });
    }
    void collectLongTaskFrames(windows).then((frames) => {
        if (frames.length)
            snapshot.longTaskFrames = frames;
        paintPerformancePrompt(buildPerformancePayload(snapshot), snapshot);
    });
}
function maybePromptForHeapPressure(): void {
    const heap = readHeapSnapshot();
    if (!heap?.usagePercent)
        return;
    if (heap.usedMb >= 512 && heap.usagePercent >= 85) {
        promptPerformanceReport(`js heap ${fmtNumber(heap.usagePercent)}% of limit`);
    }
}
export function installPerformancePressureMonitor() {
    if (performanceMonitorInstalled || typeof window === "undefined" || typeof performance === "undefined")
        return;
    if (!window.runtime)
        return;
    setPerformanceMonitorInstalled(true);
    const startedAt = performance.now();
    const graceUntil = startedAt + STARTUP_GRACE_MS;
    const isHidden = () => typeof document !== "undefined" && document.visibilityState === "hidden";
    const isFocused = () => typeof document === "undefined" || document.hasFocus?.() !== false;
    let visibleSince = isHidden() ? Number.POSITIVE_INFINITY : startedAt;
    let focusedSince = isFocused() ? startedAt : Number.POSITIVE_INFINITY;
    let expected = performance.now() + 1000;
    let eventLoopLagPrimed = false;
    // When the view is shown or focused again, overdue timer callbacks can run before
    // the queued visibilitychange/focus task, so visibleSince/focusedSince may still
    // describe the previous settled period at that point. The sampler tracks hidden and
    // unfocused observations itself and restarts both windows on the first settled tick
    // instead of trusting the listener-maintained timestamps.
    let pendingResume = isHidden() || !isFocused();
    const pastGrace = () => performance.now() >= graceUntil;
    const inspectLongTasks = () => {
        if (!pastGrace())
            return;
        const summary = longTaskSummary();
        if (!summary)
            return;
        if (shouldPromptForLongTasks(summary)) {
            promptPerformanceReport(`long task ${fmtNumber(summary.maxMs)}ms`);
        }
    };
    startLongTaskProfiler();
    // Blur/hide park the timestamps at +Infinity so a stale read before the matching
    // resume listener has run can never satisfy the grace windows.
    const resetSamples = () => {
        const now = performance.now();
        longTasks.length = 0;
        lagSamples.length = 0;
        expected = now + 1000;
        eventLoopLagPrimed = false;
        visibleSince = isHidden() ? Number.POSITIVE_INFINITY : now;
        focusedSince = isFocused() ? now : Number.POSITIVE_INFINITY;
        pendingResume = isHidden() || !isFocused();
    };
    if (typeof document !== "undefined") {
        document.addEventListener("visibilitychange", resetSamples);
    }
    window.addEventListener("focus", resetSamples);
    window.addEventListener("blur", resetSamples);
    if (typeof PerformanceObserver !== "undefined") {
        try {
            const observer = new PerformanceObserver((list) => {
                for (const entry of list.getEntries()) {
                    if (!shouldRecordLongTaskSample(entry.startTime, entry.duration, graceUntil, isHidden(), visibleSince, isFocused()))
                        continue;
                    const attribution = formatLongTaskAttribution(entry.name, (entry as PerformanceEntry & {
                        attribution?: TaskAttributionLike[];
                    }).attribution);
                    longTasks.push({
                        startMs: Math.round(entry.startTime),
                        durationMs: Math.round(entry.duration),
                        ...(attribution ? { attribution } : {}),
                    });
                }
                pruneLongTasks();
                inspectLongTasks();
            });
            observer.observe({ entryTypes: ["longtask"] });
        }
        catch {
            // Some WebViews expose PerformanceObserver without the longtask entry type.
        }
    }
    window.setInterval(() => {
        const now = performance.now();
        if (isHidden() || !isFocused()) {
            pendingResume = true;
        }
        else if (pendingResume) {
            pendingResume = false;
            visibleSince = now;
            focusedSince = now;
            longTasks.length = 0;
            lagSamples.length = 0;
            expected = now + 1000;
            eventLoopLagPrimed = false;
            return;
        }
        if (!pastGrace()) {
            expected = now + 1000;
            return;
        }
        if (!eventLoopLagPrimed) {
            expected = now + 1000;
            eventLoopLagPrimed = true;
            return;
        }
        const lagMs = Math.max(0, now - expected);
        expected = now + 1000;
        if (!shouldRecordEventLoopLagSample(isHidden(), now - visibleSince, isFocused(), now - focusedSince))
            return;
        lagSamples.push(lagMs);
        if (lagSamples.length > MAX_LAG_SAMPLES)
            lagSamples.shift();
        if (shouldPromptForEventLoopLag(lagSamples, longTaskSummary(now))) {
            promptPerformanceReport(`event loop lag ${fmtNumber(lagMs)}ms`, lagMs);
        }
        maybePromptForHeapPressure();
    }, 1000);
}
export function installGlobalCrashHandlers() {
    window.addEventListener("error", (e) => {
        if (!shouldReportGlobalCrashEvent(e))
            return;
        const payload = buildCrashPayload("window.error", globalCrashReportReason(e));
        if (isOpaqueScriptErrorEvent(e))
            payload.fingerprintHint = opaqueScriptFingerprintHint();
        paint(payload);
    });
    window.addEventListener("unhandledrejection", (e) => {
        if (shouldReportGlobalCrashEvent(e))
            reportCrash("unhandledrejection", e.reason);
    });
}
export type { CrashKind as CrashKind } from "./crash_types";
export type { PerformanceSnapshot as PerformanceSnapshot } from "./crash_types";
export type { CrashPayload as CrashPayload } from "./crash_types";
export type { ProfilerTrace as ProfilerTrace } from "./crash_types";
export type { NormalizedError as NormalizedError } from "./crash_types";
export type { BrowserPerformanceMemory as BrowserPerformanceMemory } from "./crash_types";
export type { BrowserNavigator as BrowserNavigator } from "./crash_types";
export { LONG_TASK_WINDOW_MS as LONG_TASK_WINDOW_MS } from "./crash_types";
export { LONG_TASK_PROMPT_MS as LONG_TASK_PROMPT_MS } from "./crash_types";
export { LONG_TASK_TOTAL_PROMPT_MS as LONG_TASK_TOTAL_PROMPT_MS } from "./crash_types";
export { EVENT_LOOP_LAG_PROMPT_MS as EVENT_LOOP_LAG_PROMPT_MS } from "./crash_types";
export { EVENT_LOOP_LAG_CONSECUTIVE_SAMPLES as EVENT_LOOP_LAG_CONSECUTIVE_SAMPLES } from "./crash_types";
export { STARTUP_GRACE_MS as STARTUP_GRACE_MS } from "./crash_types";
export { PROMPT_COOLDOWN_MS as PROMPT_COOLDOWN_MS } from "./crash_types";
export { MAX_LAG_SAMPLES as MAX_LAG_SAMPLES } from "./crash_types";
export { VISIBILITY_RESUME_GRACE_MS as VISIBILITY_RESUME_GRACE_MS } from "./crash_types";
export { longTasks as longTasks } from "./crash_types";
export { lagSamples as lagSamples } from "./crash_types";
export { performanceMonitorInstalled as performanceMonitorInstalled } from "./crash_types";
export { lastPerformancePromptAt as lastPerformancePromptAt } from "./crash_types";
export { activeProfiler as activeProfiler } from "./crash_types";
export { startLongTaskProfiler as startLongTaskProfiler } from "./crash_types";
export { parseReportedPerf as parseReportedPerf } from "./crash_perf";
export { serializeReportedPerf as serializeReportedPerf } from "./crash_perf";
export { formatPerformanceContext as formatPerformanceContext } from "./crash_perf";
export { performanceLabelForReason as performanceLabelForReason } from "./crash_perf";
export { performanceFingerprintHintForReason as performanceFingerprintHintForReason } from "./crash_perf";
export { shouldRecordLongTaskSample as shouldRecordLongTaskSample } from "./crash_perf";
export { shouldPromptForLongTasks as shouldPromptForLongTasks } from "./crash_perf";
export { shouldPromptForEventLoopLag as shouldPromptForEventLoopLag } from "./crash_perf";
export { formatLongTaskAttribution as formatLongTaskAttribution } from "./crash_perf";
export { aggregateLongTaskProfile as aggregateLongTaskProfile } from "./crash_perf";
export { dismissedPerfLabels as dismissedPerfLabels } from "./crash_perf";
export { currentBuildCommit as currentBuildCommit } from "./crash_perf";
export { getReportedPerfLabels as getReportedPerfLabels } from "./crash_perf";
export { markPerfReported as markPerfReported } from "./crash_perf";
export { currentView as currentView } from "./crash_perf";
export { kindForLabel as kindForLabel } from "./crash_perf";
export { sourceForLabel as sourceForLabel } from "./crash_perf";
export { formatText as formatText } from "./crash_perf";
export { fmtNumber as fmtNumber } from "./crash_perf";
export { readHeapSnapshot as readHeapSnapshot } from "./crash_perf";
export { pruneLongTasks as pruneLongTasks } from "./crash_perf";
export { longTaskSummary as longTaskSummary } from "./crash_perf";
export { performanceSnapshot as performanceSnapshot } from "./crash_perf";
export type { TaskAttributionLike as TaskAttributionLike } from "./crash_perf";

