import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { app } from "../lib/bridge";
import { activeWorkBusyNoticeText, installMCPServer } from "../lib/capabilityMutations";
import { useT } from "../lib/i18n";
import type { CapabilitiesView, ServerView, SkillsSettingsView } from "../lib/types";
import { ResizableDrawer } from "./ResizableDrawer";
import { Tooltip } from "./Tooltip";
import { ModalCloseButton } from "./ModalCloseButton";
import { CapTab, connectMCPServer, skillsSettingsSnapshot, setSkillsSettingsSnapshot, settingsSnapshotKey, normalizeCapabilitiesView, normalizeSkillsSettingsView, sortServersForDisplay, skillListSummary } from "./capabilities_helpers";
import { SkillSources } from "./capabilities_skills";
import { summarizeServerError, ServerGroup, FailedServersNotice, serverStatusLabel, retryableAvailableServerNames } from "./capabilities_servers";
import { SkillRow } from "./capabilities_plugins";
import { mcpServerSchemaIssueCount, MCPServerSettingsEditor } from "./capabilities_mcp";
export type { CapTab as CapTab } from "./capabilities_helpers";
export { connectMCPServer as connectMCPServer } from "./capabilities_helpers";
export { mcpSettingsSnapshot as mcpSettingsSnapshot } from "./capabilities_helpers";
export { skillsSettingsSnapshot as skillsSettingsSnapshot } from "./capabilities_helpers";
export { pluginsSettingsSnapshot as pluginsSettingsSnapshot } from "./capabilities_helpers";
export { settingsSnapshotKey as settingsSnapshotKey } from "./capabilities_helpers";
export { normalizeCapabilitiesView as normalizeCapabilitiesView } from "./capabilities_helpers";
export { normalizeServerViews as normalizeServerViews } from "./capabilities_helpers";
export { normalizeSkillsSettingsView as normalizeSkillsSettingsView } from "./capabilities_helpers";
export { sortServersForDisplay as sortServersForDisplay } from "./capabilities_helpers";
export { skillListSummary as skillListSummary } from "./capabilities_helpers";
export { mcpServerSummary as mcpServerSummary } from "./capabilities_helpers";
export { skillSourceSummary as skillSourceSummary } from "./capabilities_helpers";
export { SkillSources as SkillSources } from "./capabilities_skills";
export { summarizeServerError as summarizeServerError } from "./capabilities_servers";
export type { FailureKind as FailureKind } from "./capabilities_servers";
export { failureKind as failureKind } from "./capabilities_servers";
export { ServerGroup as ServerGroup } from "./capabilities_servers";
export { FailedServersNotice as FailedServersNotice } from "./capabilities_servers";
export { ServerDetails as ServerDetails } from "./capabilities_servers";
export { serverCommand as serverCommand } from "./capabilities_servers";
export { normalizeTransportValue as normalizeTransportValue } from "./capabilities_servers";
export { parseKeyValueText as parseKeyValueText } from "./capabilities_servers";
export { serverStatusLabel as serverStatusLabel } from "./capabilities_servers";
export { retryableAvailableServerNames as retryableAvailableServerNames } from "./capabilities_servers";
export { serverActionLabel as serverActionLabel } from "./capabilities_servers";
export { shouldOpenAuth as shouldOpenAuth } from "./capabilities_servers";
export { skillScopeLabel as skillScopeLabel } from "./capabilities_plugins";
export { parseMCPQuickDefinition as parseMCPQuickDefinition } from "./capabilities_plugins";
export { PluginsSettingsPage as PluginsSettingsPage } from "./capabilities_plugins";
export { SkillRow as SkillRow } from "./capabilities_plugins";
export function CapabilitiesPanel({ onClose, initialTab = "servers", }: {
    onClose: () => void;
    initialTab?: CapTab;
}) {
    const t = useT();
    const [view, setView] = useState<CapabilitiesView | null>(null);
    const [busy, setBusy] = useState(false);
    const [err, setErr] = useState<string | null>(null);
    const [adding, setAdding] = useState(false);
    const [editing, setEditing] = useState<string | null>(null);
    const [tab, setTab] = useState<CapTab>(initialTab);
    const [skillQuery, setSkillQuery] = useState("");
    const [expandedSkills, setExpandedSkills] = useState<Set<string>>(() => new Set());
    const [expandedErrors, setExpandedErrors] = useState<Set<string>>(() => new Set());
    const [expandedServers, setExpandedServers] = useState<Set<string>>(() => new Set());
    const [expandedServerTools, setExpandedServerTools] = useState<Set<string>>(() => new Set());
    const reload = useCallback(async () => {
        setView(normalizeCapabilitiesView(await app.Capabilities().catch(() => ({ servers: [], skills: [], skillRoots: [], plugins: [] }))));
    }, []);
    useEffect(() => {
        void reload();
    }, [reload]);
    useEffect(() => {
        if (tab !== "servers" || !view?.servers.some((s) => s.status === "initializing" || s.status === "deferred"))
            return;
        const id = window.setInterval(() => void reload(), 2500);
        return () => window.clearInterval(id);
    }, [reload, tab, view?.servers]);
    // mutate runs an MCP edit, re-reads the snapshot, and surfaces any failure as an
    // inline banner (a connect error, a missing binary, a bad URL).
    const mutate = async (fn: () => Promise<unknown>) => {
        setBusy(true);
        setErr(null);
        try {
            await fn();
            await reload();
            return true;
        }
        catch (e) {
            setErr(activeWorkBusyNoticeText(e, t) ?? String((e as Error)?.message ?? e));
            await reload();
            return false;
        }
        finally {
            setBusy(false);
        }
    };
    const summary = useMemo(() => {
        if (!view)
            return "";
        return t("caps.summary", {
            connected: view.servers.filter((s) => s.status === "connected").length,
            failed: view.servers.filter((s) => s.status === "failed").length,
            skills: view.skills.length,
        });
    }, [view, t]);
    const filteredSkills = useMemo(() => {
        if (!view)
            return [];
        const q = skillQuery.trim().toLowerCase();
        if (!q)
            return view.skills;
        return view.skills.filter((sk) => {
            const text = [sk.name, `/${sk.name}`, sk.invocation, sk.plugin, sk.description, sk.scope, sk.sourceDir, sk.runAs].join(" ").toLowerCase();
            return text.includes(q);
        });
    }, [view, skillQuery]);
    const skillSummary = useMemo(() => {
        if (!view)
            return "";
        return skillListSummary(view.skills, filteredSkills, skillQuery.trim().length > 0, t);
    }, [filteredSkills, skillQuery, t, view]);
    const serverGroups = useMemo(() => {
        const servers = sortServersForDisplay(view?.servers ?? []);
        return {
            failed: servers.filter((s) => s.status === "failed"),
            active: servers.filter((s) => s.status !== "failed"),
        };
    }, [view]);
    const retryableActiveServerNames = useMemo(() => retryableAvailableServerNames(serverGroups.active), [serverGroups.active]);
    const toggleSkill = useCallback((name: string) => {
        setExpandedSkills((prev) => {
            const next = new Set(prev);
            if (next.has(name))
                next.delete(name);
            else
                next.add(name);
            return next;
        });
    }, []);
    const toggleError = useCallback((name: string) => {
        setExpandedErrors((prev) => {
            const next = new Set(prev);
            if (next.has(name))
                next.delete(name);
            else
                next.add(name);
            return next;
        });
    }, []);
    const toggleServer = useCallback((name: string) => {
        setExpandedServers((prev) => {
            const next = new Set(prev);
            if (next.has(name))
                next.delete(name);
            else
                next.add(name);
            return next;
        });
    }, []);
    const toggleServerTools = useCallback((name: string) => {
        setExpandedServerTools((prev) => {
            const next = new Set(prev);
            if (next.has(name))
                next.delete(name);
            else
                next.add(name);
            return next;
        });
    }, []);
    return (<ResizableDrawer onClose={onClose} subtle>
        <header className="drawer__head">
          <div>
            <div className="drawer__title">{t("caps.title")}</div>
            {view && <div className="drawer__summary">{summary}</div>}
          </div>
          <div className="drawer__actions">
            <Tooltip label={t("caps.refresh")}>
              <button className="chip" disabled={busy} onClick={() => void reload()}>
                ↻
              </button>
            </Tooltip>
            <ModalCloseButton label={t("common.close")} onClick={onClose}/>
          </div>
        </header>

        {!view ? (<div className="empty">{t("caps.loading")}</div>) : (<div className="drawer__body">
            {err && <div className="banner banner--error">{err}</div>}

            <div className="cap-tabs" role="tablist" aria-label={t("caps.title")}>
              <button className={`cap-tab${tab === "servers" ? " cap-tab--active" : ""}`} role="tab" aria-selected={tab === "servers"} onClick={() => setTab("servers")}>
                {t("caps.connectorsTab")}
              </button>
              <button className={`cap-tab${tab === "skills" ? " cap-tab--active" : ""}`} role="tab" aria-selected={tab === "skills"} onClick={() => setTab("skills")}>
                {t("caps.skillsTab")}
              </button>
            </div>

            {tab === "servers" ? (<section className="mem-section">
                <div className="cap-mcp-toolbar cap-mcp-toolbar--drawer">
                  {!adding && (<button className="btn btn--small" disabled={busy} onClick={() => setAdding(true)}>
                      {t("caps.addServer")}
                    </button>)}
                </div>
                {serverGroups.failed.length > 0 && (<FailedServersNotice servers={serverGroups.failed} expanded={expandedErrors} onToggle={toggleError} onRetry={(name) => void mutate(() => connectMCPServer(name, view.servers))} onRetryMany={(names) => void mutate(() => Promise.allSettled(names.map((name) => app.ReconnectMCPServer(name))))} onConfirmClearAuth={(name) => void mutate(() => app.ClearMCPServerAuthentication(name))} onConfirm={(name) => void mutate(() => app.RemoveMCPServer(name))} onConfirmMany={(names) => void mutate(() => Promise.allSettled(names.map((name) => app.RemoveMCPServer(name))))} busy={busy}/>)}
                {view.servers.length === 0 && !adding && (<div className="mem-empty">{t("caps.noServers")}</div>)}
                {serverGroups.active.length > 0 && (<div className="cap-server-section">
                    <div className="cap-server-section__head">
                      <div className="cap-server-section__title">{t("caps.availableServers")}</div>
                      <button className="btn btn--small" disabled={busy || retryableActiveServerNames.length === 0} type="button" onClick={() => void mutate(() => Promise.allSettled(retryableActiveServerNames.map((name) => app.ReconnectMCPServer(name))))}>
                        {t("caps.retryAll")}
                      </button>
                    </div>
                    <ServerGroup busy={busy} servers={serverGroups.active} expanded={expandedServers} expandedTools={expandedServerTools} editing={editing} onConfirm={(name) => void mutate(() => app.RemoveMCPServer(name))} onEdit={(name) => {
                        setEditing(name);
                    }} onCancelEdit={() => setEditing(null)} onRetry={(name) => void mutate(() => connectMCPServer(name, view.servers))} onReconnect={(name) => void mutate(() => app.ReconnectMCPServer(name))} onConfirmClearAuth={(name) => void mutate(() => app.ClearMCPServerAuthentication(name))} onToggle={(name, on) => void mutate(() => app.SetMCPServerEnabled(name, on))} onUpdate={(name, input) => void mutate(() => app.UpdateMCPServer(name, input)).then((ok) => {
                        if (ok)
                            setEditing(null);
                    })} onToggleDetails={toggleServer} onToggleTools={toggleServerTools}/>
                  </div>)}
                {adding ? (<MCPServerSettingsEditor busy={busy} onCancel={() => setAdding(false)} onSubmit={(input) => void mutate(() => installMCPServer(input)).then((ok) => {
                        if (ok)
                            setAdding(false);
                    })}/>) : null}
              </section>) : (<section className="mem-section">
                <div className="cap-search">
                  <input className="mem-input" type="search" placeholder={t("caps.searchSkills")} value={skillQuery} onChange={(e) => setSkillQuery(e.target.value)}/>
                </div>
                <SkillSources roots={view.skillRoots ?? []} busy={busy} onAdd={() => mutate(async () => {
                    const path = await app.PickSkillFolder();
                    if (path)
                        await app.AddSkillPath(path);
                })} onRefresh={() => mutate(() => app.RefreshSkills())} onToggle={(path, enabled) => mutate(() => app.SetSkillPathEnabled(path, enabled))}/>
                <div className="cap-skills-head">
                  <div className="cap-skills-head__copy">
                    <div className="cap-skills-head__title">{t("caps.skills")}</div>
                    <div className="cap-skills-head__summary">{skillSummary}</div>
                  </div>
                </div>
                {view.skills.length === 0 ? (<div className="mem-empty">{t("caps.noSkills")}</div>) : filteredSkills.length === 0 ? (<div className="mem-empty">{t("caps.noSkillMatches")}</div>) : (<div className="cap-skills">
                    {filteredSkills.map((sk) => (<SkillRow key={sk.name} skill={sk} busy={busy} expanded={expandedSkills.has(sk.name)} onToggle={() => toggleSkill(sk.name)} onToggleEnabled={(enabled) => void mutate(() => app.SetSkillEnabled(sk.name, enabled))}/>))}
                  </div>)}
              </section>)}
          </div>)}
    </ResizableDrawer>);
}
export function mcpSettingsServerSummary(server: ServerView, t: ReturnType<typeof useT>): string {
    if (server.status === "failed") {
        return server.authStatus === "required" ? t("caps.authRequiredSummary") : summarizeServerError(server.error || t("caps.failed"));
    }
    if (server.status !== "connected")
        return serverStatusLabel(server, t);
    const unavailable = mcpServerSchemaIssueCount(server);
    const parts = [serverStatusLabel(server, t), t("caps.serverToolSummary", { tools: server.tools || 0 })];
    if (unavailable > 0)
        parts.push(t("caps.schemaIssues", { count: unavailable }));
    return parts.join(" · ");
}
export function mcpServerSourceLabel(server: ServerView, t: ReturnType<typeof useT>): string {
    switch (server.source) {
        case "project":
            return server.configSource
                ? t("caps.sourceProjectConfig", { config: server.configSource })
                : t("caps.sourceProject");
        case "plugin":
            return t("caps.sourcePlugin");
        case "builtin":
            return t("caps.sourceBuiltin");
        default:
            return t("caps.sourceUser");
    }
}
// SkillsSettingsPage is a self-contained skills management page embedded inside
// the settings centre.
export function SkillsSettingsPage({ activeWorkspaceKey = "" }: {
    activeWorkspaceKey?: string;
}) {
    const t = useT();
    const [snapshotKey, setSnapshotKey] = useState("");
    const [view, setView] = useState<SkillsSettingsView | null>(null);
    const [busy, setBusy] = useState(false);
    const [err, setErr] = useState<string | null>(null);
    const [skillQuery, setSkillQuery] = useState("");
    const [expandedSkills, setExpandedSkills] = useState<Set<string>>(() => new Set());
    const reloadSequence = useRef(0);
    const reload = useCallback(async () => {
        const sequence = ++reloadSequence.current;
        const [meta, tabs] = await Promise.all([
            app.Meta().catch(() => null),
            app.ListTabs().catch(() => []),
        ]);
        if (sequence !== reloadSequence.current)
            return;
        const key = settingsSnapshotKey(meta, tabs);
        setSnapshotKey(key);
        const cached = key ? skillsSettingsSnapshot : null;
        if (cached?.key === key) {
            setView(cached.value);
        }
        else {
            setView(null);
        }
        const next = normalizeSkillsSettingsView(await app.SkillsSettings().catch(() => ({ skills: [], skillRoots: [] })));
        if (sequence !== reloadSequence.current)
            return;
        setSkillsSettingsSnapshot({ key, value: next });
        setView(next);
    }, [activeWorkspaceKey]);
    useEffect(() => {
        setView(null);
        void reload();
    }, [reload]);
    const mutate = async (fn: () => Promise<unknown>) => {
        setBusy(true);
        setErr(null);
        try {
            await fn();
            await reload();
            return true;
        }
        catch (e) {
            setErr(activeWorkBusyNoticeText(e, t) ?? String((e as Error)?.message ?? e));
            await reload();
            return false;
        }
        finally {
            setBusy(false);
        }
    };
    const filteredSkills = useMemo(() => {
        if (!view)
            return [];
        const q = skillQuery.trim().toLowerCase();
        if (!q)
            return view.skills;
        return view.skills.filter((sk) => {
            const text = [sk.name, "/" + sk.name, sk.invocation, sk.plugin, sk.description, sk.scope, sk.sourceDir, sk.runAs].join(" ").toLowerCase();
            return text.includes(q);
        });
    }, [view, skillQuery]);
    const skillSummary = useMemo(() => {
        if (!view)
            return "";
        return skillListSummary(view.skills, filteredSkills, skillQuery.trim().length > 0, t);
    }, [filteredSkills, skillQuery, t, view]);
    const toggleSkill = useCallback((name: string) => {
        setExpandedSkills((prev) => {
            const next = new Set(prev);
            if (next.has(name))
                next.delete(name);
            else
                next.add(name);
            return next;
        });
    }, []);
    if (!view)
        return <div className="empty">{t("caps.loading")}</div>;
    const actionBusy = busy || !snapshotKey;
    return (<section className="mem-section">
			{err && <div className="banner banner--error">{err}</div>}
			<div className="cap-search">
				<input className="mem-input" type="search" placeholder={t("caps.searchSkills")} value={skillQuery} onChange={(e) => setSkillQuery(e.target.value)}/>
			</div>
			<label className="provider-capability-row cap-skill-policy">
				<span className="provider-capability-row__copy">
					<span className="provider-capability-row__title">{t("caps.skillImplicitInvocation")}</span>
					<span className="cap-skill-policy__hint">{t("caps.skillImplicitInvocationHint")}</span>
				</span>
				<input className="provider-capability-row__switch" type="checkbox" role="switch" checked={view.allowImplicitInvocation} disabled={actionBusy} onChange={(e) => void mutate(() => app.SetSkillImplicitInvocation(e.target.checked))}/>
			</label>
			<SkillSources roots={view.skillRoots ?? []} busy={actionBusy} onAdd={() => mutate(async () => {
            const path = await app.PickSkillFolder();
            if (path)
                await app.AddSkillPath(path);
        })} onRefresh={() => mutate(() => app.RefreshSkills())} onToggle={(path, enabled) => mutate(() => app.SetSkillPathEnabled(path, enabled))}/>
			<div className="cap-skills-head">
				<div className="cap-skills-head__copy">
					<div className="cap-skills-head__title">{t("caps.skills")}</div>
					<div className="cap-skills-head__summary">{skillSummary}</div>
				</div>
			</div>
			{view.skills.length === 0 ? (<div className="mem-empty">{t("caps.noSkills")}</div>) : filteredSkills.length === 0 ? (<div className="mem-empty">{t("caps.noSkillMatches")}</div>) : (<div className="cap-skills">
					{filteredSkills.map((sk) => (<SkillRow key={sk.name} skill={sk} busy={actionBusy} expanded={expandedSkills.has(sk.name)} onToggle={() => toggleSkill(sk.name)} onToggleEnabled={(enabled) => void mutate(() => app.SetSkillEnabled(sk.name, enabled))}/>))}
				</div>)}
		</section>);
}
export type { MCPServerJSONError as MCPServerJSONError } from "./capabilities_mcp";
export { mcpServerSchemaIssueCount as mcpServerSchemaIssueCount } from "./capabilities_mcp";
export { mcpServerDraftJSON as mcpServerDraftJSON } from "./capabilities_mcp";
export { withExplicitMCPClears as withExplicitMCPClears } from "./capabilities_mcp";
export { parseMCPServerJSON as parseMCPServerJSON } from "./capabilities_mcp";
export { MCPServersSettingsPage as MCPServersSettingsPage } from "./capabilities_mcp";
export { MCPServerSettingsEditor as MCPServerSettingsEditor } from "./capabilities_mcp";

