import { useT } from "../../../lib/i18n";
import type { HeartbeatTask } from "./heartbeat.types";
const INTERVAL_MS: Record<"s" | "m" | "h", number> = {
    s: 1000,
    m: 60000,
    h: 3600000,
};
function heartbeatIntervalMs(interval?: string): number | null {
    const clean = (interval || "").replace(/\|.*$/, "");
    const m = clean.match(/^(\d+)([smh])$/);
    if (!m)
        return null;
    return parseInt(m[1], 10) * INTERVAL_MS[m[2] as "s" | "m" | "h"];
}
function heartbeatClockMinutes(value?: string): number | null {
    const m = (value || "").match(/^(\d{2}):(\d{2})$/);
    if (!m)
        return null;
    const hour = parseInt(m[1], 10);
    const minute = parseInt(m[2], 10);
    if (hour < 0 || hour > 23 || minute < 0 || minute > 59)
        return null;
    return hour * 60 + minute;
}
function dateAtMinutes(base: Date, minutes: number): Date {
    const d = new Date(base);
    d.setHours(Math.floor(minutes / 60), minutes % 60, 0, 0);
    return d;
}
function heartbeatWithinWindow(date: Date, start: number | null, end: number | null): boolean {
    if (start === null && end === null)
        return true;
    const minutes = date.getHours() * 60 + date.getMinutes();
    if (start !== null && end === null)
        return minutes >= start;
    if (start === null && end !== null)
        return minutes < end;
    if (start === end)
        return true;
    if (start! < end!)
        return minutes >= start! && minutes < end!;
    return minutes >= start! || minutes < end!;
}
function nextHeartbeatWindowTime(from: Date, start: number | null, end: number | null): Date {
    if (heartbeatWithinWindow(from, start, end))
        return from;
    if (start !== null && end === null)
        return dateAtMinutes(from, start);
    if (start === null && end !== null) {
        const next = new Date(from);
        next.setDate(next.getDate() + 1);
        next.setHours(0, 0, 0, 0);
        return next;
    }
    const minutes = from.getHours() * 60 + from.getMinutes();
    if (start! < end! && minutes < start!)
        return dateAtMinutes(from, start!);
    if (start! > end! && minutes < start! && minutes >= end!)
        return dateAtMinutes(from, start!);
    const next = dateAtMinutes(from, start!);
    next.setDate(next.getDate() + 1);
    return next;
}
export function heartbeatNextRunAt(task: Pick<HeartbeatTask, "interval" | "lastRunAt" | "timeWindowStart" | "timeWindowEnd">, now = Date.now()): number | null {
    if (!task.lastRunAt)
        return null;
    const intervalMs = heartbeatIntervalMs(task.interval);
    if (intervalMs === null)
        return null;
    const rawNext = task.lastRunAt + intervalMs;
    if ((task.interval || "").includes("|"))
        return rawNext;
    const start = heartbeatClockMinutes(task.timeWindowStart);
    const end = heartbeatClockMinutes(task.timeWindowEnd);
    if (start === null && end === null)
        return rawNext;
    const candidate = new Date(Math.max(rawNext, now));
    return nextHeartbeatWindowTime(candidate, start, end).getTime();
}
export function heartbeatIntervalLabel(interval: string | undefined, t: ReturnType<typeof useT>): string {
    const cycleMatch = (interval || "").match(/^(\d+)[smh]\|(daily|weekly|biweekly|monthly|yearly)(?::([^@]*))?(?:@(\d{2}:\d{2}))?$/);
    if (cycleMatch) {
        const [, , type, days, time] = cycleMatch;
        const timeStr = time ? ` ${time}` : "";
        if (type === "daily")
            return `${t("heartbeat.cycleDaily")}${timeStr}`;
        if (type === "weekly")
            return `${t("heartbeat.cycleWeekly")}${timeStr}`;
        if (type === "biweekly")
            return `${t("heartbeat.cycleBiweekly")}${timeStr}`;
        if (type === "monthly")
            return `${t("heartbeat.cycleMonthly")}${days ? ` ${days}` : ""}${timeStr}`;
        if (type === "yearly") {
            const parts = (days || "").split("-");
            return `${t("heartbeat.cycleYearly")} ${parts[0] || "1"}/${parts[1] || "1"}${timeStr}`;
        }
    }
    const clean = (interval || "").replace(/\|.*$/, "");
    const m = clean.match(/^(\d+)([smh])$/);
    if (!m)
        return clean;
    const unitLabels: Record<string, string> = {
        s: t("heartbeat.unitSec"),
        m: t("heartbeat.unitMin"),
        h: t("heartbeat.unitHour"),
    };
    return `${t("heartbeat.freqEvery")}${t("heartbeat.everyJoiner")}${m[1]}${unitLabels[m[2]] || m[2]}`;
}
// ── Cycle Editor ──────────────────────────────────────────────────────────────
export const WEEKDAYS = [
    { key: "mon", labelKey: "heartbeat.weekdayMon" },
    { key: "tue", labelKey: "heartbeat.weekdayTue" },
    { key: "wed", labelKey: "heartbeat.weekdayWed" },
    { key: "thu", labelKey: "heartbeat.weekdayThu" },
    { key: "fri", labelKey: "heartbeat.weekdayFri" },
    { key: "sat", labelKey: "heartbeat.weekdaySat" },
    { key: "sun", labelKey: "heartbeat.weekdaySun" },
] as const;
const ALL_WEEKDAYS = WEEKDAYS.map(w => w.key);
const DEFAULT_WEEKLY_DAY = "mon";
export function defaultHeartbeatCycleDays(cycleType: string): string[] {
    if (cycleType === "daily")
        return [...ALL_WEEKDAYS];
    if (cycleType === "weekly" || cycleType === "biweekly")
        return [DEFAULT_WEEKLY_DAY];
    return [];
}
export function heartbeatBuildCycleInterval(cycleType: string, days: string[], time: string): string {
    const base: Record<string, string> = {
        daily: "24h",
        weekly: "168h",
        biweekly: "336h",
        monthly: "720h",
        yearly: "8760h",
    };
    const selectedDays = days.filter(Boolean);
    const isDailyWithSelection = cycleType === "daily" && selectedDays.length > 0 && selectedDays.length < 7;
    const isDailyWithoutSelection = cycleType === "daily" && selectedDays.length === 0;
    const effectiveType = isDailyWithoutSelection || isDailyWithSelection ? "weekly" : cycleType;
    const scheduleDays = (effectiveType === "weekly" || effectiveType === "biweekly") && selectedDays.length === 0
        ? defaultHeartbeatCycleDays(effectiveType)
        : selectedDays;
    let suffix = `|${effectiveType}`;
    if (effectiveType === "weekly" || effectiveType === "biweekly") {
        suffix += `:${scheduleDays.join(",")}`;
    }
    else if (effectiveType === "monthly") {
        suffix += `:${scheduleDays[0] || "1"}`;
    }
    else if (effectiveType === "yearly") {
        suffix += `:${scheduleDays[0] || "1"}-${scheduleDays[1] || "1"}`;
    }
    suffix += `@${time}`;
    return (base[cycleType] || "24h") + suffix;
}
// ── Editor ─────────────────────────────────────────────────────────────────────
export function normalizeMode(mode: "ask" | "auto" | "yolo" | undefined): "ask" | "auto" | "yolo" {
    if (mode === "ask" || mode === "auto" || mode === "yolo")
        return mode;
    return "yolo"; // default
}

