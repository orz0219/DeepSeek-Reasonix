import type { Env } from "./env";
import { DEVELOPMENT_FINGERPRINT_PREFIX, crashGroups, currentWindowSince, developmentGroupSQL, diagnosticFacets as loadDiagnosticFacets, diagnosticWindowWhere, effectiveGroupSeverity, groupDiagnosticSummary, isDevelopmentGroup, reportAggregateStatements, type DiagnosticFacets, } from "./diagnostics_v2";
import { newestReleaseVersion, storageUnavailable, UserAction, GroupAction, OverviewCounts, previousWindowSince, previousWindowUntil, Bar, MetricTotals, requireViewer, registryBindings, communityStatus } from "./index_stats";
import { RETENTION, RETENTION_CHUNK_ROWS, RETENTION_MAX_CHUNKS, SENTINEL_CRON, ROLLUP_CRON, ROLLUP_WINDOW_DAYS, ROLLUP_SIGNALS_PER_RUN, ensureRollupSchema, CANARY_INSTALL_ID, errText } from "./index_rollup";
import { ClientSurfaceName, telemetryTableNames, latestAdoptionPct } from "./index";
export async function formObject(request: Request): Promise<Record<string, string>> {
    const form = await request.formData();
    const out: Record<string, string> = {};
    for (const [k, v] of form)
        out[k] = typeof v === "string" ? v : "";
    return out;
}
export async function latestObservedVersion(env: Env, surface: ClientSurfaceName): Promise<string> {
    const table = telemetryTableNames(surface).pings;
    // Require independent installations and use pings as the sole source of
    // release truth. A single synthetic diagnostic must never promote v9.9.9 (or
    // a prerelease) to "latest" for every report group.
    const sql = `SELECT version FROM ${table}
    WHERE date >= date('now', '-29 day') AND version <> ''
    GROUP BY version HAVING COUNT(DISTINCT install_id) >= 2`;
    const rows = await env.DB.prepare(sql).all<{
        version: string;
    }>();
    return newestReleaseVersion(rows.results.map((r) => r.version));
}
export async function diagnosticOverview(env: Env, latestVersion: string, days: 7 | 30, surface: ClientSurfaceName): Promise<OverviewCounts> {
    if (surface === "cli") {
        return {
            latestAdoptionPct: await latestAdoptionPct(env, latestVersion, days, surface),
            openReports: 0,
            newLatestReports: 0,
            regressedReports: 0,
            criticalOpenReports: 0,
        };
    }
    // Keep the overview's red state aligned with the effective severity used by
    // the diagnostics list. Historical rows retain their stored severity, so
    // known browser notices and development builds must be discounted here too.
    const criticalActionable = `(severity = 'critical' OR (
    severity = 'high'
    AND kind <> 'performance'
    AND NOT ${developmentGroupSQL}
    AND title <> '[window.error] Script error.'
    AND title NOT LIKE '%ResizeObserver loop %'
    AND title NOT LIKE '%Minified React error #520%'
    AND title NOT LIKE '%additional File object is not a file on the disk%'
  ))`;
    const diagnosticCounts = latestVersion
        ? env.DB.prepare(`SELECT
          SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END) AS open_reports,
          SUM(CASE WHEN first_version = ?1 THEN 1 ELSE 0 END) AS new_latest_reports,
          SUM(CASE WHEN regressed_at <> '' THEN 1 ELSE 0 END) AS regressed_reports,
          SUM(CASE WHEN status = 'open' AND ${criticalActionable} THEN 1 ELSE 0 END) AS critical_open_reports
        FROM groups WHERE ${diagnosticWindowWhere(days)}`)
            .bind(latestVersion)
            .first<{
            open_reports: number;
            new_latest_reports: number;
            regressed_reports: number;
            critical_open_reports: number;
        }>()
        : env.DB.prepare(`SELECT
          SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END) AS open_reports,
          0 AS new_latest_reports,
          SUM(CASE WHEN regressed_at <> '' THEN 1 ELSE 0 END) AS regressed_reports,
          SUM(CASE WHEN status = 'open' AND ${criticalActionable} THEN 1 ELSE 0 END) AS critical_open_reports
        FROM groups WHERE ${diagnosticWindowWhere(days)}`).first<{
            open_reports: number;
            new_latest_reports: number;
            regressed_reports: number;
            critical_open_reports: number;
        }>();
    const [row, adoptionPct] = await Promise.all([
        diagnosticCounts,
        latestAdoptionPct(env, latestVersion, days, surface),
    ]);
    return {
        latestAdoptionPct: adoptionPct,
        openReports: Number(row?.open_reports ?? 0),
        newLatestReports: Number(row?.new_latest_reports ?? 0),
        regressedReports: Number(row?.regressed_reports ?? 0),
        criticalOpenReports: Number(row?.critical_open_reports ?? 0),
    };
}
export async function metricRows(env: Env, days: 7 | 30, surface: ClientSurfaceName, previous = false): Promise<{
    signal: string;
    bucket: string;
    total: number;
}[]> {
    const where = previous
        ? `date >= date('now', '${previousWindowSince(days)}') AND date < date('now', '${previousWindowUntil(days)}')`
        : `date >= date('now', '${currentWindowSince(days)}')`;
    const table = telemetryTableNames(surface).metrics;
    const rows = await env.DB.prepare(`SELECT signal, bucket, SUM(count) AS total FROM ${table} WHERE ${where} GROUP BY signal, bucket ORDER BY signal, total DESC`).all<{
        signal: string;
        bucket: string;
        total: number;
    }>();
    return rows.results;
}
// The 30-day desktop window is served from the cron-built rollup: computing it
// live exceeds what D1 spends on one query (see refreshMetricUserRollup). Null
// here means "not computed yet", which the dashboard says out loud rather than
// rendering as an empty result.
async function rollupMetricUserRows(env: Env): Promise<{
    rows: {
        signal: string;
        bucket: string;
        total: number;
    }[];
    computedAt: string;
} | null> {
    try {
        await ensureRollupSchema(env);
        const rows = await env.DB.prepare(`SELECT signal, bucket, total, computed_at FROM metric_user_rollup WHERE window_days = ?1 ORDER BY signal, total DESC`)
            .bind(ROLLUP_WINDOW_DAYS)
            .all<{
            signal: string;
            bucket: string;
            total: number;
            computed_at: string;
        }>();
        if (!rows.results.length)
            return null;
        // Oldest wins: the cursor refreshes a slice at a time, so this is how far
        // behind the least recently recomputed signal is.
        const computedAt = rows.results.reduce((min, r) => (r.computed_at < min ? r.computed_at : min), rows.results[0].computed_at);
        return { rows: rows.results, computedAt };
    }
    catch (err) {
        console.warn("metric_user_rollup read failed", err);
        return null;
    }
}
// Null means the query did not complete, which is distinct from "no rows": at
// ~1M rows a day, the 30-day COUNT(DISTINCT install_id) exceeds what D1 will
// spend on one query and comes back as a CPU-limit reset. Rendering that as an
// empty dashboard would read as "nobody uses these settings".
export async function metricUserRows(env: Env, days: 7 | 30, surface: ClientSurfaceName): Promise<{
    rows: {
        signal: string;
        bucket: string;
        total: number;
    }[];
    computedAt: string;
} | null> {
    if (surface === "desktop" && days === ROLLUP_WINDOW_DAYS)
        return rollupMetricUserRows(env);
    try {
        const table = telemetryTableNames(surface).metricUsers;
        const rows = await env.DB.prepare(`SELECT signal, bucket, COUNT(DISTINCT install_id) AS total FROM ${table} WHERE date >= date('now', '${currentWindowSince(days)}') GROUP BY signal, bucket ORDER BY signal, total DESC`).all<{
            signal: string;
            bucket: string;
            total: number;
        }>();
        return { rows: rows.results, computedAt: "" };
    }
    catch (err) {
        console.warn("metric_users query failed", err);
        return null;
    }
}

