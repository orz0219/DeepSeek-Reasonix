import type { Env } from "./env";
import { html, redirect } from "./shell";
import { renderStats, type StatsModule } from "./stats";
import { renderGroup, type Group } from "./group";
import { renderUsers, renderAudit, type UserRow, type AuditRow } from "./admin";
import { atLeast, currentUser, loginUrl, logAction, sameOrigin, sharedLogout, type Role, type User, } from "./auth";
import { PackageRepo } from "./registry/db/packages";
import { EventRepo } from "./registry/db/events";
import { renderCommunity } from "./community";
import { DEVELOPMENT_FINGERPRINT_PREFIX, crashGroups, currentWindowSince, developmentGroupSQL, diagnosticFacets as loadDiagnosticFacets, diagnosticWindowWhere, effectiveGroupSeverity, groupDiagnosticSummary, isDevelopmentGroup, reportAggregateStatements, type DiagnosticFacets, } from "./diagnostics_v2";
import { Report, WebRuntimeDiagnostic, type ReportPayload } from "./report_schema";
import { statsFilters, type StatsFilters } from "./stats_filters";
import { newestReleaseVersion, storageUnavailable, UserAction, GroupAction, OverviewCounts, previousWindowSince, previousWindowUntil, Bar, MetricTotals, requireViewer, registryBindings, communityStatus } from "./index_stats";
import { RETENTION, RETENTION_CHUNK_ROWS, RETENTION_MAX_CHUNKS, SENTINEL_CRON, ROLLUP_CRON, ROLLUP_WINDOW_DAYS, ROLLUP_SIGNALS_PER_RUN, ensureRollupSchema, CANARY_INSTALL_ID, errText } from "./index_rollup";
import { MAX_BODY_BYTES, scrubSensitiveText, normalizedWebRuntime, basenameOnly, nativeWebRuntimeFingerprintBasis, hasStructuredCrashFields, normalizeForFingerprint, crashTitle, isDevelopmentReport, namespaceReportFingerprint, severityForReport, LATEST_SAMPLES_PER_GROUP, Ping, telemetryTableNames, ensureCLITelemetrySchema, Metrics, latestAdoptionPct } from "./index";
import { latestObservedVersion, metricRows, diagnosticOverview, metricUserRows, formObject } from "./index_dashboard";
async function sha256Hex(s: string): Promise<string> {
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
    return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}
async function readJSON(request: Request): Promise<unknown | Response> {
    const length = Number(request.headers.get("content-length") ?? "0");
    if (!length || length > MAX_BODY_BYTES)
        return new Response("payload too large", { status: 413 });
    try {
        return JSON.parse(await request.text());
    }
    catch {
        return new Response("bad request", { status: 400 });
    }
}
export async function handleReport(request: Request, env: Env): Promise<Response> {
    const ip = request.headers.get("cf-connecting-ip") ?? "unknown";
    const { success } = await env.RATE_LIMITER.limit({ key: ip });
    if (!success)
        return new Response("rate limited", { status: 429 });
    const raw = await readJSON(request);
    if (raw instanceof Response)
        return raw;
    const parsed = Report.safeParse(raw);
    if (!parsed.success)
        return new Response("bad request", { status: 400 });
    const r = parsed.data;
    const message = scrubSensitiveText(r.message);
    const errorMessage = scrubSensitiveText(r.errorMessage ?? "");
    const stack = scrubSensitiveText(r.stack ?? "");
    const componentStack = scrubSensitiveText(r.componentStack ?? "");
    const topFrame = scrubSensitiveText(r.topFrame ?? "");
    const fingerprintHint = scrubSensitiveText(r.fingerprintHint ?? "");
    const view = scrubSensitiveText(r.view ?? "");
    const breadcrumbs = (r.breadcrumbs ?? []).map((b) => ({
        ...b,
        msg: b.msg ? scrubSensitiveText(b.msg) : b.msg,
    }));
    const webRuntime = normalizedWebRuntime(r);
    const webview2 = r.webview2
        ? {
            ...r.webview2,
            processDescription: scrubSensitiveText(r.webview2.processDescription ?? "").slice(0, 255),
            failureSourceModule: basenameOnly(r.webview2.failureSourceModule),
        }
        : undefined;
    const fingerprintBasis = (r.source === "web.runtime.native" || r.source === "webview2.process.native") && webRuntime
        ? nativeWebRuntimeFingerprintBasis(webRuntime)
        : hasStructuredCrashFields(r)
            ? normalizeForFingerprint({
                kind: r.kind,
                message,
                source: r.source,
                label: r.label,
                errorType: r.errorType,
                errorMessage,
                topFrame,
                fingerprintHint,
            })
            : normalizeForFingerprint(r.kind, message);
    const now = new Date().toISOString();
    const title = crashTitle(message);
    const source = r.source ?? "legacy";
    const label = r.label ?? "";
    const errorType = r.errorType ?? "";
    const buildCommit = r.buildCommit ?? "";
    const channel = r.channel ?? "";
    const severityInput = {
        kind: r.kind,
        version: r.version,
        source,
        label,
        errorType,
        errorMessage,
        topFrame,
        channel,
        recovery: webRuntime?.recovery,
    };
    const development = isDevelopmentReport(severityInput);
    const fingerprint = namespaceReportFingerprint(await sha256Hex(fingerprintBasis), development);
    const severity = severityForReport(severityInput);
    try {
        const prior = await env.DB.prepare("SELECT status FROM groups WHERE fingerprint = ?1")
            .bind(fingerprint)
            .first<{
            status: string;
        }>();
        const regressedAt = prior?.status === "resolved" ? now : "";
        const groupWrite = env.DB.prepare(`INSERT INTO groups (
         fingerprint, kind, count, first_seen, last_seen, first_version, last_version,
         status, title, source, label, error_type, top_frame, severity,
         last_os, last_arch, last_build_commit, last_channel, last_sample_at, regressed_at
       )
       VALUES (?1, ?2, 1, ?3, ?3, ?4, ?4, 'open', ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?3, ?15)
       ON CONFLICT (fingerprint) DO UPDATE SET
         kind = CASE
           WHEN severity = 'critical' THEN kind
           WHEN (CASE ?10 WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END) >
                (CASE severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END)
             THEN ?2 ELSE kind END,
         count = count + 1,
         last_seen = ?3,
         last_version = ?4,
         title = ?5,
         source = ?6,
         label = ?7,
         error_type = ?8,
         top_frame = ?9,
         severity = CASE
           WHEN severity = 'critical' THEN severity
           WHEN (CASE ?10 WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END) >
                (CASE severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END)
             THEN ?10 ELSE severity END,
         last_os = ?11,
         last_arch = ?12,
         last_build_commit = ?13,
         last_channel = ?14,
         last_sample_at = ?3,
         status = CASE WHEN status = 'resolved' THEN 'open' ELSE status END,
         regressed_at = CASE WHEN status = 'resolved' THEN ?3 ELSE regressed_at END`)
            .bind(fingerprint, r.kind, now, r.version, title, source, label, errorType, topFrame, severity, r.os, r.arch, buildCommit, channel, regressedAt);
        const sampleWrite = env.DB.prepare(`INSERT INTO reports (
         fingerprint, kind, version, os, arch, message, device, created_at,
         source, label, error_type, error_message, top_frame, build_commit, channel,
         language, view, breadcrumbs, component_stack, stack, occurred_at, webview2, web_runtime
       )
       VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20, ?21, ?22, ?23)`)
            .bind(fingerprint, r.kind, r.version, r.os, r.arch, message, JSON.stringify(r.device ?? {}), now, source, label, errorType, errorMessage, topFrame, buildCommit, channel, r.language ?? "", view, JSON.stringify(breadcrumbs), componentStack, stack, r.occurredAt ?? "", webview2 ? JSON.stringify(webview2) : "", webRuntime ? JSON.stringify(webRuntime) : "");
        const pruneSamples = env.DB.prepare(`DELETE FROM reports
       WHERE fingerprint = ?1
         AND id NOT IN (
           SELECT id FROM (SELECT id FROM reports WHERE fingerprint = ?1 ORDER BY id ASC LIMIT 1)
           UNION
           SELECT id FROM (SELECT id FROM reports WHERE fingerprint = ?1 ORDER BY id DESC LIMIT ?2)
         )`).bind(fingerprint, LATEST_SAMPLES_PER_GROUP);
        await env.DB.batch([
            groupWrite,
            sampleWrite,
            ...reportAggregateStatements(env.DB, r, fingerprint, channel, webRuntime),
            pruneSamples,
        ]);
    }
    catch (err) {
        return storageUnavailable("report", err);
    }
    return new Response("ok", { status: 202 });
}
export async function handlePing(request: Request, env: Env): Promise<Response> {
    const ip = request.headers.get("cf-connecting-ip") ?? "unknown";
    const { success } = await env.PING_LIMITER.limit({ key: ip });
    if (!success)
        return new Response("rate limited", { status: 429 });
    const raw = await readJSON(request);
    if (raw instanceof Response)
        return raw;
    const parsed = Ping.safeParse(raw);
    if (!parsed.success)
        return new Response("bad request", { status: 400 });
    const p = parsed.data;
    const tables = telemetryTableNames(p.surface);
    try {
        if (p.surface === "cli")
            await ensureCLITelemetrySchema(env);
        await env.DB.prepare(`INSERT INTO ${tables.pings} (
         date, install_id, version, os, arch, os_version, os_build, os_revision, channel,
         distro_id, distro_version, kernel_version, session_type, runtime_engine, runtime_version, gpu_mode, opens
       )
       VALUES (date('now'), ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, 1)
       ON CONFLICT (date, install_id) DO UPDATE SET
         opens = opens + 1, version = ?2, os_version = ?5, os_build = ?6, os_revision = ?7,
         channel = ?8, distro_id = ?9, distro_version = ?10, kernel_version = ?11,
         session_type = ?12, runtime_engine = ?13, runtime_version = ?14, gpu_mode = ?15`)
            .bind(p.installId, p.version, p.os, p.arch, p.osVersion ?? "", p.osBuild ?? 0, p.osRevision ?? 0, p.channel ?? "", p.distroId ?? "", p.distroVersion ?? "", p.kernelVersion ?? "", p.sessionType ?? "", p.runtimeEngine ?? "", p.runtimeVersion ?? "", p.gpuMode ?? "")
            .run();
    }
    catch (err) {
        return storageUnavailable("ping", err);
    }
    return new Response("ok", { status: 202 });
}
export async function handleMetrics(request: Request, env: Env): Promise<Response> {
    const ip = request.headers.get("cf-connecting-ip") ?? "unknown";
    const { success } = await env.METRICS_LIMITER.limit({ key: ip });
    if (!success)
        return new Response("rate limited", { status: 429 });
    const raw = await readJSON(request);
    if (raw instanceof Response)
        return raw;
    const parsed = Metrics.safeParse(raw);
    if (!parsed.success)
        return new Response("bad request", { status: 400 });
    const m = parsed.data;
    if (m.counters.length === 0)
        return new Response("ok", { status: 202 });
    const tables = telemetryTableNames(m.surface);
    try {
        if (m.surface === "cli")
            await ensureCLITelemetrySchema(env);
        const upsert = env.DB.prepare(`INSERT INTO ${tables.metrics} (date, version, os, signal, bucket, count)
       VALUES (date('now'), ?1, ?2, ?3, ?4, ?5)
       ON CONFLICT (date, version, os, signal, bucket) DO UPDATE SET
         count = count + ?5`);
        await env.DB.batch(m.counters.map((c) => upsert.bind(m.version, m.os, c.signal, c.bucket, c.count)));
    }
    catch (err) {
        return storageUnavailable("metrics", err);
    }
    if (m.installId) {
        const userUpsert = env.DB.prepare(`INSERT INTO ${tables.metricUsers} (
         date, version, os, arch, os_build, os_revision, channel, distro_id, distro_version,
         kernel_version, session_type, runtime_engine, runtime_version, gpu_mode,
         signal, bucket, install_id, event_count
       )
       VALUES (date('now'), ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17)
       ON CONFLICT (date, signal, bucket, install_id) DO UPDATE SET
         version = ?1, os = ?2, arch = ?3, os_build = ?4, os_revision = ?5,
         channel = ?6, distro_id = ?7, distro_version = ?8, kernel_version = ?9,
         session_type = ?10, runtime_engine = ?11, runtime_version = ?12, gpu_mode = ?13,
         event_count = event_count + ?17`);
        try {
            await env.DB.batch(m.counters.map((c) => userUpsert.bind(m.version, m.os, m.arch ?? "", m.osBuild ?? 0, m.osRevision ?? 0, m.channel ?? "", m.distroId ?? "", m.distroVersion ?? "", m.kernelVersion ?? "", m.sessionType ?? "", m.runtimeEngine ?? "", m.runtimeVersion ?? "", m.gpuMode ?? "", c.signal, c.bucket, m.installId, c.count)));
        }
        catch (err) {
            console.warn("metric_users write failed", err);
        }
    }
    return new Response("ok", { status: 202 });
}
// Each stats module renders only its own section, so a page load should query
// only what that section shows — the 30-day COUNT(DISTINCT) over metric_users,
// the heaviest query, is read solely by the preferences module.
export async function handleStats(request: Request, env: Env, user: User, activeModule: StatsModule): Promise<Response> {
    const url = new URL(request.url);
    const filters = statsFilters(url);
    const days = filters.windowDays;
    const since = currentWindowSince(days);
    const surface = activeModule === "diagnostics" ? "desktop" : filters.surface;
    if (activeModule === "diagnostics")
        filters.surface = "desktop";
    if (surface === "cli")
        await ensureCLITelemetrySchema(env);
    const pingsTable = telemetryTableNames(surface).pings;
    const bars = (sql: string) => env.DB.prepare(sql).all<Bar>().then((r) => r.results);
    const pingVersions = () => bars(`SELECT version AS label, COUNT(DISTINCT install_id) AS users FROM ${pingsTable} WHERE date >= date('now', '${since}') GROUP BY label ORDER BY users DESC LIMIT 15`);
    const pingPlatforms = () => bars(`SELECT os || ' ' || arch AS label, COUNT(DISTINCT install_id) AS users FROM ${pingsTable} WHERE date >= date('now', '${since}') GROUP BY label ORDER BY users DESC`);
    let daily: {
        date: string;
        users: number;
        opens: number;
    }[] = [];
    let versions: Bar[] = [];
    let platforms: Bar[] = [];
    let crashes: Awaited<ReturnType<typeof crashGroups>>["results"] = [];
    let metrics: MetricTotals = [];
    let previousMetrics: MetricTotals = [];
    let metricUsers: MetricTotals = [];
    let metricUsersUnavailable = false;
    let metricUsersComputedAt = "";
    let sources: Bar[] = [];
    let diagnosticFacets: DiagnosticFacets = {
        versions: [], platforms: [],
        osBuilds: [], osRevisions: [], distros: [], distroVersions: [], kernels: [], sessions: [],
        architectures: [], channels: [], runtimes: [], runtimeEngines: [],
        failureKinds: [], failureReasons: [], exitCodes: [], recoveries: [], gpuStates: [],
    };
    let installationLinkedSince = "";
    let overview: OverviewCounts = {
        latestAdoptionPct: null,
        openReports: 0,
        newLatestReports: 0,
        regressedReports: 0,
        criticalOpenReports: 0,
    };
    let latestVersion = "";
    if (activeModule === "usage") {
        latestVersion = await latestObservedVersion(env, surface);
        const [dailyR, versionsR, platformsR, metricsR, overviewR] = await Promise.all([
            env.DB.prepare(`SELECT date, COUNT(*) AS users, SUM(opens) AS opens FROM ${pingsTable} WHERE date >= date('now', '${since}') GROUP BY date`).all<{
                date: string;
                users: number;
                opens: number;
            }>(),
            pingVersions(),
            pingPlatforms(),
            metricRows(env, days, surface),
            diagnosticOverview(env, latestVersion, days, surface),
        ]);
        daily = dailyR.results;
        versions = versionsR;
        platforms = platformsR;
        metrics = metricsR;
        overview = overviewR;
    }
    else if (activeModule === "diagnostics") {
        latestVersion = await latestObservedVersion(env, "desktop");
        const [crashesR, sourcesR, facets, linkedSince] = await Promise.all([
            crashGroups(env, filters, latestVersion),
            bars(`SELECT source AS label, COUNT(*) AS users FROM groups WHERE ${diagnosticWindowWhere(days)} GROUP BY source ORDER BY users DESC`),
            loadDiagnosticFacets(env, days),
            env.DB.prepare("SELECT value FROM diagnostics_meta WHERE key = 'installation_linked_since'").first<{
                value: string;
            }>(),
        ]);
        crashes = crashesR.results;
        sources = sourcesR;
        versions = facets.versions;
        platforms = facets.platforms;
        diagnosticFacets = facets;
        installationLinkedSince = linkedSince?.value ?? "";
    }
    else if (activeModule === "preferences") {
        const [metricsR, usersR] = await Promise.all([metricRows(env, days, surface), metricUserRows(env, days, surface)]);
        metrics = metricsR;
        metricUsersUnavailable = usersR === null;
        metricUsers = usersR?.rows ?? [];
        metricUsersComputedAt = usersR?.computedAt ?? "";
    }
    else {
        const [metricsR, previousMetricsR, usersR] = await Promise.all([
            metricRows(env, days, surface),
            metricRows(env, days, surface, true),
            metricUserRows(env, days, surface),
        ]);
        metrics = metricsR;
        previousMetrics = previousMetricsR;
        metricUsersUnavailable = usersR === null;
        metricUsers = usersR?.rows ?? [];
        metricUsersComputedAt = usersR?.computedAt ?? "";
    }
    return html(renderStats({ daily, versions, platforms, crashes, metrics, previousMetrics, metricUsers, metricUsersUnavailable, metricUsersComputedAt, sources, diagnosticFacets, installationLinkedSince, overview, latestVersion, filters }, user, activeModule));
}
export async function handleGroup(env: Env, fingerprint: string, user: User): Promise<Response> {
    const group = await env.DB.prepare("SELECT * FROM groups WHERE fingerprint = ?1").bind(fingerprint).first<Group>();
    if (!group)
        return new Response("not found", { status: 404 });
    group.severity = effectiveGroupSeverity(group);
    const reports = await env.DB.prepare(`SELECT version, os, arch, message, device, created_at, source, label, error_type, error_message,
      top_frame, build_commit, channel, language, view, breadcrumbs, component_stack, stack, occurred_at, webview2, web_runtime
     FROM reports WHERE fingerprint = ?1 ORDER BY id DESC`)
        .bind(fingerprint)
        .all<{
        version: string;
        os: string;
        arch: string;
        message: string;
        device: string;
        created_at: string;
        source: string;
        label: string;
        error_type: string;
        error_message: string;
        top_frame: string;
        build_commit: string;
        channel: string;
        language: string;
        view: string;
        breadcrumbs: string;
        component_stack: string;
        stack: string;
        occurred_at: string;
        webview2: string;
        web_runtime: string;
    }>();
    return html(renderGroup(group, reports.results, user, await groupDiagnosticSummary(env, fingerprint)));
}
export async function handleGroupAction(request: Request, env: Env, admin: User, fingerprint: string): Promise<Response> {
    if (!sameOrigin(request))
        return new Response("forbidden", { status: 403 });
    const parsed = GroupAction.safeParse(await formObject(request));
    if (!parsed.success)
        return redirect(`/stats/group/${fingerprint}`);
    const a = parsed.data;
    if (a.action === "delete") {
        await env.DB.batch([
            env.DB.prepare("DELETE FROM reports WHERE fingerprint = ?1").bind(fingerprint),
            env.DB.prepare("DELETE FROM report_daily WHERE fingerprint = ?1").bind(fingerprint),
            env.DB.prepare("DELETE FROM report_installations WHERE fingerprint = ?1").bind(fingerprint),
            env.DB.prepare("DELETE FROM report_event_dimensions WHERE fingerprint = ?1").bind(fingerprint),
            env.DB.prepare("DELETE FROM groups WHERE fingerprint = ?1").bind(fingerprint),
        ]);
        await logAction(env, admin, "delete_group", fingerprint.slice(0, 8));
        return redirect("/stats");
    }
    if (a.action === "status") {
        const status = a.status ?? "open";
        await env.DB.prepare("UPDATE groups SET status = ?1, resolved_at = CASE WHEN ?1 = 'resolved' THEN ?3 ELSE resolved_at END WHERE fingerprint = ?2")
            .bind(status, fingerprint, new Date().toISOString())
            .run();
        await logAction(env, admin, "set_status", fingerprint.slice(0, 8), status);
        return redirect(`/stats/group/${fingerprint}`);
    }
    if (a.action === "resolution") {
        await env.DB.prepare("UPDATE groups SET resolved_in = ?1 WHERE fingerprint = ?2")
            .bind(a.resolvedIn ?? "", fingerprint)
            .run();
        await logAction(env, admin, "set_resolved_in", fingerprint.slice(0, 8), a.resolvedIn ?? "");
        return redirect(`/stats/group/${fingerprint}`);
    }
    if (a.action === "severity") {
        await env.DB.prepare("UPDATE groups SET severity = ?1 WHERE fingerprint = ?2")
            .bind(a.severity ?? "medium", fingerprint)
            .run();
        await logAction(env, admin, "set_severity", fingerprint.slice(0, 8), a.severity ?? "medium");
        return redirect(`/stats/group/${fingerprint}`);
    }
    await env.DB.prepare("UPDATE groups SET note = ?1 WHERE fingerprint = ?2").bind(a.note ?? "", fingerprint).run();
    await logAction(env, admin, "set_note", fingerprint.slice(0, 8));
    return redirect(`/stats/group/${fingerprint}`);
}
export async function handleAdminUsers(request: Request, env: Env, admin: User): Promise<Response> {
    if (!sameOrigin(request))
        return new Response("forbidden", { status: 403 });
    const parsed = UserAction.safeParse(await formObject(request));
    if (!parsed.success)
        return redirect("/admin");
    const a = parsed.data;
    if (a.userId === admin.id)
        return redirect("/admin");
    const target = await env.DB.prepare("SELECT email, role FROM access WHERE id = ?1")
        .bind(a.userId)
        .first<{
        email: string;
        role: Role;
    }>();
    if (!target)
        return redirect("/admin");
    if (a.action === "delete") {
        await env.DB.prepare("DELETE FROM access WHERE id = ?1").bind(a.userId).run();
        await logAction(env, admin, "delete_user", target.email);
        return redirect("/admin");
    }
    const role: Role = a.role ?? "pending";
    const now = new Date().toISOString();
    await env.DB.prepare("UPDATE access SET role = ?1, approved_at = ?2, approved_by = ?3 WHERE id = ?4")
        .bind(role, role === "pending" ? null : now, admin.email, a.userId)
        .run();
    await logAction(env, admin, "set_role", target.email, `${target.role} → ${role}`);
    return redirect("/admin");
}
export async function handleAdminList(env: Env, admin: User): Promise<Response> {
    const users = await env.DB.prepare("SELECT id, email, role, created_at, approved_at FROM access ORDER BY (role = 'pending') DESC, created_at DESC").all<UserRow>();
    return html(renderUsers(admin, users.results));
}
export async function handleAdminAudit(env: Env, admin: User): Promise<Response> {
    const rows = await env.DB.prepare("SELECT at, actor_email, action, target, detail FROM audit_log ORDER BY id DESC LIMIT 200").all<AuditRow>();
    return html(renderAudit(admin, rows.results));
}
export async function handleCommunityList(env: Env, admin: User, status: string): Promise<Response> {
    const rows = await new PackageRepo(env.REGISTRY_DB).listByStatus(status, 200);
    return html(renderCommunity(admin, rows, status));
}
export async function handleCommunityAction(request: Request, env: Env, admin: User, handle: string, name: string, action: string): Promise<Response> {
    if (!sameOrigin(request))
        return new Response("forbidden", { status: 403 });
    const form = await formObject(request);
    const backStatus = ["pending", "active", "hidden", "rejected"].includes(form.status) ? form.status : "pending";
    const back = redirect(`/community?status=${backStatus}`);
    const slug = `${handle}/${name}`;
    const repo = new PackageRepo(env.REGISTRY_DB);
    const now = new Date().toISOString();
    if (action === "verify" || action === "unverify") {
        await repo.setVerified(slug, action === "verify", now);
        await logAction(env, admin, `pkg_${action}`, slug);
        return back;
    }
    if (action === "approve") {
        const expectedStatus = ["pending", "hidden", "rejected"].includes(form.expectedStatus)
            ? form.expectedStatus
            : "";
        if (!form.expectedVersion || !form.expectedUpdatedAt || !expectedStatus) {
            return new Response("Package review revision is missing. Refresh the review page and try again.", {
                status: 409,
            });
        }
        const row = await repo.setStatusIfCurrent(slug, "active", form.expectedVersion, form.expectedUpdatedAt, expectedStatus, now);
        if (!row) {
            return new Response("Package changed since it was reviewed. Refresh and review the latest version.", {
                status: 409,
            });
        }
        // Emit the publish event only after the reviewed revision becomes public.
        await new EventRepo(env.REGISTRY_DB).log({
            type: "publish",
            packageId: row.id,
            actorHandle: row.scope_handle,
            summary: `published ${row.slug}@${row.latest_version}`,
            now,
        });
        await logAction(env, admin, "pkg_approve", slug);
        return back;
    }
    await repo.setStatus(slug, action === "reject" ? "rejected" : "hidden", now);
    await logAction(env, admin, `pkg_${action}`, slug);
    return back;
}
async function sendAlert(env: Env, text: string): Promise<void> {
    if (!env.ALERT_WEBHOOK)
        return;
    try {
        const webhook = new URL(env.ALERT_WEBHOOK);
        const feishu = webhook.hostname === "open.feishu.cn" || webhook.hostname === "open.larksuite.com";
        const body = feishu ? { msg_type: "text", content: { text } } : { text };
        const res = await fetch(webhook.toString(), {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify(body),
        });
        if (!res.ok)
            console.error(`alert webhook responded ${res.status}`);
    }
    catch (err) {
        console.error("alert webhook unreachable", err);
    }
}
export async function runIngestSentinel(env: Env): Promise<void> {
    const problems: string[] = [];
    try {
        await env.DB.prepare(`INSERT INTO pings (date, install_id, version, os, arch, opens)
       VALUES (date('now'), ?1, 'canary', 'canary', 'canary', 0)
       ON CONFLICT (date, install_id) DO NOTHING`)
            .bind(CANARY_INSTALL_ID)
            .run();
        // Also removes any leftover canary from a run that died mid-way.
        await env.DB.prepare("DELETE FROM pings WHERE install_id = ?1").bind(CANARY_INSTALL_ID).run();
    }
    catch (err) {
        problems.push(`canary write failed: ${errText(err)}`);
    }
    try {
        // Auto-create the one-row checkpoint so existing databases do not need a
        // manual migration before this worker version is deployed.
        await env.DB.prepare(`CREATE TABLE IF NOT EXISTS ingest_sentinel_state (
         id INTEGER PRIMARY KEY CHECK (id = 1),
         day TEXT NOT NULL,
         ping_count INTEGER NOT NULL,
         open_count INTEGER NOT NULL,
         checked_at TEXT NOT NULL
       )`).run();
        const row = await env.DB.prepare(`SELECT date('now') AS day,
              COUNT(*) AS ping_count,
              COALESCE(SUM(opens), 0) AS open_count
       FROM pings
       WHERE date = date('now') AND install_id <> ?1`)
            .bind(CANARY_INSTALL_ID)
            .first<{
            day: string;
            ping_count: number;
            open_count: number;
        }>();
        const day = row?.day ?? "";
        const pingCount = Number(row?.ping_count ?? 0);
        const openCount = Number(row?.open_count ?? 0);
        const previous = await env.DB.prepare("SELECT day, ping_count, open_count, checked_at FROM ingest_sentinel_state WHERE id = 1").first<{
            day: string;
            ping_count: number;
            open_count: number;
            checked_at: string;
        }>();
        if (!pingCount) {
            problems.push("no launch pings recorded today (UTC)");
        }
        else if (previous?.day === day &&
            pingCount <= Number(previous.ping_count) &&
            openCount <= Number(previous.open_count)) {
            problems.push(`launch ping totals unchanged since ${previous.checked_at} UTC (${pingCount} install rows, ${openCount} opens)`);
        }
        await env.DB.prepare(`INSERT INTO ingest_sentinel_state (id, day, ping_count, open_count, checked_at)
       VALUES (1, ?1, ?2, ?3, datetime('now'))
       ON CONFLICT (id) DO UPDATE SET
         day = ?1, ping_count = ?2, open_count = ?3, checked_at = datetime('now')`)
            .bind(day, pingCount, openCount)
            .run();
    }
    catch (err) {
        problems.push(`ping progress check failed: ${errText(err)}`);
    }
    if (!problems.length)
        return;
    const message = `crash.reasonix.io ingest sentinel: ${problems.join("; ")} — https://crash.reasonix.io/stats`;
    console.error(message);
    await sendAlert(env, message);
}
export async function purgeExpiredStatsRows(env: Env): Promise<void> {
    try {
        await ensureCLITelemetrySchema(env);
    }
    catch (err) {
        console.error("retention: CLI telemetry schema unavailable", err);
    }
    for (const { table, keepDays } of RETENTION) {
        // Keep exactly the newest `keepDays` dates: today plus keepDays-1 back,
        // matching the `date >= date('now', '-{keepDays-1} day')` reads.
        const cutoff = `-${keepDays - 1} day`;
        let purged = 0;
        try {
            for (let i = 0; i < RETENTION_MAX_CHUNKS; i++) {
                const res = await env.DB.prepare(`DELETE FROM ${table} WHERE rowid IN (
             SELECT rowid FROM ${table} WHERE date < date('now', ?1) LIMIT ${RETENTION_CHUNK_ROWS}
           )`)
                    .bind(cutoff)
                    .run();
                const changes = res.meta.changes ?? 0;
                purged += changes;
                if (changes < RETENTION_CHUNK_ROWS)
                    break;
            }
            console.log(`retention: purged ${purged} rows from ${table} (keep ${keepDays}d)`);
        }
        catch (err) {
            // One broken table must not stop the others; the cron retries tomorrow.
            console.error(`retention: purge failed for ${table} after ${purged} rows`, err);
        }
    }
}

