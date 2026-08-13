import { useDeferredValue, useEffect, useId, useMemo, useState } from "react";
import { useT, type DictKey } from "../lib/i18n";
import { providerIsConfigured, providerRequiresKey } from "../lib/providerModels";
import { opencodeGoPresetDescriptionKeys } from "../lib/providerPresetDescriptions";
import type { BotAllowlistView, BotSettingsView, ProviderPresetView, ProviderView } from "../lib/types";
import { ProviderPresetStatus } from "./settings_normalize";
import { BotAllowlistTextKey, BotSelfUserTextKey } from "./settings_bot_helpers";
export type ProviderAccessGroup = {
    id: string;
    label: string;
    description: string;
    builtIn: boolean;
    providers: ProviderView[];
    apiKeyEnv: string;
    keySet: boolean;
    requiresKey: boolean;
    configured: boolean;
    keySource?: string;
    keySourcePath?: string;
    baseUrl: string;
    kind: string;
    models: string[];
    recommendedUpgradeAvailable: boolean;
};
export type ProviderFetchResult = {
    kind: "ok" | "warn";
    text: string;
};
export type ProviderModelDraft = {
    providerName: string;
    candidates: string[];
    selected: string[];
    visionModels: string[];
    visionCapability: ProviderVisionCapability;
};
export type AddProviderMode = null | "official" | "custom";
export type OfficialProviderKind = "deepseek";
export const OFFICIAL_PROVIDER_CHOICES: Array<{
    kind: OfficialProviderKind;
    labelKey: DictKey;
    descKey: DictKey;
    keyEnv: string;
}> = [
    { kind: "deepseek", labelKey: "settings.addProvider.official.deepseek", descKey: "settings.addProvider.official.deepseekDesc", keyEnv: "DEEPSEEK_API_KEY" },
];
export type ProviderTemplateChoice = {
    id: string;
    source: "official";
    kind: OfficialProviderKind;
    label: string;
    description: string;
    keyEnv: string;
    added: boolean;
    keySet: boolean;
} | {
    id: string;
    source: "preset";
    presetID: string;
    label: string;
    description: string;
    keyEnv: string;
    added: boolean;
    status: ProviderPresetStatus;
    statusProviderNames: string[];
    keySet: boolean;
};
export function providerTemplateCanAdd(choice: ProviderTemplateChoice | undefined): boolean {
    if (!choice)
        return false;
    if (choice.source === "official")
        return !choice.added;
    return choice.status !== "installed" && choice.status !== "installed_modified" && choice.status !== "name_conflict";
}
export function providerTemplateStatusBadge(choice: ProviderTemplateChoice, t: ReturnType<typeof useT>): string {
    if (choice.source === "official")
        return choice.added ? t("settings.addProvider.addedBadge") : "";
    if (choice.status === "installed")
        return t("settings.addProvider.addedBadge");
    if (choice.status === "installed_modified")
        return t("settings.addProvider.modifiedBadge");
    if (choice.status === "name_conflict")
        return t("settings.addProvider.nameConflictBadge");
    if (choice.status === "similar_existing")
        return t("settings.addProvider.similarExistingBadge");
    return "";
}
export function providerTemplateActionLabel(choice: ProviderTemplateChoice | undefined, t: ReturnType<typeof useT>): string {
    if (!choice)
        return t("settings.addProvider.confirm");
    if (choice.source === "preset" && choice.status === "name_conflict")
        return t("settings.addProvider.nameConflictAction");
    if (!providerTemplateCanAdd(choice))
        return t("settings.addProvider.alreadyAddedAction");
    return t("settings.addProvider.confirm");
}
export function providerTemplateStatusClass(choice: ProviderTemplateChoice): string {
    if (choice.source !== "preset" || choice.status === "available")
        return "";
    return ` provider-template-card--${choice.status.split("_").join("-")}`;
}
export function providerTemplateConflictProviderName(choice: ProviderTemplateChoice): string {
    if (choice.source !== "preset" || (choice.status !== "name_conflict" && choice.status !== "installed_modified"))
        return "";
    return choice.statusProviderNames[0] ?? "";
}
export function providerPresetDescription(preset: ProviderPresetView, t: ReturnType<typeof useT>): string {
    switch (preset.id) {
        case "deepseek-responses":
            return t("settings.addProvider.preset.deepseekResponsesDesc");
        case "longcat-openai":
            return t("settings.addProvider.preset.longcatOpenAIDesc");
        case "longcat-anthropic":
            return t("settings.addProvider.preset.longcatAnthropicDesc");
        case "token-rhythm":
            return t("settings.addProvider.preset.tokenRhythmDesc");
        case "kimi-cn":
            return t("settings.addProvider.preset.kimiCnDesc");
        case "kimi-global":
            return t("settings.addProvider.preset.kimiGlobalDesc");
        case "kimi-coding-plan":
            return t("settings.addProvider.preset.kimiCodingPlanDesc");
        case "mimo-api":
            return t("settings.addProvider.preset.mimoApiDesc");
        case "mimo-anthropic":
            return t("settings.addProvider.preset.mimoAnthropicDesc");
        case "mimo-token-plan-cn":
            return t("settings.addProvider.preset.mimoTokenPlanCnDesc");
        case "mimo-token-plan-cn-anthropic":
            return t("settings.addProvider.preset.mimoTokenPlanCnAnthropicDesc");
        case "mimo-token-plan-sgp":
            return t("settings.addProvider.preset.mimoTokenPlanSgpDesc");
        case "mimo-token-plan-sgp-anthropic":
            return t("settings.addProvider.preset.mimoTokenPlanSgpAnthropicDesc");
        case "mimo-token-plan-ams":
            return t("settings.addProvider.preset.mimoTokenPlanAmsDesc");
        case "mimo-token-plan-ams-anthropic":
            return t("settings.addProvider.preset.mimoTokenPlanAmsAnthropicDesc");
        case "minimax-cn-api":
            return t("settings.addProvider.preset.minimaxCnApiDesc");
        case "minimax-global-api":
            return t("settings.addProvider.preset.minimaxGlobalApiDesc");
        case "minimax-cn-anthropic":
            return t("settings.addProvider.preset.minimaxCnAnthropicDesc");
        case "minimax-global-anthropic":
            return t("settings.addProvider.preset.minimaxGlobalAnthropicDesc");
        case "glm-cn":
            return t("settings.addProvider.preset.glmCnDesc");
        case "zai-global":
            return t("settings.addProvider.preset.zaiGlobalDesc");
        case "glm-coding-plan-cn":
            return t("settings.addProvider.preset.glmCodingPlanCnDesc");
        case "glm-coding-plan-cn-anthropic":
            return t("settings.addProvider.preset.glmCodingPlanCnAnthropicDesc");
        case "zai-coding-plan-global":
            return t("settings.addProvider.preset.zaiCodingPlanGlobalDesc");
        case "zai-coding-plan-global-anthropic":
            return t("settings.addProvider.preset.zaiCodingPlanGlobalAnthropicDesc");
        case "opencode-go":
        case "opencode-go-anthropic":
        case "opencode-go-deepseek-anthropic":
        case "opencode-go-deepseek-responses":
            return t(opencodeGoPresetDescriptionKeys[preset.id]);
        case "opencode-zen-anthropic":
            return t("settings.addProvider.preset.opencodeZenAnthropicDesc");
        case "qwen-cn":
            return t("settings.addProvider.preset.qwenCnDesc");
        case "qwen-global":
            return t("settings.addProvider.preset.qwenGlobalDesc");
        case "qwen-coding-plan-cn":
            return t("settings.addProvider.preset.qwenCodingPlanCnDesc");
        case "qwen-coding-plan-cn-anthropic":
            return t("settings.addProvider.preset.qwenCodingPlanCnAnthropicDesc");
        case "qwen-coding-plan-global":
            return t("settings.addProvider.preset.qwenCodingPlanGlobalDesc");
        case "qwen-coding-plan-global-anthropic":
            return t("settings.addProvider.preset.qwenCodingPlanGlobalAnthropicDesc");
        case "stepfun":
            return t("settings.addProvider.preset.stepfunDesc");
        case "stepfun-anthropic":
            return t("settings.addProvider.preset.stepfunAnthropicDesc");
        case "novita":
            return t("settings.addProvider.preset.novitaDesc");
        case "gmi":
            return t("settings.addProvider.preset.gmiDesc");
        case "vercel-ai-gateway":
            return t("settings.addProvider.preset.vercelAiGatewayDesc");
        case "huggingface":
            return t("settings.addProvider.preset.huggingfaceDesc");
        case "nvidia":
            return t("settings.addProvider.preset.nvidiaDesc");
        case "kilocode":
            return t("settings.addProvider.preset.kilocodeDesc");
        case "ollama-cloud":
            return t("settings.addProvider.preset.ollamaCloudDesc");
        default:
            return preset.description;
    }
}
export function providerPresetLabel(preset: ProviderPresetView, t: ReturnType<typeof useT>): string {
    switch (preset.id) {
        case "deepseek-responses":
            return t("settings.addProvider.preset.deepseekResponsesLabel");
        case "token-rhythm":
            return t("settings.addProvider.preset.tokenRhythmLabel");
        default:
            return preset.label;
    }
}
export function ProviderModelSummary({ configured, models, hiddenModelCount, compact = false, }: {
    configured: boolean;
    models: string[];
    hiddenModelCount: number;
    compact?: boolean;
}) {
    const t = useT();
    const label = t(configured ? "settings.enabledModels" : "settings.modelList");
    return (<div className={`provider-card-block${compact ? " provider-card-block--inline" : ""}`}>
      <div className="provider-card-block__label">{label}</div>
      <div className="provider-model-chips" aria-label={label}>
        {models.length > 0 ? models.map((model) => (<span className="provider-model-chip" key={model}>
            {model}
          </span>)) : <span className="provider-model-chip provider-model-chip--empty">{t("settings.noModelsConfigured")}</span>}
        {hiddenModelCount > 0 && (<span className="provider-model-chip provider-model-chip--more">
            {t("settings.moreModels", { n: hiddenModelCount })}
          </span>)}
      </div>
    </div>);
}
export function ProviderTechnicalDetails({ group }: {
    group: ProviderAccessGroup;
}) {
    const t = useT();
    const imageInputUnsupported = group.providers.length > 0 && group.providers.every((provider) => providerVisionCapabilityForView(provider) === "unsupported");
    return (<details className="provider-technical-details">
      <summary>{t("settings.providerAccess")}</summary>
      <dl>
        {group.providers.length === 1 ? (<>
            <div><dt>{t("settings.providerProtocol")}</dt><dd>{providerProtocolDisplayName(group.kind)}</dd></div>
            <div><dt>{t("settings.providerBaseUrlLabel")}</dt><dd>{group.baseUrl || t("common.none")}</dd></div>
          </>) : group.providers.map((provider) => (<div key={provider.name}><dt>{provider.name}</dt><dd>{providerProtocolDisplayName(provider.kind)} · {provider.baseUrl || t("common.none")}</dd></div>))}
        <div>
          <dt>{t("settings.providerApiKeyEnv")}</dt>
          <dd>{group.apiKeyEnv || t("common.none")}</dd>
        </div>
        {imageInputUnsupported && (<div>
            <dt>{t("settings.visionModel")}</dt>
            <dd>{t("settings.imageInputUnsupported")}</dd>
          </div>)}
        {group.keySource && (<div>
            <dt>{t("settings.providerKey")}</dt>
            <dd title={group.keySourcePath || undefined}>{group.keySource}</dd>
          </div>)}
      </dl>
    </details>);
}
function providerProtocolDisplayName(kind: string): string {
    switch (kind.trim().toLowerCase()) {
        case "anthropic":
            return "Anthropic Messages";
        case "responses":
            return "Responses API";
        case "openai":
            return "OpenAI Chat Completions";
        default:
            return kind;
    }
}
export function ProviderModelDraftPicker({ draft, busy, fetching, onToggle, onToggleVision, onSelectAll, onClear, onCancel, onSave, }: {
    draft: ProviderModelDraft;
    busy: boolean;
    fetching: boolean;
    onToggle: (model: string) => void;
    onToggleVision: (model: string) => void;
    onSelectAll: () => void;
    onClear: () => void;
    onCancel: () => void;
    onSave: () => void;
}) {
    const t = useT();
    const [query, setQuery] = useState("");
    const [debouncedQuery, setDebouncedQuery] = useState("");
    // Debounce search to avoid expensive filtering on every keystroke
    useEffect(() => {
        const timer = setTimeout(() => setDebouncedQuery(query), 150);
        return () => clearTimeout(timer);
    }, [query]);
    const selected = new Set(draft.selected);
    const vision = new Set(draft.visionModels);
    const q = debouncedQuery.trim().toLowerCase();
    const visibleCandidates = useMemo(() => (q ? draft.candidates.filter((model) => model.toLowerCase().includes(q)) : draft.candidates), [draft.candidates, q]);
    const deferredCandidates = useDeferredValue(visibleCandidates);
    const disabled = busy || fetching;
    return (<div className="provider-model-draft">
      <div className="provider-model-draft__head">
        <div>
          <div className="provider-card-block__label">{t("settings.modelCandidates")}</div>
          <span>{t("settings.modelCandidatesSelected", { n: draft.selected.length })}</span>
        </div>
        <div className="provider-model-draft__tools">
          <button type="button" className="btn btn--small" disabled={disabled || draft.selected.length === draft.candidates.length} onClick={onSelectAll}>
            {t("settings.selectAllModels")}
          </button>
          <button type="button" className="btn btn--small" disabled={disabled || draft.selected.length === 0} onClick={onClear}>
            {t("settings.clearModelSelection")}
          </button>
        </div>
      </div>
      <input className="mem-input provider-model-draft__search" placeholder={t("settings.modelCandidateSearch")} value={query} disabled={disabled} onChange={(e) => setQuery(e.target.value)}/>
      <div className="provider-model-draft__list" role="list" aria-label={t("settings.modelCandidates")}>
        {deferredCandidates.length > 0 ? deferredCandidates.map((model) => {
            const enabled = selected.has(model);
            return (<div className="provider-model-draft__option" key={model} role="listitem" style={{ contentVisibility: "auto", containIntrinsicSize: "auto 48px" }}>
              <label className="provider-model-draft__model">
                <input type="checkbox" checked={enabled} disabled={disabled} onChange={() => onToggle(model)}/>
                <span>{model}</span>
              </label>
              {draft.visionCapability === "configurable" ? (<label className="provider-model-draft__vision">
                  <input type="checkbox" checked={enabled && vision.has(model)} disabled={disabled || !enabled} aria-label={t("settings.visionModelAria", { model })} onChange={() => onToggleVision(model)}/>
                  <span>{t("settings.visionModel")}</span>
                </label>) : (<div className="provider-model-draft__capabilities" aria-label={t("settings.modelCapabilitiesAria", { model })}>
                  <span>{t("settings.textInput")}</span>
                  <span>{t("settings.imageInputUnsupported")}</span>
                </div>)}
            </div>);
        }) : (<div className="provider-model-draft__empty">{t("settings.noMatchingCandidateModels")}</div>)}
      </div>
      <div className="provider-model-draft__actions">
        <button type="button" className="btn btn--small" disabled={disabled} onClick={onCancel}>
          {t("common.cancel")}
        </button>
        <button type="button" className="btn btn--primary btn--small" disabled={disabled || draft.selected.length === 0} onClick={onSave}>
          {t("settings.saveEnabledModels")}
        </button>
      </div>
    </div>);
}
export function ProviderServiceCapabilities({ supported, configured, models, hiddenModelCount, showModelSummary = false, enabled, disabled, onChange, }: {
    supported: boolean;
    configured?: boolean;
    models: string[];
    hiddenModelCount?: number;
    showModelSummary?: boolean;
    enabled: boolean;
    disabled: boolean;
    onChange: (enabled: boolean) => void;
}) {
    const t = useT();
    const capabilityID = useId();
    if (!supported)
        return null;
    return (<section className="provider-capabilities" aria-labelledby={capabilityID}>
      <div className="provider-card-block__label" id={capabilityID}>
        {t("settings.providerCapabilities")}
      </div>
      {showModelSummary && (<ProviderModelSummary configured={Boolean(configured)} models={models} hiddenModelCount={hiddenModelCount ?? 0} compact/>)}
      <label className="provider-capability-row">
        <span className="provider-capability-row__copy">
          <span className="provider-capability-row__title">
            {t("settings.serverWebSearch")}
            <span className="badge badge--project">{t("settings.recommended")}</span>
          </span>
          <span>{t("settings.serverWebSearchHint")}</span>
        </span>
        <input className="provider-capability-row__switch" type="checkbox" role="switch" checked={enabled} disabled={disabled} onChange={(event) => onChange(event.target.checked)}/>
      </label>
    </section>);
}
export function providerAccessGroups(providers: ProviderView[], t: ReturnType<typeof useT>): ProviderAccessGroup[] {
    const groups = new Map<string, ProviderAccessGroup>();
    for (const p of providers) {
        const id = providerGroupID(p);
        const builtIn = id.startsWith("builtin:");
        const existing = groups.get(id);
        if (existing) {
            existing.providers.push(p);
            existing.keySet = existing.keySet || p.keySet;
            existing.requiresKey = existing.requiresKey && providerRequiresKey(p);
            existing.configured = existing.configured || providerIsConfigured(p);
            existing.recommendedUpgradeAvailable = existing.recommendedUpgradeAvailable || Boolean(p.recommendedUpgradeAvailable);
            if (existing.recommendedUpgradeAvailable && existing.id === "builtin:deepseek") {
                existing.description = "";
            }
            if (!existing.keySource && p.keySource)
                existing.keySource = p.keySource;
            if (!existing.keySourcePath && p.keySourcePath)
                existing.keySourcePath = p.keySourcePath;
            existing.models = uniqueStrings([...existing.models, ...p.models]);
            continue;
        }
        groups.set(id, {
            id,
            label: providerGroupLabel(p, t),
            description: providerGroupDescription(p, t),
            builtIn,
            providers: [p],
            apiKeyEnv: p.apiKeyEnv,
            keySet: p.keySet,
            requiresKey: providerRequiresKey(p),
            configured: providerIsConfigured(p),
            keySource: p.keySource,
            keySourcePath: p.keySourcePath,
            baseUrl: p.baseUrl,
            kind: p.kind,
            models: uniqueStrings(p.models),
            recommendedUpgradeAvailable: Boolean(p.recommendedUpgradeAvailable),
        });
    }
    return Array.from(groups.values());
}
function providerBaseHost(baseUrl: string): string {
    try {
        return new URL(baseUrl).hostname.toLowerCase();
    }
    catch {
        return "";
    }
}
export type ProviderVisionCapability = "configurable" | "unsupported";
function isDeepSeekOfficialEndpoint(baseUrl: string): boolean {
    return providerBaseHost(baseUrl).endsWith(".deepseek.com");
}
export function providerSupportsServerWebSearch(kind: string, baseUrl: string): boolean {
    try {
        const endpoint = new URL(baseUrl.trim());
        if (endpoint.protocol !== "https:" ||
            endpoint.hostname.toLowerCase() !== "api.deepseek.com" ||
            endpoint.port ||
            endpoint.username ||
            endpoint.password ||
            endpoint.search ||
            endpoint.hash)
            return false;
        const path = endpoint.pathname.replace(/\/+$/, "");
        switch (kind.trim().toLowerCase()) {
            case "responses":
                return path === "";
            case "anthropic":
                return path === "/anthropic";
            default:
                return false;
        }
    }
    catch {
        return false;
    }
}
export function providerSupportsServerWebSearchForView(provider: Pick<ProviderView, "kind" | "baseUrl" | "serverWebSearchCapability">): boolean {
    if (typeof provider.serverWebSearchCapability === "boolean") {
        return provider.serverWebSearchCapability;
    }
    return providerSupportsServerWebSearch(provider.kind, provider.baseUrl);
}
function providerVisionCapability(kind: string, baseUrl: string): ProviderVisionCapability {
    if (!isDeepSeekOfficialEndpoint(baseUrl))
        return "configurable";
    switch (kind.trim().toLowerCase()) {
        case "openai":
        case "responses":
        case "anthropic":
            return "unsupported";
        default:
            return "configurable";
    }
}
export function providerVisionCapabilityForView(provider: Pick<ProviderView, "kind" | "baseUrl" | "visionCapability">): ProviderVisionCapability {
    if (provider.visionCapability === "unsupported" || provider.visionCapability === "configurable") {
        return provider.visionCapability;
    }
    return providerVisionCapability(provider.kind, provider.baseUrl);
}
export function canonicalOfficialProviderName(name: string): string {
    switch (name.trim()) {
        case "deepseek-flash":
        case "deepseek-pro":
            return "deepseek";
        default:
            return name.trim();
    }
}
export function officialProviderKind(p: ProviderView): string {
    if (!p.builtIn)
        return "";
    const name = canonicalOfficialProviderName(p.name);
    const host = providerBaseHost(p.baseUrl);
    if (name === "deepseek" && host === "api.deepseek.com")
        return "deepseek";
    return "";
}
export function providerGroupID(p: ProviderView): string {
    const official = officialProviderKind(p);
    if (official)
        return `builtin:${official}`;
    return `custom:${p.name}`;
}
export function providerGroupLabel(p: ProviderView, t?: ReturnType<typeof useT>): string {
    const id = providerGroupID(p);
    if (id === "builtin:deepseek")
        return t ? t("settings.providerLabel.deepseek") : "DeepSeek";
    return p.name;
}
function providerGroupDescription(p: ProviderView, t: ReturnType<typeof useT>): string {
    const id = providerGroupID(p);
    if (id === "builtin:deepseek") {
        return p.recommendedUpgradeAvailable ? "" : t("settings.providerDesc.deepseek");
    }
    return "";
}
export function uniqueStrings(values: string[]): string[] {
    const seen = new Set<string>();
    const out: string[] = [];
    for (const value of values) {
        if (value && !seen.has(value)) {
            seen.add(value);
            out.push(value);
        }
    }
    return out;
}
export function parseProviderListInput(value: string): string[] {
    return uniqueStrings(value
        .split(/[,，]/)
        .map((entry) => entry.trim())
        .filter(Boolean));
}
export function botAllowlistTextValues(allowlist: BotAllowlistView): Record<BotAllowlistTextKey, string> {
    return {
        qqUsers: allowlist.qqUsers.join("\n"),
        feishuUsers: allowlist.feishuUsers.join("\n"),
        weixinUsers: allowlist.weixinUsers.join("\n"),
        qqApprovers: allowlist.qqApprovers.join("\n"),
        feishuApprovers: allowlist.feishuApprovers.join("\n"),
        weixinApprovers: allowlist.weixinApprovers.join("\n"),
        qqAdmins: allowlist.qqAdmins.join("\n"),
        feishuAdmins: allowlist.feishuAdmins.join("\n"),
        weixinAdmins: allowlist.weixinAdmins.join("\n"),
        qqGroups: allowlist.qqGroups.join("\n"),
        feishuGroups: allowlist.feishuGroups.join("\n"),
        weixinGroups: allowlist.weixinGroups.join("\n"),
    };
}
export function botSelfUserTextValues(selfUserIds: BotSettingsView["selfUserIds"]): Record<BotSelfUserTextKey, string> {
    return {
        qq: selfUserIds.qq.join("\n"),
        feishu: selfUserIds.feishu.join("\n"),
        weixin: selfUserIds.weixin.join("\n"),
    };
}
export function parseBotListInput(value: string): string[] {
    return uniqueStrings(value
        .split(/[\n,，]+/)
        .map((entry) => entry.trim())
        .filter(Boolean));
}

