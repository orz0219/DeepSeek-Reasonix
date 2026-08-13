import { asArray } from "../lib/array";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import type { CapabilitiesView, PluginView, ServerView, SkillsSettingsView, SkillView, TabMeta } from "../lib/types";
import { mcpServerSchemaIssueCount } from "./CapabilitiesPanel";
import { shouldOpenAuth } from "./capabilities_servers";
// CapabilitiesPanel is the desktop MCP & Skills drawer — the GUI counterpart to
// the CLI's /mcp + /skill, aligning with Claude Code's Customize → Connectors:
// each server shows a connected/failed dot, transport, and tool/prompt/resource
// counts, with add / remove / retry; skills list their scope and run mode.
export type CapTab = "servers" | "skills";
type SettingsSnapshot<T> = {
    key: string;
    value: T;
};
export function connectMCPServer(name: string, servers: ServerView[]): Promise<void> {
    const server = servers.find((candidate) => candidate.name === name);
    if (server && shouldOpenAuth(server))
        return app.AuthenticateMCPServer(name);
    return app.ReconnectMCPServer(name);
}
export let mcpSettingsSnapshot: SettingsSnapshot<ServerView[]> | null = null;
export let skillsSettingsSnapshot: SettingsSnapshot<SkillsSettingsView> | null = null;
export let pluginsSettingsSnapshot: SettingsSnapshot<PluginView[]> | null = null;
export function setMCPSettingsSnapshot(value: SettingsSnapshot<ServerView[]> | null): void { mcpSettingsSnapshot = value; }
export function setSkillsSettingsSnapshot(value: SettingsSnapshot<unknown> | null): void { skillsSettingsSnapshot = value as SettingsSnapshot<SkillsSettingsView> | null; }
export function setPluginsSettingsSnapshot(value: SettingsSnapshot<unknown> | null): void { pluginsSettingsSnapshot = value as SettingsSnapshot<PluginView[]> | null; }
export function settingsSnapshotKey(meta: Awaited<ReturnType<typeof app.Meta>> | null | undefined, tabs: TabMeta[] | null | undefined): string {
    const active = tabs?.find((tab) => tab.active);
    const tabID = (active?.id || "").trim();
    const root = (active?.workspaceRoot || active?.workspacePath || active?.cwd || meta?.workspaceRoot || meta?.workspacePath || meta?.cwd || "").trim();
    const channel = (meta?.eventChannel || "").trim();
    return `${channel}|${tabID}|${root}`;
}
export function normalizeCapabilitiesView(view: CapabilitiesView | null | undefined): CapabilitiesView {
    return {
        servers: normalizeServerViews(view?.servers),
        plugins: asArray(view?.plugins),
        ...normalizeSkillsSettingsView(view),
    };
}
export function normalizeServerViews(servers: ServerView[] | null | undefined): ServerView[] {
    return sortServersForDisplay(asArray(servers).map((server) => ({
        ...server,
        args: asArray(server.args),
        envKeys: asArray(server.envKeys),
        headerKeys: asArray(server.headerKeys),
        toolList: asArray(server.toolList),
    })));
}
export function normalizeSkillsSettingsView(view: SkillsSettingsView | CapabilitiesView | null | undefined): SkillsSettingsView {
    return {
        skills: asArray(view?.skills),
        skillRoots: asArray(view?.skillRoots).map((root) => ({
            ...root,
            enabled: root.enabled !== false,
            removable: Boolean(root.removable),
            skillItems: asArray(root.skillItems),
        })),
        allowImplicitInvocation: view?.allowImplicitInvocation !== false,
    };
}
export function sortServersForDisplay(servers: ServerView[]): ServerView[] {
    return [...servers].sort((a, b) => {
        const priority = serverDisplayPriority(a) - serverDisplayPriority(b);
        if (priority !== 0)
            return priority;
        return a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
    });
}
function serverDisplayPriority(server: ServerView): number {
    if (server.status === "failed" || server.authStatus === "required")
        return 0;
    if (server.builtIn)
        return 1;
    if (server.status !== "disabled")
        return 2;
    return 3;
}
export function skillListSummary(skills: SkillView[], filtered: SkillView[], searching: boolean, t: ReturnType<typeof useT>): string {
    if (searching) {
        return t("caps.skillsSummaryMatches", { matched: filtered.length, total: skills.length });
    }
    const parts = [t("caps.skillsSummaryAvailable", { skills: skills.length })];
    const scopes = ["project", "custom", "global", "builtin"];
    for (const scope of scopes) {
        const count = skills.filter((skill) => skill.scope === scope).length;
        if (count > 0)
            parts.push(skillScopeSummary(scope, count, t));
    }
    return parts.join(" · ");
}
export function mcpServerSummary(servers: ServerView[], t: ReturnType<typeof useT>): string {
    return t("caps.mcpSummary", {
        connected: servers.filter((s) => s.status === "connected").length,
        failed: servers.filter((s) => s.status === "failed").length,
        tools: servers.reduce((total, server) => total + (server.tools || 0), 0),
        unavailable: servers.reduce((total, server) => total + mcpServerSchemaIssueCount(server), 0),
    });
}
function skillScopeSummary(scope: string, count: number, t: ReturnType<typeof useT>): string {
    switch (scope) {
        case "builtin":
            return t("caps.skillsSummaryBuiltin", { count });
        case "project":
            return t("caps.skillsSummaryProject", { count });
        case "custom":
            return t("caps.skillsSummaryCustom", { count });
        case "global":
            return t("caps.skillsSummaryGlobal", { count });
        default:
            return `${count} ${scope}`;
    }
}
export function skillSourceSummary(active: number, missing: number, empty: number, disabled: number, t: ReturnType<typeof useT>): string {
    const parts: string[] = [];
    if (active > 0)
        parts.push(t("caps.sourcesSummaryActive", { active }));
    if (missing > 0)
        parts.push(t("caps.sourcesSummaryMissing", { missing }));
    if (empty > 0)
        parts.push(t("caps.sourcesSummaryEmpty", { empty }));
    if (disabled > 0)
        parts.push(t("caps.sourcesSummaryDisabled", { disabled }));
    return parts.length > 0 ? parts.join(" · ") : t("caps.sourcesSummaryNone");
}

