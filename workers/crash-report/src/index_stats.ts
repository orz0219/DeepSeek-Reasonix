// Ingest + dashboard for desktop crash/feedback/performance reports and the
// anonymous launch ping. Frontend reports are user-initiated; native fatal and
// lifecycle reports are sent on the next launch under the same opt-out desktop
// telemetry gate as pings.
import { z } from "zod";
import type { Env } from "./env";
import { html, redirect } from "./shell";
import { atLeast, currentUser, loginUrl, logAction, sameOrigin, sharedLogout, type Role, type User, } from "./auth";
import type { Bindings as RegistryBindings } from "./registry/env";
import { DEVELOPMENT_FINGERPRINT_PREFIX, crashGroups, currentWindowSince, developmentGroupSQL, diagnosticFacets as loadDiagnosticFacets, diagnosticWindowWhere, effectiveGroupSeverity, groupDiagnosticSummary, isDevelopmentGroup, reportAggregateStatements, type DiagnosticFacets, } from "./diagnostics_v2";
import { latestAdoptionPct } from "./index";
// Ingest writes fail together when the database is unhealthy (e.g. the D1
// size cap: every INSERT throws while reads stay fine). Surface that as a
// deliberate 503 with a loud log instead of an opaque worker exception, so
// `wrangler tail` / observability show the root cause and clients see a
// retryable status.
export function storageUnavailable(op: string, err: unknown): Response {
    console.error(`${op}: D1 write failed`, err);
    return new Response("storage unavailable", { status: 503 });
}
export const UserAction = z.object({
    action: z.enum(["role", "delete"]),
    userId: z.coerce.number().int().positive(),
    role: z.enum(["pending", "viewer", "admin"]).optional(),
});
export const GroupAction = z.object({
    action: z.enum(["status", "delete", "note", "resolution", "severity"]),
    status: z.enum(["open", "resolved", "ignored"]).optional(),
    note: z.string().max(500).optional(),
    resolvedIn: z.string().max(64).optional(),
    severity: z.enum(["low", "medium", "high", "critical"]).optional(),
});
type ParsedVersion = {
    version: string;
    major: number;
    minor: number;
    patch: number;
};
function parseReleaseVersion(version: string): ParsedVersion | null {
    // The dashboard's "latest" lane is for shipped stable builds. Development,
    // prerelease, and build-metadata values remain visible in version facets but
    // must not become the release baseline used for regression triage.
    const m = version.trim().match(/^v?(\d+)\.(\d+)\.(\d+)$/);
    if (!m)
        return null;
    return {
        version,
        major: Number(m[1]),
        minor: Number(m[2]),
        patch: Number(m[3]),
    };
}
export function newestReleaseVersion(versions: string[]): string {
    const parsed = versions
        .filter((v) => v && v.toLowerCase() !== "dev")
        .map(parseReleaseVersion)
        .filter((v): v is ParsedVersion => v !== null);
    parsed.sort((a, b) => b.major - a.major ||
        b.minor - a.minor ||
        b.patch - a.patch ||
        b.version.localeCompare(a.version));
    return parsed[0]?.version ?? "";
}
export type OverviewCounts = {
    latestAdoptionPct: number | null;
    openReports: number;
    newLatestReports: number;
    regressedReports: number;
    criticalOpenReports: number;
};
export function previousWindowSince(days: 7 | 30): string {
    return `-${days * 2 - 1} day`;
}
export function previousWindowUntil(days: 7 | 30): string {
    return currentWindowSince(days);
}
export type Bar = {
    label: string;
    users: number;
};
export type MetricTotals = {
    signal: string;
    bucket: string;
    total: number;
}[];
export function requireViewer(user: User | null, login: string): Response | null {
    if (!user)
        return redirect(login);
    if (!atLeast(user.role, "viewer"))
        return redirect("/account");
    return null;
}
// The folded registry API runs against its own database and resolves identity
// itself; hand it the second binding plus the account/site origins it expects.
export function registryBindings(env: Env): RegistryBindings {
    return {
        DB: env.REGISTRY_DB,
        WRITE_LIMITER: env.WRITE_LIMITER,
        ACCOUNTS_ORIGIN: env.ID_ORIGIN ?? "https://id.reasonix.io",
        APP_ORIGIN: env.APP_ORIGIN ?? "https://reasonix.io",
        ALLOWED_ORIGINS: env.ALLOWED_ORIGINS ?? "https://reasonix.io,https://www.reasonix.io",
    };
}
export function communityStatus(url: URL): string {
    const s = url.searchParams.get("status") ?? "pending";
    return ["pending", "active", "hidden", "rejected"].includes(s) ? s : "pending";
}

