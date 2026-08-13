import type { DailyTokenUsage, ModelTokenUsage } from "../lib/types";
export const RANGE_PRESETS = ["7", "14", "30", "90"] as const;
// Every entry point that records usage (see StatsSource tags in the Go
// kernel). "all" is the unfiltered aggregate; the rest match one source label.
export const SOURCES = ["all", "desktop", "cli", "serve", "bot", "remote"] as const;
// The heatmap always shows a fixed 40-week window regardless of the range
// preset (it only follows the source filter).
export const HEAT_WEEKS = 40;
// Custom ranges may span up to ten years. Keep the detailed trend bounded so
// one unusual range cannot create thousands of interactive SVG nodes.
export const MAX_TREND_DAYS = 180;
// Keep this lazy panel's translations in its own chunk. Putting them in the
// eager global English dictionary makes an unopened settings subtab part of
// every desktop startup bundle.
export const USAGE_STATS_TRANSLATIONS = {
    en: {
        "common.loading": "Loading…",
        "common.none": "none",
        "settings.stats.range": "Time range",
        "settings.stats.rangePreset.7": "Last 7 days",
        "settings.stats.rangePreset.14": "Last 14 days",
        "settings.stats.rangePreset.30": "Last 30 days",
        "settings.stats.rangePreset.90": "Last 90 days",
        "settings.stats.rangeCustom": "Custom",
        "settings.stats.from": "From",
        "settings.stats.to": "To",
        "settings.stats.source": "Source",
        "settings.stats.source.all": "All",
        "settings.stats.source.desktop": "Desktop",
        "settings.stats.source.cli": "CLI",
        "settings.stats.source.serve": "Web",
        "settings.stats.source.bot": "Bot",
        "settings.stats.source.remote": "Remote",
        "settings.stats.refresh": "Refresh",
        "settings.stats.tokens": "Token usage",
        "settings.stats.sessions": "Completed turns",
        "settings.stats.requests": "Requests",
        "settings.stats.activeDays": "Active days",
        "settings.stats.cacheRate": "Avg cache hit rate",
        "settings.stats.cacheRateHint": "Cached input tokens as a share of all input tokens in the range",
        "settings.stats.cacheHitRate": "Cache hit rate",
        "settings.stats.hitRateLegend": "Cache hit rate",
        "settings.stats.topModel": "Most used model",
        "settings.stats.topModelHint": "Ranked by token volume, not call count",
        "settings.stats.heatmap": "Activity heatmap",
        "settings.stats.heatLess": "Less",
        "settings.stats.heatMore": "More",
        "settings.stats.dailyTrend": "Daily token trend",
        "settings.stats.trendLimited": "Showing the latest 180 days",
        "settings.stats.modelUsage": "Model usage",
        "settings.stats.other": "Other",
        "settings.stats.moreModels": "more models",
        "settings.stats.total": "Total",
        "settings.stats.percent": "Share",
        "settings.stats.asOf": "As of",
        "settings.stats.empty": "No usage data in this range yet. Token usage is recorded from the day this feature ships.",
    },
    zh: {
        "common.loading": "加载中…",
        "common.none": "无",
        "settings.stats.range": "时间范围",
        "settings.stats.rangePreset.7": "最近 7 天",
        "settings.stats.rangePreset.14": "最近 14 天",
        "settings.stats.rangePreset.30": "最近 30 天",
        "settings.stats.rangePreset.90": "最近 90 天",
        "settings.stats.rangeCustom": "自定义",
        "settings.stats.from": "开始日期",
        "settings.stats.to": "结束日期",
        "settings.stats.source": "统计来源",
        "settings.stats.source.all": "全部",
        "settings.stats.source.desktop": "桌面端",
        "settings.stats.source.cli": "命令行",
        "settings.stats.source.serve": "网页端",
        "settings.stats.source.bot": "机器人",
        "settings.stats.source.remote": "远程工作台",
        "settings.stats.refresh": "刷新",
        "settings.stats.tokens": "Tokens 用量",
        "settings.stats.sessions": "完成轮次",
        "settings.stats.requests": "请求数量",
        "settings.stats.activeDays": "活跃天数",
        "settings.stats.cacheRate": "平均缓存命中率",
        "settings.stats.cacheRateHint": "时间段内缓存命中 token 占输入 token 的比例",
        "settings.stats.cacheHitRate": "缓存命中率",
        "settings.stats.hitRateLegend": "缓存命中率",
        "settings.stats.topModel": "最常用模型",
        "settings.stats.topModelHint": "按 token 用量排序，非调用次数",
        "settings.stats.heatmap": "活跃热力图",
        "settings.stats.heatLess": "较少",
        "settings.stats.heatMore": "较多",
        "settings.stats.dailyTrend": "按天 Token 趋势",
        "settings.stats.trendLimited": "仅显示最近 180 天",
        "settings.stats.modelUsage": "模型用量",
        "settings.stats.other": "其他",
        "settings.stats.moreModels": "个其他模型",
        "settings.stats.total": "总用量",
        "settings.stats.percent": "占比",
        "settings.stats.asOf": "统计截至",
        "settings.stats.empty": "当前时间范围内暂无用量数据。Token 用量从本功能启用后开始累计。",
    },
    "zh-TW": {
        "common.loading": "載入中…",
        "common.none": "無",
        "settings.stats.range": "時間範圍",
        "settings.stats.rangePreset.7": "最近 7 天",
        "settings.stats.rangePreset.14": "最近 14 天",
        "settings.stats.rangePreset.30": "最近 30 天",
        "settings.stats.rangePreset.90": "最近 90 天",
        "settings.stats.rangeCustom": "自訂",
        "settings.stats.from": "開始日期",
        "settings.stats.to": "結束日期",
        "settings.stats.source": "統計來源",
        "settings.stats.source.all": "全部",
        "settings.stats.source.desktop": "桌面端",
        "settings.stats.source.cli": "命令列",
        "settings.stats.source.serve": "網頁端",
        "settings.stats.source.bot": "機器人",
        "settings.stats.source.remote": "遠端工作台",
        "settings.stats.refresh": "重新整理",
        "settings.stats.tokens": "Tokens 用量",
        "settings.stats.sessions": "完成輪次",
        "settings.stats.requests": "請求數量",
        "settings.stats.activeDays": "活躍天數",
        "settings.stats.cacheRate": "平均快取命中率",
        "settings.stats.cacheRateHint": "時間範圍內快取命中 token 佔輸入 token 的比例",
        "settings.stats.cacheHitRate": "快取命中率",
        "settings.stats.hitRateLegend": "快取命中率",
        "settings.stats.topModel": "最常用模型",
        "settings.stats.topModelHint": "依 token 用量排序，非呼叫次數",
        "settings.stats.heatmap": "活躍熱力圖",
        "settings.stats.heatLess": "較少",
        "settings.stats.heatMore": "較多",
        "settings.stats.dailyTrend": "按天 Token 趨勢",
        "settings.stats.trendLimited": "僅顯示最近 180 天",
        "settings.stats.modelUsage": "模型用量",
        "settings.stats.other": "其他",
        "settings.stats.moreModels": "個其他模型",
        "settings.stats.total": "總用量",
        "settings.stats.percent": "佔比",
        "settings.stats.asOf": "統計截至",
        "settings.stats.empty": "目前時間範圍內暫無用量資料。Token 用量從此功能啟用後開始累計。",
    },
} as const;
type UsageStatsKey = keyof typeof USAGE_STATS_TRANSLATIONS.en;
export type UsageStatsTranslator = (key: UsageStatsKey) => string;
// Model colour palette: a fixed two-set categorical series (--chart-1..5 with
// light/dark variants defined in styles.css, from GitHub Primer's data-viz
// tokens). A model's colour is its rank among the top five; models beyond the
// top five share one gray "Other" step (--chart-other). The palette is
// deliberately independent of --accent so charts stay readable in every theme
// style and theme pack, whose tokens only cover the app chrome.
// Each series colour is mixed toward --bg-elev like the heatmap levels
// (MODEL_COLOR_MIX% colour + the rest background), so the pure hexes sit
// softly on the card instead of glaring; the two themes still get the same
// hues, only the background differs.
export const TOP_MODELS = 5;
export const OTHER_MODEL = "\u0000other"; // sentinel; cannot collide with a real model ref
export const OTHER_COLOR = "var(--chart-other)";
export const MODEL_COLOR_MIX = 72; // percent of the series colour in the --bg-elev mix
export const MAX_TOOLTIP_OTHER_DETAILS = 5;
// A grouped day carries the raw tail split so hover tooltips can expand the
// "Other" step into its per-model detail without extra queries.
export type GroupedDaily = DailyTokenUsage & {
    otherByModel: Record<string, number>;
};
// A grouped model may carry the tail list that "Other" aggregates.
export type GroupedModel = ModelTokenUsage & {
    items?: ModelTokenUsage[];
};
// localDay returns today's date (plus/minus offsetDays) in the local calendar,
// matching the backend's "2006-01-02" day keys.
export function localDay(offsetDays: number): string {
    const d = new Date();
    d.setDate(d.getDate() + offsetDays);
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    const day = String(d.getDate()).padStart(2, "0");
    return `${y}-${m}-${day}`;
}

