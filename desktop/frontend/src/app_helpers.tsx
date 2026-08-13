import { useShellExpand } from "./lib/shellExpand";
import { t } from "./lib/i18n";
import { localizedNoticeText, type Item, type LiveStream } from "./lib/useController";
import { NoticeCard } from "./components/Transcript";
import { type DecisionSurfaceKind as MockDecisionSurfaceKind } from "./lib/decisionSurfaceMock";
import { type SessionMeta, type TabMeta, type TokenMode } from "./lib/types";
import { type PendingNavigationRequest } from "./lib/openTopicCoalescing";
import { type Theme } from "./lib/theme";
import { applyTextSize, DEFAULT_TEXT_SIZE, getTextSize, nextTextSize } from "./lib/textSize";
import { useGlobalShortcut } from "./lib/keyboardShortcuts";
import { SidebarImConnection } from "./app_sidebar";
import { DesktopPlatform, browserPlatformOverride } from "./app_window";
/** Footer decision surface kinds. Runtime blockers are explicit recovery choices. */
export type DecisionSurfaceKind = MockDecisionSurfaceKind | "extension_form";
export const TERMINAL_CLOSE_TRANSITION_MS = 250;
export function noticePreviewMockEnabled(): boolean {
    const value = browserMockScenarioParam();
    return value === "notice" || value === "notices" || value === "notice-preview";
}
export function runtimeProfileShortKey(mode: TokenMode) {
    return mode === "economy"
        ? "composer.runtimeProfileEconomyShort" as const
        : mode === "delivery"
            ? "composer.runtimeProfileDeliveryShort" as const
            : "composer.runtimeProfileBalancedShort" as const;
}
function noticePreviewItems(): Item[] {
    const notice = (index: number, level: "info" | "warn", text: string, detail: string, code?: string): Item => ({
        kind: "notice",
        id: `notice-preview-${index}`,
        level,
        text: localizedNoticeText(text, code),
        detail,
    });
    return [
        {
            kind: "notice",
            id: "notice-preview-delivery",
            level: "info",
            variant: "delivery",
            title: t("notice.deliveryIncompleteTitle"),
            text: t("notice.deliveryIncompleteBody"),
            detail: "final-answer readiness failed 3 times: missing verification, review_report, and complete_step receipts",
            action: "continue_delivery",
        },
        notice(1, "info", "No visible answer was produced; asking the assistant to respond again.", "empty final answer blocked: qwen3.7-plus returned no visible answer text (finish=stop, reasoning=2314 chars); retrying", "empty_final"),
        notice(2, "info", "The assistant answered before taking action; asking it to use the required tools.", "executor handoff: assistant produced a proposal before running required repository commands; nudged to execute", "executor_handoff"),
        notice(3, "info", "Tool round limit reached; asking the assistant to summarize progress.", "tool budget reached after 128 tool calls; requesting a progress summary before continuing", "tool_budget"),
        notice(4, "info", "The assistant is stuck retrying a blocked action; asking it to change approach.", "loop guard: repeated command failure matched the same stderr signature across 3 attempts", "loop_guard"),
        notice(5, "info", "Context is getting large; preserving cache until cleanup is needed.", "context window 82% full; deferred cleanup to preserve reusable prompt cache"),
        notice(6, "info", "Context cleanup skipped for now.", "cleanup skipped: recent turn included unresolved user approval state"),
        notice(7, "info", "Automatic context cleanup paused because the context window is too small.", "configured compact threshold exceeds current model context window; auto cleanup paused for this model"),
        notice(8, "info", "Context was compacted without a generated summary.", "compaction completed after upstream summary generation returned empty content; retained transcript checkpoint"),
        notice(9, "info", "Goal is not ready to complete yet; continuing the remaining work.", "goal completion check found pending validation: desktop/frontend typecheck"),
        notice(13, "info", "Goal still has unfinished task state; continuing the remaining work.", "active goal has open task state: implement preview, verify browser, report result"),
        notice(16, "warn", "background export failed: needs attention", "background export failed: session archive upload returned 503 after 3 retries"),
        notice(17, "warn", "Job artifact migration failed.", "artifact migration failed for job job_123: checksum mismatch while moving output.zip"),
        notice(18, "warn", "Background job teardown timed out.", "job job_123 did not stop within 10s; process is still marked running by the supervisor"),
        notice(19, "warn", "Some plan-mode tool settings were ignored.", "plan-mode tool settings ignored: unsupported tool allowlist entry \"browser.screenshot\""),
        notice(20, "warn", "Some plan-mode command settings were ignored.", "plan-mode command settings ignored: invalid read-only prefix \"npm && test\""),
        notice(21, "warn", "Config migration did not complete.", "config migration failed at providers.defaultModel: unknown provider reference \"old/deepseek\""),
        notice(22, "warn", "Selected model is missing its API key.", "selected model deepseek/deepseek-v4-pro requires DEEPSEEK_API_KEY, but no key is configured"),
        notice(23, "warn", "An MCP server failed to start.", "mcp server \"github\" failed to start: command not found: mcp-server-github"),
        notice(24, "warn", "Some MCP servers failed to start; run /mcp for details.", "mcp startup failures: github(command not found), linear(authentication expired)"),
        notice(25, "warn", "Guardian was disabled because its model was not found.", "guardian model \"glm-5-guard\" is not present in the configured provider catalog"),
        notice(26, "warn", "Guardian was disabled because it could not start.", "guardian startup failed: provider returned 401 unauthorized"),
    ];
}
export function NoticePreviewPanel() {
    return (<div style={{
            flex: "1 1 auto",
            minHeight: 0,
            overflow: "auto",
            padding: "44px 24px 128px",
        }}>
      <div style={{ maxWidth: 920, margin: "0 auto" }}>
        {noticePreviewItems().map((item) => {
            if (item.kind !== "notice")
                return null;
            return <NoticeCard key={item.id} item={item} onAction={item.action ? () => undefined : undefined}/>;
        })}
      </div>
    </div>);
}
export function stripLegacyGoalBudgetFlags(arg: string): string {
    const parts = arg.trim().split(/\s+/).filter(Boolean);
    while (parts.length > 0) {
        const flag = parts[0].toLowerCase();
        if (flag !== "--research" && flag !== "--auto-research" && flag !== "--deep" && flag !== "--simple" && flag !== "--no-research")
            break;
        parts.shift();
    }
    return parts.join(" ");
}
export function hasLegacyGoalBudgetFlag(arg: string): boolean {
    const first = arg.trim().split(/\s+/, 1)[0]?.toLowerCase();
    return first === "--research" || first === "--auto-research" || first === "--deep" || first === "--simple" || first === "--no-research";
}
export function isThemeMode(value: string): value is Theme {
    return value === "auto" || value === "light" || value === "dark";
}
export type DesktopLayoutStyle = "classic" | "workbench" | "creation";
export function normalizeDesktopLayoutStyle(style: string | undefined): DesktopLayoutStyle {
    if (style === "workbench")
        return "workbench";
    if (style === "creation")
        return "creation";
    return "classic";
}
export const SHOW_CONTEXT_DOCK = true;
const DISMISSED_TODO_STORAGE_KEY = "todoPanel:dismissedKeys";
const MAX_DISMISSED_TODO_KEYS = 160;
type HistoryScopeFilter = {
    scope: "global" | "project";
    workspaceRoot: string;
};
export type WorkspaceInsertTarget = "composer" | "planRevision";
export type HistoryViewState = {
    kind: "history";
    source: "scope";
    filter: HistoryScopeFilter;
    sessions: SessionMeta[];
} | {
    kind: "history";
    source: "all";
    sessions: SessionMeta[];
} | {
    kind: "trash";
    sessions: SessionMeta[];
};
export type DesktopNavigationIntent = {
    kind: "topic";
    scope: string;
    workspaceRoot: string;
    topicId: string;
    sessionPath?: string;
} | {
    kind: "blank";
    scope: string;
    workspaceRoot: string;
} | {
    kind: "delivery-worktree";
    workspaceRoot: string;
} | {
    kind: "sidebar-im";
    connection: SidebarImConnection;
} | {
    kind: "resume-session";
    session: SessionMeta;
};
export type DesktopNavigationInput = DesktopNavigationIntent & {
    navigationIntentSeq: number;
};
export type PendingDesktopNavigationRequest = PendingNavigationRequest<DesktopNavigationInput>;
export function loadDismissedTodoKeys(): Set<string> {
    try {
        const saved = window.localStorage.getItem(DISMISSED_TODO_STORAGE_KEY);
        if (!saved)
            return new Set();
        const parsed = JSON.parse(saved) as unknown;
        if (!Array.isArray(parsed))
            return new Set();
        return new Set(parsed.filter((value): value is string => typeof value === "string" && value.length > 0));
    }
    catch {
        return new Set();
    }
}
export function saveDismissedTodoKeys(keys: ReadonlySet<string>): void {
    try {
        window.localStorage.setItem(DISMISSED_TODO_STORAGE_KEY, JSON.stringify(Array.from(keys).slice(-MAX_DISMISSED_TODO_KEYS)));
    }
    catch {
        /* ignore quota errors */
    }
}
export const GUIDANCE_QUEUE_MOCK_ITEMS = [
    "先确认发送后输入框为什么残留刚发的消息，再决定修哪里。",
    "保持真实 steer 协议不变，只调整前端乐观队列和按钮状态。",
    "最后补后端 submit 悬挂时的回归测试，确保输入框会立刻释放。",
] as const;
export function browserMockScenarioParam(): string {
    if (typeof window === "undefined" || window.runtime)
        return "";
    return new URLSearchParams(window.location.search).get("mock")?.trim().toLowerCase() ?? "";
}
export function isGuidanceMockScenario(value: string): boolean {
    return value === "guidance" || value === "guide" || value === "steer";
}
export function detectBrowserPlatform(): DesktopPlatform {
    const override = browserPlatformOverride();
    if (override)
        return override;
    if (typeof navigator === "undefined")
        return "linux";
    const marker = `${navigator.platform} ${navigator.userAgent}`;
    if (/Win/i.test(marker))
        return "windows";
    if (/Mac/i.test(marker))
        return "darwin";
    return "linux";
}
export function tabWorkspaceTitle(tab?: TabMeta): string {
    if (!tab)
        return "Global";
    if (tab.scope === "project")
        return tab.workspaceName || tab.workspaceRoot || "Project";
    if (tab.scope === "global")
        return tab.workspaceName || "Global";
    return tab.workspaceName || tab.workspaceRoot || "Global";
}
export function topicTitle(tab?: TabMeta): string {
    if (!tab)
        return "Global";
    const workspaceTitle = tabWorkspaceTitle(tab);
    const topic = tab.topicTitle || (tab.scope === "global" ? workspaceTitle : "Untitled");
    return topic === workspaceTitle ? workspaceTitle : `${workspaceTitle} / ${topic}`;
}
export function topicDisplayTitle(tab?: TabMeta): string {
    if (!tab)
        return "Global";
    return tab.topicTitle || (tab.scope === "global" ? tabWorkspaceTitle(tab) : "Untitled");
}
export function sessionsForScope(sessions: SessionMeta[], filter: HistoryScopeFilter): SessionMeta[] {
    if (filter.scope === "project") {
        return sessions.filter((session) => session.scope === "project" && session.workspaceRoot === filter.workspaceRoot);
    }
    return sessions.filter((session) => (session.scope || "global") === "global");
}
export function isMissingSessionError(err: unknown): boolean {
    const message = err instanceof Error ? err.message : String(err ?? "");
    return /no such file|cannot find the file|file does not exist|session is pending cleanup|session .*not found/i.test(message);
}
export function workspaceDisplayName(path?: string): string {
    if (!path)
        return "";
    const parts = path.split(/[/\\]/).filter(Boolean);
    return parts.length > 0 ? parts[parts.length - 1] : path;
}
function materializeLiveItems(items: Item[], live?: LiveStream): Item[] {
    if (!live)
        return items;
    return items.map((item) => {
        if (item.kind !== "assistant" || item.id !== live.id)
            return item;
        return { ...item, text: live.text, reasoning: live.reasoning, streaming: true };
    });
}
function fence(label: string, value: string): string {
    if (!value.trim())
        return "";
    const fenceToken = value.includes("```") ? "````" : "```";
    return `${label}\n${fenceToken}\n${value.trim()}\n${fenceToken}`;
}
export function sessionItemsToMarkdown(title: string, items: Item[], live?: LiveStream): string {
    const lines: string[] = [`# ${title.trim() || "Reasonix session"}`, ""];
    for (const item of materializeLiveItems(items, live)) {
        switch (item.kind) {
            case "user":
                lines.push("## User", "", item.text.trim(), "");
                break;
            case "assistant":
                lines.push("## Assistant");
                if (item.reasoning.trim()) {
                    lines.push("", "### Reasoning", "", item.reasoning.trim());
                }
                if (item.text.trim()) {
                    lines.push("", item.text.trim());
                }
                lines.push("");
                break;
            case "tool":
                lines.push(`### Tool: ${item.name}`);
                if (item.args.trim())
                    lines.push("", fence("Args", item.args));
                if (item.output?.trim())
                    lines.push("", fence("Output", item.output));
                if (item.error?.trim())
                    lines.push("", fence("Error", item.error));
                lines.push("");
                break;
            case "phase":
                lines.push(`### Phase`, "", item.text.trim(), "");
                break;
            case "notice":
                lines.push(`### ${item.level === "warn" ? "Warning" : "Notice"}`, "", item.text.trim(), "");
                if (item.detail?.trim()) {
                    lines.push("Details:", "", item.detail.trim(), "");
                }
                break;
            case "compaction":
                lines.push("### Context Compaction", "");
                if (item.pending) {
                    lines.push("Compaction pending.");
                }
                else {
                    lines.push(`Messages: ${item.messages}`);
                    if (item.trigger)
                        lines.push(`Trigger: ${item.trigger}`);
                    if (item.summary.trim())
                        lines.push("", item.summary.trim());
                }
                lines.push("");
                break;
        }
    }
    return lines.join("\n").replace(/\n{3,}/g, "\n\n").trimEnd() + "\n";
}
export function sessionItemsToJson(title: string, items: Item[], live?: LiveStream): string {
    return JSON.stringify({
        title,
        exportedAt: new Date().toISOString(),
        items: materializeLiveItems(items, live),
    }, null, 2);
}
export function safeFilename(name: string): string {
    const cleaned = name.trim().replace(/[\\/:*?"<>|]+/g, "-").replace(/\s+/g, " ").slice(0, 80);
    return cleaned || "reasonix-session";
}
/** Global hotkey handler for shell-expand toggle (Ctrl/Cmd+B). */
export function ShellHotkeys() {
    const shellExpand = useShellExpand();
    useGlobalShortcut("shell.toggle", () => shellExpand?.toggleLast(), [shellExpand], Boolean(shellExpand));
    return null;
}
/** Global hotkey handler for text-size shortcuts (Ctrl/Cmd + Plus/Minus/0). */
export function TextSizeHotkeys() {
    useGlobalShortcut("textSize.increase", () => applyTextSize(nextTextSize(getTextSize(), 1)));
    useGlobalShortcut("textSize.decrease", () => applyTextSize(nextTextSize(getTextSize(), -1)));
    useGlobalShortcut("textSize.reset", () => applyTextSize(DEFAULT_TEXT_SIZE));
    return null;
}

