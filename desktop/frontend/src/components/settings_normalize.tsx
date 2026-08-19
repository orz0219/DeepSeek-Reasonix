import { asArray } from "../lib/array";
import { normalizeLangPref, useT, type LangPref } from "../lib/i18n";
import { providerIsConfigured, providerRequiresKey } from "../lib/providerModels";
import { normalizeThemePreference, normalizeThemeStyleForTheme } from "../lib/theme";
import { normalizeTerminalThemePreference } from "../lib/terminalTheme";
import { normalizeConversationWidth } from "../lib/conversationWidth";
import { normalizeStatusBarItems, type StatusBarItemId } from "../lib/statusBarItems";
import { normalizeToolApprovalMode } from "../lib/types";
import type { NetworkView, ProviderPresetView, ProviderView, SettingsView } from "../lib/types";
// allRefs flattens providers into "provider/model" refs for the model selectors.
export function allRefs(s: SettingsView): string[] {
    const out: string[] = [];
    for (const p of s.providers) {
        if (!p.added || p.enabled === false || !providerIsConfigured(p))
            continue;
        for (const m of p.models)
            out.push(`${p.name}/${m}`);
    }
    return out;
}
// toRef normalises a stored model id (a provider name, a bare model, or a ref) to
// a "provider/model" ref so a <select> of refs can show it selected.
export function toRef(model: string, s: SettingsView): string {
    if (!model)
        return "";
    if (model.includes("/"))
        return model;
    const byName = s.providers.find((p) => p.name === model);
    if (byName)
        return `${byName.name}/${byName.default || byName.models[0] || ""}`;
    const byModel = s.providers.find((p) => p.models.includes(model));
    if (byModel)
        return `${byModel.name}/${model}`;
    return model;
}
export const PROXY_MODES = ["auto", "custom", "off"] as const;
// EFFORT_PRESETS is the canonical union of /effort levels the kernel recognises.
// The settings UI uses it for subagent defaults; provider-specific levels are
// inferred by the backend or edited in TOML for rare gateways.
export const EFFORT_PRESETS: readonly string[] = ["low", "medium", "high", "xhigh", "max"];
export const COMPACT_RATIO_PRESETS = [
    [0.7, "settings.compactRatioPreset.70"],
    [0.8, "settings.compactRatioPreset.80"],
    [0.85, "settings.compactRatioPreset.85"],
] as const;
export const REASONING_PROTOCOLS: readonly string[] = ["", "deepseek", "glm", "kimi-k3", "openai", "none"];
export const THINKING_MODES: readonly string[] = ["", "enabled", "disabled", "adaptive"];
export const PROXY_TYPES = ["http", "https", "socks5", "socks5h"] as const;
export const LANGUAGE_PREFS: LangPref[] = ["", "zh", "en"];
export const TOOL_APPROVAL_MODES = ["ask", "auto", "yolo"] as const;
export const BOT_TOOL_APPROVAL_MODES = ["", "ask", "auto", "yolo"] as const;
export type ProxyMode = (typeof PROXY_MODES)[number];
export function normalizeProxyMode(mode: string): ProxyMode {
    switch (mode) {
        case "custom":
            return "custom";
        case "off":
            return "off";
        default:
            return "auto";
    }
}
export function normalizeNetworkView(network: NetworkView): NetworkView {
    return { ...network, proxyMode: normalizeProxyMode(network.proxyMode) };
}
export function normalizeReasoningProtocol(protocol: string | undefined): string {
    return REASONING_PROTOCOLS.includes(protocol ?? "") ? protocol ?? "" : "";
}
export function normalizeThinkingMode(thinking: string | undefined): string {
    const v = String(thinking ?? "").trim().toLowerCase();
    return THINKING_MODES.includes(v) ? v : "";
}
export function providerEditorEffectiveKind(isNewCustomProvider: boolean, kind: string, kinds: string[]): string {
    void isNewCustomProvider;
    const selected = kind.trim();
    return selected || kinds[0] || "openai";
}
export function formatProviderHeaders(headers: Record<string, string> | null | undefined): string {
    const entries = Object.entries(headers ?? {})
        .map(([key, value]) => [key.trim(), String(value ?? "").trim()] as const)
        .filter(([key, value]) => key && value)
        .sort(([a], [b]) => a.localeCompare(b));
    return entries.map(([key, value]) => `${key}: ${value}`).join("\n");
}
export function parseProviderHeaders(raw: string): Record<string, string> {
    const out: Record<string, string> = {};
    for (const line of raw.split(/\r?\n/)) {
        const trimmed = line.trim();
        if (!trimmed || trimmed.startsWith("#"))
            continue;
        const colon = trimmed.indexOf(":");
        const eq = trimmed.indexOf("=");
        const cut = colon >= 0 && (eq < 0 || colon < eq) ? colon : eq;
        if (cut <= 0)
            continue;
        const key = trimmed.slice(0, cut).trim();
        const value = trimmed.slice(cut + 1).trim();
        if (key && value)
            out[key] = value;
    }
    return out;
}
function sortedJSONValue(value: unknown): unknown {
    if (Array.isArray(value))
        return value.map(sortedJSONValue);
    if (value && typeof value === "object") {
        const out: Record<string, unknown> = {};
        for (const key of Object.keys(value as Record<string, unknown>).sort((a, b) => a.localeCompare(b))) {
            out[key] = sortedJSONValue((value as Record<string, unknown>)[key]);
        }
        return out;
    }
    return value;
}
export function formatSettingsError(error: unknown, t: ReturnType<typeof useT>): string {
    const msg = String((error as Error)?.message ?? error ?? "").trim();
    const unknownModel = /^unknown model (.+)$/i.exec(msg);
    if (unknownModel)
        return t("settings.errorUnknownModel", { model: unknownModel[1] });
    const providerNotAdded = /^model (.+) is not available because provider (.+) is not added$/i.exec(msg);
    if (providerNotAdded)
        return t("settings.errorModelProviderMissing", { model: providerNotAdded[1], provider: providerNotAdded[2] });
    const providerNoKey = /^model (.+) is not available because provider (.+) has no key$/i.exec(msg);
    if (providerNoKey)
        return t("settings.errorModelProviderNoKey", { model: providerNoKey[1], provider: providerNoKey[2] });
    if (/^background session is still open; reopen or close it before upgrading the DeepSeek provider protocol$/i.test(msg)) {
        return t("settings.errorProviderDetached");
    }
    const removeAccessBusy = /^finish or cancel active work using (.+) before removing the provider access$/i.exec(msg);
    if (removeAccessBusy)
        return t("settings.errorRemoveAccessBusy", { provider: removeAccessBusy[1] });
    const removeAccessDetached = /^background session is still using (.+); reopen or close it before removing the provider access$/i.exec(msg);
    if (removeAccessDetached)
        return t("settings.errorProviderDetached");
    const removeAccessNoFallback = /^remove provider access: (.+) is in use and no other configured provider exists$/i.exec(msg);
    if (removeAccessNoFallback)
        return t("settings.errorRemoveProviderNoFallback", { provider: removeAccessNoFallback[1] });
    const deleteProviderNoFallback = /^remove provider: (.+) is in use and no other configured provider exists$/i.exec(msg);
    if (deleteProviderNoFallback)
        return t("settings.errorRemoveProviderNoFallback", { provider: deleteProviderNoFallback[1] });
    const deleteProviderBusy = /^finish or cancel active work using (.+) before deleting the provider$/i.exec(msg);
    if (deleteProviderBusy)
        return t("settings.errorDeleteProviderBusy", { provider: deleteProviderBusy[1] });
    const deleteProviderDetached = /^background session is still using (.+); reopen or close it before deleting the provider$/i.exec(msg);
    if (deleteProviderDetached)
        return t("settings.errorProviderDetached");
    const saveBeforeRemoveAccess = /^save current session before removing provider access: (.+)$/is.exec(msg);
    if (saveBeforeRemoveAccess)
        return t("settings.errorSaveBeforeRemoveAccess", { err: saveBeforeRemoveAccess[1] });
    const saveBeforeDeleteProvider = /^save current session before deleting provider: (.+)$/is.exec(msg);
    if (saveBeforeDeleteProvider)
        return t("settings.errorSaveBeforeDeleteProvider", { err: saveBeforeDeleteProvider[1] });
    const removeProviderUsed = /^remove provider: (.+) is used by open tabs and no other configured provider exists$/i.exec(msg);
    if (removeProviderUsed)
        return t("settings.errorRemoveProviderNoFallback", { provider: removeProviderUsed[1] });
    return msg || t("settings.errorUnknown");
}
function validateProviderExtraBodyValue(value: unknown, path = "extra_body", t?: ReturnType<typeof useT>): void {
    if (value === null) {
        throw new Error(t ? t("settings.providerExtraBodyNull", { path }) : `${path} cannot contain null`);
    }
    if (Array.isArray(value)) {
        value.forEach((item, index) => validateProviderExtraBodyValue(item, `${path}[${index}]`, t));
        return;
    }
    if (typeof value === "object") {
        for (const [key, child] of Object.entries(value as Record<string, unknown>)) {
            validateProviderExtraBodyValue(child, `${path}.${key}`, t);
        }
    }
}
export function formatProviderExtraBody(extraBody: Record<string, unknown> | null | undefined): string {
    const cleaned: Record<string, unknown> = {};
    for (const [rawKey, value] of Object.entries(extraBody ?? {})) {
        const key = rawKey.trim();
        if (!key || value === undefined)
            continue;
        cleaned[key] = value;
    }
    if (Object.keys(cleaned).length === 0)
        return "";
    return JSON.stringify(sortedJSONValue(cleaned), null, 2);
}
export function parseProviderExtraBody(raw: string, t?: ReturnType<typeof useT>): Record<string, unknown> {
    const trimmed = raw.trim();
    if (!trimmed)
        return {};
    const parsed = JSON.parse(trimmed) as unknown;
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
        throw new Error(t ? t("settings.providerExtraBodyObjectRequired") : "extra body must be a JSON object");
    }
    validateProviderExtraBodyValue(parsed, "extra_body", t);
    const out: Record<string, unknown> = {};
    for (const [rawKey, value] of Object.entries(parsed as Record<string, unknown>)) {
        const key = rawKey.trim();
        if (key)
            out[key] = value;
    }
    return out;
}
export function providerExtraBodyParseError(error: unknown, t: ReturnType<typeof useT>): string {
    if (error instanceof SyntaxError)
        return t("settings.providerExtraBodyError");
    const message = String((error as Error)?.message ?? error ?? "").trim();
    return message || t("settings.providerExtraBodyError");
}
export function providerModelFetchFallbackMessage(error: unknown, t: ReturnType<typeof useT>): string {
    const message = String((error as Error)?.message ?? error);
    if (/\bstatus\s+(401|403)\b/i.test(message)) {
        return t("settings.fetchModelsManualFallbackAuth");
    }
    if (/\bstatus\s+(404|405)\b/i.test(message)) {
        return t("settings.fetchModelsManualFallbackUnsupported");
    }
    if (/\b(status\s+5\d\d|request failed|network|timeout|timed out|connection|deadline|fetch failed)\b/i.test(message)) {
        return t("settings.fetchModelsManualFallbackNetwork");
    }
    if (/\b(decode response|invalid character|unexpected end|unexpected format)\b/i.test(message)) {
        return t("settings.fetchModelsManualFallbackDecode");
    }
    return t("settings.fetchModelsManualFallbackGeneric", { err: message });
}
function normalizeReasoningLanguage(lang: string | undefined): string {
    const v = String(lang ?? "").trim().toLowerCase();
    return v === "zh" || v === "en" ? v : "auto";
}
function normalizeStringMap(value: unknown): Record<string, string> {
    if (!value || typeof value !== "object" || Array.isArray(value))
        return {};
    const out: Record<string, string> = {};
    for (const [rawKey, rawValue] of Object.entries(value as Record<string, unknown>)) {
        const key = rawKey.trim();
        const val = String(rawValue ?? "").trim();
        if (key && val)
            out[key] = val;
    }
    return out;
}
function normalizeExtraBodyMap(value: unknown): Record<string, unknown> {
    if (!value || typeof value !== "object" || Array.isArray(value))
        return {};
    const out: Record<string, unknown> = {};
    for (const [rawKey, rawValue] of Object.entries(value as Record<string, unknown>)) {
        const key = rawKey.trim();
        if (key && rawValue !== undefined)
            out[key] = rawValue;
    }
    return out;
}
export function normalizeProviderView(p: ProviderView): ProviderView {
    const visionModels = asArray(p.visionModels);
    const requiresKey = providerRequiresKey(p);
    return {
        ...p,
        name: String(p.name ?? ""),
        baseUrl: String(p.baseUrl ?? ""),
        builtIn: Boolean(p.builtIn),
        added: Boolean(p.added),
        enabled: p.enabled !== false,
        chatUrl: p.chatUrl ?? "",
        requestUrl: p.requestUrl ?? "",
        models: asArray(p.models),
        visionModels,
        visionModelsConfigured: Boolean(p.visionModelsConfigured ?? visionModels.length > 0),
        visionCapability: p.visionCapability === "unsupported" || p.visionCapability === "configurable"
            ? p.visionCapability
            : undefined,
        modelsUrl: p.modelsUrl ?? "",
        headers: normalizeStringMap(p.headers),
        extraBody: normalizeExtraBodyMap(p.extraBody),
        authHeader: Boolean(p.authHeader),
        reasoningProtocol: normalizeReasoningProtocol(p.reasoningProtocol),
        thinking: normalizeThinkingMode(p.thinking),
        webSearch: Boolean(p.webSearch),
        serverWebSearchCapability: typeof p.serverWebSearchCapability === "boolean"
            ? p.serverWebSearchCapability
            : undefined,
        supportedEfforts: asArray(p.supportedEfforts),
        modelOverrides: asArray(p.modelOverrides),
        recommendedUpgradeAvailable: Boolean(p.recommendedUpgradeAvailable),
        requiresKey,
        configured: providerIsConfigured({ ...p, requiresKey }),
        keySource: p.keySource ?? "",
        keySourcePath: p.keySourcePath ?? "",
        modelCatalogFingerprint: p.modelCatalogFingerprint ?? "",
    };
}
export type ProviderPresetStatus = NonNullable<ProviderPresetView["status"]>;
export function normalizeProviderPresetStatus(status: ProviderPresetView["status"] | undefined, added: boolean): ProviderPresetStatus {
    if (status === "installed" || status === "installed_modified" || status === "name_conflict" || status === "similar_existing")
        return status;
    return added ? "installed" : "available";
}
function normalizeProviderPresetView(p: ProviderPresetView): ProviderPresetView {
    const requiresKey = Boolean(p.requiresKey ?? p.keyEnv);
    const configured = Boolean(p.configured ?? (!requiresKey || p.keySet));
    const status = normalizeProviderPresetStatus(p.status, Boolean(p.added));
    return {
        ...p,
        id: String(p.id ?? "").trim(),
        label: String(p.label ?? "").trim(),
        description: String(p.description ?? "").trim(),
        keyEnv: String(p.keyEnv ?? "").trim(),
        providerNames: asArray(p.providerNames),
        models: asArray(p.models),
        added: Boolean(p.added || status === "installed" || status === "installed_modified" || status === "name_conflict"),
        status,
        statusProviderNames: asArray(p.statusProviderNames),
        keySet: Boolean(p.keySet),
        requiresKey,
        configured,
        keySource: p.keySource ?? "",
        keySourcePath: p.keySourcePath ?? "",
    };
}
export function normalizeSettingsView(view: SettingsView | null | undefined): SettingsView | null {
    if (!view)
        return null;
    const permissions = view.permissions ?? { mode: "ask", allow: [], ask: [], deny: [] };
    const sandbox = view.sandbox ?? { bash: "enforce", network: false, workspaceRoot: "", allowWrite: [], effectiveWorkspaceRoot: "", effectiveWriteRoots: [], shell: "auto", effectiveShell: "" };
    const network = view.network ?? {
        proxyMode: "auto",
        proxyUrl: "",
        noProxy: "",
        proxy: { type: "socks5", server: "", port: 0, username: "", password: "" },
    };
    const agent = view.agent ?? { temperature: 0, maxSteps: 0, plannerMaxSteps: 0, maxSubagentDepth: 2, maxSubagentConcurrency: 6, maxParallelWriters: 3, systemPrompt: "", reasoningLanguage: "auto", compactRatio: 0.85 };
    agent.plannerMaxSteps = Number.isFinite(agent.plannerMaxSteps) ? Math.max(0, Math.trunc(agent.plannerMaxSteps)) : 0;
    agent.maxSteps = Number.isFinite(agent.maxSteps) ? Math.max(0, Math.trunc(agent.maxSteps)) : 0;
    agent.maxSubagentDepth = Number.isFinite(agent.maxSubagentDepth) && agent.maxSubagentDepth <= 1 ? 1 : 2;
    agent.reasoningLanguage = normalizeReasoningLanguage(agent.reasoningLanguage);
    agent.compactRatio = Number.isFinite(agent.compactRatio) && Number(agent.compactRatio) > 0 ? Number(agent.compactRatio) : 0.85;
    agent.effectiveCompactRatio = Number.isFinite(agent.effectiveCompactRatio) && Number(agent.effectiveCompactRatio) > 0
        ? Number(agent.effectiveCompactRatio)
        : agent.compactRatio;
    agent.compactRatioOverridden = Boolean(agent.compactRatioOverridden);
    return {
        ...view,
        providers: asArray(view.providers).map(normalizeProviderView),
        officialProviders: asArray(view.officialProviders).map(normalizeProviderView),
        providerPresets: asArray(view.providerPresets).map(normalizeProviderPresetView).filter((p) => p.id),
        providerKinds: asArray(view.providerKinds),
        permissions: {
            ...permissions,
            allow: asArray(permissions.allow),
            ask: asArray(permissions.ask),
            deny: asArray(permissions.deny),
        },
        sandbox: {
            ...sandbox,
            allowWrite: asArray(sandbox.allowWrite),
            effectiveWorkspaceRoot: String(sandbox.effectiveWorkspaceRoot ?? ""),
            effectiveWriteRoots: asArray(sandbox.effectiveWriteRoots),
            effectiveShell: String(sandbox.effectiveShell ?? sandbox.shell ?? ""),
        },
        network: {
            ...network,
            proxy: network.proxy ?? { type: "socks5", server: "", port: 0, username: "", password: "" },
        },
        agent,
        autoPlan: "off",
        defaultToolApprovalMode: normalizeToolApprovalMode(view.defaultToolApprovalMode),
        autoApproveTools: Boolean(view.autoApproveTools ?? view.bypass),
        bypass: Boolean(view.autoApproveTools ?? view.bypass),
        desktopLanguage: normalizeLangPref(view.desktopLanguage),
        desktopCurrency: normalizeDesktopCurrency(view.desktopCurrency),
        desktopTheme: normalizeThemePreference(view.desktopTheme),
        desktopThemeStyle: normalizeThemeStyleForTheme(view.desktopThemeStyle, normalizeThemePreference(view.desktopTheme)),
        desktopTerminalTheme: normalizeTerminalThemePreference(view.desktopTerminalTheme),
        closeBehavior: normalizeCloseBehavior(view.closeBehavior),
        displayMode: normalizeDisplayMode(view.displayMode),
        statusBarStyle: normalizeStatusBarStyle(view.statusBarStyle),
        statusBarItems: normalizeStatusBarItems(view.statusBarItems),
        conversationWidth: normalizeConversationWidth(view.conversationWidth),
    };
}
export type DesktopCurrency = "" | "CNY" | "USD";
export function normalizeDesktopCurrency(currency: string | undefined): DesktopCurrency {
    return currency === "CNY" || currency === "USD" ? currency : "";
}
type CloseBehavior = "background" | "quit";
export function normalizeCloseBehavior(mode: string | undefined): CloseBehavior {
    return mode === "quit" ? "quit" : "background";
}
export type DisplayMode = "standard" | "compact";
export function normalizeDisplayMode(mode: string | undefined): DisplayMode {
    return mode === "standard" || mode === "compact" ? mode : "standard";
}
type StatusBarStyle = "icon" | "text";
export function normalizeStatusBarStyle(style: string | undefined): StatusBarStyle {
    return style === "icon" ? "icon" : "text";
}
export function statusBarItemLabel(id: StatusBarItemId, t: ReturnType<typeof useT>): string {
    switch (id) {
        case "model":
            return t("settings.statusBarItem.model");
        case "workspace":
            return t("settings.statusBarItem.workspace");
        case "git_branch":
            return t("settings.statusBarItem.gitBranch");
        case "cache":
            return t("status.cacheLabel");
        case "cache_avg":
            return t("status.cacheAvgLabel");
        case "session_tokens":
            return t("status.sessionTokensLabel");
        case "turn_tokens":
            return t("status.turnTokensLabel");
        case "turn_tps":
            return t("status.tpsLabel");
        case "turn_output_tokens":
            return t("status.outputTokensLabel");
        case "turn_cache_tokens":
            return t("status.cacheTokensLabel");
        case "turn_cost":
            return t("status.turnCostLabel");
        case "session_turns":
            return t("status.sessionTurnsLabel");
        case "context":
            return t("status.ctxLabel");
        case "compact":
            return t("status.compactLabel");
        case "cost":
            return t("status.costLabel");
        case "balance":
            return t("status.balanceLabel");
    }
}
export function closeBehaviorLabel(mode: CloseBehavior, t: ReturnType<typeof useT>): string {
    return mode === "quit" ? t("settings.closeBehavior.quit") : t("settings.closeBehavior.background");
}
export function permissionModeLabel(mode: string, t: ReturnType<typeof useT>): string {
    switch (mode) {
        case "allow":
            return t("settings.modeAllowShort");
        case "deny":
            return t("settings.modeDenyShort");
        default:
            return t("settings.modeAskShort");
    }
}
export function sandboxModeLabel(mode: string, t: ReturnType<typeof useT>): string {
    return mode === "off" ? t("settings.bashOffShort") : t("settings.bashEnforceShort");
}
export function providerKindLabel(kind: string, t: ReturnType<typeof useT>): string {
    switch (kind) {
        case "anthropic":
            return t("settings.providerProtocolAnthropic");
        case "openai":
            return t("settings.providerProtocolOpenAI");
        default:
            return kind;
    }
}
export function providerKindHint(kind: string, t: ReturnType<typeof useT>): string {
    return kind === "anthropic" ? t("settings.providerProtocolAnthropicHint") : t("settings.providerProtocolOpenAIHint");
}
export function reasoningProtocolLabel(protocol: string, t: ReturnType<typeof useT>): string {
    switch (protocol) {
        case "deepseek":
            return t("settings.reasoningProtocol.deepseek");
        case "glm": return t("settings.reasoningProtocol.glm");
        case "kimi-k3": return t("settings.reasoningProtocol.kimiK3");
        case "openai":
            return t("settings.reasoningProtocol.openai");
        case "none":
            return t("settings.reasoningProtocol.none");
        default:
            return t("settings.reasoningProtocol.auto");
    }
}
export function thinkingModeLabel(mode: string, t: ReturnType<typeof useT>): string {
    switch (mode) {
        case "enabled":
            return t("settings.thinkingMode.enabled");
        case "disabled":
            return t("settings.thinkingMode.disabled");
        case "adaptive":
            return t("settings.thinkingMode.adaptive");
        default:
            return t("settings.thinkingMode.auto");
    }
}

