import { useCallback, useEffect, useMemo, useState } from "react";
import { ArrowLeft, ChevronDown, ChevronRight, Plus, RefreshCw, Search, Server as ServerIcon } from "lucide-react";
import { asArray } from "../lib/array";
import { app } from "../lib/bridge";
import { activeWorkBusyNoticeText, installMCPServer } from "../lib/capabilityMutations";
import { useT } from "../lib/i18n";
import { mcpServerLifecycleActions } from "../lib/mcpServerLifecycle";
import type { MCPMarketplaceEntry, MCPMarketplaceView, MCPServerInput, ServerView } from "../lib/types";
import { InlineConfirmButton } from "./InlineConfirmButton";
import { Tooltip } from "./Tooltip";
import { connectMCPServer, mcpSettingsSnapshot, setMCPSettingsSnapshot, settingsSnapshotKey, normalizeServerViews, sortServersForDisplay, mcpServerSummary } from "./capabilities_helpers";
import { summarizeServerError, ServerDetails, serverCommand, normalizeTransportValue, parseKeyValueText, serverActionLabel } from "./capabilities_servers";
import { parseMCPQuickDefinition } from "./capabilities_plugins";
import { mcpSettingsServerSummary } from "./CapabilitiesPanel";
type MCPSettingsScreen = {
    kind: "list";
} | {
    kind: "add";
} | {
    kind: "marketplace";
} | {
    kind: "detail";
    name: string;
} | {
    kind: "edit";
    name: string;
};
type MCPServerEditorDraft = {
    name: string;
    transport: string;
    command: string;
    structuredCommand?: {
        display: string;
        command: string;
        args: string[];
    };
    url: string;
    env: string;
    headers: string;
    autoStart?: boolean;
    callTimeoutSeconds?: number;
    toolTimeoutSeconds?: Record<string, number>;
};
export type MCPServerJSONError = "invalid" | "single" | "name" | "required" | "unsupported";
export function mcpServerSchemaIssueCount(server: ServerView): number {
    return (server.toolList ?? []).filter((tool) => tool.schemaError).length;
}
function mcpSettingsSearchText(server: ServerView): string {
    return [
        server.name,
        server.transport,
        serverCommand(server),
        server.error,
        server.source,
        server.configSource,
        server.managedByPlugin,
        ...(server.toolList ?? []).flatMap((tool) => [tool.name, tool.description]),
    ].filter(Boolean).join(" ").toLowerCase();
}
function MCPSettingsSubpageHeader({ title, description, onBack, }: {
    title: string;
    description: string;
    onBack: () => void;
}) {
    const t = useT();
    return (<header className="cap-mcp-subpage__header">
			<button className="cap-mcp-subpage__back" type="button" onClick={onBack}>
				<ArrowLeft aria-hidden size={14}/>
				{t("caps.backToServers")}
			</button>
			<h3 className="cap-mcp-subpage__title">{title}</h3>
			<p className="cap-mcp-subpage__desc">{description}</p>
		</header>);
}
function MCPSettingsServerRow({ server, busy, onOpen, onRetry, onToggle, onRemove, }: {
    server: ServerView;
    busy: boolean;
    onOpen: () => void;
    onRetry: () => void;
    onToggle: (enabled: boolean) => void;
    onRemove: () => void;
}) {
    const t = useT();
    const lifecycle = mcpServerLifecycleActions(server);
    const target = serverCommand(server);
    const actionLabel = serverActionLabel(server, t);
    const canRemove = server.configured && !server.builtIn && !server.managedByPlugin;
    const handlePrimaryAction = () => {
        onRetry();
    };
    return (<div className={`cap-mcp-list-row${server.status === "disabled" ? " cap-mcp-list-row--disabled" : ""}`} data-status={server.status}>
			<button className="cap-mcp-list-row__main" type="button" onClick={onOpen}>
				<span className="cap-mcp-list-row__icon" aria-hidden>
					<ServerIcon size={16} strokeWidth={1.8}/>
				</span>
				<span className="cap-mcp-list-row__copy">
					<span className="cap-mcp-list-row__head">
						<span className={`cap-dot cap-dot--${server.status}`} aria-hidden/>
						<span className="cap-mcp-list-row__name">{server.name}</span>
						<span className="cap-mcp-list-row__transport">{server.transport}</span>
						{server.source === "project" && <span className="cap-row__builtin">{t("caps.projectServerBadge")}</span>}
						{server.builtIn && <span className="cap-row__builtin">{t("caps.builtIn")}</span>}
					</span>
					<span className={`cap-mcp-list-row__summary${server.status === "failed" ? " cap-mcp-list-row__summary--error" : ""}`}>
						{mcpSettingsServerSummary(server, t)}
					</span>
					{target && <span className="cap-mcp-list-row__target">{target}</span>}
					{server.managedByPlugin && (<span className="cap-mcp-list-row__owner">{t("caps.managedByPlugin", { plugin: server.managedByPlugin })}</span>)}
				</span>
				<ChevronRight className="cap-mcp-list-row__chevron" aria-hidden size={16}/>
			</button>
			<div className="cap-mcp-list-row__actions">
				{canRemove && (<InlineConfirmButton label={t("caps.remove")} confirmLabel={t("caps.confirmRemove")} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={onRemove}/>)}
				{lifecycle.showRetryInRow ? (<button className="btn btn--small" disabled={busy} type="button" onClick={handlePrimaryAction}>
						{actionLabel}
					</button>) : !server.managedByPlugin ? (<Tooltip label={lifecycle.enabled ? t("caps.disable") : t("caps.enable")}>
						<label className="cap-switch">
							<input type="checkbox" checked={lifecycle.enabled} disabled={busy} onChange={(event) => onToggle(event.target.checked)}/>
							<span className="cap-switch__track"/>
						</label>
					</Tooltip>) : null}
			</div>
		</div>);
}
function MCPSettingsServerGroup({ title, hint, servers, busy, onOpen, onRetry, onToggle, onRemove, }: {
    title: string;
    hint?: string;
    servers: ServerView[];
    busy: boolean;
    onOpen: (name: string) => void;
    onRetry: (name: string) => void;
    onToggle: (name: string, enabled: boolean) => void;
    onRemove: (name: string) => void;
}) {
    if (servers.length === 0)
        return null;
    return (<section className="cap-mcp-list-section">
			<div className="cap-mcp-list-section__head">
				<div>
					<div className="cap-mcp-list-section__title">{title} <span>{servers.length}</span></div>
					{hint && <div className="cap-mcp-list-section__hint">{hint}</div>}
				</div>
			</div>
			<div className="cap-mcp-list">
				{servers.map((server) => (<MCPSettingsServerRow key={server.name} server={server} busy={busy} onOpen={() => onOpen(server.name)} onRetry={() => onRetry(server.name)} onToggle={(enabled) => onToggle(server.name, enabled)} onRemove={() => onRemove(server.name)}/>))}
			</div>
		</section>);
}
function mcpServerEditorDraft(server?: ServerView): MCPServerEditorDraft {
    const transport = normalizeTransportValue(server?.transport || "stdio");
    const command = server && transport === "stdio" ? serverCommand(server) : "";
    return {
        name: server?.name || "",
        transport,
        command,
        structuredCommand: server && transport === "stdio" ? {
            display: command,
            command: server.command || "",
            args: [...(server.args ?? [])],
        } : undefined,
        url: server && transport !== "stdio" ? server.url || serverCommand(server) : "",
        env: "",
        headers: "",
        autoStart: server?.autoStart,
        callTimeoutSeconds: server?.callTimeoutSeconds,
        toolTimeoutSeconds: server?.toolTimeoutSeconds ? { ...server.toolTimeoutSeconds } : undefined,
    };
}
function mcpServerInputDraft(input: MCPServerInput): MCPServerEditorDraft {
    const transport = normalizeTransportValue(input.transport);
    const command = transport === "stdio" ? [input.command, ...input.args].filter(Boolean).join(" ").trim() : "";
    return {
        name: input.name,
        transport,
        command,
        structuredCommand: transport === "stdio" ? {
            display: command,
            command: input.command,
            args: [...input.args],
        } : undefined,
        url: transport === "stdio" ? "" : input.url,
        env: input.env ? Object.entries(input.env).map(([key, value]) => `${key}=${value}`).join("\n") : "",
        headers: input.headers ? Object.entries(input.headers).map(([key, value]) => `${key}=${value}`).join("\n") : "",
        autoStart: input.autoStart ?? undefined,
        callTimeoutSeconds: input.callTimeoutSeconds ?? undefined,
        toolTimeoutSeconds: input.toolTimeoutSeconds ? { ...input.toolTimeoutSeconds } : undefined,
    };
}
function mcpServerDraftInput(draft: MCPServerEditorDraft): MCPServerInput {
    const isStdio = draft.transport === "stdio";
    const structuredCommand = draft.structuredCommand?.display === draft.command ? draft.structuredCommand : undefined;
    const envText = draft.env.trim();
    const headerText = draft.headers.trim();
    return {
        name: draft.name.trim(),
        transport: draft.transport,
        command: isStdio ? structuredCommand?.command || draft.command.trim() : "",
        args: isStdio ? structuredCommand?.args ?? [] : [],
        url: isStdio ? "" : draft.url.trim(),
        env: envText ? parseKeyValueText(envText) : null,
        headers: !isStdio && headerText ? parseKeyValueText(headerText) : null,
        autoStart: draft.autoStart ?? null,
        callTimeoutSeconds: draft.callTimeoutSeconds ?? null,
        toolTimeoutSeconds: draft.toolTimeoutSeconds ?? null,
    };
}
function mcpMarketplaceServerInput(entry: MCPMarketplaceEntry, servers: ServerView[]): MCPServerInput {
    const used = new Set(servers.map((server) => server.name));
    const base = entry.suggestedName || entry.name.split("/").filter(Boolean).pop() || "mcp-server";
    let name = base;
    for (let suffix = 2; used.has(name); suffix += 1)
        name = `${base}-${suffix}`;
    const transport = entry.transport || "stdio";
    return {
        name,
        transport,
        command: transport === "stdio" ? entry.command || "" : "",
        args: transport === "stdio" ? [...(entry.args ?? [])] : [],
        url: transport === "stdio" ? "" : entry.url || "",
        env: null,
        headers: null,
        autoStart: null,
        callTimeoutSeconds: null,
        toolTimeoutSeconds: null,
    };
}
export function mcpServerDraftJSON(draft: MCPServerEditorDraft): string {
    const input = mcpServerDraftInput(draft);
    const entry: Record<string, unknown> = { type: input.transport };
    if (input.transport === "stdio") {
        entry.command = input.command;
        if (input.args.length > 0)
            entry.args = input.args;
    }
    else
        entry.url = input.url;
    if (input.env && Object.keys(input.env).length > 0)
        entry.env = input.env;
    if (input.headers && Object.keys(input.headers).length > 0)
        entry.headers = input.headers;
    if (input.autoStart != null)
        entry.auto_start = input.autoStart;
    if (input.callTimeoutSeconds != null)
        entry.call_timeout_seconds = input.callTimeoutSeconds;
    if (input.toolTimeoutSeconds && Object.keys(input.toolTimeoutSeconds).length > 0)
        entry.tool_timeout_seconds = input.toolTimeoutSeconds;
    return JSON.stringify({ [input.name || "server-name"]: entry }, null, 2);
}
function isRecord(value: unknown): value is Record<string, unknown> {
    return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}
function stringRecord(value: unknown): Record<string, string> | null {
    if (value == null)
        return null;
    if (!isRecord(value) || Object.values(value).some((item) => typeof item !== "string"))
        throw new Error("invalid");
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, item as string]));
}
function assertSupportedKeys(value: Record<string, unknown>, supported: readonly string[]) {
    const allowed = new Set(supported);
    if (Object.keys(value).some((key) => !allowed.has(key)))
        throw new Error("unsupported" satisfies MCPServerJSONError);
}
function nonNegativeInteger(value: unknown): number | undefined {
    if (value == null)
        return undefined;
    if (typeof value !== "number" || !Number.isInteger(value) || value < 0)
        throw new Error("invalid" satisfies MCPServerJSONError);
    return value;
}
function nonNegativeIntegerRecord(value: unknown): Record<string, number> | undefined {
    if (value == null)
        return undefined;
    if (!isRecord(value))
        throw new Error("invalid" satisfies MCPServerJSONError);
    const out: Record<string, number> = {};
    for (const [name, item] of Object.entries(value)) {
        if (!name.trim())
            throw new Error("invalid" satisfies MCPServerJSONError);
        const seconds = nonNegativeInteger(item);
        if (seconds === undefined)
            throw new Error("invalid" satisfies MCPServerJSONError);
        out[name] = seconds;
    }
    return out;
}
// withExplicitMCPClears finalizes an edit of an existing server. The editor
// seeds every non-secret setting into the draft/JSON, so a field the user
// removed must clear the persisted value instead of being preserved as
// "absent". env/headers stay preserve-on-absent because their values are
// deliberately never seeded into the editor.
export function withExplicitMCPClears(input: MCPServerInput): MCPServerInput {
    return {
        ...input,
        autoStart: input.autoStart ?? true,
        callTimeoutSeconds: input.callTimeoutSeconds ?? 0,
        toolTimeoutSeconds: input.toolTimeoutSeconds ?? {},
    };
}
export function parseMCPServerJSON(raw: string, fixedName?: string, options?: {
    allowIncomplete?: boolean;
}): {
    input: MCPServerInput;
    draft: MCPServerEditorDraft;
} {
    let parsed: unknown;
    try {
        parsed = JSON.parse(raw);
    }
    catch {
        throw new Error("invalid" satisfies MCPServerJSONError);
    }
    if (!isRecord(parsed))
        throw new Error("single" satisfies MCPServerJSONError);
    if (isRecord(parsed.mcpServers))
        assertSupportedKeys(parsed, ["mcpServers"]);
    const container = isRecord(parsed.mcpServers) ? parsed.mcpServers : parsed;
    const entries = Object.entries(container);
    if (entries.length !== 1)
        throw new Error("single" satisfies MCPServerJSONError);
    const [name, value] = entries[0];
    if (!name.trim() || !isRecord(value))
        throw new Error("single" satisfies MCPServerJSONError);
    assertSupportedKeys(value, [
        "type", "transport", "command", "args", "url", "env", "headers", "auto_start",
        "call_timeout_seconds", "tool_timeout_seconds", "trusted_read_only_tools",
        "default_tools_approval_mode", "tools", "approvals_reviewer",
    ]);
    if (fixedName && name !== fixedName)
        throw new Error("name" satisfies MCPServerJSONError);
    if (value.type != null && typeof value.type !== "string")
        throw new Error("invalid" satisfies MCPServerJSONError);
    if (value.transport != null && typeof value.transport !== "string")
        throw new Error("invalid" satisfies MCPServerJSONError);
    if (value.type != null && value.transport != null)
        throw new Error("unsupported" satisfies MCPServerJSONError);
    const transportValue = typeof value.type === "string" ? value.type : value.transport;
    const transport = normalizeTransportValue(typeof transportValue === "string" ? transportValue : (typeof value.url === "string" ? "http" : "stdio"));
    if (transport !== "stdio" && transport !== "http" && transport !== "sse")
        throw new Error("invalid" satisfies MCPServerJSONError);
    if (transport === "stdio" && (value.url != null || value.headers != null))
        throw new Error("unsupported" satisfies MCPServerJSONError);
    if (transport !== "stdio" && (value.command != null || value.args != null))
        throw new Error("unsupported" satisfies MCPServerJSONError);
    const command = typeof value.command === "string" ? value.command.trim() : "";
    if (value.args != null && (!Array.isArray(value.args) || !value.args.every((arg) => typeof arg === "string")))
        throw new Error("invalid" satisfies MCPServerJSONError);
    const args = value.args ? value.args as string[] : [];
    const url = typeof value.url === "string" ? value.url.trim() : "";
    if (!options?.allowIncomplete && ((transport === "stdio" && !command) || (transport !== "stdio" && !url))) {
        throw new Error("required" satisfies MCPServerJSONError);
    }
    let env: Record<string, string> | null;
    let headers: Record<string, string> | null;
    try {
        env = stringRecord(value.env);
        headers = stringRecord(value.headers);
    }
    catch {
        throw new Error("invalid" satisfies MCPServerJSONError);
    }
    if (value.auto_start != null && typeof value.auto_start !== "boolean")
        throw new Error("invalid" satisfies MCPServerJSONError);
    const autoStart = value.auto_start as boolean | undefined;
    const callTimeoutSeconds = nonNegativeInteger(value.call_timeout_seconds);
    const toolTimeoutSeconds = nonNegativeIntegerRecord(value.tool_timeout_seconds);
    const input: MCPServerInput = {
        name: fixedName || name,
        transport,
        command: transport === "stdio" ? command : "",
        args: transport === "stdio" ? args : [],
        url: transport === "stdio" ? "" : url,
        env,
        headers: transport === "stdio" ? null : headers,
        autoStart: autoStart ?? null,
        callTimeoutSeconds: callTimeoutSeconds ?? null,
        toolTimeoutSeconds: toolTimeoutSeconds ?? null,
    };
    return {
        input,
        draft: {
            name: input.name,
            transport,
            command: [command, ...args].filter(Boolean).join(" "),
            structuredCommand: transport === "stdio" ? {
                display: [command, ...args].filter(Boolean).join(" "),
                command,
                args: [...args],
            } : undefined,
            url,
            env: env ? Object.entries(env).map(([key, item]) => `${key}=${item}`).join("\n") : "",
            headers: headers ? Object.entries(headers).map(([key, item]) => `${key}=${item}`).join("\n") : "",
            autoStart,
            callTimeoutSeconds,
            toolTimeoutSeconds,
        },
    };
}
function mcpServerJSONErrorLabel(error: unknown, t: ReturnType<typeof useT>): string {
    const code = error instanceof Error ? error.message as MCPServerJSONError : "invalid";
    if (code === "single")
        return t("caps.jsonSingleServer");
    if (code === "name")
        return t("caps.jsonNameMismatch");
    if (code === "required")
        return t("caps.jsonRequired");
    if (code === "unsupported")
        return t("caps.jsonUnsupported");
    return t("caps.jsonInvalid");
}
export function MCPServerSettingsEditor({ server, busy, onCancel, onSubmit, }: {
    server?: ServerView;
    busy: boolean;
    onCancel: () => void;
    onSubmit: (input: MCPServerInput) => void;
}) {
    const t = useT();
    type EditorMode = "quick" | "form" | "json";
    const [mode, setMode] = useState<EditorMode>(server ? "form" : "quick");
    const [definition, setDefinition] = useState("");
    const [quickError, setQuickError] = useState("");
    const [draft, setDraft] = useState<MCPServerEditorDraft>(() => mcpServerEditorDraft(server));
    const [json, setJSON] = useState(() => mcpServerDraftJSON(mcpServerEditorDraft(server)));
    const [jsonError, setJSONError] = useState("");
    const [advancedOpen, setAdvancedOpen] = useState(false);
    const isStdio = draft.transport === "stdio";
    const ready = Boolean(draft.name.trim() && (isStdio ? draft.command.trim() : draft.url.trim()));
    const updateDraft = (patch: Partial<MCPServerEditorDraft>) => setDraft((current) => ({ ...current, ...patch }));
    const switchMode = (next: EditorMode) => {
        if (next === mode)
            return;
        if (next === "quick") {
            setQuickError("");
            setMode("quick");
            return;
        }
        if (mode === "quick") {
            if (definition.trim()) {
                try {
                    const nextDraft = mcpServerInputDraft(parseMCPQuickDefinition(definition));
                    setDraft(nextDraft);
                    if (next === "json")
                        setJSON(mcpServerDraftJSON(nextDraft));
                    setQuickError("");
                }
                catch (error) {
                    setQuickError(mcpServerJSONErrorLabel(error, t));
                    return;
                }
            }
            setMode(next);
            return;
        }
        if (next === "json") {
            setJSON(mcpServerDraftJSON(draft));
            setJSONError("");
            setMode("json");
            return;
        }
        if (json === mcpServerDraftJSON(draft)) {
            setJSONError("");
            setMode("form");
            return;
        }
        try {
            const parsed = parseMCPServerJSON(json, server?.name, { allowIncomplete: true });
            setDraft(parsed.draft);
            setJSONError("");
            setMode("form");
        }
        catch (error) {
            setJSONError(mcpServerJSONErrorLabel(error, t));
        }
    };
    const finalize = (input: MCPServerInput) => (server ? withExplicitMCPClears(input) : input);
    const submit = () => {
        if (mode === "quick") {
            try {
                setQuickError("");
                onSubmit(parseMCPQuickDefinition(definition));
            }
            catch (error) {
                setQuickError(mcpServerJSONErrorLabel(error, t));
            }
            return;
        }
        if (mode === "form") {
            onSubmit(finalize(mcpServerDraftInput(draft)));
            return;
        }
        try {
            const parsed = parseMCPServerJSON(json, server?.name);
            setJSONError("");
            onSubmit(finalize(parsed.input));
        }
        catch (error) {
            setJSONError(mcpServerJSONErrorLabel(error, t));
        }
    };
    return (<div className="cap-mcp-editor">
			<div className="cap-mcp-editor__mode set-seg" role="tablist" aria-label={t("caps.editorMode")}>
				{!server && (<button className={`set-seg__btn${mode === "quick" ? " set-seg__btn--on" : ""}`} type="button" role="tab" aria-selected={mode === "quick"} onClick={() => switchMode("quick")}>
						{t("caps.quickMode")}
					</button>)}
				<button className={`set-seg__btn${mode === "form" ? " set-seg__btn--on" : ""}`} type="button" role="tab" aria-selected={mode === "form"} onClick={() => switchMode("form")}>
					{t("caps.formMode")}
				</button>
				<button className={`set-seg__btn${mode === "json" ? " set-seg__btn--on" : ""}`} type="button" role="tab" aria-selected={mode === "json"} onClick={() => switchMode("json")}>
					{t("caps.jsonMode")}
				</button>
			</div>
			{mode === "quick" ? (<div className="cap-mcp-quick">
					<label className="cap-mcp-field">
						<span>{t("caps.installDefinition")}</span>
						<textarea className="mem-textarea cap-mcp-quick__input" value={definition} disabled={busy} onChange={(event) => { setDefinition(event.target.value); setQuickError(""); }} placeholder={t("caps.installDefinitionPlaceholder")} spellCheck={false}/>
					</label>
					<div className="cap-mcp-quick__hint">{t("caps.installDefinitionHint")}</div>
					<div className="cap-mcp-quick__benefits" aria-label={t("caps.quickBenefitsLabel")}>
						<span>{t("caps.quickDetectTransport")}</span>
						<span>{t("caps.quickVerifyConnection")}</span>
						<span>{t("caps.quickEnableTools")}</span>
					</div>
					{quickError && <div className="banner banner--error" role="alert">{quickError}</div>}
				</div>) : mode === "form" ? (<div className="cap-mcp-form-grid">
					<label className="cap-mcp-field cap-mcp-field--name">
						<span>{t("caps.name")}</span>
						<input className="mem-input" value={draft.name} disabled={busy || Boolean(server)} onChange={(event) => updateDraft({ name: event.target.value })} placeholder={t("caps.namePlaceholder")}/>
					</label>
					<label className="cap-mcp-field cap-mcp-field--transport">
						<span>{t("caps.transport")}</span>
						<select className="mem-select" value={draft.transport} disabled={busy} onChange={(event) => updateDraft({ transport: normalizeTransportValue(event.target.value) })}>
							<option value="stdio">stdio</option>
							<option value="http">http</option>
							<option value="sse">sse</option>
						</select>
					</label>
					{isStdio ? (<label className="cap-mcp-field cap-mcp-field--wide">
							<span>{t("caps.command")}</span>
							<input className="mem-input" value={draft.command} disabled={busy} onChange={(event) => updateDraft({ command: event.target.value })} placeholder={t("caps.commandPlaceholder")}/>
						</label>) : (<label className="cap-mcp-field cap-mcp-field--wide">
							<span>{t("caps.url")}</span>
							<input className="mem-input" value={draft.url} disabled={busy} onChange={(event) => updateDraft({ url: event.target.value })} placeholder={t("caps.urlPlaceholder")}/>
						</label>)}
					<div className="cap-mcp-advanced cap-mcp-field--wide">
						<button className="cap-mcp-advanced__toggle" type="button" aria-expanded={advancedOpen} onClick={() => setAdvancedOpen((open) => !open)}>
							{advancedOpen ? <ChevronDown aria-hidden size={14}/> : <ChevronRight aria-hidden size={14}/>}
							{advancedOpen ? t("caps.hideAdvancedOptions") : t("caps.advancedOptions")}
						</button>
						{advancedOpen && (<div className="cap-mcp-advanced__body">
								{!isStdio && (<label className="cap-mcp-field">
										<span>{t("caps.headersLabel")}</span>
										<textarea className="mem-textarea" value={draft.headers} disabled={busy} onChange={(event) => updateDraft({ headers: event.target.value })} placeholder={t("caps.headersPlaceholder")} spellCheck={false}/>
										{server?.headerKeys && server.headerKeys.length > 0 && <small>{t("caps.headersPreserveHint")}</small>}
									</label>)}
								<label className="cap-mcp-field">
									<span>{t("caps.envLabel")}</span>
									<textarea className="mem-textarea" value={draft.env} disabled={busy} onChange={(event) => updateDraft({ env: event.target.value })} placeholder={t("caps.envPlaceholder")} spellCheck={false}/>
									{server?.envKeys && server.envKeys.length > 0 && <small>{t("caps.envPreserveHint")}</small>}
								</label>
							</div>)}
					</div>
				</div>) : (<div className="cap-mcp-json-editor">
					<label className="cap-mcp-field">
						<span>{t("caps.jsonConfig")}</span>
						<textarea className="mem-textarea cap-mcp-json-editor__input" value={json} disabled={busy} onInput={(event) => { setJSON(event.currentTarget.value); setJSONError(""); }} spellCheck={false}/>
					</label>
					<div className="cap-mcp-json-editor__hint">{t("caps.jsonPasteHint")}</div>
					{jsonError && <div className="banner banner--error" role="alert">{jsonError}</div>}
				</div>)}
			<div className="cap-mcp-editor__actions">
				<button className="btn btn--small" disabled={busy} type="button" onClick={onCancel}>{t("common.cancel")}</button>
				<button className="btn btn--primary btn--small" disabled={busy || (mode === "quick" ? !definition.trim() : mode === "form" && !ready)} type="button" onClick={submit}>
					{server ? t("caps.saveConfig") : t("caps.addAndConnect")}
				</button>
			</div>
		</div>);
}
// MCPServersSettingsPage is a self-contained MCP servers management page
// embedded inside the settings centre.
export function MCPServersSettingsPage() {
    const t = useT();
    const [snapshotKey, setSnapshotKey] = useState("");
    const [servers, setServers] = useState<ServerView[] | null>(null);
    const [busy, setBusy] = useState(false);
    const [err, setErr] = useState<string | null>(null);
    const [query, setQuery] = useState("");
    const [screen, setScreen] = useState<MCPSettingsScreen>({ kind: "list" });
    const [marketplace, setMarketplace] = useState<MCPMarketplaceView | null>(null);
    const [marketplaceQuery, setMarketplaceQuery] = useState("");
    const reload = useCallback(async () => {
        const [meta, tabs] = await Promise.all([
            app.Meta().catch(() => null),
            app.ListTabs().catch(() => []),
        ]);
        const key = settingsSnapshotKey(meta, tabs);
        setSnapshotKey(key);
        const cached = key ? mcpSettingsSnapshot : null;
        if (cached?.key === key) {
            setServers(cached.value);
        }
        else {
            setServers(null);
        }
        const next = normalizeServerViews(await app.MCPServers().catch(() => []));
        setMCPSettingsSnapshot({ key, value: next });
        setServers(next);
    }, []);
    useEffect(() => { void reload(); }, [reload]);
    useEffect(() => {
        if (!servers?.some((s) => s.status === "initializing" || s.status === "deferred"))
            return;
        const id = window.setInterval(() => void reload(), 2500);
        return () => window.clearInterval(id);
    }, [reload, servers]);
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
    const browseMarketplace = async (search = marketplaceQuery) => {
        setBusy(true);
        setErr(null);
        try {
            const result = await app.MCPMarketplace(search);
            setMarketplace({ ...result, servers: asArray(result.servers) });
            return true;
        }
        catch (error) {
            setErr(String((error as Error)?.message ?? error));
            return false;
        }
        finally {
            setBusy(false);
        }
    };
    const openMarketplace = () => {
        setScreen({ kind: "marketplace" });
        if (marketplace === null)
            void browseMarketplace("");
    };
    const installMarketplaceEntry = async (entry: MCPMarketplaceEntry) => {
        const current = await app.MCPMarketplaceResolve(entry.name);
        return installMCPServer(mcpMarketplaceServerInput(current, servers ?? []));
    };
    const filteredServers = useMemo(() => {
        const sorted = sortServersForDisplay(servers ?? []);
        const normalizedQuery = query.trim().toLowerCase();
        return normalizedQuery ? sorted.filter((server) => mcpSettingsSearchText(server).includes(normalizedQuery)) : sorted;
    }, [query, servers]);
    const projectServers = useMemo(() => filteredServers.filter((server) => server.source === "project"), [filteredServers]);
    const managedServers = useMemo(() => filteredServers.filter((server) => server.source === "plugin" || Boolean(server.managedByPlugin)), [filteredServers]);
    const installedServers = useMemo(() => filteredServers.filter((server) => server.source !== "project" && server.source !== "plugin" && !server.managedByPlugin), [filteredServers]);
    const selectedServer = screen.kind === "detail" || screen.kind === "edit"
        ? servers?.find((server) => server.name === screen.name)
        : undefined;
    useEffect(() => {
        if (servers && (screen.kind === "detail" || screen.kind === "edit") && !servers.some((server) => server.name === screen.name)) {
            setScreen({ kind: "list" });
        }
    }, [screen, servers]);
    const summary = useMemo(() => {
        if (!servers)
            return "";
        return mcpServerSummary(servers, t);
    }, [servers, t]);
    const loading = servers === null;
    const actionBusy = busy || !snapshotKey || loading;
    return (<section className="cap-mcp-settings">
			{err && <div className="banner banner--error" role="alert">{err}</div>}
			{screen.kind === "list" && (<>
					<div className="cap-mcp-list-toolbar">
						{servers && servers.length > 0 ? <div className="drawer__summary">{summary}</div> : <span />}
						<div className="cap-mcp-list-toolbar__actions">
							<Tooltip label={t("caps.refresh")}>
								<button className="cap-mcp-icon-btn" type="button" aria-label={t("caps.refresh")} disabled={actionBusy} onClick={() => void reload()}>
									<RefreshCw aria-hidden size={15}/>
								</button>
							</Tooltip>
							<button className="btn btn--small" disabled={actionBusy} type="button" onClick={openMarketplace}>
								<Search aria-hidden size={14}/>
								{t("caps.browseRegistry")}
							</button>
							<button className="btn btn--primary btn--small cap-mcp-add-btn" disabled={actionBusy} type="button" onClick={() => setScreen({ kind: "add" })}>
								<Plus aria-hidden size={14}/>
								{t("caps.addServer")}
							</button>
						</div>
					</div>
					<label className="cap-mcp-search">
						<Search aria-hidden size={15}/>
						<input type="search" value={query} onInput={(event) => setQuery(event.currentTarget.value)} placeholder={t("caps.searchServers")}/>
					</label>
					{loading && <div className="mem-empty">{t("caps.loading")}</div>}
					{!loading && servers.length === 0 && <div className="mem-empty">{t("caps.noServers")}</div>}
					{!loading && servers.length > 0 && filteredServers.length === 0 && <div className="mem-empty">{t("caps.noServerMatches")}</div>}
					<MCPSettingsServerGroup title={t("caps.projectServers")} hint={t("caps.projectServersHint")} servers={projectServers} busy={actionBusy} onOpen={(name) => setScreen({ kind: "detail", name })} onRetry={(name) => void mutate(() => connectMCPServer(name, servers ?? []))} onToggle={(name, enabled) => void mutate(() => app.SetMCPServerEnabled(name, enabled))} onRemove={(name) => void mutate(() => app.RemoveMCPServer(name))}/>
					<MCPSettingsServerGroup title={t("caps.installedServers")} hint={t("caps.installedServersHint")} servers={installedServers} busy={actionBusy} onOpen={(name) => setScreen({ kind: "detail", name })} onRetry={(name) => void mutate(() => connectMCPServer(name, servers ?? []))} onToggle={(name, enabled) => void mutate(() => app.SetMCPServerEnabled(name, enabled))} onRemove={(name) => void mutate(() => app.RemoveMCPServer(name))}/>
					<MCPSettingsServerGroup title={t("caps.pluginServers")} hint={t("caps.pluginServersHint")} servers={managedServers} busy={actionBusy} onOpen={(name) => setScreen({ kind: "detail", name })} onRetry={(name) => void mutate(() => connectMCPServer(name, servers ?? []))} onToggle={(name, enabled) => void mutate(() => app.SetMCPServerEnabled(name, enabled))} onRemove={(name) => void mutate(() => app.RemoveMCPServer(name))}/>
				</>)}
			{screen.kind === "marketplace" && (<div className="cap-mcp-subpage">
					<MCPSettingsSubpageHeader title={t("caps.registryTitle")} description={t("caps.registryHint")} onBack={() => setScreen({ kind: "list" })}/>
					<form className="cap-mcp-search cap-mcp-search--action" onSubmit={(event) => { event.preventDefault(); void browseMarketplace(); }}>
						<Search aria-hidden size={15}/>
						<input type="search" value={marketplaceQuery} onInput={(event) => setMarketplaceQuery(event.currentTarget.value)} placeholder={t("caps.searchRegistry")}/>
						<button className="btn btn--small" disabled={busy} type="submit">{t("caps.search")}</button>
					</form>
					{marketplace?.warning && <div className="banner" role="status">{t("caps.registryCached")} {marketplace.warning}</div>}
					{busy && marketplace === null && <div className="mem-empty">{t("caps.loading")}</div>}
					{!busy && marketplace && marketplace.servers.length === 0 && <div className="mem-empty">{t("caps.noRegistryMatches")}</div>}
					{marketplace && marketplace.servers.length > 0 && (<div className="cap-mcp-list">
							{marketplace.servers.map((entry) => (<div className="cap-mcp-list-row" key={entry.name}>
									<div className="cap-mcp-list-row__main">
										<span className="cap-mcp-list-row__icon" aria-hidden><ServerIcon size={16} strokeWidth={1.8}/></span>
										<span className="cap-mcp-list-row__copy">
											<span className="cap-mcp-list-row__head">
												<span className="cap-mcp-list-row__name">{entry.title || entry.name}</span>
												{entry.version && <span className="cap-mcp-list-row__transport">{entry.version}</span>}
												{entry.transport && <span className="cap-mcp-list-row__transport">{entry.transport}</span>}
											</span>
											<span className="cap-mcp-list-row__target">{entry.name}</span>
											<span className="cap-mcp-list-row__summary">{entry.description || entry.unavailableReason}</span>
											{!entry.installable && entry.unavailableReason && <span className="cap-mcp-list-row__owner">{entry.unavailableReason}</span>}
										</span>
									</div>
									<div className="cap-mcp-list-row__actions">
										{entry.installable ? (<button className="btn btn--primary btn--small" disabled={actionBusy || marketplace.cached} type="button" onClick={() => void mutate(() => installMarketplaceEntry(entry)).then((ok) => {
                            if (ok)
                                setScreen({ kind: "list" });
                        })}>
												{t("caps.install")}
											</button>) : <span className="cap-mcp-list-row__owner">{t("caps.manualSetup")}</span>}
									</div>
								</div>))}
						</div>)}
				</div>)}
			{screen.kind === "add" && (<div className="cap-mcp-subpage">
					<MCPSettingsSubpageHeader title={t("caps.addServerTitle")} description={t("caps.addServerHint")} onBack={() => setScreen({ kind: "list" })}/>
					<MCPServerSettingsEditor busy={busy} onCancel={() => setScreen({ kind: "list" })} onSubmit={(input) => void mutate(() => installMCPServer(input)).then((ok) => {
                if (ok)
                    setScreen({ kind: "list" });
            })}/>
				</div>)}
			{screen.kind === "edit" && selectedServer && (<div className="cap-mcp-subpage">
					<MCPSettingsSubpageHeader title={t("caps.editServerTitle", { name: selectedServer.name })} description={t("caps.editServerHint")} onBack={() => setScreen({ kind: "detail", name: selectedServer.name })}/>
					<MCPServerSettingsEditor server={selectedServer} busy={busy} onCancel={() => setScreen({ kind: "detail", name: selectedServer.name })} onSubmit={(input) => void mutate(() => app.UpdateMCPServer(selectedServer.name, input)).then((ok) => {
                if (ok)
                    setScreen({ kind: "detail", name: selectedServer.name });
            })}/>
				</div>)}
			{screen.kind === "detail" && selectedServer && (<div className="cap-mcp-subpage">
					<MCPSettingsSubpageHeader title={selectedServer.name} description={t("caps.serverDetailsHint")} onBack={() => setScreen({ kind: "list" })}/>
					{selectedServer.error && (<div className="cap-mcp-detail-error">
							<div className="banner banner--error">{summarizeServerError(selectedServer.error)}</div>
							<details>
								<summary>{t("caps.rawLog")}</summary>
								<pre>{selectedServer.error}</pre>
							</details>
						</div>)}
					<ServerDetails s={selectedServer} tools={selectedServer.toolList ?? []} busy={actionBusy} onConfirm={() => void mutate(() => app.RemoveMCPServer(selectedServer.name)).then((ok) => {
                if (ok)
                    setScreen({ kind: "list" });
            })} onConnectNow={() => void mutate(() => connectMCPServer(selectedServer.name, servers ?? []))} onReconnect={() => void mutate(() => app.ReconnectMCPServer(selectedServer.name))} onConfirmClearAuth={() => void mutate(() => app.ClearMCPServerAuthentication(selectedServer.name))} toolsExpanded editing={false} onEdit={() => setScreen({ kind: "edit", name: selectedServer.name })} onCancelEdit={() => undefined} onUpdate={() => undefined} onToggleTools={() => undefined} standalone showToolsToggle={false}/>
				</div>)}
		</section>);
}

