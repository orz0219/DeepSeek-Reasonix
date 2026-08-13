// UsageStatsPanel renders the "usage statistics" subtab inside the Models
// settings page. It reads aggregated stats from the Go backend (App.UsageStats)
// and draws three charts by hand in SVG — a GitHub-style activity heatmap, a
// stacked per-day token trend, and a per-model donut — so no chart library is
// needed and theme variables (--accent, --fg, --bg-elev-*) drive
// the palette for both stock themes and theme packs. Model colours come from
// a fixed two-set categorical palette (--chart-1..5 plus the gray
// --chart-other, light/dark variants defined in styles.css from GitHub
// Primer's data-viz tokens): a model's colour is its rank among the top five
// by token volume, and everything beyond the top five collapses into one gray
// "Other" step.
// The component's styles live in UsageStatsPanel.css (loaded on demand with
// this chunk), so the ~7 KB rule block never inflates the settings bundle.
import { useLayoutEffect, useRef, useState } from "react";
import { Activity, CalendarDays, Coins, Cpu, MessageSquare, MessagesSquare } from "lucide-react";
import type { UsageStatsRange } from "../lib/types";
import { cacheRateText } from "./UsageStatsPanel";
import { UsageStatsTranslator } from "./usage_stats_helpers";
// ── Section 2+3: numeric cards ────────────────────────────────────────────
export function StatCards({ stats, t }: {
    stats: UsageStatsRange;
    t: UsageStatsTranslator;
}) {
    const topModel = stats.topModel || t("common.none");
    // The model name is the longest value: it may wrap to a second line on
    // narrow windows instead of being shrunk or truncated; tokens shows the
    // exact number and stays on one line (FitText shrinks it if needed).
    const cards: Array<{
        icon: typeof Coins;
        label: string;
        value: string;
        sm?: boolean;
        wrap?: boolean;
        hint?: string;
    }> = [
        { icon: Coins, label: t("settings.stats.tokens"), value: stats.tokens.toLocaleString("en-US") },
        // Turn markers count completed top-level turns; a conversation session may
        // contain many of them, so the user-facing label names the exact metric.
        { icon: MessageSquare, label: t("settings.stats.sessions"), value: String(stats.turns) },
        { icon: MessagesSquare, label: t("settings.stats.requests"), value: String(stats.requests) },
        { icon: CalendarDays, label: t("settings.stats.activeDays"), value: String(stats.activeDays) },
        // Average prompt-cache hit ratio over the range: cached input tokens
        // divided by all input tokens. "—" when no usage is in range yet.
        { icon: Activity, label: t("settings.stats.cacheRate"), value: cacheRateText(stats.cacheHit, stats.cacheMiss), hint: t("settings.stats.cacheRateHint") },
        // "top model" ranks by token volume (not call count) — the hint keeps the
        // metric's meaning visible next to the value.
        { icon: Cpu, label: t("settings.stats.topModel"), value: topModel, sm: true, wrap: true, hint: t("settings.stats.topModelHint") },
    ];
    return (<div className="usage-stats__cards">
      {cards.map((c) => (<div className="usage-stats__card" key={c.label} title={c.hint}>
          <div className="usage-stats__card-head">
            <c.icon className="usage-stats__card-icon" size={14} strokeWidth={2} aria-hidden="true"/>
            <span className="usage-stats__card-label">{c.label}</span>
          </div>
          {c.wrap ? (<div className="usage-stats__card-value usage-stats__card-value--sm usage-stats__card-value--wrap">{c.value}</div>) : (<FitText text={c.value} className={`usage-stats__card-value${c.sm ? " usage-stats__card-value--sm" : ""}`} maxSize={c.sm ? 14 : 22}/>)}
        </div>))}
    </div>);
}
// FitText renders `text` on a single line, shrinking the font until it fits
// the card width (long token numbers never overflow or wrap).
function FitText({ text, className, maxSize }: {
    text: string;
    className?: string;
    maxSize: number;
}) {
    const ref = useRef<HTMLDivElement>(null);
    const [size, setSize] = useState(maxSize);
    useLayoutEffect(() => {
        const el = ref.current;
        if (!el)
            return;
        const fit = () => {
            let s = maxSize;
            el.style.fontSize = `${s}px`;
            while (el.scrollWidth > el.clientWidth + 1 && s > 11) {
                s -= 0.5;
                el.style.fontSize = `${s}px`;
            }
            setSize(s);
        };
        fit();
        const ro = new ResizeObserver(fit);
        ro.observe(el);
        return () => ro.disconnect();
    }, [text, maxSize]);
    return (<div ref={ref} className={className} style={{ fontSize: size }}>
      {text}
    </div>);
}

