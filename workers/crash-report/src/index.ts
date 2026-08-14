// Ingest + dashboard for desktop crash/feedback/performance reports and the
// anonymous launch ping. Frontend reports are user-initiated; native fatal and
// lifecycle reports are sent on the next launch under the same opt-out desktop
// telemetry gate as pings.
import { z } from "zod";
import type { Env } from "./env";
import { html, redirect } from "./shell";
import { renderStats, type StatsModule } from "./stats";
import { renderAccount } from "./auth_pages";
import { atLeast, currentUser, loginUrl, logAction, sameOrigin, sharedLogout, type Role, type User, } from "./auth";
import registryApp from "./registry/app";
import { cliReleaseChannel, desktopReleaseChannel, handleCLIRelease, handleDesktopReleaseManifest, handleReleaseGatewayRequest, } from "./desktop_release";
import { DEVELOPMENT_FINGERPRINT_PREFIX, crashGroups, currentWindowSince, developmentGroupSQL, diagnosticFacets as loadDiagnosticFacets, diagnosticWindowWhere, effectiveGroupSeverity, groupDiagnosticSummary, isDevelopmentGroup, reportAggregateStatements, type DiagnosticFacets, } from "./diagnostics_v2";
import { Report, WebRuntimeDiagnostic, type ReportPayload } from "./report_schema";
import { newestReleaseVersion, storageUnavailable, UserAction, GroupAction, OverviewCounts, previousWindowSince, previousWindowUntil, Bar, MetricTotals, requireViewer, registryBindings, communityStatus } from "./index_stats";
import { RETENTION, RETENTION_CHUNK_ROWS, RETENTION_MAX_CHUNKS, SENTINEL_CRON, ROLLUP_CRON, ROLLUP_WINDOW_DAYS, ROLLUP_SIGNALS_PER_RUN, ensureRollupSchema, CANARY_INSTALL_ID, errText } from "./index_rollup";
import { handleReport, handlePing, handleMetrics, handleStats, handleGroup, handleGroupAction, handleAdminUsers, handleAdminList, handleAdminAudit, handleCommunityList, handleCommunityAction, runIngestSentinel, purgeExpiredStatsRows } from "./index_handlers";
export { Report } from "./report_schema";
export { diagnosticWindowWhere, effectiveGroupSeverity, isDevelopmentGroup } from "./diagnostics_v2";
export default {
    async fetch(request: Request, env: Env): Promise<Response> {
        const url = new URL(request.url);
        const path = url.pathname;
        const method = request.method;
        const desktopRelease = desktopReleaseChannel(path);
        if (desktopRelease) {
            return handleReleaseGatewayRequest(method, () => handleDesktopReleaseManifest(desktopRelease));
        }
        const cliRelease = cliReleaseChannel(path);
        if (cliRelease) {
            return handleReleaseGatewayRequest(method, () => handleCLIRelease(cliRelease));
        }
        if (path === "/v1/report" && method === "POST")
            return handleReport(request, env);
        if (path === "/v1/ping" && method === "POST")
            return handlePing(request, env);
        if (path === "/v1/metrics" && method === "POST")
            return handleMetrics(request, env);
        // Skill/MCP registry API — the folded Hono app handles its own auth, CORS
        // and rate limiting against the registry database (public reads + publish,
        // plus the JSON /v1/admin the site's moderation panel calls).
        if (path.startsWith("/v1/packages") || path === "/v1/activity" || path.startsWith("/v1/admin")) {
            return registryApp.fetch(request, registryBindings(env));
        }
        const login = loginUrl(env, request);
        // Authentication moved to id.reasonix.io; these paths just bounce there.
        if ((path === "/login" || path === "/register") && method === "GET")
            return redirect(login);
        if (path === "/logout" && method === "POST")
            return redirect(login, await sharedLogout(request, env));
        const user = await currentUser(request, env);
        if (path === "/")
            return redirect(user ? (atLeast(user.role, "viewer") ? "/stats" : "/account") : login);
        if (path === "/account" && method === "GET")
            return user ? html(renderAccount(user)) : redirect(login);
        const groupFingerprint = groupFingerprintFromPath(path);
        const statsModuleMatch = path.match(/^\/stats\/(diagnostics|usage|preferences|health)$/);
        if ((path === "/stats" || statsModuleMatch) && method === "GET")
            return requireViewer(user, login) ?? handleStats(request, env, user as User, (statsModuleMatch?.[1] as StatsModule | undefined) ?? "usage");
        if (groupFingerprint && method === "GET")
            return requireViewer(user, login) ?? handleGroup(env, groupFingerprint, user as User);
        if (groupFingerprint && method === "POST") {
            if (user?.role !== "admin")
                return new Response("forbidden", { status: 403 });
            return handleGroupAction(request, env, user, groupFingerprint);
        }
        if (path === "/admin" && method === "GET") {
            if (!user)
                return redirect(login);
            return user.role === "admin" ? handleAdminList(env, user) : redirect("/account");
        }
        if (path === "/admin/audit" && method === "GET") {
            if (!user)
                return redirect(login);
            return user.role === "admin" ? handleAdminAudit(env, user) : redirect("/account");
        }
        if (path === "/admin/users" && method === "POST") {
            if (user?.role !== "admin")
                return new Response("forbidden", { status: 403 });
            return handleAdminUsers(request, env, user);
        }
        if (path === "/community" && method === "GET") {
            if (!user)
                return redirect(login);
            return user.role === "admin" ? handleCommunityList(env, user, communityStatus(url)) : redirect("/account");
        }
        const pkgActionMatch = path.match(/^\/community\/([^/]+)\/([^/]+)\/(approve|reject|hide|verify|unverify)$/);
        if (pkgActionMatch && method === "POST") {
            if (user?.role !== "admin")
                return new Response("forbidden", { status: 403 });
            return handleCommunityAction(request, env, user, pkgActionMatch[1], pkgActionMatch[2], pkgActionMatch[3]);
        }
        if (path === "/v1/report" ||
            path === "/v1/ping" ||
            path === "/v1/metrics" ||
            path === "/login" ||
            path === "/register" ||
            path === "/logout" ||
            path === "/account" ||
            path.startsWith("/stats") ||
            path.startsWith("/admin") ||
            path.startsWith("/community")) {
            return new Response("method not allowed", { status: 405 });
        }
        return new Response("not found", { status: 404 });
    },
    async scheduled(controller: ScheduledController, env: Env, ctx: ExecutionContext): Promise<void> {
        if (controller.cron === SENTINEL_CRON) {
            ctx.waitUntil(runIngestSentinel(env));
            return;
        }
        if (controller.cron === ROLLUP_CRON) {
            ctx.waitUntil(refreshMetricUserRollup(env));
            return;
        }
        ctx.waitUntil(purgeExpiredStatsRows(env));
    },
};
export { newestReleaseVersion as newestReleaseVersion } from "./index_stats";
export { storageUnavailable as storageUnavailable } from "./index_stats";
export { UserAction as UserAction } from "./index_stats";
export { GroupAction as GroupAction } from "./index_stats";
export { OverviewCounts as OverviewCounts } from "./index_stats";
export { previousWindowSince as previousWindowSince } from "./index_stats";
export { previousWindowUntil as previousWindowUntil } from "./index_stats";
export { Bar as Bar } from "./index_stats";
export { MetricTotals as MetricTotals } from "./index_stats";
export { requireViewer as requireViewer } from "./index_stats";
export { registryBindings as registryBindings } from "./index_stats";
export { communityStatus as communityStatus } from "./index_stats";
export { RETENTION as RETENTION } from "./index_rollup";
export { RETENTION_CHUNK_ROWS as RETENTION_CHUNK_ROWS } from "./index_rollup";
export { RETENTION_MAX_CHUNKS as RETENTION_MAX_CHUNKS } from "./index_rollup";
export { SENTINEL_CRON as SENTINEL_CRON } from "./index_rollup";
export { ROLLUP_CRON as ROLLUP_CRON } from "./index_rollup";
export { ROLLUP_WINDOW_DAYS as ROLLUP_WINDOW_DAYS } from "./index_rollup";
export { ROLLUP_SIGNALS_PER_RUN as ROLLUP_SIGNALS_PER_RUN } from "./index_rollup";
export { ensureRollupSchema as ensureRollupSchema } from "./index_rollup";
export { CANARY_INSTALL_ID as CANARY_INSTALL_ID } from "./index_rollup";
export { errText as errText } from "./index_rollup";
export const MAX_BODY_BYTES = 96 * 1024;
export const LATEST_SAMPLES_PER_GROUP = 5;
const GROUP_PATH_RE = /^\/stats\/group\/((?:dev:)?[0-9a-f]{64})$/;
const ClientSurface = z.enum(["desktop", "cli"]);
export type ClientSurfaceName = z.infer<typeof ClientSurface>;
type TelemetryTableNames = {
    pings: "pings" | "cli_pings";
    metrics: "metrics" | "cli_metrics";
    metricUsers: "metric_users" | "cli_metric_users";
};
const TELEMETRY_TABLES: Record<ClientSurfaceName, TelemetryTableNames> = {
    desktop: { pings: "pings", metrics: "metrics", metricUsers: "metric_users" },
    cli: { pings: "cli_pings", metrics: "cli_metrics", metricUsers: "cli_metric_users" },
};
export function telemetryTableNames(surface: ClientSurfaceName): TelemetryTableNames {
    return TELEMETRY_TABLES[surface];
}
export const CLI_TELEMETRY_SCHEMA_SQL = [
    `CREATE TABLE IF NOT EXISTS cli_pings (
     date TEXT NOT NULL,
     install_id TEXT NOT NULL,
     version TEXT NOT NULL,
     os TEXT NOT NULL,
     arch TEXT NOT NULL,
     os_version TEXT NOT NULL DEFAULT '',
     os_build INTEGER NOT NULL DEFAULT 0,
     os_revision INTEGER NOT NULL DEFAULT 0,
     channel TEXT NOT NULL DEFAULT '',
     distro_id TEXT NOT NULL DEFAULT '',
     distro_version TEXT NOT NULL DEFAULT '',
     kernel_version TEXT NOT NULL DEFAULT '',
     session_type TEXT NOT NULL DEFAULT '',
     runtime_engine TEXT NOT NULL DEFAULT '',
     runtime_version TEXT NOT NULL DEFAULT '',
     gpu_mode TEXT NOT NULL DEFAULT '',
     opens INTEGER NOT NULL DEFAULT 1,
     PRIMARY KEY (date, install_id)
   )`,
    `CREATE TABLE IF NOT EXISTS cli_metrics (
     date TEXT NOT NULL,
     version TEXT NOT NULL,
     os TEXT NOT NULL,
     signal TEXT NOT NULL,
     bucket TEXT NOT NULL,
     count INTEGER NOT NULL DEFAULT 0,
     PRIMARY KEY (date, version, os, signal, bucket)
   )`,
    `CREATE TABLE IF NOT EXISTS cli_metric_users (
     date TEXT NOT NULL,
     signal TEXT NOT NULL,
     bucket TEXT NOT NULL,
     install_id TEXT NOT NULL,
     version TEXT NOT NULL,
     os TEXT NOT NULL,
     arch TEXT NOT NULL DEFAULT '',
     os_build INTEGER NOT NULL DEFAULT 0,
     os_revision INTEGER NOT NULL DEFAULT 0,
     channel TEXT NOT NULL DEFAULT '',
     distro_id TEXT NOT NULL DEFAULT '',
     distro_version TEXT NOT NULL DEFAULT '',
     kernel_version TEXT NOT NULL DEFAULT '',
     session_type TEXT NOT NULL DEFAULT '',
     runtime_engine TEXT NOT NULL DEFAULT '',
     runtime_version TEXT NOT NULL DEFAULT '',
     gpu_mode TEXT NOT NULL DEFAULT '',
     event_count INTEGER NOT NULL DEFAULT 0,
     PRIMARY KEY (date, signal, bucket, install_id)
   )`,
    // No secondary indexes: each primary key already leads with `date`, which is
    // what every dashboard query filters on. See migrate-window-index-fix.sql.
] as const;
const cliTelemetrySchemaPromises = new WeakMap<object, Promise<void>>();
export function ensureCLITelemetrySchema(env: Pick<Env, "DB">): Promise<void> {
    const key = env.DB as unknown as object;
    const existing = cliTelemetrySchemaPromises.get(key);
    if (existing)
        return existing;
    const creation = env.DB
        .batch(CLI_TELEMETRY_SCHEMA_SQL.map((sql) => env.DB.prepare(sql)))
        .then(() => undefined)
        .catch((err) => {
        cliTelemetrySchemaPromises.delete(key);
        throw err;
    });
    cliTelemetrySchemaPromises.set(key, creation);
    return creation;
}
export const Ping = z.object({
    installId: z.string().regex(/^[0-9a-f]{32}$/),
    version: z.string().min(1).max(64),
    os: z.string().min(1).max(32),
    arch: z.string().min(1).max(32),
    osVersion: z.string().max(128).optional(),
    osBuild: z.number().int().min(0).max(1000000).optional(),
    osRevision: z.number().int().min(0).max(1000000).optional(),
    channel: z.string().max(32).optional(),
    distroId: z.string().max(64).optional(),
    distroVersion: z.string().max(64).optional(),
    kernelVersion: z.string().max(128).optional(),
    sessionType: z.enum(["wayland", "x11", "remote", "unknown"]).optional(),
    runtimeEngine: z.enum(["webview2", "webkitgtk", "unknown"]).optional(),
    runtimeVersion: z.string().max(128).optional(),
    gpuMode: z.enum(["enabled", "disabled", "always", "on_demand", "unknown"]).optional(),
    surface: ClientSurface.default("desktop"),
});
// Opt-in aggregate client metrics: a per-launch snapshot of (signal, bucket)
// counters. The optional surface-specific random install id deduplicates DAU;
// there is no user content. Unknown signals are discarded before storage so
// older workers can accept batches from newer clients safely.
const METRIC_SIGNALS = [
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
    "desktop_webview2_outcome",
    "desktop_web_runtime_failure",
    "desktop_web_runtime_outcome",
    "desktop_web_runtime_dropped",
    "desktop_legacy_exit",
    "desktop_legacy_exit_phase",
    "cli_mode",
    "cli_profile",
    "cli_permission_mode",
    "cli_session_mode",
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
    "client_surface",
    "client_version",
    "settings_language",
    "settings_desktop_layout",
    "settings_theme",
    "settings_theme_style",
    "settings_close_behavior",
    "settings_display_mode",
    "settings_status_bar_style",
    "settings_status_bar_items_count",
    "settings_check_updates",
    "settings_default_model",
    "settings_planner_model",
    "settings_subagent_model",
    "settings_subagent_effort",
    "settings_reasoning_language",
    "settings_provider_count",
    "settings_provider_access_count",
    "settings_provider_access",
] as const;
type MetricSignal = (typeof METRIC_SIGNALS)[number];
const METRIC_SIGNAL_SET: ReadonlySet<string> = new Set(METRIC_SIGNALS);
const KnownMetricCounter = z.object({
    signal: z.enum(METRIC_SIGNALS),
    bucket: z
        .string()
        .min(1)
        .max(96)
        .regex(/^[a-z0-9_]+$/),
    count: z.number().int().min(1).max(1000000),
});
const UnknownMetricCounter = z
    .object({
    signal: z
        .string()
        .min(1)
        .max(96)
        .refine((signal) => !METRIC_SIGNAL_SET.has(signal)),
})
    .passthrough()
    .transform(() => null);
export const Metrics = z.object({
    installId: z
        .string()
        .regex(/^[0-9a-f]{32}$/)
        .optional(),
    version: z.string().min(1).max(64),
    os: z.string().min(1).max(32),
    arch: z.string().max(32).optional(),
    osBuild: z.number().int().min(0).max(1000000).optional(),
    osRevision: z.number().int().min(0).max(1000000).optional(),
    channel: z.string().max(32).optional(),
    distroId: z.string().max(64).optional(),
    distroVersion: z.string().max(64).optional(),
    kernelVersion: z.string().max(128).optional(),
    sessionType: z.enum(["wayland", "x11", "remote", "unknown"]).optional(),
    runtimeEngine: z.enum(["webview2", "webkitgtk", "unknown"]).optional(),
    runtimeVersion: z.string().max(128).optional(),
    gpuMode: z.enum(["enabled", "disabled", "always", "on_demand", "unknown"]).optional(),
    surface: ClientSurface.default("desktop"),
    counters: z
        .array(z.union([KnownMetricCounter, UnknownMetricCounter]))
        .min(1)
        .max(128)
        .transform((counters) => counters.filter((counter): counter is z.infer<typeof KnownMetricCounter> & {
        signal: MetricSignal;
    } => counter !== null)),
});
type FingerprintInput = {
    kind: string;
    message: string;
    source?: string;
    label?: string;
    errorType?: string;
    errorMessage?: string;
    topFrame?: string;
    fingerprintHint?: string;
};
export function scrubSensitiveText(input: string): string {
    return input
        .replace(/([A-Z]:\\Users\\)[^/\\:\s"']+/gi, "$1_")
        .replace(/(\/(?:home|Users)\/)[^/\\:\s"']+/g, "$1_")
        .replace(/\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b/g, "[redacted-email]")
        .replace(/\bBearer\s+[A-Za-z0-9._~+/=-]{16,}/gi, "Bearer [redacted]")
        .replace(/\b(api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|authorization|secret|password|passwd|pwd|token)\b\s*[:=]\s*(?:Bearer\s+)?['"]?[^'"\s,;]+['"]?/gi, "$1=[redacted]")
        .replace(/\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b/g, "[redacted-jwt]")
        .replace(/\b(?:sk|rk)-(?:proj-)?[A-Za-z0-9_-]{16,}\b/g, "[redacted-key]")
        .replace(/\b[0-9a-fA-F]{32,}\b/g, "[redacted-hex]")
        .replace(/[A-Za-z0-9+/]{40,}={0,2}/g, "[redacted-token]")
        .replace(/\b[A-Za-z0-9_-]{48,}\b/g, "[redacted-token]");
}
function normalizeStackFrame(frame: string): string {
    return frame
        .replace(/[A-Za-z]:\\[^\s)('"]+/g, "<path>")
        .replace(/\/(?:home|Users)\/[^\s)('"]+/g, "/<home>")
        .replace(/(?:wails|https?|file):\/\/[^\s)('"]+/g, "<url>")
        .replace(/0x[0-9a-fA-F]+/g, "<addr>")
        .replace(/:\d+(?::\d+)?/g, ":<n>");
}
function normalizeFingerprintText(text: string): string {
    return text
        .replace(/[A-Za-z]:\\[^\s)('"]+/g, "<path>")
        .replace(/(?:wails|https?|file):\/\/[^\s)('"]+/g, "<url>")
        .replace(/0x[0-9a-fA-F]+/g, "<addr>")
        .replace(/^build [0-9a-f]+$/gm, "build <commit>")
        .replace(/:\d+(?::\d+)?/g, ":<n>");
}
export function normalizeForFingerprint(inputOrKind: FingerprintInput | string, legacyMessage = ""): string {
    if (typeof inputOrKind === "string") {
        const head = legacyMessage.split("\n").slice(0, 12).join("\n");
        return inputOrKind + "\n" + normalizeFingerprintText(head);
    }
    const input = inputOrKind;
    const messageBasis = input.errorMessage || input.message;
    const head = messageBasis.split("\n").slice(0, 6).join("\n");
    return (input.kind +
        "\n" +
        (input.source || "legacy") +
        "\n" +
        (input.label || "") +
        "\n" +
        (input.errorType || "") +
        "\n" +
        normalizeStackFrame(input.topFrame || "") +
        "\n" +
        (input.fingerprintHint ? `${input.fingerprintHint}\n` : "") +
        normalizeFingerprintText(head));
}
export function nativeWebRuntimeFingerprintBasis(input: {
    engine: string;
    kind: string;
    reason: string;
    exitCode?: number;
}): string {
    const kind = normalizeRuntimeBucket(input.engine, "kind", input.kind);
    const reason = normalizeRuntimeBucket(input.engine, "reason", input.reason);
    const normalizedExitCode = input.engine === "webview2" && kind === "render_process_unresponsive" && input.exitCode === 259 ? undefined : input.exitCode;
    const exitCode = normalizedExitCode === undefined ? "unknown" : String(normalizedExitCode);
    return [input.engine, kind, reason, exitCode].join("\n");
}
type NormalizedWebRuntime = z.infer<typeof WebRuntimeDiagnostic>;
export function basenameOnly(value: string | undefined): string {
    return (value ?? "").split(/[\\/]/).pop()?.slice(0, 255) ?? "";
}
function normalizeRuntimeBucket(engine: string, field: "kind" | "reason", input: string): string {
    const buckets = engine === "webview2"
        ? field === "kind"
            ? ["browser_process_exited", "render_process_exited", "render_process_unresponsive", "frame_render_process_exited", "utility_process_exited", "sandbox_helper_process_exited", "gpu_process_exited", "ppapi_plugin_process_exited", "ppapi_broker_process_exited", "unknown_process_exited", "unknown"]
            : ["unexpected", "unresponsive", "terminated", "crashed", "launch_failed", "out_of_memory", "profile_deleted", "normal_exit", "abnormal_exit", "integrity_failure", "unknown"]
        : field === "kind"
            ? ["web_process", "unknown"]
            : ["crashed", "out_of_memory", "terminated_by_api", "unknown"];
    const value = input.trim().toLowerCase();
    return buckets.includes(value) ? value : "unknown";
}
export function normalizedWebRuntime(r: ReportPayload): NormalizedWebRuntime | undefined {
    const input: NormalizedWebRuntime | undefined = r.webRuntime ?? (r.webview2
        ? {
            engine: "webview2",
            kind: r.webview2.kind,
            reason: r.webview2.reason,
            exitCode: r.webview2.exitCode,
            processDescription: r.webview2.processDescription,
            failureSourceModule: r.webview2.failureSourceModule,
            runtimeVersion: r.webview2.runtimeVersion,
            gpuMode: r.webview2.gpuDisabled ? "disabled" : "enabled",
            recovery: r.webview2.recovery,
        }
        : undefined);
    if (!input)
        return undefined;
    return {
        ...input,
        kind: normalizeRuntimeBucket(input.engine, "kind", input.kind),
        reason: normalizeRuntimeBucket(input.engine, "reason", input.reason),
        runtimeVersion: input.runtimeVersion.trim() || "unknown",
        exitCode: input.engine === "webview2" && normalizeRuntimeBucket(input.engine, "kind", input.kind) === "render_process_unresponsive" && input.exitCode === 259 ? undefined : input.exitCode,
        processDescription: scrubSensitiveText(input.processDescription ?? "").slice(0, 255),
        failureSourceModule: basenameOnly(input.failureSourceModule),
    };
}
export function hasStructuredCrashFields(r: ReportPayload): boolean {
    return Boolean(r.schemaVersion ||
        r.source ||
        r.label ||
        r.errorType ||
        r.errorMessage ||
        r.stack ||
        r.componentStack ||
        r.topFrame ||
        r.fingerprintHint ||
        r.buildCommit ||
        r.channel ||
        r.language ||
        r.view ||
        r.breadcrumbs?.length ||
        r.occurredAt);
}
// One-line human summary for the dashboard list. Frontend reports are formatted
// "[label]\n\n<detail>", so a bare label alone is folded together with its detail.
export function crashTitle(message: string): string {
    const lines = message
        .split("\n")
        .map((l) => l.trim())
        .filter(Boolean);
    let head = lines[0] ?? "";
    if (/^\[[^\]]+\]$/.test(head) && lines[1])
        head = `${head} ${lines[1]}`;
    return head.slice(0, 200);
}
type SeverityInput = {
    kind: string;
    version?: string;
    source: string;
    label: string;
    errorType: string;
    errorMessage: string;
    topFrame: string;
    channel?: string;
    recovery?: string;
};
const RESIZE_OBSERVER_NOTICE_RE = /^ResizeObserver loop (?:limit exceeded|completed with undelivered notifications\.?)$/;
export function isDevelopmentReport(input: SeverityInput): boolean {
    const channel = input.channel?.trim().toLowerCase();
    return channel === "dev" || channel === "test" || input.version?.trim().toLowerCase().startsWith("dev") === true;
}
export function namespaceReportFingerprint(hash: string, development: boolean): string {
    return development ? `${DEVELOPMENT_FINGERPRINT_PREFIX}${hash}` : hash;
}
export function groupFingerprintFromPath(path: string): string | null {
    return path.match(GROUP_PATH_RE)?.[1] ?? null;
}
export function isKnownNonCrashDiagnostic(input: SeverityInput): boolean {
    const message = input.errorMessage.trim();
    return (RESIZE_OBSERVER_NOTICE_RE.test(message) ||
        /Minified React error #520\b/.test(message) ||
        message.includes("additional File object is not a file on the disk"));
}
export function isOpaqueScriptErrorReport(input: SeverityInput): boolean {
    return (input.kind === "crash" &&
        input.source === "frontend.global" &&
        input.label === "window.error" &&
        input.errorType === "string" &&
        input.errorMessage.trim() === "Script error." &&
        input.topFrame.trim() === "");
}
function severityForKind(kind: string): string {
    if (kind === "crash")
        return "high";
    if (kind === "performance")
        return "medium";
    if (kind === "exception")
        return "medium";
    return "low";
}
export function severityForReport(input: SeverityInput): string {
    if (isDevelopmentReport(input) || isOpaqueScriptErrorReport(input) || isKnownNonCrashDiagnostic(input))
        return "low";
    if ((input.source === "web.runtime.native" || input.source === "webview2.process.native") && input.recovery === "reload_succeeded")
        return "low";
    if ((input.source === "web.runtime.native" || input.source === "webview2.process.native") && input.kind === "exception")
        return "high";
    return severityForKind(input.kind);
}
export function severityRank(severity: string): number {
    return ({ low: 1, medium: 2, high: 3, critical: 4 })[severity] ?? 0;
}
export function maxSeverity(current: string, incoming: string): string {
    return severityRank(incoming) > severityRank(current) ? incoming : current;
}
export async function latestAdoptionPct(env: Env, latestVersion: string, days: 7 | 30, surface: ClientSurfaceName): Promise<number | null> {
    if (!latestVersion)
        return null;
    const table = telemetryTableNames(surface).pings;
    const row = await env.DB.prepare(`SELECT
      COUNT(DISTINCT install_id) AS total_installs,
      COUNT(DISTINCT CASE WHEN version = ?1 THEN install_id END) AS latest_installs
    FROM ${table} WHERE date >= date('now', '${currentWindowSince(days)}')`)
        .bind(latestVersion)
        .first<{
        total_installs: number;
        latest_installs: number;
    }>();
    const total = Number(row?.total_installs ?? 0);
    if (!total)
        return null;
    return (Number(row?.latest_installs ?? 0) / total) * 100;
}
export async function refreshMetricUserRollup(env: Env, signalsPerRun = ROLLUP_SIGNALS_PER_RUN): Promise<void> {
    await ensureRollupSchema(env);
    const state = await env.DB.prepare("SELECT next_signal FROM metric_user_rollup_state WHERE id = 1").first<{
        next_signal: number;
    }>();
    const start = Number(state?.next_signal ?? 0) % METRIC_SIGNALS.length;
    const now = new Date().toISOString();
    let advanced = 0;
    for (let i = 0; i < signalsPerRun && i < METRIC_SIGNALS.length; i++) {
        const signal = METRIC_SIGNALS[(start + i) % METRIC_SIGNALS.length];
        try {
            const rows = await env.DB.prepare(`SELECT bucket, COUNT(DISTINCT install_id) AS total FROM metric_users
         WHERE date >= date('now', '-${ROLLUP_WINDOW_DAYS - 1} day') AND signal = ?1
         GROUP BY bucket`)
                .bind(signal)
                .all<{
                bucket: string;
                total: number;
            }>();
            // Delete and insert in one batch so a reader never sees a signal
            // half-replaced; an empty result still clears the previous window's rows.
            await env.DB.batch([
                env.DB
                    .prepare("DELETE FROM metric_user_rollup WHERE window_days = ?1 AND signal = ?2")
                    .bind(ROLLUP_WINDOW_DAYS, signal),
                ...rows.results.map((r) => env.DB
                    .prepare(`INSERT INTO metric_user_rollup (window_days, signal, bucket, total, computed_at)
               VALUES (?1, ?2, ?3, ?4, ?5)`)
                    .bind(ROLLUP_WINDOW_DAYS, signal, r.bucket, r.total, now)),
            ]);
            advanced++;
        }
        catch (err) {
            // One signal timing out must not strand the cursor on it forever.
            console.error(`rollup: ${signal} failed`, err);
            advanced++;
        }
    }
    await env.DB
        .prepare(`INSERT INTO metric_user_rollup_state (id, next_signal, updated_at) VALUES (1, ?1, ?2)
       ON CONFLICT(id) DO UPDATE SET next_signal = ?1, updated_at = ?2`)
        .bind((start + advanced) % METRIC_SIGNALS.length, now)
        .run();
    console.log(`rollup: refreshed ${advanced} signals from index ${start}`);
}
export { handleReport as handleReport } from "./index_handlers";
export { handlePing as handlePing } from "./index_handlers";
export { handleMetrics as handleMetrics } from "./index_handlers";
export { handleStats as handleStats } from "./index_handlers";
export { handleGroup as handleGroup } from "./index_handlers";
export { handleGroupAction as handleGroupAction } from "./index_handlers";
export { handleAdminUsers as handleAdminUsers } from "./index_handlers";
export { handleAdminList as handleAdminList } from "./index_handlers";
export { handleAdminAudit as handleAdminAudit } from "./index_handlers";
export { handleCommunityList as handleCommunityList } from "./index_handlers";
export { handleCommunityAction as handleCommunityAction } from "./index_handlers";
export { runIngestSentinel as runIngestSentinel } from "./index_handlers";
export { purgeExpiredStatsRows as purgeExpiredStatsRows } from "./index_handlers";
export { formObject as formObject } from "./index_dashboard";
export { latestObservedVersion as latestObservedVersion } from "./index_dashboard";
export { diagnosticOverview as diagnosticOverview } from "./index_dashboard";
export { metricRows as metricRows } from "./index_dashboard";
export { metricUserRows as metricUserRows } from "./index_dashboard";

