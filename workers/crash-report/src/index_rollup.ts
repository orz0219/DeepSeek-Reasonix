import type { Env } from "./env";
// Time-series retention, run by the daily cron trigger. Every dashboard query
// against the per-install tables reads at most the current window (-29 day),
// while the aggregate `metrics` table also serves the 30d view's
// previous-window delta (back to -59 day), so it keeps a doubled horizon.
// `reports`/`groups` are excluded on purpose: they are the triage queue and
// the regression baseline, are not date-partitioned, and their growth is
// already bounded by per-group sampling. Without this purge the database
// grows until D1's size cap, at which point every ingest write starts
// throwing (all of /v1/ping, /v1/metrics and /v1/report 500 while reads keep
// working — exactly the 2026-07-03 stats blackout).
export const RETENTION = [
    { table: "report_daily", keepDays: 30 },
    { table: "report_installations", keepDays: 30 },
    { table: "report_event_dimensions", keepDays: 30 },
    { table: "pings", keepDays: 30 },
    { table: "metrics", keepDays: 60 },
    { table: "metric_users", keepDays: 30 },
    { table: "cli_pings", keepDays: 30 },
    { table: "cli_metrics", keepDays: 60 },
    { table: "cli_metric_users", keepDays: 30 },
] as const;
// Deletes run in rowid chunks so a run never holds one giant transaction.
// Steady state is one expired day per table; the chunk cap is a backstop that
// still drains ~2M rows per table per run after an ingest outage or backlog.
export const RETENTION_CHUNK_ROWS = 10000;
export const RETENTION_MAX_CHUNKS = 200;
// Must match the sentinel entry in wrangler.toml [triggers] exactly — the
// scheduled handler dispatches on controller.cron; every other trigger
// (the retention cron, manual runs) falls through to the purge.
export const SENTINEL_CRON = "17 1,7,13,19 * * *";
export const ROLLUP_CRON = "23 * * * *";
// The preferences module's 30-day COUNT(DISTINCT install_id) spans ~28M rows
// and D1 abandons it mid-query. It cannot be summed from per-day totals either:
// an install active on twelve days would count twelve times. So the window is
// computed here instead, one signal at a time — a single signal takes ~1s, and
// the cursor spreads the ~57 of them across hourly runs rather than blowing one
// invocation's CPU budget.
export const ROLLUP_WINDOW_DAYS = 30;
export const ROLLUP_SIGNALS_PER_RUN = 8;
const ROLLUP_SCHEMA_SQL = [
    `CREATE TABLE IF NOT EXISTS metric_user_rollup (
     window_days INTEGER NOT NULL,
     signal TEXT NOT NULL,
     bucket TEXT NOT NULL,
     total INTEGER NOT NULL,
     computed_at TEXT NOT NULL,
     PRIMARY KEY (window_days, signal, bucket)
   )`,
    `CREATE TABLE IF NOT EXISTS metric_user_rollup_state (
     id INTEGER PRIMARY KEY CHECK (id = 1),
     next_signal INTEGER NOT NULL,
     updated_at TEXT NOT NULL
   )`,
] as const;
const rollupSchemaPromises = new WeakMap<object, Promise<void>>();
export function ensureRollupSchema(env: Pick<Env, "DB">): Promise<void> {
    const key = env.DB as unknown as object;
    const existing = rollupSchemaPromises.get(key);
    if (existing)
        return existing;
    const creation = env.DB
        .batch(ROLLUP_SCHEMA_SQL.map((sql) => env.DB.prepare(sql)))
        .then(() => undefined)
        .catch((err) => {
        rollupSchemaPromises.delete(key);
        throw err;
    });
    rollupSchemaPromises.set(key, creation);
    return creation;
}
// Ingest sentinel. The 2026-07-03 blackout went unnoticed for ten days because
// clients swallow ping failures by design and nothing watched the write path.
// Four times a day (hours chosen so the UTC day always has >1h of traffic;
// ~14k DAU means a healthy hour is never empty) this probes the two failure
// shapes independently:
//   1. canary write into `pings` (immediately deleted) — catches writes
//      throwing, e.g. the D1 size cap, regardless of traffic;
//   2. today's real ping and open totals compared with the previous run —
//      catches ingest dying upstream of the worker (edge blocking, client
//      regression) even after the UTC day already has traffic.
// Alerts go to the optional ALERT_WEBHOOK secret; without it they still land
// in the worker logs. While broken this fires at most 4 alerts/day.
export const CANARY_INSTALL_ID = "ffffffffffffffffffffffffffffffff";
export function errText(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
}

