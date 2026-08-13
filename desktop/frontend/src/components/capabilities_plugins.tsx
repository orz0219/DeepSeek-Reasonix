import { useCallback, useEffect, useState } from "react";
import { Folder } from "lucide-react";
import { asArray } from "../lib/array";
import { app } from "../lib/bridge";
import { activeWorkBusyNoticeText } from "../lib/capabilityMutations";
import { useT } from "../lib/i18n";
import type { MCPServerInput, PluginAgentView, PluginCommandView, PluginCompatibilityIssue, PluginHookView, PluginInstallOptions, PluginMCPServerView, PluginSkillView, PluginView, SkillView } from "../lib/types";
import { InlineConfirmButton } from "./InlineConfirmButton";
import { Tooltip } from "./Tooltip";
import { pluginsSettingsSnapshot, setPluginsSettingsSnapshot, settingsSnapshotKey } from "./capabilities_helpers";
import { parseMCPServerJSON, MCPServerJSONError } from "./CapabilitiesPanel";
import { normalizePluginViews, normalizePluginView, pluginListSummary, pluginCapabilitiesSummary, pluginCompatibilityLabel, pluginWarnings, parsePluginInstallPlan, pluginPlanActionLabel, pluginPlanNotice } from "./capabilities_plugin_helpers";
export function SkillRow({ skill, busy, expanded, onToggle, onToggleEnabled, }: {
    skill: SkillView;
    busy: boolean;
    expanded: boolean;
    onToggle: () => void;
    onToggleEnabled: (enabled: boolean) => void;
}) {
    const t = useT();
    const summary = summarizeSkillDescription(skill.description);
    const canExpand = summary !== skill.description;
    return (<div className={`cap-skill-card${expanded ? " cap-skill-card--expanded" : ""}${canExpand ? " cap-skill-card--expandable" : ""}${!skill.enabled ? " cap-skill-card--disabled" : ""}`}>
      <div className="cap-skill-card__top">
        <button className="cap-skill-card__toggle" type="button" onClick={onToggle} aria-expanded={expanded}>
          <span className="cap-skill-card__head">
            <span className="cap-skill-card__icon">/</span>
            <span className="cap-skill-card__main">
              <span className="cap-skill-card__identity">
                <span className="cap-skill-card__command">{(skill.invocation || `/${skill.name}`).replace(/^\//, "")}</span>
                {skill.sourceDir && (<span className="cap-skill-card__source" title={skill.sourceDir}>
                    <Folder aria-hidden size={11}/>
                    <span>{skill.sourceDir}</span>
                  </span>)}
              </span>
              <span className="cap-skill-card__badges">
                <span className={`cap-skill-badge cap-skill-badge--${skill.scope}`}>{skillScopeLabel(skill.scope, t)}</span>
                {skill.plugin && <span className="cap-skill-badge">{t("slash.plugin", { name: skill.plugin })}</span>}
                {skill.runAs === "subagent" && <span className="cap-skill-badge cap-skill-badge--run">{t("caps.subagent")}</span>}
                {!skill.enabled && <span className="cap-skill-badge cap-skill-badge--off">{t("caps.skillDisabled")}</span>}
              </span>
            </span>
          </span>
        </button>
        <Tooltip label={skill.enabled ? t("caps.disableSkill") : t("caps.enableSkill")}>
          <label className="cap-switch">
            <input type="checkbox" checked={skill.enabled} disabled={busy} onChange={(e) => onToggleEnabled(e.target.checked)}/>
            <span className="cap-switch__track"/>
          </label>
        </Tooltip>
      </div>
      <div className="cap-skill-card__desc">{expanded ? skill.description : summary}</div>
      {canExpand && (<button className="cap-skill-card__more" type="button" onClick={onToggle} aria-expanded={expanded}>
          {expanded ? t("common.collapse") : t("common.expand")}
        </button>)}
    </div>);
}
export function skillScopeLabel(scope: string, t: ReturnType<typeof useT>): string {
    switch (scope) {
        case "builtin":
            return t("caps.skillScopeBuiltin");
        case "project":
            return t("caps.skillScopeProject");
        case "custom":
            return t("caps.skillScopeCustom");
        case "global":
            return t("caps.skillScopeGlobal");
        default:
            return scope;
    }
}
function summarizeSkillDescription(description: string): string {
    const normalized = description.replace(/\s+/g, " ").trim();
    if (normalized.length <= 132)
        return normalized;
    const sentence = normalized.match(/^.{48,132}?[。.!?；;，,]/u)?.[0]?.trim();
    if (sentence && sentence.length >= 48)
        return sentence.replace(/[。.!?；;，,]$/u, "");
    return `${normalized.slice(0, 128).trim()}…`;
}
function tokenizeMCPCommand(raw: string): string[] {
    const tokens: string[] = [];
    let token = "";
    let quote = "";
    for (let i = 0; i < raw.length; i += 1) {
        const ch = raw[i];
        if (quote) {
            if (ch === quote) {
                quote = "";
                continue;
            }
            if (ch === "\\" && quote === '"' && i + 1 < raw.length && /["\\]/.test(raw[i + 1])) {
                token += raw[i + 1];
                i += 1;
                continue;
            }
            token += ch;
            continue;
        }
        if (ch === '"' || ch === "'") {
            quote = ch;
            continue;
        }
        if (ch === "\\" && i + 1 < raw.length && /\s/.test(raw[i + 1])) {
            token += raw[i + 1];
            i += 1;
            continue;
        }
        if (/\s/.test(ch)) {
            if (token)
                tokens.push(token);
            token = "";
            continue;
        }
        token += ch;
    }
    if (token)
        tokens.push(token);
    return tokens;
}
function firstMCPCommandOperand(args: string[]): string {
    const valueFlags = new Set(["-p", "--package", "-c", "--call", "--node-options", "--python"]);
    let options = true;
    for (let i = 0; i < args.length; i += 1) {
        const arg = args[i];
        if (options && arg === "--") {
            options = false;
            continue;
        }
        if (options && arg.startsWith("-")) {
            if (valueFlags.has(arg))
                i += 1;
            continue;
        }
        return arg;
    }
    return "";
}
function quickMCPName(raw: string): string {
    const argv = tokenizeMCPCommand(raw);
    const executable = argv[0]?.split(/[\\/]/).pop()?.toLowerCase().replace(/\.(?:cmd|exe|bat)$/i, "") || "";
    let candidate = argv[0] || "mcp-server";
    if (["npx", "bunx", "uvx"].includes(executable)) {
        candidate = firstMCPCommandOperand(argv.slice(1)) || candidate;
    }
    else if (["python", "python3", "py"].includes(executable)) {
        const moduleIndex = argv.findIndex((arg) => arg === "-m");
        candidate = moduleIndex >= 0 ? argv[moduleIndex + 1] || candidate : firstMCPCommandOperand(argv.slice(1)) || candidate;
    }
    else if (executable === "node") {
        candidate = firstMCPCommandOperand(argv.slice(1)) || candidate;
    }
    else if (executable === "uv" && argv[1] === "run") {
        candidate = firstMCPCommandOperand(argv.slice(2)) || candidate;
    }
    const base = candidate.split(/[\\/]/).pop() || candidate;
    const unversioned = base.replace(/@[^@]+$/, "").replace(/\.(?:cmd|exe|bat)$/i, "");
    const sanitized = unversioned.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
    return sanitized && /[a-z0-9]/.test(sanitized) && !["npx", "uvx", "uv", "node", "bunx", "python", "python3", "py"].includes(sanitized)
        ? sanitized
        : "mcp-server";
}
export function parseMCPQuickDefinition(raw: string): MCPServerInput {
    const definition = raw.trim();
    if (definition.startsWith("{"))
        return parseMCPServerJSON(definition).input;
    if (/^https?:\/\//i.test(definition)) {
        let name = "remote-mcp";
        try {
            name = new URL(definition).hostname.replace(/^www\./, "").split(".")[0] || name;
        }
        catch {
            throw new Error("invalid" satisfies MCPServerJSONError);
        }
        return { name, transport: "http", command: "", args: [], url: definition, env: null, headers: null };
    }
    return { name: quickMCPName(definition), transport: "stdio", command: definition, args: [], url: "", env: null, headers: null };
}
export type PluginRuntimePlan = {
    command?: string;
    args?: string[];
    intercepts?: string[];
    replaces?: string[];
    capabilities?: string[];
    fullTrust?: boolean;
};
export type PluginInstallPlanAction = {
    action?: string;
    kind?: string;
    name?: string;
    source?: string;
    status?: string;
    message?: string;
    error?: string;
    compatibility?: string;
    mappedCapabilities?: string[];
    skippedCapabilities?: PluginCompatibilityIssue[];
    runtime?: PluginRuntimePlan;
    agentCount?: number;
    skillCount?: number;
    commandCount?: number;
    hookCount?: number;
    toolCount?: number;
};
export type PluginInstallPlanView = {
    raw: string;
    ok?: boolean;
    status?: string;
    name?: string;
    actions: PluginInstallPlanAction[];
    warnings: string[];
    error?: string;
};
type PluginInstallMode = "local" | "git";
// PluginsSettingsPage is the desktop plugin package manager embedded inside
// Settings. It mirrors the MCP/Skills density: install planning on top, package
// rows below, and diagnostics/details only when a row is expanded.
export function PluginsSettingsPage() {
    const t = useT();
    const [snapshotKey, setSnapshotKey] = useState("");
    const [plugins, setPlugins] = useState<PluginView[] | null>(null);
    const [busy, setBusy] = useState(false);
    const [err, setErr] = useState<string | null>(null);
    const [installMode, setInstallMode] = useState<PluginInstallMode>("local");
    const [localSource, setLocalSource] = useState("");
    const [gitSource, setGitSource] = useState("");
    const [name, setName] = useState("");
    const [link, setLink] = useState(false);
    const [replace, setReplace] = useState(false);
    const [plan, setPlan] = useState<PluginInstallPlanView | null>(null);
    const [notice, setNotice] = useState<string | null>(null);
    const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
    const [diagnostics, setDiagnostics] = useState<Record<string, PluginView>>({});
    const reload = useCallback(async () => {
        const [meta, tabs] = await Promise.all([
            app.Meta().catch(() => null),
            app.ListTabs().catch(() => []),
        ]);
        const key = settingsSnapshotKey(meta, tabs);
        setSnapshotKey(key);
        const cached = key ? pluginsSettingsSnapshot : null;
        if (cached?.key === key) {
            setPlugins(cached.value);
        }
        else {
            setPlugins(null);
        }
        const next = normalizePluginViews(await app.Plugins().catch(() => []));
        setPluginsSettingsSnapshot({ key, value: next });
        setPlugins(next);
    }, []);
    useEffect(() => { void reload(); }, [reload]);
    const run = async (fn: () => Promise<unknown>, reloadAfter = true) => {
        setBusy(true);
        setErr(null);
        setNotice(null);
        try {
            const result = await fn();
            if (typeof result === "string" && result.trim()) {
                const parsed = parsePluginInstallPlan(result);
                setNotice(pluginPlanNotice(parsed, t));
            }
            if (reloadAfter)
                await reload();
            return true;
        }
        catch (e) {
            setErr(activeWorkBusyNoticeText(e, t) ?? String((e as Error)?.message ?? e));
            if (reloadAfter)
                await reload();
            return false;
        }
        finally {
            setBusy(false);
        }
    };
    const sourceValue = (installMode === "local" ? localSource : gitSource).trim();
    const installOptions = (): PluginInstallOptions => ({
        dryRun: false,
        link: installMode === "local" ? link : false,
        replace,
        name: installMode === "git" ? name.trim() || undefined : undefined,
    });
    const actionBusy = busy || !snapshotKey || !plugins;
    const canPlan = sourceValue.length > 0 && !actionBusy;
    const summary = plugins ? pluginListSummary(plugins, t) : "";
    const togglePlugin = useCallback((pluginName: string) => {
        setExpanded((prev) => {
            const next = new Set(prev);
            if (next.has(pluginName))
                next.delete(pluginName);
            else
                next.add(pluginName);
            return next;
        });
    }, []);
    const setMode = (mode: PluginInstallMode) => {
        setInstallMode(mode);
        setPlan(null);
    };
    const previewInstall = () => {
        if (!sourceValue)
            return;
        void run(async () => {
            const raw = await app.PlanPluginInstall(sourceValue, { ...installOptions(), dryRun: true });
            setPlan(parsePluginInstallPlan(raw));
        }, false);
    };
    const install = () => {
        if (!sourceValue)
            return;
        void run(async () => {
            const raw = await app.InstallPlugin(sourceValue, installOptions());
            setPlan(parsePluginInstallPlan(raw));
            return raw;
        });
    };
    const runDoctor = (pluginName: string) => {
        void run(async () => {
            const view = normalizePluginView(await app.PluginDoctor(pluginName));
            setDiagnostics((prev) => ({ ...prev, [pluginName]: view }));
            setExpanded((prev) => {
                const next = new Set(prev);
                next.add(pluginName);
                return next;
            });
        }, false);
    };
    const updateLocalSource = (value: string) => {
        setLocalSource(value);
        setPlan(null);
    };
    const updateGitSource = (value: string) => {
        setGitSource(value);
        setPlan(null);
    };
    const pickPluginFolder = () => {
        void run(async () => {
            const path = await app.PickPluginFolder();
            if (path) {
                setInstallMode("local");
                updateLocalSource(path);
            }
        }, false);
    };
    return (<section className="mem-section">
			{err && <div className="banner banner--error">{err}</div>}
			{notice && !err && <div className="banner banner--success">{notice}</div>}
			<div className="cap-plugin-installer">
				<div className="cap-plugin-installer__head">
					<div className="cap-plugin-installer__copy">
						<div className="cap-plugin-installer__title">{t("caps.pluginInstallTitle")}</div>
						<div className="cap-plugin-installer__hint">{t("caps.pluginInstallHint")}</div>
					</div>
					<div className="cap-tabs cap-plugin-installer__mode" role="group" aria-label={t("caps.pluginInstallMethod")}>
						<button className={`cap-tab${installMode === "local" ? " cap-tab--active" : ""}`} type="button" aria-pressed={installMode === "local"} onClick={() => setMode("local")}>
							{t("caps.pluginInstallLocal")}
						</button>
						<button className={`cap-tab${installMode === "git" ? " cap-tab--active" : ""}`} type="button" aria-pressed={installMode === "git"} onClick={() => setMode("git")}>
							{t("caps.pluginInstallGit")}
						</button>
					</div>
				</div>
				<div className="cap-plugin-form-grid">
					{installMode === "local" ? (<div className="cap-plugin-fields cap-plugin-fields--local">
							<div className="cap-plugin-folder-field">
								<button className="btn btn--small" disabled={actionBusy} type="button" onClick={pickPluginFolder}>
									{t("caps.pluginChooseLocalFolder")}
								</button>
								<div className={`cap-plugin-path${localSource ? "" : " cap-plugin-path--empty"}`} aria-label={t("caps.pluginLocalFolder")}>
									{localSource || t("caps.pluginNoLocalFolder")}
								</div>
							</div>
						</div>) : (<div className="cap-plugin-fields cap-plugin-fields--git">
							<input className="mem-input" aria-label={t("caps.pluginGitSource")} placeholder={t("caps.pluginSourcePlaceholder")} value={gitSource} onInput={(e) => updateGitSource(e.currentTarget.value)} onChange={(e) => updateGitSource(e.target.value)}/>
							<div className="cap-plugin-field">
								<input className="mem-input" aria-label={t("caps.pluginInstallName")} placeholder={t("caps.pluginInstallNamePlaceholder")} value={name} onChange={(e) => setName(e.target.value)}/>
							</div>
						</div>)}
					<div className="cap-plugin-installer__options">
						<div className="cap-plugin-option-block">
							<label className="cap-plugin-option">
								<input type="checkbox" checked={replace} disabled={actionBusy} onChange={(e) => setReplace(e.target.checked)}/>
								<span>{t("caps.pluginReplace")}</span>
							</label>
							<div className="cap-plugin-option-hint">{t("caps.pluginReplaceHint")}</div>
						</div>
						{installMode === "local" && (<div className="cap-plugin-option-block">
								<label className="cap-plugin-option">
									<input type="checkbox" checked={link} disabled={actionBusy} onChange={(e) => setLink(e.target.checked)}/>
									<span>{t("caps.pluginLink")}</span>
								</label>
								<div className="cap-plugin-option-hint">{t("caps.pluginLinkHint")}</div>
							</div>)}
					</div>
					<div className="cap-plugin-installer__actions">
						<button className="btn btn--small" type="button" disabled={!canPlan} onClick={previewInstall}>
							{t("caps.pluginPreview")}
						</button>
						<button className="btn btn--primary btn--small" type="button" disabled={!canPlan} onClick={install}>
							{t("caps.pluginInstall")}
						</button>
					</div>
				</div>
			</div>
			{plan && <PluginPlanPreview plan={plan}/>}
			<div className="cap-server-section cap-plugin-section">
				<div className="cap-server-section__head">
					<div className="cap-server-section__copy">
						<div className="cap-server-section__title">{t("caps.installedPlugins")}</div>
						{plugins && plugins.length > 0 && <div className="drawer__summary">{summary}</div>}
					</div>
					<button className="btn btn--small" disabled={actionBusy} type="button" onClick={() => void reload()}>
						{t("caps.pluginRefresh")}
					</button>
				</div>
				{!plugins ? (<div className="mem-empty">{t("caps.loading")}</div>) : plugins.length === 0 ? (<div className="mem-empty mem-empty--cta">
						<strong>{t("caps.noPluginsTitle")}</strong>
						<span>{t("caps.noPluginsHint")}</span>
					</div>) : (<div className="cap-server-group">
						{plugins.map((plugin) => (<PluginRow key={plugin.name} plugin={plugin} diagnostic={diagnostics[plugin.name]} busy={actionBusy} expanded={expanded.has(plugin.name)} onToggleDetails={() => togglePlugin(plugin.name)} onToggleEnabled={(enabled) => void run(() => app.SetPluginEnabled(plugin.name, enabled))} onUpdate={() => void run(() => app.UpdatePlugin(plugin.name))} onDoctor={() => runDoctor(plugin.name)} onRemove={() => void run(() => app.RemovePlugin(plugin.name))}/>))}
					</div>)}
			</div>
		</section>);
}
function PluginPlanPreview({ plan }: {
    plan: PluginInstallPlanView;
}) {
    const t = useT();
    return (<div className={`cap-plugin-plan${plan.error ? " cap-plugin-plan--error" : ""}`}>
			<div className="cap-plugin-plan__head">
				<div className="cap-plugin-plan__title">{plan.error ? t("caps.pluginPlanError") : t("caps.pluginPlanReady")}</div>
				{plan.status && <span className="cap-source-badge">{plan.status}</span>}
			</div>
			{plan.name && <div className="cap-plugin-plan__meta">{plan.name}</div>}
			{plan.error && <div className="cap-plugin-plan__warning">{plan.error}</div>}
			{plan.warnings.map((warning, idx) => (<div className="cap-plugin-plan__warning" key={`${warning}-${idx}`}>{warning}</div>))}
			{plan.actions.length > 0 ? (<div className="cap-plugin-actions">
					{plan.actions.map((action, idx) => (<div className="cap-plugin-action" key={`${action.action || action.kind || "action"}-${idx}`}>
							<span className="cap-plugin-action__name">{pluginPlanActionLabel(action, t)}</span>
							{action.status && <span className="cap-source-badge">{action.status}</span>}
							{action.compatibility && <span className="cap-source-badge">{pluginCompatibilityLabel(action.compatibility, t)}</span>}
							{action.source && <span className="cap-plugin-action__source">{action.source}</span>}
							{asArray(action.mappedCapabilities).length > 0 && <span className="cap-plugin-action__source">{t("caps.pluginMappedCapabilities", { capabilities: asArray(action.mappedCapabilities).join(", ") })}</span>}
							{asArray(action.skippedCapabilities).map((issue, issueIndex) => <span className="cap-plugin-plan__warning" key={`${issue.capability}-${issue.path || ""}-${issueIndex}`}>{issue.capability}: {issue.reason}</span>)}
							{action.message && <span className="cap-plugin-action__source">{action.message}</span>}
							{action.error && <span className="cap-plugin-plan__warning">{action.error}</span>}
							{action.runtime ? <PluginRuntimeTrustBlock runtime={action.runtime}/> : null}
						</div>))}
				</div>) : (<pre className="cap-plugin-plan__raw">{plan.raw}</pre>)}
		</div>);
}
// PluginRuntimeTrustBlock renders the prominent FULL TRUST warning for a
// plugin that declares a runtime process. Install/update/replace/--link
// already imply full trust, so this is disclosure, not a second confirmation.
function PluginRuntimeTrustBlock({ runtime }: {
    runtime: PluginRuntimePlan;
}) {
    const t = useT();
    const commandLine = [runtime.command, ...asArray(runtime.args)].filter(Boolean).join(" ");
    const groups: {
        label: string;
        values: string[];
    }[] = [
        { label: t("caps.pluginRuntimeIntercepts"), values: asArray(runtime.intercepts) },
        { label: t("caps.pluginRuntimeReplaces"), values: asArray(runtime.replaces) },
        { label: t("caps.pluginRuntimeCapabilities"), values: asArray(runtime.capabilities) },
    ];
    return (<div className="cap-plugin-runtime" role="alert">
			<div className="cap-plugin-runtime__title">{t("caps.pluginRuntimeFullTrust")}</div>
			{commandLine ? (<div className="cap-plugin-runtime__row">
					<span className="cap-plugin-runtime__label">{t("caps.pluginRuntimeCommand")}</span>
					<code className="cap-plugin-runtime__cmd">{commandLine}</code>
				</div>) : null}
			{groups
            .filter((group) => group.values.length > 0)
            .map((group) => (<div className="cap-plugin-runtime__row" key={group.label}>
						<span className="cap-plugin-runtime__label">{group.label}</span>
						<span>{group.values.join(", ")}</span>
					</div>))}
			<div className="cap-plugin-runtime__risk">{t("caps.pluginRuntimeRisk")}</div>
		</div>);
}
function PluginRow({ plugin, diagnostic, busy, expanded, onToggleDetails, onToggleEnabled, onUpdate, onDoctor, onRemove, }: {
    plugin: PluginView;
    diagnostic?: PluginView;
    busy: boolean;
    expanded: boolean;
    onToggleDetails: () => void;
    onToggleEnabled: (enabled: boolean) => void;
    onUpdate: () => void;
    onDoctor: () => void;
    onRemove: () => void;
}) {
    const t = useT();
    const status = plugin.error ? "failed" : plugin.enabled ? "connected" : "disabled";
    const warnings = pluginWarnings(plugin, diagnostic);
    const sub = plugin.error || pluginCapabilitiesSummary(plugin, t);
    return (<div className={`cap-server-entry cap-plugin-entry${plugin.enabled ? "" : " cap-server-entry--disabled"}`}>
			<Tooltip label={plugin.error} disabled={!plugin.error} fill block>
				<div className={`cap-row${plugin.enabled ? "" : " cap-row--disabled"}`}>
					<Tooltip label={expanded ? t("caps.collapseDetails") : t("caps.expandDetails")}>
						<button className="cap-disclosure" aria-expanded={expanded} type="button" onClick={onToggleDetails}>
							{expanded ? "⌄" : "›"}
						</button>
					</Tooltip>
					<span className={`cap-dot cap-dot--${status}`}/>
					<div className="cap-row__text">
						<div className="cap-row__head">
							<span className="cap-row__name">{plugin.name}</span>
							{plugin.manifestKind && <span className="cap-row__transport">{plugin.manifestKind}</span>}
							{plugin.compatibility && <span className="cap-source-badge">{pluginCompatibilityLabel(plugin.compatibility, t)}</span>}
							{plugin.version && <span className="cap-source-badge">{plugin.version}</span>}
							{warnings.length > 0 && <span className="cap-row__update cap-row__update--error">{t("caps.pluginWarnings", { count: warnings.length })}</span>}
						</div>
						<div className="cap-row__sub">{sub}</div>
					</div>
					<div className="cap-row__actions">
						<Tooltip label={plugin.enabled ? t("caps.pluginDisable") : t("caps.pluginEnable")}>
							<label className="cap-switch">
								<input type="checkbox" checked={plugin.enabled} disabled={busy} onChange={(e) => onToggleEnabled(e.target.checked)}/>
								<span className="cap-switch__track"/>
							</label>
						</Tooltip>
					</div>
				</div>
			</Tooltip>
			{expanded && (<div className="cap-server-details">
					<div className="cap-detail-grid">
						<div className="cap-detail">
							<span className="cap-detail__label">{t("caps.status")}</span>
							<span className="cap-detail__value">{plugin.enabled ? t("caps.pluginEnabled") : t("caps.pluginDisabled")}</span>
						</div>
						{plugin.version && (<div className="cap-detail">
								<span className="cap-detail__label">{t("caps.pluginVersion")}</span>
								<span className="cap-detail__value">{plugin.version}</span>
							</div>)}
						{plugin.source && (<div className="cap-detail cap-detail--wide">
								<span className="cap-detail__label">{t("caps.pluginSource")}</span>
								<span className="cap-detail__code">{plugin.source}</span>
							</div>)}
						{plugin.root && (<div className="cap-detail cap-detail--wide">
								<span className="cap-detail__label">{t("caps.pluginRoot")}</span>
								<span className="cap-detail__code">{plugin.root}</span>
							</div>)}
					</div>
					{plugin.description && <div className="cap-plugin-description">{plugin.description}</div>}
					{asArray(plugin.mappedCapabilities).length > 0 && <div className="cap-plugin-description">{t("caps.pluginMappedCapabilities", { capabilities: asArray(plugin.mappedCapabilities).join(", ") })}</div>}
					<PluginUsageDetails plugin={plugin}/>
					{asArray(plugin.skippedCapabilities).map((issue, idx) => (<div className="cap-source__warning" key={`${issue.capability}-${issue.path || ""}-${idx}`}>{t("caps.pluginSkippedCapability", { capability: issue.capability, reason: issue.reason })}</div>))}
					{diagnostic?.error && <div className="cap-source__warning">{diagnostic.error}</div>}
					{warnings.map((warning, idx) => (<div className="cap-source__warning" key={`${plugin.name}-warning-${idx}`}>{warning}</div>))}
					<div className="cap-detail-actions">
						<button className="btn btn--small" disabled={busy} type="button" onClick={onUpdate}>
							{t("caps.pluginUpdate")}
						</button>
						<button className="btn btn--small" disabled={busy} type="button" onClick={onDoctor}>
							{t("caps.pluginDoctor")}
						</button>
						<InlineConfirmButton label={t("caps.pluginRemove")} confirmLabel={t("caps.pluginConfirmRemove")} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={onRemove}/>
					</div>
				</div>)}
		</div>);
}
function PluginUsageDetails({ plugin }: {
    plugin: PluginView;
}) {
    const t = useT();
    const skills = asArray(plugin.skillDetails);
    const agents = asArray(plugin.agentDetails);
    const commands = asArray(plugin.commandDetails);
    const hooks = asArray(plugin.hookDetails);
    const mcps = asArray(plugin.mcpServerDetails);
    const hasDetails = skills.length > 0 || agents.length > 0 || commands.length > 0 || hooks.length > 0 || mcps.length > 0;
    return (<div className="cap-plugin-usage">
			<div className="cap-plugin-usage__title">{t("caps.pluginUsageTitle")}</div>
			<div className="cap-plugin-usage__hint">
				{plugin.enabled ? t("caps.pluginUsageEnabledHint") : t("caps.pluginUsageDisabledHint")}
			</div>
			{hasDetails ? (<div className="cap-plugin-capabilities">
					{commands.length > 0 && <PluginCommandList commands={commands}/>}
					{skills.length > 0 && <PluginSkillList skills={skills}/>}
					{agents.length > 0 && <PluginAgentList agents={agents}/>}
					{hooks.length > 0 && <PluginHookList hooks={hooks}/>}
					{mcps.length > 0 && <PluginMCPList servers={mcps}/>}
				</div>) : (<div className="cap-plugin-usage__empty">{t("caps.pluginNoCapabilityDetails")}</div>)}
		</div>);
}
function PluginAgentList({ agents }: {
    agents: PluginAgentView[];
}) {
    const t = useT();
    return (<div className="cap-plugin-capability">
			<div className="cap-plugin-capability__head">{t("caps.pluginAgentsTitle")}</div>
			<div className="cap-plugin-capability__hint">{t("caps.pluginAgentsHint")}</div>
			<div className="cap-plugin-capability__list">
				{agents.map((agent) => (<div className="cap-plugin-capability__item" key={`${agent.name}-${agent.path || ""}`}>
						<div className="cap-plugin-capability__line">
							<span className="cap-plugin-capability__name">{agent.invocation || agent.name}</span>
							{agent.model && <span className="cap-source-badge">{agent.model}</span>}
						</div>
						<div className="cap-plugin-capability__desc">{agent.description || t("caps.pluginNoDescription")}</div>
					</div>))}
			</div>
		</div>);
}
function PluginCommandList({ commands }: {
    commands: PluginCommandView[];
}) {
    const t = useT();
    return (<div className="cap-plugin-capability">
			<div className="cap-plugin-capability__head">{t("caps.pluginCommandsTitle")}</div>
			<div className="cap-plugin-capability__hint">{t("caps.pluginCommandsHint")}</div>
			<div className="cap-plugin-capability__list">
				{commands.map((command) => (<div className="cap-plugin-capability__item" key={`${command.name}-${command.path || command.invocation || ""}`}>
						<div className="cap-plugin-capability__line">
							<span className="cap-plugin-capability__name">{command.invocation || `/${command.name}`}</span>
							{command.argHint && <span className="cap-source-badge">{command.argHint}</span>}
							{command.shadowed && <span className="cap-source-badge">{t("caps.pluginCommandShadowed")}</span>}
						</div>
						<div className="cap-plugin-capability__desc">{command.description || t("caps.pluginNoDescription")}</div>
						{command.shadowed && (<div className="cap-plugin-capability__hint">
								{command.shadowedByPlugin
                    ? t("caps.pluginCommandQualifiedOccupiedByPlugin", { plugin: command.shadowedByPlugin })
                    : t("caps.pluginCommandQualifiedOccupiedByCustom")}
							</div>)}
					</div>))}
			</div>
		</div>);
}
function PluginSkillList({ skills }: {
    skills: PluginSkillView[];
}) {
    const t = useT();
    return (<div className="cap-plugin-capability">
			<div className="cap-plugin-capability__head">{t("caps.pluginSkillsTitle")}</div>
			<div className="cap-plugin-capability__hint">{t("caps.pluginSkillsHint")}</div>
			<div className="cap-plugin-capability__list">
				{skills.map((skill) => (<div className="cap-plugin-capability__item" key={`${skill.name}-${skill.path || skill.invocation || ""}`}>
						<div className="cap-plugin-capability__line">
							<span className="cap-plugin-capability__name">{skill.invocation || `/${skill.name}`}</span>
							{skill.runAs && <span className="cap-source-badge">{skill.runAs}</span>}
						</div>
						<div className="cap-plugin-capability__desc">{skill.description || t("caps.pluginNoDescription")}</div>
					</div>))}
			</div>
		</div>);
}
function PluginHookList({ hooks }: {
    hooks: PluginHookView[];
}) {
    const t = useT();
    return (<div className="cap-plugin-capability">
			<div className="cap-plugin-capability__head">{t("caps.pluginHooksTitle")}</div>
			<div className="cap-plugin-capability__hint">{t("caps.pluginHooksHint")}</div>
			<div className="cap-plugin-capability__list">
				{hooks.map((hook, idx) => {
            const target = hook.command || hook.contextFile || t("caps.pluginHookNoTarget");
            return (<div className="cap-plugin-capability__item" key={`${hook.event}-${hook.match || "*"}-${target}-${idx}`}>
							<div className="cap-plugin-capability__line">
								<span className="cap-plugin-capability__name">{hook.event}</span>
								<span className="cap-source-badge">{hook.match || "*"}</span>
							</div>
							<div className="cap-plugin-capability__desc">{hook.description || target}</div>
						</div>);
        })}
			</div>
		</div>);
}
function PluginMCPList({ servers }: {
    servers: PluginMCPServerView[];
}) {
    const t = useT();
    return (<div className="cap-plugin-capability">
			<div className="cap-plugin-capability__head">{t("caps.pluginMCPTitle")}</div>
			<div className="cap-plugin-capability__hint">{t("caps.pluginMCPHint")}</div>
			<div className="cap-plugin-capability__list">
				{servers.map((server) => (<div className="cap-plugin-capability__item" key={server.name}>
						<div className="cap-plugin-capability__line">
							<span className="cap-plugin-capability__name">{server.displayName || server.name}</span>
							{server.transport && <span className="cap-source-badge">{server.transport}</span>}
							<span className="cap-source-badge">{server.autoStart ? t("caps.pluginMCPAutoStart") : t("caps.pluginMCPOnDemand")}</span>
						</div>
						<div className="cap-plugin-capability__desc">{server.description || server.command || server.url || t("caps.pluginMCPNoTarget")}</div>
					</div>))}
			</div>
		</div>);
}
export { normalizePluginViews as normalizePluginViews } from "./capabilities_plugin_helpers";
export { normalizePluginView as normalizePluginView } from "./capabilities_plugin_helpers";
export { pluginListSummary as pluginListSummary } from "./capabilities_plugin_helpers";
export { pluginCapabilitiesSummary as pluginCapabilitiesSummary } from "./capabilities_plugin_helpers";
export { pluginCompatibilityLabel as pluginCompatibilityLabel } from "./capabilities_plugin_helpers";
export { pluginWarnings as pluginWarnings } from "./capabilities_plugin_helpers";
export { parsePluginInstallPlan as parsePluginInstallPlan } from "./capabilities_plugin_helpers";
export { pluginPlanActionLabel as pluginPlanActionLabel } from "./capabilities_plugin_helpers";
export { pluginPlanNotice as pluginPlanNotice } from "./capabilities_plugin_helpers";

