import { useMemo, useState } from "react";
import { CircleAlert } from "lucide-react";
import { useT } from "../lib/i18n";
import { mcpServerLifecycleActions, mcpServerRetryableFromAvailableList } from "../lib/mcpServerLifecycle";
import { canUseNativeMCPOAuth } from "../lib/mcpOAuthEligibility";
import type { MCPServerInput, ServerView } from "../lib/types";
import { InlineConfirmButton } from "./InlineConfirmButton";
import { Tooltip } from "./Tooltip";
import { mcpServerSourceLabel } from "./CapabilitiesPanel";
export function ServerGroup({ servers, expanded, expandedTools, busy, editing, onConfirm, onEdit, onCancelEdit, onRetry, onReconnect, onConfirmClearAuth, onToggle, onUpdate, onToggleDetails, onToggleTools, }: {
    servers: ServerView[];
    expanded: Set<string>;
    expandedTools: Set<string>;
    busy: boolean;
    editing: string | null;
    onConfirm: (name: string) => void;
    onEdit: (name: string) => void;
    onCancelEdit: () => void;
    onRetry: (name: string) => void;
    onReconnect: (name: string) => void;
    onConfirmClearAuth: (name: string) => void;
    onToggle: (name: string, on: boolean) => void;
    onUpdate: (name: string, input: MCPServerInput) => void;
    onToggleDetails: (name: string) => void;
    onToggleTools: (name: string) => void;
}) {
    if (servers.length === 0)
        return null;
    return (<div className="cap-server-group">
      {servers.map((s) => (<ServerRow key={s.name} s={s} expanded={expanded.has(s.name)} toolsExpanded={expandedTools.has(s.name)} busy={busy} editing={editing === s.name} onConfirm={() => onConfirm(s.name)} onEdit={() => onEdit(s.name)} onCancelEdit={onCancelEdit} onRetry={() => onRetry(s.name)} onReconnect={() => onReconnect(s.name)} onConfirmClearAuth={() => onConfirmClearAuth(s.name)} onToggle={(on) => onToggle(s.name, on)} onUpdate={(input) => onUpdate(s.name, input)} onToggleDetails={() => onToggleDetails(s.name)} onToggleTools={() => onToggleTools(s.name)}/>))}
    </div>);
}
export function FailedServersNotice({ servers, expanded, busy, onToggle, onRetry, onRetryMany, onConfirmClearAuth, onConfirm, onConfirmMany, }: {
    servers: ServerView[];
    expanded: Set<string>;
    busy: boolean;
    onToggle: (name: string) => void;
    onRetry: (name: string) => void;
    onRetryMany: (names: string[]) => void;
    onConfirmClearAuth: (name: string) => void;
    onConfirm: (name: string) => void;
    onConfirmMany: (names: string[]) => void;
}) {
    const t = useT();
    const [detailsOpen, setDetailsOpen] = useState(false);
    const [bulkOpen, setBulkOpen] = useState(false);
    const groups = useMemo(() => failureGroups(servers, t), [servers, t]);
    const removableFailures = useMemo(() => servers.filter(canBulkRemoveFailure), [servers]);
    const retryNames = useMemo(() => servers.map((s) => s.name), [servers]);
    return (<div className="cap-failures" role="region" aria-label={t("caps.failureTitle", { failed: servers.length })}>
      <div className="cap-failures__head">
        <div>
          <div className="cap-failures__title">{t("caps.failureTitle", { failed: servers.length })}</div>
          <div className="cap-failures__hint">{t("caps.failureHint")}</div>
        </div>
        <div className="cap-failures__actions">
          <button className="btn btn--small" disabled={busy} type="button" onClick={() => setDetailsOpen((v) => !v)} aria-expanded={detailsOpen}>
            {detailsOpen ? t("caps.hideFailureDetails") : t("caps.showFailureDetails")}
          </button>
          <button className="btn btn--small" disabled={busy || retryNames.length === 0} type="button" onClick={() => onRetryMany(retryNames)}>
            {t("caps.retryAll")}
          </button>
          {removableFailures.length > 0 && (<button className="btn btn--small" disabled={busy} type="button" onClick={() => setBulkOpen((v) => !v)} aria-expanded={bulkOpen}>
              {t("caps.bulkActions")}
            </button>)}
        </div>
      </div>
      <div className="cap-failures__meta">
        <div className="cap-failures__chips" aria-label={t("caps.failureGroups")}>
          {groups.map((group) => (<span className="cap-failure-chip" key={group.kind}>{group.label}</span>))}
        </div>
      </div>
      {bulkOpen && removableFailures.length > 0 && (<div className="cap-failures__bulk">
          <InlineConfirmButton label={t("caps.removeInvalid", { count: removableFailures.length })} confirmLabel={t("caps.confirmRemoveInvalid", { count: removableFailures.length })} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={() => onConfirmMany(removableFailures.map((s) => s.name))}/>
        </div>)}
      {detailsOpen && <div className="cap-failures__list">
        {servers.map((s) => {
                const open = expanded.has(s.name);
                const error = s.error || t("caps.failed");
                const actionLabel = serverActionLabel(s, t);
                const handlePrimaryAction = () => {
                    onRetry(s.name);
                };
                return (<div className="cap-failure" key={s.name}>
              <div className="cap-failure__main">
                <span className="cap-dot cap-dot--failed"/>
                <div className="cap-failure__text">
                  <div className="cap-failure__name">{s.name}</div>
                  <div className="cap-failure__summary">{s.authStatus === "required" ? t("caps.authRequiredSummary") : summarizeServerError(error)}</div>
                </div>
              </div>
              <div className="cap-failure__actions">
                <button className="btn btn--small" disabled={busy} onClick={handlePrimaryAction}>
                  {actionLabel}
                </button>
                {canClearAuth(s) && (<InlineConfirmButton label={t("caps.clearAuth")} confirmLabel={t("caps.confirmClearAuth")} cancelLabel={t("common.cancel")} disabled={busy} onConfirm={() => onConfirmClearAuth(s.name)}/>)}
                <button className="btn btn--small" onClick={() => onToggle(s.name)} aria-expanded={open}>
                  {open ? t("common.collapse") : t("caps.showLog")}
                </button>
                {!s.builtIn && !s.managedByPlugin && s.configured && (<InlineConfirmButton label={t("caps.remove")} confirmLabel={t("caps.confirmRemove")} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={() => onConfirm(s.name)}/>)}
              </div>
              {open && (<div className="cap-failure__logbox">
                  <div className="cap-failure__logbar">
                    <span>{t("caps.rawLog")}</span>
                    <button className="btn btn--small" onClick={() => void navigator.clipboard?.writeText(error)}>
                      {t("caps.copyLog")}
                    </button>
                  </div>
                  <pre className="cap-failure__log">{error}</pre>
                </div>)}
            </div>);
            })}
      </div>}
    </div>);
}
function ServerRow({ s, expanded, toolsExpanded, busy, editing, onConfirm, onEdit, onCancelEdit, onRetry, onReconnect, onConfirmClearAuth, onToggle, onUpdate, onToggleDetails, onToggleTools, }: {
    s: ServerView;
    expanded: boolean;
    toolsExpanded: boolean;
    busy: boolean;
    editing: boolean;
    onConfirm: () => void;
    onEdit: () => void;
    onCancelEdit: () => void;
    onRetry: () => void;
    onReconnect: () => void;
    onConfirmClearAuth: () => void;
    onToggle: (on: boolean) => void;
    onUpdate: (input: MCPServerInput) => void;
    onToggleDetails: () => void;
    onToggleTools: () => void;
}) {
    const t = useT();
    const actionLabel = serverActionLabel(s, t);
    const lifecycle = mcpServerLifecycleActions(s);
    const tools = s.toolList ?? [];
    const schemaIssueCount = tools.filter((tool) => tool.schemaError).length;
    let sub = s.status === "failed"
        ? s.error || t("caps.failed")
        : s.status === "initializing"
            ? t("caps.initializing")
            : s.status === "deferred"
                ? t("caps.deferred")
                : s.status === "disabled"
                    ? s.configured && !s.autoStart
                        ? t("caps.disabledAutoStart")
                        : t("caps.disabled")
                    : t("caps.counts", { tools: s.tools, prompts: s.prompts, resources: s.resources });
    if (schemaIssueCount > 0) {
        sub = `${sub} · ${t("caps.schemaIssues", { count: schemaIssueCount })}`;
    }
    if (s.managedByPlugin) {
        sub = `${sub} · ${t("caps.managedByPlugin", { plugin: s.managedByPlugin })}`;
    }
    if (s.authStatus === "possible" && s.status !== "failed") {
        sub = `${sub} · ${t("caps.authPossibleShort")}`;
    }
    const handlePrimaryAction = () => {
        onRetry();
    };
    return (<div className={`cap-server-entry${s.status === "disabled" ? " cap-server-entry--disabled" : ""}`}>
      <Tooltip label={s.error} disabled={!s.error} fill block>
        <div className={`cap-row${s.status === "disabled" ? " cap-row--disabled" : ""}`}>
          <Tooltip label={expanded ? t("caps.collapseDetails") : t("caps.expandDetails")}>
            <button className="cap-disclosure" aria-expanded={expanded} onClick={onToggleDetails}>
              {expanded ? "⌄" : "›"}
            </button>
          </Tooltip>
          <span className={`cap-dot cap-dot--${s.status}`}/>
          <div className="cap-row__text">
            <div className="cap-row__head">
              <span className="cap-row__name">{s.name}</span>
              <span className="cap-row__transport">{s.transport}</span>
              {s.builtIn && <span className="cap-row__builtin">{t("caps.builtIn")}</span>}
            </div>
            <div className="cap-row__sub">{sub}</div>
          </div>
          <div className="cap-row__actions">
            {lifecycle.showRetryInRow ? (<button className="btn btn--small" disabled={busy} onClick={handlePrimaryAction}>
                {actionLabel}
              </button>) : (<Tooltip label={lifecycle.enabled ? t("caps.disable") : t("caps.enable")}>
                <label className="cap-switch">
                  <input type="checkbox" checked={lifecycle.enabled} disabled={busy} onChange={(e) => onToggle(e.target.checked)}/>
                  <span className="cap-switch__track"/>
                </label>
              </Tooltip>)}
          </div>
        </div>
      </Tooltip>
      {expanded && (<ServerDetails s={s} tools={tools} busy={busy} onConfirm={onConfirm} onConnectNow={onRetry} onReconnect={onReconnect} onConfirmClearAuth={onConfirmClearAuth} toolsExpanded={toolsExpanded} editing={editing} onEdit={onEdit} onCancelEdit={onCancelEdit} onUpdate={onUpdate} onToggleTools={onToggleTools}/>)}
    </div>);
}
export function ServerDetails({ s, tools, busy, onConfirm, onConnectNow, onReconnect, onConfirmClearAuth, toolsExpanded, editing, onEdit, onCancelEdit, onUpdate, onToggleTools, standalone = false, showToolsToggle = true, }: {
    s: ServerView;
    tools: ServerView["toolList"];
    busy: boolean;
    onConfirm: () => void;
    onConnectNow: () => void;
    onReconnect: () => void;
    onConfirmClearAuth: () => void;
    toolsExpanded: boolean;
    editing: boolean;
    onEdit: () => void;
    onCancelEdit: () => void;
    onUpdate: (input: MCPServerInput) => void;
    onToggleTools: () => void;
    standalone?: boolean;
    showToolsToggle?: boolean;
}) {
    const t = useT();
    const command = serverCommand(s);
    const canMutateConfig = s.configured && !s.builtIn && !s.managedByPlugin;
    const canEditConfig = canMutateConfig;
    const lifecycle = mcpServerLifecycleActions(s);
    const canConnectNow = lifecycle.canConnectNow;
    const canReconnect = lifecycle.canReconnect;
    const canShowTools = s.status === "connected" && ((s.tools ?? 0) > 0 || (tools?.length ?? 0) > 0);
    const showClearAuth = canMutateConfig && canClearAuth(s);
    const authLabel = serverAuthLabel(s, t);
    if (editing && canEditConfig) {
        return (<div className={`cap-server-details${standalone ? " cap-server-details--page" : ""}`}>
        <EditServerForm s={s} busy={busy} onCancel={onCancelEdit} onSave={onUpdate}/>
      </div>);
    }
    return (<div className={`cap-server-details${standalone ? " cap-server-details--page" : ""}`}>
      <div className="cap-detail-grid">
        <div className="cap-detail">
          <span className="cap-detail__label">{t("caps.status")}</span>
          <span className="cap-detail__value">{serverStatusLabel(s, t)}</span>
        </div>
        {s.source && (<div className="cap-detail">
            <span className="cap-detail__label">{t("caps.serverSource")}</span>
            <span className="cap-detail__value">{mcpServerSourceLabel(s, t)}</span>
          </div>)}
        <div className="cap-detail">
          <span className="cap-detail__label">{t("caps.transport")}</span>
          <span className="cap-detail__value">{s.transport}</span>
        </div>
        {authLabel && (<div className="cap-detail">
            <span className="cap-detail__label">{t("caps.auth")}</span>
            <span className="cap-detail__value">{authLabel}</span>
          </div>)}
        {command && (<div className="cap-detail cap-detail--wide">
            <span className="cap-detail__label">{s.transport === "stdio" ? t("caps.command") : t("caps.url")}</span>
            <span className="cap-detail__code">{command}</span>
          </div>)}
        {s.envKeys && s.envKeys.length > 0 && (<div className="cap-detail cap-detail--wide">
            <span className="cap-detail__label">{t("caps.envKeys")}</span>
            <span className="cap-detail__value">{s.envKeys.join(", ")}</span>
          </div>)}
        {s.headerKeys && s.headerKeys.length > 0 && (<div className="cap-detail cap-detail--wide">
            <span className="cap-detail__label">{t("caps.headerKeys")}</span>
            <span className="cap-detail__value">{s.headerKeys.join(", ")}</span>
          </div>)}
      </div>
      <div className="cap-detail-actions">
        {canConnectNow && (<button className="btn btn--small" disabled={busy} onClick={onConnectNow}>
            {t("caps.connectNow")}
          </button>)}
        {canReconnect && (<button className="btn btn--small" disabled={busy} onClick={onReconnect}>
            {t("caps.reconnect")}
          </button>)}
        {canShowTools && showToolsToggle && (<button className="btn btn--small" disabled={busy} onClick={onToggleTools} aria-expanded={toolsExpanded}>
            {toolsExpanded ? t("caps.hideTools") : t("caps.showTools")}
          </button>)}
        {showClearAuth && (<InlineConfirmButton label={t("caps.clearAuth")} confirmLabel={t("caps.confirmClearAuth")} cancelLabel={t("common.cancel")} disabled={busy} onConfirm={onConfirmClearAuth}/>)}
        {canEditConfig && (<>
            <button className="btn btn--small" disabled={busy} onClick={onEdit}>
              {t("caps.editConfig")}
            </button>
            <InlineConfirmButton label={t("caps.remove")} confirmLabel={t("caps.confirmRemove")} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={onConfirm}/>
          </>)}
      </div>
      {toolsExpanded && (tools && tools.length > 0 ? (<div className="cap-tool-list">
            <div className="cap-tool-list__title">{t("caps.tools")}</div>
            {tools.map((tool) => {
                const unavailable = Boolean(tool.schemaError);
                return (<div className={`cap-tool${unavailable ? " cap-tool--unavailable" : ""}`} key={tool.name}>
                  <div className="cap-tool__name">{tool.name}</div>
                  <div className="cap-tool__desc">
                    <span>{unavailable ? tool.schemaError : tool.description}</span>
                    {unavailable ? (<span className="cap-tool-hint cap-tool-hint--error" title={tool.schemaError}>
                        <CircleAlert aria-hidden size={11} strokeWidth={2.2}/>
                        {t("caps.toolUnavailable")}
                      </span>) : null}
                  </div>
                </div>);
            })}
          </div>) : (<div className="cap-tool-empty">{t("caps.noToolDetails")}</div>))}
    </div>);
}
function EditServerForm({ s, busy, onCancel, onSave, }: {
    s: ServerView;
    busy: boolean;
    onCancel: () => void;
    onSave: (input: MCPServerInput) => void;
}) {
    const t = useT();
    const initialTransport = normalizeTransportValue(s.transport);
    const [transport, setTransport] = useState(initialTransport);
    const [command, setCommand] = useState(initialTransport === "stdio" ? serverCommand(s) : "");
    const [url, setUrl] = useState(initialTransport === "stdio" ? "" : s.url || serverCommand(s));
    const [headers, setHeaders] = useState("");
    const [env, setEnv] = useState("");
    const isStdio = transport === "stdio";
    const ready = isStdio ? command.trim() !== "" : url.trim() !== "";
    const submit = () => {
        const envText = env.trim();
        const headerText = headers.trim();
        onSave({
            name: s.name,
            transport,
            command: isStdio ? command.trim() : "",
            args: [],
            url: isStdio ? "" : url.trim(),
            env: envText === "" ? null : parseKeyValueText(envText),
            headers: isStdio || headerText === "" ? null : parseKeyValueText(headerText),
        });
    };
    return (<div className="cap-config-edit">
      <div className="cap-detail-grid">
        <div className="cap-detail">
          <span className="cap-detail__label">{t("caps.name")}</span>
          <span className="cap-detail__value">{s.name}</span>
        </div>
        <label className="cap-detail cap-detail--select">
          <span className="cap-detail__label">{t("caps.transport")}</span>
          <select className="mem-select" value={transport} disabled={busy} onChange={(e) => setTransport(e.target.value)}>
            <option value="stdio">stdio</option>
            <option value="http">http</option>
            <option value="sse">sse</option>
          </select>
        </label>
        {isStdio ? (<label className="cap-detail cap-detail--wide">
            <span className="cap-detail__label">{t("caps.command")}</span>
            <input className="mem-input" value={command} disabled={busy} onChange={(e) => setCommand(e.target.value)} placeholder={t("caps.commandPlaceholder")}/>
          </label>) : (<label className="cap-detail cap-detail--wide">
            <span className="cap-detail__label">{t("caps.url")}</span>
            <input className="mem-input" value={url} disabled={busy} onChange={(e) => setUrl(e.target.value)} placeholder={t("caps.urlPlaceholder")}/>
          </label>)}
        {!isStdio && (<label className="cap-detail cap-detail--wide">
            <span className="cap-detail__label">{t("caps.headersLabel")}</span>
            <textarea className="mem-textarea cap-config-edit__env" value={headers} disabled={busy} onChange={(e) => setHeaders(e.target.value)} placeholder={t("caps.headersPlaceholder")} spellCheck={false}/>
          </label>)}
        {!isStdio && s.headerKeys && s.headerKeys.length > 0 && (<div className="cap-detail cap-detail--wide">
            <span className="cap-detail__label">{t("caps.headerKeys")}</span>
            <span className="cap-detail__value">{s.headerKeys.join(", ")}</span>
            <span className="cap-edit-hint">{t("caps.headersPreserveHint")}</span>
          </div>)}
        <label className="cap-detail cap-detail--wide">
          <span className="cap-detail__label">{t("caps.envLabel")}</span>
          <textarea className="mem-textarea cap-config-edit__env" value={env} disabled={busy} onChange={(e) => setEnv(e.target.value)} placeholder={t("caps.envPlaceholder")} spellCheck={false}/>
        </label>
        {s.envKeys && s.envKeys.length > 0 && (<div className="cap-detail cap-detail--wide">
            <span className="cap-detail__label">{t("caps.envKeys")}</span>
            <span className="cap-detail__value">{s.envKeys.join(", ")}</span>
            <span className="cap-edit-hint">{t("caps.envPreserveHint")}</span>
          </div>)}
      </div>
      <div className="cap-detail-actions">
        <button className="btn btn--small" disabled={busy} onClick={onCancel}>
          {t("common.cancel")}
        </button>
        <button className="btn btn--primary btn--small" disabled={busy || !ready} onClick={submit}>
          {t("caps.saveConfig")}
        </button>
      </div>
    </div>);
}
export function serverCommand(s: ServerView): string {
    if (s.transport === "stdio")
        return [s.command, ...(s.args ?? [])].filter(Boolean).join(" ").trim();
    return (s.url || "").trim();
}
export function normalizeTransportValue(transport: string): string {
    const value = transport.trim().toLowerCase();
    if (value === "http" || value === "streamable-http")
        return "http";
    if (value === "sse")
        return "sse";
    if (value === "" || value === "stdio")
        return "stdio";
    return value;
}
export function parseKeyValueText(text: string): Record<string, string> {
    const values: Record<string, string> = {};
    for (const rawLine of text.split("\n")) {
        const line = rawLine.trim();
        if (!line)
            continue;
        const eq = line.indexOf("=");
        if (eq > 0)
            values[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
    }
    return values;
}
export function serverStatusLabel(s: ServerView, t: ReturnType<typeof useT>): string {
    // Prefer product availability so idle enabled servers are not shown as disconnected.
    const availability = s.availability
        || (s.enabled === false || s.status === "disabled"
            ? "disabled"
            : s.status === "connected"
                ? "connected"
                : s.status === "initializing"
                    ? "starting"
                    : s.status === "failed"
                        ? (s.authStatus === "required" ? "auth_required" : "start_failed")
                        : s.status === "deferred"
                            ? "available_on_demand"
                            : s.status);
    switch (availability) {
        case "connected":
            return t("caps.connected");
        case "available_on_demand":
            return t("caps.deferred");
        case "starting":
            return t("caps.initializing");
        case "disabled":
            return t("caps.disabled");
        case "auth_required":
            return t("caps.authRequired");
        case "project_auth_changed":
            return t("caps.projectAuthChanged");
        case "start_failed":
            return t("caps.failed");
        default:
            switch (s.status) {
                case "connected":
                    return t("caps.connected");
                case "deferred":
                    return t("caps.deferred");
                case "initializing":
                    return t("caps.initializing");
                case "disabled":
                    return t("caps.disabled");
                case "failed":
                    if (s.authStatus === "required")
                        return t("caps.authRequired");
                    return t("caps.failed");
                default:
                    return s.status;
            }
    }
}
export function summarizeServerError(error: string): string {
    const normalized = error.replace(/\s+/g, " ").trim();
    const plugin = normalized.match(/plugin "([^"]+)"/i)?.[1];
    const npmCode = normalized.match(/\bnpm (?:error|ERR!) code ([A-Z0-9_]+)/i)?.[1];
    const errno = normalized.match(/\berrno (-?\d+)/i)?.[1];
    const networkContext = npmCode ? npmNetworkContext(normalized, npmCode) : "";
    const reason = npmCode
        ? `npm ${npmCode}${errno ? ` (${errno})` : ""}${networkContext}`
        : normalized.split(/(?:\.\s+|\n)/)[0];
    const summary = plugin ? `${plugin}: ${reason}` : reason;
    return summary.length > 180 ? `${summary.slice(0, 176).trim()}…` : summary;
}
function npmNetworkContext(error: string, code: string): string {
    if (!/^(?:ECONNREFUSED|ECONNRESET|ENETUNREACH|ETIMEDOUT|EAI_AGAIN|ENOTFOUND)$/i.test(code))
        return "";
    let registry = "";
    const requestURL = error.match(/\brequest to (https?:\/\/[^\s]+)/i)?.[1]?.replace(/[),.;]+$/, "");
    if (requestURL) {
        try {
            registry = new URL(requestURL).host;
        }
        catch {
            registry = "";
        }
    }
    let endpoint = error.match(/\b(?:connect\s+)?(?:ECONNREFUSED|ECONNRESET|ENETUNREACH|ETIMEDOUT|EAI_AGAIN|ENOTFOUND)\s+((?:\[[0-9a-f:]+\]|[a-z0-9._-]+):\d{1,5})\b/i)?.[1] ?? "";
    if (!endpoint) {
        const address = error.match(/\baddress\s+([^\s,;]+)/i)?.[1];
        const port = error.match(/\bport\s+(\d{1,5})\b/i)?.[1];
        if (address && port)
            endpoint = `${address}:${port}`;
    }
    if (registry && endpoint && registry.toLowerCase() !== endpoint.toLowerCase())
        return ` · ${registry} → ${endpoint}`;
    if (registry || endpoint)
        return ` · ${registry || endpoint}`;
    return "";
}
export type FailureKind = "auth" | "missing-command" | "command-unavailable" | "network" | "other";
export function failureKind(server: ServerView): FailureKind {
    if (server.authStatus === "required")
        return "auth";
    const err = (server.error || "").toLowerCase();
    if (err.includes("command is required"))
        return "missing-command";
    if (err.includes("command not found") ||
        err.includes("executable file not found") ||
        err.includes("no such file") ||
        err.includes("enoent")) {
        return "command-unavailable";
    }
    if (err.includes("401") ||
        err.includes("403") ||
        err.includes("unauthorized") ||
        err.includes("forbidden") ||
        err.includes("timeout") ||
        err.includes("network") ||
        err.includes("econnrefused") ||
        err.includes("econnreset") ||
        err.includes("enetunreach") ||
        err.includes("etimedout") ||
        err.includes("eai_again") ||
        err.includes("enotfound")) {
        return "network";
    }
    return "other";
}
function failureGroups(servers: ServerView[], t: ReturnType<typeof useT>): Array<{
    kind: FailureKind;
    label: string;
}> {
    const counts = new Map<FailureKind, number>();
    for (const server of servers) {
        const kind = failureKind(server);
        counts.set(kind, (counts.get(kind) ?? 0) + 1);
    }
    const order: FailureKind[] = ["missing-command", "command-unavailable", "auth", "network", "other"];
    return order.flatMap((kind) => {
        const count = counts.get(kind) ?? 0;
        if (count === 0)
            return [];
        return [{ kind, label: failureGroupLabel(kind, count, t) }];
    });
}
function failureGroupLabel(kind: FailureKind, count: number, t: ReturnType<typeof useT>): string {
    switch (kind) {
        case "auth":
            return t("caps.failureGroupAuth", { count });
        case "missing-command":
            return t("caps.failureGroupMissingCommand", { count });
        case "command-unavailable":
            return t("caps.failureGroupCommandUnavailable", { count });
        case "network":
            return t("caps.failureGroupNetwork", { count });
        default:
            return t("caps.failureGroupOther", { count });
    }
}
function canBulkRemoveFailure(server: ServerView): boolean {
    if (server.builtIn || server.managedByPlugin || !server.configured)
        return false;
    const kind = failureKind(server);
    return kind === "missing-command" || kind === "command-unavailable";
}
export function retryableAvailableServerNames(servers: ServerView[]): string[] {
    return servers.filter(mcpServerRetryableFromAvailableList).map((s) => s.name);
}
export function serverActionLabel(s: ServerView, t: ReturnType<typeof useT>): string {
    const err = (s.error || "").toLowerCase();
    if (shouldOpenAuth(s))
        return t("caps.reauthorize");
    if (err.includes("command not found") ||
        err.includes("executable file not found") ||
        err.includes("no such file") ||
        err.includes("enoent")) {
        return t("caps.checkCommand");
    }
    return t("caps.retry");
}
function serverAuthLabel(s: ServerView, t: ReturnType<typeof useT>): string {
    if (s.authStatus === "required")
        return t("caps.authRequired");
    if (s.authStatus === "possible")
        return t("caps.authPossible");
    return "";
}
export function shouldOpenAuth(s: ServerView): boolean {
    return s.authStatus === "required" && canUseNativeMCPOAuth(s);
}
function canClearAuth(s: ServerView): boolean {
    if (!s.configured || s.builtIn || s.managedByPlugin)
        return false;
    return Boolean(s.authConfigured || s.authStatus === "required" || s.authStatus === "possible" || isRemoteTransport(s.transport));
}
function isRemoteTransport(transport?: string): boolean {
    const value = (transport || "").trim().toLowerCase();
    return value === "http" || value === "streamable-http" || value === "sse";
}

