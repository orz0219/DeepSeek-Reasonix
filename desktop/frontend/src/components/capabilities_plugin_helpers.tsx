import { asArray } from "../lib/array";
import { useT } from "../lib/i18n";
import type { PluginCompatibilityIssue, PluginView } from "../lib/types";
import { PluginInstallPlanView, PluginRuntimePlan, PluginInstallPlanAction } from "./capabilities_plugins";
export function normalizePluginViews(plugins: PluginView[] | null | undefined): PluginView[] {
    return sortPluginsForDisplay(asArray(plugins).map(normalizePluginView));
}
export function normalizePluginView(plugin: PluginView): PluginView {
    return {
        ...plugin,
        name: plugin.name || "plugin",
        root: plugin.root || "",
        enabled: Boolean(plugin.enabled),
        skills: Number.isFinite(plugin.skills) ? plugin.skills : 0,
        commands: Number.isFinite(plugin.commands) ? plugin.commands : 0,
        agents: Number.isFinite(plugin.agents) ? plugin.agents : 0,
        hooks: Number.isFinite(plugin.hooks) ? plugin.hooks : 0,
        mcpServers: Number.isFinite(plugin.mcpServers) ? plugin.mcpServers : 0,
        skillDetails: asArray(plugin.skillDetails),
        agentDetails: asArray(plugin.agentDetails),
        commandDetails: asArray(plugin.commandDetails),
        hookDetails: asArray(plugin.hookDetails),
        mcpServerDetails: asArray(plugin.mcpServerDetails),
        warnings: asArray(plugin.warnings),
    };
}
function sortPluginsForDisplay(plugins: PluginView[]): PluginView[] {
    return [...plugins].sort((a, b) => {
        const priority = pluginDisplayPriority(a) - pluginDisplayPriority(b);
        if (priority !== 0)
            return priority;
        return a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
    });
}
function pluginDisplayPriority(plugin: PluginView): number {
    if (plugin.error)
        return 0;
    if (plugin.enabled)
        return 1;
    return 2;
}
export function pluginListSummary(plugins: PluginView[], t: ReturnType<typeof useT>): string {
    const enabled = plugins.filter((plugin) => plugin.enabled && !plugin.error).length;
    const issues = plugins.filter((plugin) => Boolean(plugin.error) || asArray(plugin.warnings).length > 0).length;
    return t("caps.pluginsSummary", { enabled, total: plugins.length, issues });
}
export function pluginCapabilitiesSummary(plugin: PluginView, t: ReturnType<typeof useT>): string {
    if (plugin.skills === 0 && (plugin.agents || 0) === 0 && (plugin.commands || 0) === 0 && plugin.hooks === 0 && plugin.mcpServers === 0)
        return t("caps.pluginNoCapabilities");
    return t("caps.pluginCounts", { skills: plugin.skills, agents: plugin.agents || 0, commands: plugin.commands || 0, hooks: plugin.hooks, mcps: plugin.mcpServers });
}
export function pluginCompatibilityLabel(status: string, t: ReturnType<typeof useT>): string {
    if (status === "full")
        return t("caps.pluginCompatibilityFull");
    if (status === "partial")
        return t("caps.pluginCompatibilityPartial");
    if (status === "none")
        return t("caps.pluginCompatibilityNone");
    return status;
}
export function pluginWarnings(plugin: PluginView, diagnostic?: PluginView): string[] {
    const warnings = [...asArray(plugin.warnings), ...asArray(diagnostic?.warnings)];
    return Array.from(new Set(warnings.filter((warning) => warning.trim().length > 0)));
}
export function parsePluginInstallPlan(raw: string): PluginInstallPlanView {
    try {
        const value = JSON.parse(raw) as Record<string, unknown>;
        const actions = (Array.isArray(value.actions) ? value.actions : []).flatMap((action) => {
            if (!action || typeof action !== "object")
                return [];
            const item = action as Record<string, unknown>;
            return [{
                    action: stringValue(item.action),
                    kind: stringValue(item.kind),
                    name: stringValue(item.name),
                    source: stringValue(item.source),
                    status: stringValue(item.status),
                    message: stringValue(item.message),
                    error: stringValue(item.error),
                    compatibility: stringValue(item.compatibility),
                    mappedCapabilities: (Array.isArray(item.mappedCapabilities) ? item.mappedCapabilities : []).filter((value): value is string => typeof value === "string"),
                    skippedCapabilities: (Array.isArray(item.skippedCapabilities) ? item.skippedCapabilities : []) as PluginCompatibilityIssue[],
                    runtime: parsePluginRuntimePlan(item.runtime),
                    agentCount: numericValue(item.agentCount), skillCount: numericValue(item.skillCount), commandCount: numericValue(item.commandCount), hookCount: numericValue(item.hookCount), toolCount: numericValue(item.toolCount),
                }];
        });
        return {
            raw,
            ok: typeof value.ok === "boolean" ? value.ok : undefined,
            status: stringValue(value.status),
            name: stringValue(value.name),
            actions,
            warnings: (Array.isArray(value.warnings) ? value.warnings : []).flatMap((warning) => typeof warning === "string" ? [warning] : []),
            error: stringValue(value.error),
        };
    }
    catch {
        return { raw, actions: [], warnings: [] };
    }
}
function numericValue(value: unknown): number | undefined {
    return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}
function stringValue(value: unknown): string | undefined {
    return typeof value === "string" && value.trim() ? value.trim() : undefined;
}
// parsePluginRuntimePlan extracts the FULL TRUST runtime block a plugin
// install plan carries (installsource.RuntimePlanInfo). Anything malformed
// simply drops out — the risk UI is additive and must never break planning.
function parsePluginRuntimePlan(value: unknown): PluginRuntimePlan | undefined {
    if (!value || typeof value !== "object")
        return undefined;
    const item = value as Record<string, unknown>;
    const command = stringValue(item.command);
    if (!command)
        return undefined;
    const list = (v: unknown): string[] => (Array.isArray(v) ? v : []).filter((entry): entry is string => typeof entry === "string" && entry.trim().length > 0);
    return {
        command,
        args: list(item.args),
        intercepts: list(item.intercepts),
        replaces: list(item.replaces),
        capabilities: list(item.capabilities),
        fullTrust: item.fullTrust === true,
    };
}
export function pluginPlanActionLabel(action: PluginInstallPlanAction, t: ReturnType<typeof useT>): string {
    const verb = action.action || action.kind || t("caps.pluginAction");
    return [verb, action.name].filter(Boolean).join(" · ");
}
export function pluginPlanNotice(plan: PluginInstallPlanView, t: ReturnType<typeof useT>): string {
    if (plan.error)
        return plan.error;
    if (plan.status === "done" || plan.status === "applied" || plan.status === "complete")
        return t("caps.pluginPlanInstalled");
    return plan.status ? t("caps.pluginPlanStatus", { status: plan.status }) : t("caps.pluginPlanComplete");
}

