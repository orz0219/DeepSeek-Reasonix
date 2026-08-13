import { MessageSquare, Settings as SettingsIcon } from "lucide-react";
import { asArray } from "./lib/array";
import { useT, type Translator } from "./lib/i18n";
import { CopyButton } from "./components/CopyButton";
import { type BotConnectionView, type BotRuntimeStatusView, type BotSettingsView, type SessionMeta } from "./lib/types";
type SidebarImPlatform = "qq" | "feishu" | "lark" | "weixin";
type SidebarImStatus = "connected" | "disabled" | "pending" | "error" | "disconnected";
export type SidebarImConnection = {
    id: string;
    connectionId: string;
    platform: SidebarImPlatform;
    title: string;
    platformLabel: string;
    subtitle: string;
    status: SidebarImStatus;
    statusLabel: string;
    remoteId: string;
    sessionId: string;
    sessionSource: string;
    scope: "global" | "project";
    workspaceRoot: string;
    allowAll: boolean;
    allowlistEnabled: boolean;
    allowlistUsers: string[];
    allowlistMatched: boolean;
};
export type SidebarImTopicSource = {
    platform: SidebarImPlatform;
    label: string;
    title: string;
    remoteId: string;
    connectionId: string;
};
type SidebarImConnectionDetailProps = {
    connection: SidebarImConnection;
    onClose: () => void;
    onOpenSession: () => void;
    onOpenSettings: () => void;
    onManageAllowlist: () => void;
};
function isSidebarImConnection(connection: BotConnectionView): boolean {
    return connection.provider === "feishu" || connection.provider === "weixin";
}
function sidebarImPlatform(connection: BotConnectionView): SidebarImPlatform {
    if (connection.provider === "weixin")
        return "weixin";
    return connection.domain === "lark" ? "lark" : "feishu";
}
function sidebarImPlatformLabel(platform: SidebarImPlatform, translate: Translator): string {
    if (platform === "qq")
        return "QQ";
    if (platform === "lark")
        return "Lark";
    if (platform === "weixin")
        return translate("settings.botWeixin");
    return translate("settings.botFeishu");
}
function botMappingScope(mapping: BotConnectionView["sessionMappings"][number] | null | undefined, connectionWorkspaceRoot: string): "global" | "project" {
    if (mapping?.scope === "project")
        return "project";
    if ((mapping?.workspaceRoot ?? "").trim())
        return "project";
    return connectionWorkspaceRoot.trim() ? "project" : "global";
}
function botMappingWorkspaceRoot(mapping: BotConnectionView["sessionMappings"][number] | null | undefined, connectionWorkspaceRoot: string): string {
    const workspaceRoot = (mapping?.workspaceRoot ?? "").trim() || connectionWorkspaceRoot.trim();
    return botMappingScope(mapping, connectionWorkspaceRoot) === "project" ? workspaceRoot : "";
}
function compactRemoteId(value: string): string {
    const trimmed = value.trim();
    if (trimmed.length <= 28)
        return trimmed;
    return `${trimmed.slice(0, 12)}…${trimmed.slice(-8)}`;
}
function botMappingIdentityLabel(mapping: BotConnectionView["sessionMappings"][number] | null | undefined): string {
    const chatType = (mapping?.chatType ?? "").trim();
    const userId = (mapping?.userId ?? "").trim();
    const threadId = (mapping?.threadId ?? "").trim();
    if (threadId)
        return compactRemoteId(threadId);
    if ((chatType === "group" || chatType === "guild") && userId)
        return compactRemoteId(userId);
    return "";
}
function sidebarImStatus(connection: BotConnectionView, botEnabled: boolean): SidebarImStatus {
    if (!botEnabled || !connection.enabled)
        return "disabled";
    if (connection.status === "connected")
        return "connected";
    if (connection.status === "pending")
        return "pending";
    if (connection.status === "error")
        return "error";
    return "disconnected";
}
function sidebarImStatusLabel(status: SidebarImStatus, translate: Translator): string {
    switch (status) {
        case "connected":
            return translate("sidebar.imConnected");
        case "disabled":
            return translate("sidebar.imDisabled");
        case "pending":
            return translate("sidebar.imPending");
        case "error":
            return translate("sidebar.imError");
        default:
            return translate("sidebar.imDisconnected");
    }
}
function uniqueTrimmedValues(values: string[]): string[] {
    return Array.from(new Set(values.map((value) => value.trim()).filter(Boolean)));
}
function sidebarImAllowlistUsers(bot: BotSettingsView, platform: SidebarImPlatform): string[] {
    if (platform === "qq")
        return uniqueTrimmedValues(asArray(bot.allowlist.qqUsers));
    if (platform === "weixin")
        return uniqueTrimmedValues(asArray(bot.allowlist.weixinUsers));
    return uniqueTrimmedValues(asArray(bot.allowlist.feishuUsers));
}
function sidebarImQQAdded(qq: BotSettingsView["qq"]): boolean {
    return Boolean(qq.enabled || qq.secretSet || qq.appId.trim());
}
function sidebarImQQStatus(bot: BotSettingsView, runtimeStatus: BotRuntimeStatusView | null | undefined): SidebarImStatus {
    const appId = bot.qq.appId.trim();
    if (!bot.enabled || !bot.qq.enabled)
        return "disabled";
    if (!appId || !bot.qq.secretSet)
        return "disconnected";
    if (typeof window !== "undefined" && !window.runtime)
        return "pending";
    if (!runtimeStatus)
        return "pending";
    const status = runtimeStatus.status.trim().toLowerCase();
    if (runtimeStatus.running && runtimeStatus.connections > 0 && status === "running") {
        return "connected";
    }
    if (status === "error" || status === "blocked" || status === "degraded")
        return "error";
    if (status === "stopped")
        return "disconnected";
    return "pending";
}
function sidebarImQQConnection(bot: BotSettingsView, translate: Translator, runtimeStatus?: BotRuntimeStatusView | null): SidebarImConnection | null {
    if (!sidebarImQQAdded(bot.qq))
        return null;
    const remoteId = bot.qq.appId.trim();
    const status = sidebarImQQStatus(bot, runtimeStatus);
    const statusLabel = sidebarImStatusLabel(status, translate);
    const allowlistUsers = sidebarImAllowlistUsers(bot, "qq");
    const subtitleParts = [
        remoteId ? compactRemoteId(remoteId) : "QQ",
        statusLabel,
    ].filter(Boolean);
    return {
        id: "__qq_bot__",
        connectionId: "__qq_bot__",
        platform: "qq",
        title: "QQ Bot",
        platformLabel: "QQ",
        subtitle: subtitleParts.join(" · "),
        status,
        statusLabel,
        remoteId,
        sessionId: "",
        sessionSource: "",
        scope: "global",
        workspaceRoot: "",
        allowAll: bot.allowlist.allowAll,
        allowlistEnabled: bot.allowlist.enabled,
        allowlistUsers,
        allowlistMatched: remoteId ? allowlistUsers.includes(remoteId) : false,
    };
}
export function sidebarImConnectionsFromBot(bot: BotSettingsView | null | undefined, translate: Translator, runtimeStatus?: BotRuntimeStatusView | null): SidebarImConnection[] {
    if (!bot)
        return [];
    const qqConnection = sidebarImQQConnection(bot, translate, runtimeStatus);
    const connectionItems: SidebarImConnection[] = [];
    for (const connection of asArray(bot.connections)) {
        if (!isSidebarImConnection(connection))
            continue;
        const mappings = connection.sessionMappings.filter((mapping) => mapping.sessionId.trim() || mapping.remoteId.trim());
        const rowMappings = mappings.length > 0 ? mappings : [null];
        rowMappings.forEach((mapping, index) => {
            const platform = sidebarImPlatform(connection);
            const platformLabel = sidebarImPlatformLabel(platform, translate);
            const remoteId = mapping?.remoteId.trim() ?? "";
            const sessionId = mapping?.sessionId.trim() ?? "";
            const sessionSource = mapping?.sessionSource.trim() ?? "";
            const scope = botMappingScope(mapping, connection.workspaceRoot);
            const workspaceRoot = botMappingWorkspaceRoot(mapping, connection.workspaceRoot);
            const status = sidebarImStatus(connection, bot.enabled);
            const title = connection.label.trim() || platformLabel;
            const allowlistUsers = sidebarImAllowlistUsers(bot, platform);
            const identityLabel = botMappingIdentityLabel(mapping);
            const mappedUserId = mapping?.userId.trim() ?? "";
            const subtitleParts = [
                remoteId ? compactRemoteId(remoteId) : platformLabel,
                identityLabel,
                connection.model.trim() || "",
                sidebarImStatusLabel(status, translate),
            ].filter(Boolean);
            connectionItems.push({
                id: mapping ? `${connection.id}:mapping:${index}` : connection.id,
                connectionId: connection.id,
                platform,
                title,
                platformLabel,
                subtitle: subtitleParts.join(" · "),
                status,
                statusLabel: sidebarImStatusLabel(status, translate),
                remoteId,
                sessionId,
                sessionSource,
                scope,
                workspaceRoot,
                allowAll: bot.allowlist.allowAll,
                allowlistEnabled: bot.allowlist.enabled,
                allowlistUsers,
                allowlistMatched: remoteId
                    ? allowlistUsers.includes(remoteId) || (mappedUserId ? allowlistUsers.includes(mappedUserId) : false)
                    : false,
            });
        });
    }
    return qqConnection ? [qqConnection, ...connectionItems] : connectionItems;
}
function mappedSessionTarget(sessionId: string): {
    kind: "path" | "topic";
    value: string;
} | null {
    const trimmed = sessionId.trim();
    if (!trimmed)
        return null;
    const lower = trimmed.toLowerCase();
    if (lower.startsWith("path:")) {
        const value = trimmed.slice(5).trim();
        return value ? { kind: "path", value } : null;
    }
    if (lower.startsWith("topic:")) {
        const value = trimmed.slice(6).trim();
        return value ? { kind: "topic", value } : null;
    }
    if (trimmed.endsWith(".jsonl") || trimmed.includes("/") || trimmed.includes("\\") || trimmed.startsWith("~")) {
        return { kind: "path", value: trimmed };
    }
    return { kind: "topic", value: trimmed };
}
export function taskSessionIDFromPath(path: string): string {
    const base = path.replace(/\\/g, "/").split("/").pop() || "";
    const extension = base.lastIndexOf(".");
    return extension > 0 ? base.slice(0, extension) : base;
}
export function sidebarImSessionTarget(connection: SidebarImConnection): {
    kind: "path" | "topic";
    value: string;
} | null {
    return mappedSessionTarget(connection.sessionId);
}
export function isChannelSession(session: SessionMeta): boolean {
    return session.kind === "channel" || session.sessionSource === "auto";
}
export function sidebarImTopicSourcesFromBot(bot: BotSettingsView | null | undefined, translate: Translator): Record<string, SidebarImTopicSource> {
    if (!bot?.connections?.length)
        return {};
    const sources: Record<string, SidebarImTopicSource> = {};
    for (const connection of bot.connections) {
        if (!isSidebarImConnection(connection))
            continue;
        const platform = sidebarImPlatform(connection);
        const label = sidebarImPlatformLabel(platform, translate);
        const title = connection.label.trim() || label;
        for (const mapping of asArray(connection.sessionMappings)) {
            const scope = botMappingScope(mapping, connection.workspaceRoot);
            if (scope !== "global")
                continue;
            const target = mappedSessionTarget(mapping.sessionId);
            if (!target || target.kind !== "topic")
                continue;
            if (sources[target.value])
                continue;
            sources[target.value] = {
                platform,
                label,
                title,
                remoteId: mapping.remoteId.trim(),
                connectionId: connection.id,
            };
        }
    }
    return sources;
}
export function sidebarImScopeLabel(connection: SidebarImConnection, translate: Translator): string {
    if (connection.scope === "project")
        return translate("botDetail.scopeProject", { name: connection.workspaceRoot || "Project" });
    return translate("botDetail.scopeGlobal");
}
function sidebarImSessionLabel(connection: SidebarImConnection, translate: Translator): string {
    const target = sidebarImSessionTarget(connection);
    if (!target) {
        return connection.remoteId ? translate("botDetail.readOnlyChannel") : translate("botDetail.noSession");
    }
    if (connection.sessionSource === "auto")
        return translate("botDetail.readOnlyChannel");
    if (target.kind === "path")
        return target.value.split(/[\\/]/).pop() || target.value;
    return target.value;
}
function sidebarImAccessModeLabel(connection: SidebarImConnection, translate: Translator): string {
    if (connection.allowAll)
        return translate("botDetail.accessAllowAll");
    if (connection.allowlistEnabled)
        return translate("botDetail.accessWhitelist");
    return translate("botDetail.accessDisabled");
}
function sidebarImAccessStatusLabel(connection: SidebarImConnection, translate: Translator): string {
    if (connection.allowAll)
        return translate("botDetail.accessOpen");
    if (!connection.remoteId)
        return translate("botDetail.accessUnknown");
    return connection.allowlistMatched ? translate("botDetail.accessMatched") : translate("botDetail.accessMissing");
}
function sidebarImAccessStatusClass(connection: SidebarImConnection): string {
    if (connection.allowAll || connection.allowlistMatched)
        return "ok";
    if (!connection.remoteId)
        return "muted";
    return "warn";
}
export function SidebarImConnectionDetail({ connection, onClose, onOpenSession, onOpenSettings, onManageAllowlist }: SidebarImConnectionDetailProps) {
    const translate = useT();
    const target = sidebarImSessionTarget(connection);
    const accessStatusClass = sidebarImAccessStatusClass(connection);
    return (<div className="bot-detail">
      <section className="bot-detail__summary">
        <div className={`bot-detail__avatar bot-detail__avatar--${connection.platform}`} aria-hidden="true">
          {connection.platform === "qq" ? "Q" : connection.platform === "weixin" ? "微" : connection.platform === "lark" ? "L" : "飞"}
        </div>
        <div className="bot-detail__summary-main">
          <span>{translate("botDetail.subtitle")}</span>
          <h2>{connection.title}</h2>
          <div className="bot-detail__chips">
            <span>{connection.platformLabel}</span>
            <span>{connection.statusLabel}</span>
            <span>{sidebarImScopeLabel(connection, translate)}</span>
          </div>
        </div>
        <div className="bot-detail__summary-actions">
          <button type="button" className="btn btn--primary btn--small bot-detail__primary" disabled={!target} title={target ? undefined : translate("botDetail.openDisabled")} onClick={onOpenSession}>
            <MessageSquare size={14}/>
            {translate("botDetail.openSession")}
          </button>
          <button type="button" className="btn btn--secondary btn--small" onClick={onOpenSettings}>
            <SettingsIcon size={14}/>
            {translate("botDetail.manage")}
          </button>
          <button type="button" className="btn btn--secondary btn--small" onClick={onClose}>
            {translate("botDetail.close")}
          </button>
        </div>
      </section>

      <section className="bot-detail__panel bot-detail__panel--access" aria-label={translate("botDetail.access")}>
        <div className="bot-detail__section-head">
          <span>{translate("botDetail.access")}</span>
          <div className="bot-detail__section-actions">
            {connection.remoteId ? (<CopyButton text={connection.remoteId} label={translate("botDetail.copyRemoteId")}/>) : null}
            <button type="button" className="btn btn--secondary btn--small" onClick={onManageAllowlist}>
              {translate("botDetail.manageAllowlist")}
            </button>
          </div>
        </div>
        <div className="bot-detail__access-grid">
          <div>
            <span>{translate("botDetail.accessMode")}</span>
            <strong>{sidebarImAccessModeLabel(connection, translate)}</strong>
          </div>
          <div>
            <span>{translate("botDetail.accessCurrentUser")}</span>
            <code title={connection.remoteId || undefined}>{connection.remoteId || "—"}</code>
          </div>
          <div>
            <span>{translate("botDetail.accessStatus")}</span>
            <strong className={`bot-detail__access-status bot-detail__access-status--${accessStatusClass}`}>
              {sidebarImAccessStatusLabel(connection, translate)}
            </strong>
          </div>
        </div>
        <div className="bot-detail__allowlist">
          <span>{translate("botDetail.channelAllowlistUsers")}</span>
          <div className="bot-detail__id-list">
            {connection.allowlistUsers.length > 0 ? (connection.allowlistUsers.map((id) => (<code key={id} className={id === connection.remoteId ? "bot-detail__id-list-item--active" : ""} title={id}>
                  {id}
                </code>))) : (<em>{translate("botDetail.emptyAllowlistUsers")}</em>)}
          </div>
        </div>
      </section>

      <section className="bot-detail__panel bot-detail__panel--facts" aria-label={translate("botDetail.summary")}>
        <div className="bot-detail__section-head">
          <span>{translate("botDetail.summary")}</span>
        </div>
        <div className="bot-detail__facts">
          <div>
            <span>{translate("botDetail.remoteId")}</span>
            <code>{connection.remoteId || "—"}</code>
          </div>
          <div>
            <span>{translate("botDetail.localTopic")}</span>
            <strong>{sidebarImSessionLabel(connection, translate)}</strong>
          </div>
          <div>
            <span>{translate("botDetail.scope")}</span>
            <strong>{sidebarImScopeLabel(connection, translate)}</strong>
          </div>
        </div>
      </section>
    </div>);
}

