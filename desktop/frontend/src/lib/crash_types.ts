// Last-resort crash surface: a React render error with no boundary unmounts the
// whole tree (blank window), and global errors/rejections leave no trace either.
import { type Breadcrumb } from "./breadcrumbs";
import { type SessionPipelineDiagnostics } from "./sessionDiagnostics";
export type CrashKind = "crash" | "exception" | "feedback" | "performance" | "bot";
export type PerformanceSnapshot = {
    reason: string;
    uptimeMs: number;
    visibility: string;
    focused: boolean;
    online: boolean;
    hardwareConcurrency: number;
    deviceMemoryGb?: number;
    jsHeap?: {
        usedMb: number;
        totalMb: number;
        limitMb: number;
        usagePercent?: number;
    };
    eventLoopLag?: {
        currentMs: number;
        maxMs: number;
        avgMs: number;
        samples: number;
    };
    longTasks?: {
        count: number;
        totalMs: number;
        maxMs: number;
        recent: {
            startMs: number;
            durationMs: number;
            attribution?: string;
        }[];
    };
    longTaskFrames?: {
        label: string;
        samples: number;
    }[];
    connection?: {
        effectiveType?: string;
        downlinkMbps?: number;
        rttMs?: number;
        saveData?: boolean;
    };
    // Session-switch/history pipeline diagnostics (Phase F): last activation
    // timings, last HistorySlice page stats with index hit/miss, virtual mounted
    // rows, markdown worker counters, transcript cache weights. All optional —
    // absent before the first switch/page or when a provider never registered.
    sessionPipeline?: SessionPipelineDiagnostics;
};
export type CrashPayload = {
    schemaVersion: 2;
    source: "frontend" | "frontend.react" | "frontend.global" | "frontend.performance" | "bot.runtime";
    kind: CrashKind;
    label: string;
    message: string;
    errorType: string;
    errorMessage: string;
    stack?: string;
    componentStack?: string;
    topFrame?: string;
    // Optional, non-display grouping context for otherwise opaque WebView errors.
    // It is deliberately restricted to build/view/breadcrumb categories and never
    // contains breadcrumb messages, tab IDs, paths, or user content.
    fingerprintHint?: string;
    buildCommit: string;
    channel: string;
    language: string;
    view: string;
    breadcrumbs: Breadcrumb[];
    occurredAt: string;
};
export type NormalizedError = {
    errorType: string;
    errorMessage: string;
    stack?: string;
};
type LongTaskSample = {
    startMs: number;
    durationMs: number;
    attribution?: string;
};
// WICG JS Self-Profiling API (https://wicg.github.io/js-self-profiling/), available
// in Chromium WebViews when the document is served with `Document-Policy: js-profiling`.
export type ProfilerTrace = {
    resources?: string[];
    frames?: {
        name?: string;
        resourceId?: number;
        line?: number;
        column?: number;
    }[];
    stacks?: {
        frameId: number;
        parentId?: number;
    }[];
    samples?: {
        timestamp: number;
        stackId?: number;
    }[];
};
type ProfilerLike = {
    stop(): Promise<ProfilerTrace>;
    addEventListener?: (type: string, listener: () => void) => void;
};
type ProfilerConstructor = new (options: {
    sampleInterval: number;
    maxBufferSize: number;
}) => ProfilerLike;
export type BrowserPerformanceMemory = {
    usedJSHeapSize?: number;
    totalJSHeapSize?: number;
    jsHeapSizeLimit?: number;
};
export type BrowserNavigator = Navigator & {
    deviceMemory?: number;
    connection?: {
        effectiveType?: string;
        downlink?: number;
        rtt?: number;
        saveData?: boolean;
    };
};
export const LONG_TASK_WINDOW_MS = 60000;
export const LONG_TASK_PROMPT_MS = 800;
// Streaming renders routinely accumulate ~1.5s of 70-240ms tasks per minute without
// user-visible jank, so the cumulative prompt only fires past half of that budget spent blocked.
export const LONG_TASK_TOTAL_PROMPT_MS = 3000;
export const EVENT_LOOP_LAG_PROMPT_MS = 1200;
export const EVENT_LOOP_LAG_CONSECUTIVE_SAMPLES = 2;
export const STARTUP_GRACE_MS = 15000;
export const PROMPT_COOLDOWN_MS = 10 * 60000;
export const MAX_LAG_SAMPLES = 60;
export const VISIBILITY_RESUME_GRACE_MS = 5000;
export const longTasks: LongTaskSample[] = [];
export const lagSamples: number[] = [];
export let performanceMonitorInstalled = false;
export let lastPerformancePromptAt = 0;
// Rolling self-profiling sampler (Chromium WebViews only; requires the asset server
// to send `Document-Policy: js-profiling`, see jsProfilingMiddleware on the Go side).
// ~10ms native sampling; the buffer covers the same 60s window as longTasks.
const PROFILER_SAMPLE_INTERVAL_MS = 10;
const PROFILER_MAX_BUFFER_SAMPLES = LONG_TASK_WINDOW_MS / PROFILER_SAMPLE_INTERVAL_MS;
export function setActiveProfiler(v: ProfilerLike | null): void { activeProfiler = v; }
export function setLastPerformancePromptAt(v: number): void { lastPerformancePromptAt = v; }
export function setPerformanceMonitorInstalled(v: boolean): void { performanceMonitorInstalled = v; }
export let activeProfiler: ProfilerLike | null = null;
export function startLongTaskProfiler(): void {
    const ProfilerCtor = (globalThis as {
        Profiler?: ProfilerConstructor;
    }).Profiler;
    if (!ProfilerCtor)
        return;
    try {
        const profiler = new ProfilerCtor({
            sampleInterval: PROFILER_SAMPLE_INTERVAL_MS,
            maxBufferSize: PROFILER_MAX_BUFFER_SAMPLES,
        });
        // A full buffer stops sampling silently; drop the stale trace and roll over.
        profiler.addEventListener?.("samplebufferfull", () => {
            if (activeProfiler !== profiler)
                return;
            activeProfiler = null;
            void profiler.stop().catch(() => { });
            startLongTaskProfiler();
        });
        activeProfiler = profiler;
    }
    catch {
        // Document policy missing or the API is disabled in this WebView.
        activeProfiler = null;
    }
}

