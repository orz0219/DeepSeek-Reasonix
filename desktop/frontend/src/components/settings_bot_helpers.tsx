import { asArray } from "../lib/array";
import { useT } from "../lib/i18n";
import type { BotAccessView, BotConnectionDiagnostic, BotConnectionView, BotInstallStartResult, BotSettingsView } from "../lib/types";
import { normalizeBotQueueMode, normalizeBotQueueDrop, normalizeBotRoute, botRouteHasValue, normalizeBotAccess, normalizeBotSettings, normalizeBotConnection, normalizeBotToolApprovalMode } from "./settings_normalize";
import { uniqueStrings } from "./SettingsPanel";
export type BotInstallTarget = "qq" | "feishu" | "lark" | "weixin";
export type BotOfficialInstallTarget = Exclude<BotInstallTarget, "qq">;
const BOT_ALLOWLIST_TEXT_KEYS = [
    "qqUsers",
    "feishuUsers",
    "weixinUsers",
    "qqApprovers",
    "feishuApprovers",
    "weixinApprovers",
    "qqAdmins",
    "feishuAdmins",
    "weixinAdmins",
    "qqGroups",
    "feishuGroups",
    "weixinGroups",
] as const;
export type BotAllowlistTextKey = typeof BOT_ALLOWLIST_TEXT_KEYS[number];
export type BotSelfUserTextKey = keyof BotSettingsView["selfUserIds"];
export type BotInstallState = {
    target: BotInstallTarget | "";
    result: BotInstallStartResult | null;
    status: "idle" | "starting" | "showing" | "connected" | "error";
    timeLeft: number;
    message: string;
};
export const BOT_INSTALL_TARGETS: BotInstallTarget[] = ["qq", "feishu", "lark", "weixin"];
export const BOT_INSTALL_DEFAULT_TIMEOUT_SECONDS = 300;
export const BOT_INSTALL_MIN_POLL_SECONDS = 3;
export const DEFAULT_QQ_SECRET_ENV = "QQ_BOT_APP_SECRET";
export const QQ_CONNECTION_ID = "__qq_bot__";
export const BOT_PLATFORM_KEYS = ["qq", "feishu", "weixin"] as const;
export type BotPlatformKey = typeof BOT_PLATFORM_KEYS[number];
export const BOT_ALLOWLIST_ROLES = ["Users", "Groups", "Approvers", "Admins"] as const;
type BotAllowlistRole = typeof BOT_ALLOWLIST_ROLES[number];
export type BotAccessListField = "users" | "groups" | "approvers" | "admins";
export function botAllowlistKey(platform: BotPlatformKey, role: BotAllowlistRole): BotAllowlistTextKey {
    return `${platform}${role}`;
}
export function botConnectionPlatform(connection: BotConnectionView): BotPlatformKey {
    if (connection.provider === "weixin")
        return "weixin";
    if (connection.provider === "qq")
        return "qq";
    return "feishu";
}
export function botPlatformLabel(platform: BotPlatformKey, t: ReturnType<typeof useT>): string {
    if (platform === "qq")
        return "QQ";
    if (platform === "weixin")
        return t("settings.botWeixin");
    return t("settings.botPlatformFeishuLark");
}
export function diagnosticMessage(diag?: BotConnectionDiagnostic | string): string {
    if (typeof diag === "string")
        return diag;
    return diag?.message || diag?.status || "";
}
export function diagnosticReportDetail(diag?: BotConnectionDiagnostic | string): string {
    if (typeof diag === "string")
        return "";
    return diag?.reportDetail || "";
}
export function botTargetLabel(target: BotInstallTarget, t: ReturnType<typeof useT>): string {
    switch (target) {
        case "qq": return "QQ";
        case "lark": return "Lark";
        case "weixin": return t("settings.botWeixin");
        default: return t("settings.botFeishu");
    }
}
export function botTargetHint(target: BotInstallTarget, t: ReturnType<typeof useT>): string {
    switch (target) {
        case "qq": return t("settings.botInstallQQHint");
        case "lark": return t("settings.botInstallLarkHint");
        case "weixin": return t("settings.botInstallWeixinHint");
        default: return t("settings.botInstallFeishuHint");
    }
}
export function qqBotAdded(qq: BotSettingsView["qq"]): boolean {
    return Boolean(qq.enabled || qq.secretSet || qq.appId.trim());
}
export function botAccessEntryCount(access: BotAccessView): number {
    return [
        ...asArray(access.users),
        ...asArray(access.groups),
        ...asArray(access.approvers),
        ...asArray(access.admins),
    ].filter((value) => value.trim()).length;
}
export function botAccessReady(access: BotAccessView): boolean {
    if (access.allowAll || access.pairingEnabled)
        return true;
    if (!access.enabled)
        return false;
    return botAccessEntryCount(access) > 0;
}
export function botInstallTargetMatchesConnection(target: BotOfficialInstallTarget, connection: BotConnectionView): boolean {
    if (target === "weixin")
        return connection.provider === "weixin";
    if (target === "lark")
        return connection.provider === "feishu" && connection.domain === "lark";
    return connection.provider === "feishu" && connection.domain !== "lark";
}
export function botInstallTargetForConnection(connection: BotConnectionView): BotInstallTarget {
    if (connection.provider === "weixin")
        return "weixin";
    if (connection.provider === "feishu" && connection.domain === "lark")
        return "lark";
    if (connection.provider === "qq")
        return "qq";
    return "feishu";
}
export function formatInstallUserCode(code: string): string {
    const compact = code.replace(/[^a-z0-9]/gi, "").toUpperCase().slice(0, 8);
    if (compact.length <= 4)
        return compact;
    return `${compact.slice(0, 4)}-${compact.slice(4)}`;
}
export function formatInstallTimeLeft(seconds: number): string {
    const value = Math.max(0, Math.floor(seconds));
    const minutes = Math.floor(value / 60);
    const rest = value % 60;
    return `${minutes}:${String(rest).padStart(2, "0")}`;
}
export function botConnectionLabel(connection: BotConnectionView, t: ReturnType<typeof useT>): string {
    if (connection.domain === "lark")
        return "Lark";
    if (connection.provider === "weixin")
        return t("settings.botWeixin");
    if (connection.provider === "qq")
        return "QQ";
    return t("settings.botFeishu");
}
export function firstConnectionRemote(connection: BotConnectionView): string {
    return connection.sessionMappings.find((mapping) => mapping.remoteId.trim())?.remoteId ?? "";
}
export function botConnectionScopeLabel(connection: BotConnectionView, t: ReturnType<typeof useT>): string {
    return connection.workspaceRoot.trim() ? t("settings.botScopeProject") : t("settings.botScopeGlobal");
}
export function botConnectionSecretEnv(connection: BotConnectionView): string {
    return connection.provider === "weixin" ? connection.credential.tokenEnv : connection.credential.appSecretEnv;
}
export function botConnectionSecretPatch(connection: BotConnectionView, value: string): Partial<BotConnectionView["credential"]> {
    return connection.provider === "weixin" ? { tokenEnv: value } : { appSecretEnv: value };
}
export function botConnectionCredentialSummary(connection: BotConnectionView, t: ReturnType<typeof useT>): string {
    if (connection.provider === "weixin") {
        return connection.credential.accountId
            ? t("settings.botCredentialAccount", { value: connection.credential.accountId })
            : t("settings.botCredentialLocalWeixin");
    }
    if (connection.credential.appId) {
        return t("settings.botCredentialApp", { value: connection.credential.appId });
    }
    return t("settings.botCredentialConfigured");
}
function sanitizeBotDraft(draft: BotSettingsView): BotSettingsView {
    const bot = normalizeBotSettings(draft);
    return {
        ...bot,
        model: bot.model.trim(),
        toolApprovalMode: normalizeBotToolApprovalMode(bot.toolApprovalMode),
        maxSteps: Math.max(0, Math.floor(bot.maxSteps || 0)),
        debounceMs: Math.max(0, Math.floor(bot.debounceMs || 0)),
        queueMode: normalizeBotQueueMode(bot.queueMode),
        queueCap: Math.max(0, Math.floor(bot.queueCap || 0)),
        queueDrop: normalizeBotQueueDrop(bot.queueDrop),
        selfUserIds: {
            qq: uniqueStrings(bot.selfUserIds.qq.map((v) => v.trim())),
            feishu: uniqueStrings(bot.selfUserIds.feishu.map((v) => v.trim())),
            weixin: uniqueStrings(bot.selfUserIds.weixin.map((v) => v.trim())),
        },
        control: {
            enabled: bot.control.enabled,
            addr: bot.control.addr.trim(),
            tokenEnv: bot.control.tokenEnv.trim(),
        },
        pairing: {
            enabled: bot.pairing.enabled,
            requestTtlMinutes: Math.max(0, Math.floor(bot.pairing.requestTtlMinutes || 0)),
            maxPendingPerPlatform: Math.max(0, Math.floor(bot.pairing.maxPendingPerPlatform || 0)),
        },
        routes: bot.routes.map(normalizeBotRoute).filter(botRouteHasValue),
        allowlist: {
            ...bot.allowlist,
            qqUsers: uniqueStrings(bot.allowlist.qqUsers.map((v) => v.trim())),
            feishuUsers: uniqueStrings(bot.allowlist.feishuUsers.map((v) => v.trim())),
            weixinUsers: uniqueStrings(bot.allowlist.weixinUsers.map((v) => v.trim())),
            qqApprovers: uniqueStrings(bot.allowlist.qqApprovers.map((v) => v.trim())),
            feishuApprovers: uniqueStrings(bot.allowlist.feishuApprovers.map((v) => v.trim())),
            weixinApprovers: uniqueStrings(bot.allowlist.weixinApprovers.map((v) => v.trim())),
            qqAdmins: uniqueStrings(bot.allowlist.qqAdmins.map((v) => v.trim())),
            feishuAdmins: uniqueStrings(bot.allowlist.feishuAdmins.map((v) => v.trim())),
            weixinAdmins: uniqueStrings(bot.allowlist.weixinAdmins.map((v) => v.trim())),
            qqGroups: uniqueStrings(bot.allowlist.qqGroups.map((v) => v.trim())),
            feishuGroups: uniqueStrings(bot.allowlist.feishuGroups.map((v) => v.trim())),
            weixinGroups: uniqueStrings(bot.allowlist.weixinGroups.map((v) => v.trim())),
        },
        qq: {
            ...bot.qq,
            appId: bot.qq.appId.trim(),
            appSecretEnv: bot.qq.appSecretEnv.trim(),
            model: bot.qq.model.trim(),
            toolApprovalMode: normalizeBotToolApprovalMode(bot.qq.toolApprovalMode),
            workspaceRoot: bot.qq.workspaceRoot.trim(),
            access: sanitizeBotAccess(bot.qq.access),
        },
        feishu: {
            ...bot.feishu,
            domain: bot.feishu.domain === "lark" ? "lark" : "feishu",
            appId: bot.feishu.appId.trim(),
            appSecretEnv: bot.feishu.appSecretEnv.trim(),
            verificationToken: bot.feishu.verificationToken.trim(),
            mode: bot.feishu.mode === "websocket" ? "websocket" : "webhook",
            webhookPort: Math.max(0, Math.floor(bot.feishu.webhookPort || 0)),
        },
        weixin: {
            ...bot.weixin,
            accountId: bot.weixin.accountId.trim(),
            tokenEnv: bot.weixin.tokenEnv.trim(),
            apiBase: bot.weixin.apiBase.trim().replace(/\/+$/, ""),
        },
        connections: bot.connections.map((conn) => ({ ...normalizeBotConnection(conn), access: sanitizeBotAccess(conn.access) })).filter((conn) => conn.id && conn.provider),
    };
}
function sanitizeBotAccess(access: BotAccessView): BotAccessView {
    const normalized = normalizeBotAccess(access);
    return {
        ...normalized,
        users: uniqueStrings(normalized.users.map((v) => v.trim()).filter(Boolean)),
        groups: uniqueStrings(normalized.groups.map((v) => v.trim()).filter(Boolean)),
        approvers: uniqueStrings(normalized.approvers.map((v) => v.trim()).filter(Boolean)),
        admins: uniqueStrings(normalized.admins.map((v) => v.trim()).filter(Boolean)),
    };
}
export function botDraftWithDerivedGatewayState(draft: BotSettingsView): BotSettingsView {
    const bot = sanitizeBotDraft(draft);
    return {
        ...bot,
        enabled: bot.qq.enabled || bot.connections.some((connection) => connection.enabled),
    };
}

