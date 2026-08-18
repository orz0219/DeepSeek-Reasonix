import { memo, useDeferredValue, useEffect, useId, useMemo, useState } from "react";
import { ChevronDown } from "lucide-react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { apiKeyEnvFromProviderName, inferredVisionModels, mergeProviderModelContextWindows, providerApiKeyEnvForSave, providerModelContextWindowDrafts, providerModelContextWindowIsSmall } from "../lib/providerModels";
import { cachedFetchProviderModels, invalidateProviderCacheByAPIKeyEnv } from "../lib/providerModelCache";
import { providerBaseURLForSave, providerRequestURLFromConfig, trimmedBaseURL } from "../lib/providerEndpoint";
import type { ProviderView } from "../lib/types";
import { InlineConfirmButton } from "./InlineConfirmButton";
import { providerEditorEffectiveKind, formatProviderExtraBody, parseProviderExtraBody, providerExtraBodyParseError, REASONING_PROTOCOLS, THINKING_MODES, normalizeReasoningProtocol, normalizeThinkingMode, formatProviderHeaders, parseProviderHeaders, providerModelFetchFallbackMessage, providerKindLabel, providerKindHint, reasoningProtocolLabel, thinkingModeLabel } from "./settings_normalize";
import { KeyField } from "./settings_permissions";
import { providerVisionCapabilityForView, uniqueStrings, ProviderServiceCapabilities, ProviderVisionCapability, providerSupportsServerWebSearch, parseProviderListInput } from "./settings_provider_helpers";
export const ProviderEditorModelPicker = memo(function ProviderEditorModelPicker({ candidates, selectedModels, visionModels, visionCapability = "configurable", contextWindows, disabled, onToggleModel, onToggleVision, onContextWindowChange, onSelectAll, onClear, }: {
    candidates: string[];
    selectedModels: string[];
    visionModels: string[];
    visionCapability?: ProviderVisionCapability;
    contextWindows: Record<string, string>;
    disabled: boolean;
    onToggleModel: (model: string) => void;
    onToggleVision: (model: string) => void;
    onContextWindowChange: (model: string, value: string) => void;
    onSelectAll: () => void;
    onClear: () => void;
}) {
    const t = useT();
    const [query, setQuery] = useState("");
    const [debouncedQuery, setDebouncedQuery] = useState("");
    useEffect(() => {
        const timer = setTimeout(() => setDebouncedQuery(query), 150);
        return () => clearTimeout(timer);
    }, [query]);
    const q = debouncedQuery.trim().toLowerCase();
    const visibleCandidates = q
        ? candidates.filter((model) => model.toLowerCase().includes(q))
        : candidates;
    const deferredCandidates = useDeferredValue(visibleCandidates);
    if (candidates.length === 0)
        return null;
    const selected = new Set(selectedModels);
    const vision = new Set(visionModels);
    return (<div className="provider-model-draft provider-model-draft--inline">
      <div className="provider-model-draft__head">
        <div>
          <div className="provider-card-block__label">{t("settings.modelCandidates")}</div>
          <span>{t("settings.modelCandidatesSelected", { n: selectedModels.length })}</span>
        </div>
        <div className="provider-model-draft__tools">
          <button type="button" className="btn btn--small" disabled={disabled || selectedModels.length === candidates.length} onClick={onSelectAll}>
            {t("settings.selectAllModels")}
          </button>
          <button type="button" className="btn btn--small" disabled={disabled || selectedModels.length === 0} onClick={onClear}>
            {t("settings.clearModelSelection")}
          </button>
        </div>
      </div>
      <div className="provider-model-draft__context-guide">{t("settings.modelContextWindowGuide")}</div>
      {candidates.length > 8 && (<input className="mem-input provider-model-draft__search" placeholder={t("settings.modelCandidateSearch")} value={query} disabled={disabled} onChange={(e) => setQuery(e.target.value)}/>)}
      <div className="provider-model-draft__list" role="list" aria-label={t("settings.modelCandidates")}>
        {deferredCandidates.length > 0 ? deferredCandidates.map((model) => {
            const enabled = selected.has(model);
            return (<div className="provider-model-draft__option" key={model} role="listitem" style={{ contentVisibility: "auto", containIntrinsicSize: "auto 48px" }}>
              <label className="provider-model-draft__model">
                <input type="checkbox" checked={enabled} disabled={disabled} onChange={() => onToggleModel(model)}/>
                <span>{model}</span>
              </label>
              {visionCapability === "configurable" ? (<label className="provider-model-draft__vision">
                  <input type="checkbox" checked={enabled && vision.has(model)} disabled={disabled || !enabled} aria-label={t("settings.visionModelAria", { model })} onChange={() => onToggleVision(model)}/>
                  <span>{t("settings.visionModel")}</span>
                </label>) : (<div className="provider-model-draft__capabilities" aria-label={t("settings.modelCapabilitiesAria", { model })}>
                  <span>{t("settings.textInput")}</span>
                  <span>{t("settings.imageInputUnsupported")}</span>
                </div>)}
              <div className="provider-model-draft__context-field">
                <label className="provider-model-draft__context">
                  <span>{t("settings.modelContextWindow")}</span>
                  <input className="mem-input provider-model-draft__context-input" type="number" inputMode="numeric" min={1} disabled={disabled || !enabled} placeholder={t("settings.modelContextWindowPlaceholder")} title={t("settings.modelContextWindowHint")} aria-label={t("settings.modelContextWindowAria", { model })} value={contextWindows[model] ?? ""} onChange={(event) => onContextWindowChange(model, event.target.value)}/>
                </label>
                {enabled && providerModelContextWindowIsSmall(contextWindows[model]) && (<div className="provider-model-draft__context-warning" role="status">
                    {t("settings.modelContextWindowSmallWarning")}
                  </div>)}
              </div>
            </div>);
        }) : (<div className="provider-model-draft__empty">{t("settings.noMatchingCandidateModels")}</div>)}
      </div>
    </div>);
});
export function ProviderEditor({ initial, kinds, busy, onCancel, onSave, onSaveKey, onClearKey, }: {
    initial?: ProviderView;
    kinds: string[];
    busy: boolean;
    onCancel: () => void;
    onSave: (p: ProviderView, key?: string) => void | Promise<void>;
    onSaveKey?: (apiKeyEnv: string, value: string) => Promise<void>;
    onClearKey?: (apiKeyEnv: string) => Promise<void>;
}) {
    const t = useT();
    const [name, setName] = useState(initial?.name ?? "");
    const [kind, setKind] = useState(initial?.kind ?? "openai");
    const [requestUrl, setRequestUrl] = useState(() => {
        const computed = providerRequestURLFromConfig(initial?.kind ?? "openai", initial?.baseUrl ?? "", initial?.requestUrl ?? "", initial?.chatUrl ?? "");
        return computed || (!initial ? "https://api.openai.com/v1/chat/completions" : "");
    });
    const providerUrlInputId = useId();
    const providerUrlHelpId = useId();
    const [models, setModels] = useState((initial?.models ?? []).join(", "));
    const [modelCandidates, setModelCandidates] = useState<string[]>(initial?.models ?? []);
    const [visionModels, setVisionModels] = useState((initial?.visionModels ?? []).join(", "));
    const [visionModelsConfigured, setVisionModelsConfigured] = useState(Boolean(initial?.visionModelsConfigured ?? ((initial?.visionModels ?? []).length > 0)));
    const [modelsUrl, setModelsUrl] = useState(initial?.modelsUrl ?? "");
    const [apiKeyEnv, setApiKeyEnv] = useState(initial?.apiKeyEnv ?? "");
    const [headersDraft, setHeadersDraft] = useState(formatProviderHeaders(initial?.headers));
    const [extraBodyDraft, setExtraBodyDraft] = useState(formatProviderExtraBody(initial?.extraBody));
    const [authHeader, setAuthHeader] = useState(Boolean(initial?.authHeader));
    const [keyDraft, setKeyDraft] = useState("");
    const [balanceUrl, setBalanceUrl] = useState(initial?.balanceUrl ?? "");
    // Empty when unset so the placeholder (and its "0 = disabled" hint) reads instead
    // of a bare "0"; saved back as 0.
    const [ctx, setCtx] = useState(initial?.contextWindow ? String(initial.contextWindow) : "");
    const [modelContextWindows, setModelContextWindows] = useState<Record<string, string>>(() => providerModelContextWindowDrafts(initial?.modelOverrides));
    const [reasoningProtocol, setReasoningProtocol] = useState(normalizeReasoningProtocol(initial?.reasoningProtocol));
    const [thinking, setThinking] = useState(normalizeThinkingMode(initial?.thinking));
    const [webSearch, setWebSearch] = useState(Boolean(initial?.webSearch));
    const [supportedEfforts] = useState<string[]>(initial?.supportedEfforts ?? []);
    const [defaultEffort] = useState(initial?.defaultEffort ?? "");
    const [fetchingModels, setFetchingModels] = useState(false);
    const [fetchStatus, setFetchStatus] = useState<string | null>(null);
    const [fetchFallback, setFetchFallback] = useState<string | null>(null);
    const [advancedOpen, setAdvancedOpen] = useState(false);
    const builtIn = initial?.builtIn ?? false;
    const isNewCustomProvider = !initial;
    const providerKindChoices = useMemo(() => {
        const choices = uniqueStrings([kind, ...kinds].map((candidate) => candidate.trim()).filter(Boolean));
        return choices.length > 0 ? choices : ["openai"];
    }, [kind, kinds]);
    const effectiveKind = providerEditorEffectiveKind(isNewCustomProvider, kind, providerKindChoices);
    const effectiveRequestUrl = requestUrl.trim();
    const effectiveBaseUrl = providerBaseURLForSave(initial, effectiveKind, effectiveRequestUrl);
    const effectiveLegacyChatUrl = effectiveKind.toLowerCase() === "openai" ? effectiveRequestUrl : initial?.chatUrl ?? "";
    const effectiveModelsUrl = modelsUrl.trim();
    const initialEffectiveBaseUrl = initial ? trimmedBaseURL(initial.baseUrl) : "";
    const retainedVisionCapability = initial &&
        effectiveKind.trim().toLowerCase() === initial.kind.trim().toLowerCase() &&
        trimmedBaseURL(effectiveBaseUrl) === initialEffectiveBaseUrl
        ? initial.visionCapability
        : undefined;
    const effectiveVisionCapability = providerVisionCapabilityForView({
        kind: effectiveKind,
        baseUrl: effectiveBaseUrl,
        visionCapability: retainedVisionCapability,
    });
    const retainedServerWebSearchCapability = initial &&
        effectiveKind.trim().toLowerCase() === initial.kind.trim().toLowerCase() &&
        trimmedBaseURL(effectiveBaseUrl) === initialEffectiveBaseUrl
        ? initial.serverWebSearchCapability
        : undefined;
    const effectiveServerWebSearchCapability = retainedServerWebSearchCapability ??
        providerSupportsServerWebSearch(effectiveKind, effectiveBaseUrl);
    const effectiveHeaders = parseProviderHeaders(headersDraft);
    const extraBodyParse = useMemo(() => {
        try {
            return { value: parseProviderExtraBody(extraBodyDraft, t), error: "" };
        }
        catch (e) {
            return { value: {}, error: providerExtraBodyParseError(e, t) };
        }
    }, [extraBodyDraft, t]);
    const effectiveExtraBody = extraBodyParse.value;
    const extraBodyInvalid = Boolean(extraBodyDraft.trim() && extraBodyParse.error);
    const modelNames = useMemo(() => parseProviderListInput(models), [models]);
    const modelCandidateNames = useMemo(() => uniqueStrings([...modelCandidates, ...modelNames]), [modelCandidates, modelNames]);
    const visionModelNames = useMemo(() => parseProviderListInput(visionModels).filter((model) => modelNames.includes(model)), [modelNames, visionModels]);
    // Empty supportedEfforts means "use protocol defaults". The simplified
    // provider flow no longer edits these levels directly, but it preserves
    // existing advanced TOML unless the user explicitly disables reasoning.
    const cleanedSupportedEfforts = reasoningProtocol !== "none"
        ? uniqueStrings(supportedEfforts
            .map((level) => level.toLowerCase().trim())
            .filter((level) => level && level !== "auto"))
        : [];
    const normalizedDefaultEffort = defaultEffort.toLowerCase().trim();
    const cleanDefaultEffort = cleanedSupportedEfforts.includes(normalizedDefaultEffort) ? normalizedDefaultEffort : "";
    const fetchModels = async () => {
        if (extraBodyInvalid)
            return;
        setFetchingModels(true);
        setFetchStatus(null);
        setFetchFallback(null);
        try {
            const effectiveApiKeyEnv = providerApiKeyEnvForSave(name, apiKeyEnv, keyDraft);
            if (!apiKeyEnv.trim())
                setApiKeyEnv(effectiveApiKeyEnv);
            if (keyDraft.trim()) {
                await app.SaveProviderKey(effectiveApiKeyEnv, keyDraft.trim());
                invalidateProviderCacheByAPIKeyEnv(effectiveApiKeyEnv);
            }
            const fetched = await cachedFetchProviderModels((provider) => app.FetchProviderModels(provider), {
                name: name.trim() || t("settings.newProviderDraftName"),
                builtIn: initial?.builtIn ?? false,
                added: initial?.added ?? true,
                kind: effectiveKind,
                baseUrl: effectiveBaseUrl,
                chatUrl: effectiveLegacyChatUrl,
                requestUrl: effectiveRequestUrl,
                modelsUrl: effectiveModelsUrl,
                models: [],
                visionModels: [],
                visionModelsConfigured: false,
                default: "",
                apiKeyEnv: effectiveApiKeyEnv,
                headers: effectiveHeaders,
                extraBody: effectiveExtraBody,
                authHeader,
                keySet: Boolean(keyDraft.trim()) || (initial?.keySet ?? false),
                balanceUrl: balanceUrl.trim(),
                contextWindow: Number(ctx) || 0,
                reasoningProtocol,
                thinking,
                webSearch: effectiveServerWebSearchCapability && webSearch,
                serverWebSearchCapability: effectiveServerWebSearchCapability,
                supportedEfforts: cleanedSupportedEfforts,
                defaultEffort: cleanDefaultEffort,
                modelOverrides: mergeProviderModelContextWindows(initial?.modelOverrides, parseProviderListInput(models), modelContextWindows),
            }, true);
            if (fetched.length === 0) {
                setFetchFallback(t("settings.fetchModelsManualFallbackEmpty"));
                return;
            }
            setModelCandidates(fetched);
            setModels(fetched.join(", "));
            setVisionModels((current) => {
                const existing = parseProviderListInput(current).filter((model) => fetched.includes(model));
                return uniqueStrings([...existing, ...inferredVisionModels(fetched)]).filter((model) => fetched.includes(model)).join(", ");
            });
            setVisionModelsConfigured(true);
            if (keyDraft.trim())
                setKeyDraft("");
            setFetchStatus(t("settings.fetchModelsSuccess", { n: fetched.length }));
        }
        catch (e) {
            setFetchFallback(providerModelFetchFallbackMessage(e, t));
        }
        finally {
            setFetchingModels(false);
        }
    };
    const save = async () => {
        if (extraBodyInvalid)
            return;
        setFetchStatus(null);
        setFetchFallback(null);
        const ms = parseProviderListInput(models);
        const vms = effectiveVisionCapability === "unsupported"
            ? []
            : parseProviderListInput(visionModels).filter((model) => ms.includes(model));
        const effectiveApiKeyEnv = providerApiKeyEnvForSave(name, apiKeyEnv, keyDraft);
        const provider: ProviderView = {
            name: name.trim(),
            builtIn: initial?.builtIn ?? false,
            added: initial?.added ?? true,
            kind: effectiveKind,
            baseUrl: effectiveBaseUrl,
            chatUrl: effectiveLegacyChatUrl,
            requestUrl: effectiveRequestUrl,
            models: ms,
            visionModels: vms,
            visionModelsConfigured: visionModelsConfigured || vms.length > 0,
            default: ms[0] ?? "",
            apiKeyEnv: effectiveApiKeyEnv,
            headers: effectiveHeaders,
            extraBody: effectiveExtraBody,
            authHeader,
            modelsUrl: effectiveModelsUrl,
            keySet: Boolean(keyDraft.trim()) || (initial?.keySet ?? false),
            balanceUrl: balanceUrl.trim(),
            contextWindow: Number(ctx) || 0,
            reasoningProtocol,
            thinking,
            webSearch: effectiveServerWebSearchCapability && webSearch,
            serverWebSearchCapability: effectiveServerWebSearchCapability,
            supportedEfforts: cleanedSupportedEfforts,
            // Clear the stored default if no levels are selected; the backend's
            // NormalizeEffort would otherwise silently ignore an unsupported value.
            defaultEffort: cleanedSupportedEfforts.length > 0 ? cleanDefaultEffort : "",
            modelOverrides: mergeProviderModelContextWindows(initial?.modelOverrides, ms, modelContextWindows),
        };
        try {
            await onSave(provider, keyDraft.trim() || undefined);
        }
        catch (e) {
            setFetchFallback(String((e as Error)?.message ?? e));
        }
    };
    if (builtIn) {
        const keyEnv = initial?.apiKeyEnv.trim() ?? "";
        return (<div className="provider-editor provider-editor--builtin provider-editor--key-only">
        {initial && onSaveKey && keyEnv && (<>
            <div className="provider-key-status provider-key-status--managed provider-key-status--compact">
              <span title={initial.keySourcePath || undefined}>
                {initial.keySet ? t("settings.configuredKey", { env: keyEnv }) : t("settings.notConfiguredKey", { env: keyEnv })}
                {initial.keySource ? ` · ${t("settings.keySource", { source: initial.keySource })}` : ""}
              </span>
              {initial.keySet && onClearKey && (<InlineConfirmButton label={t("settings.clearKey")} confirmLabel={t("settings.confirmClearKey")} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={() => onClearKey(keyEnv)}/>)}
            </div>
            <KeyField apiKeyEnv={keyEnv} busy={busy} keySet={initial.keySet} onSet={(env, value) => onSaveKey(env, value)}/>
          </>)}
      </div>);
    }
    const canFetch = Boolean(name.trim() && effectiveBaseUrl);
    const setModelsFromList = (nextModels: string[]) => {
        setModels(uniqueStrings(nextModels).join(", "));
    };
    const updateManualModels = (value: string) => {
        setModels(value);
        const typedModels = parseProviderListInput(value);
        if (typedModels.length > 0) {
            setModelCandidates((current) => uniqueStrings([...current, ...typedModels]));
        }
    };
    const toggleEditorModel = (model: string) => {
        const selected = new Set(modelNames);
        if (selected.has(model)) {
            selected.delete(model);
            setVisionModels(visionModelNames.filter((candidate) => candidate !== model).join(", "));
        }
        else {
            selected.add(model);
        }
        setModelsFromList(modelCandidateNames.filter((candidate) => selected.has(candidate)));
        setVisionModelsConfigured(true);
    };
    const toggleEditorVisionModel = (model: string) => {
        if (!modelNames.includes(model))
            return;
        const vision = new Set(visionModelNames);
        if (vision.has(model))
            vision.delete(model);
        else
            vision.add(model);
        setVisionModels(modelCandidateNames.filter((candidate) => vision.has(candidate)).join(", "));
        setVisionModelsConfigured(true);
    };
    const updateEditorModelContextWindow = (model: string, value: string) => {
        setModelContextWindows((current) => ({ ...current, [model]: value }));
    };
    const selectAllEditorModels = () => {
        setModelsFromList(modelCandidateNames);
        setVisionModels(visionModelNames.filter((model) => modelCandidateNames.includes(model)).join(", "));
        setVisionModelsConfigured(true);
    };
    const clearEditorModels = () => {
        setModels("");
        setVisionModels("");
        setVisionModelsConfigured(true);
    };
    const advancedFields = (<details className="provider-editor-advanced" open={advancedOpen} onToggle={(e) => setAdvancedOpen(e.currentTarget.open)}>
      <summary>
        <span className="provider-editor-advanced__title">
          <ChevronDown className="provider-editor-advanced__icon" size={16} aria-hidden="true"/>
          {t("settings.providerAdvancedSettings")}
        </span>
        <span className="provider-editor-advanced__hint">
          {advancedOpen ? t("settings.providerAdvancedCollapseHint") : t("settings.providerAdvancedExpandHint")}
        </span>
      </summary>
      <div className="provider-editor-advanced__body">
        <label className="set-label">{t("settings.providerApiKeyEnv")}</label>
        <input className="mem-input" placeholder={apiKeyEnvFromProviderName(name)} value={apiKeyEnv} onChange={(e) => setApiKeyEnv(e.target.value)}/>
        <div className="mem-hint">{t("settings.providerApiKeyEnvHint")}</div>
        <label className="set-label">{t("settings.providerModelsUrl")}</label>
        <input className="mem-input" placeholder={t("settings.providerModelsUrlPlaceholder")} value={modelsUrl} onChange={(e) => setModelsUrl(e.target.value)}/>
        <div className="mem-hint">{t("settings.providerModelsUrlHint")}</div>
        <label className="set-label">{t("settings.providerHeaders")}</label>
        <textarea className="mem-textarea provider-headers-textarea" placeholder={t("settings.providerHeadersPlaceholder")} value={headersDraft} onChange={(e) => setHeadersDraft(e.target.value)} rows={3}/>
        <div className="mem-hint">{t("settings.providerHeadersHint")}</div>
        <label className="set-label">{t("settings.providerExtraBody")}</label>
        <textarea className="mem-textarea provider-headers-textarea" placeholder={t("settings.providerExtraBodyPlaceholder")} value={extraBodyDraft} onChange={(e) => setExtraBodyDraft(e.target.value)} rows={4}/>
        <div className={`mem-hint${extraBodyInvalid ? " mem-hint--error" : ""}`}>
          {extraBodyInvalid ? extraBodyParse.error : t("settings.providerExtraBodyHint")}
        </div>
        <label className="set-check">
          <input type="checkbox" checked={authHeader} onChange={(e) => setAuthHeader(e.target.checked)}/>
          {t("settings.providerAuthHeader")}
        </label>
        <div className="mem-hint">{t("settings.providerAuthHeaderHint")}</div>
        <label className="set-label">{t("settings.reasoningProtocol")}</label>
        <select className="mem-select" value={reasoningProtocol} onChange={(e) => setReasoningProtocol(e.target.value)}>
          {REASONING_PROTOCOLS.map((protocol) => (<option key={protocol || "auto"} value={protocol}>
              {reasoningProtocolLabel(protocol, t)}
            </option>))}
        </select>
        <div className="mem-hint">{t("settings.reasoningProtocolHint")}</div>
        <label className="set-label">{t("settings.thinkingMode")}</label>
        <select className="mem-select" value={thinking} onChange={(e) => setThinking(normalizeThinkingMode(e.target.value))}>
          {THINKING_MODES.map((mode) => (<option key={mode || "auto"} value={mode}>
              {thinkingModeLabel(mode, t)}
            </option>))}
        </select>
        <div className="mem-hint">{t("settings.thinkingModeHint")}</div>
        <label className="set-label">{t("settings.providerBalanceUrl")}</label>
        <input className="mem-input" placeholder={t("settings.balanceUrlPlaceholder")} value={balanceUrl} onChange={(e) => setBalanceUrl(e.target.value)}/>
        <div className="mem-hint">{t("settings.balanceUrlHint")}</div>
        <label className="set-label">{t("settings.providerContextWindow")}</label>
        <input className="mem-input" inputMode="numeric" min={0} placeholder={t("settings.contextWindowPlaceholder")} type="number" value={ctx} onChange={(e) => setCtx(e.target.value)}/>
        <div className="mem-hint">{t("settings.contextWindowHint")}</div>
      </div>
    </details>);
    return (<div className={`provider-editor${isNewCustomProvider ? " provider-editor--wizard" : ""}`}>
      <label className="set-label">{t("settings.customProviderName")}</label>
      <input className="mem-input" placeholder={t("settings.customProviderNamePlaceholder")} value={name} onChange={(e) => setName(e.target.value)} disabled={!!initial}/>
      <label className="set-label">{t("settings.providerProtocol")}</label>
      <select className="mem-select" value={kind} onChange={(e) => setKind(e.target.value)}>
        {providerKindChoices.map((choice) => (<option key={choice} value={choice}>
            {providerKindLabel(choice, t)}
          </option>))}
      </select>
      <div className="mem-hint">{providerKindHint(effectiveKind, t)}</div>
      <label className="set-label" htmlFor={providerUrlInputId}>
        {t("settings.providerBaseUrlLabel")}
      </label>
      <input id={providerUrlInputId} className="mem-input provider-url-input" aria-describedby={providerUrlHelpId} placeholder={t("settings.providerChatUrlPlaceholder")} value={requestUrl} onChange={(e) => setRequestUrl(e.target.value)}/>
      <div id={providerUrlHelpId} className="mem-hint">
        {t("settings.providerRequestUrlHint")}
      </div>
      {!initial && (<>
          <label className="set-label">{t("settings.providerKey")}</label>
          <input className="mem-input" type="password" placeholder={t("settings.providerKeyPlaceholder")} value={keyDraft} onChange={(e) => setKeyDraft(e.target.value)}/>
        </>)}
      {initial && onSaveKey && apiKeyEnv.trim() && (<>
          <label className="set-label">{t("settings.providerKey")}</label>
          {initial.keySource && (<div className="mem-hint" title={initial.keySourcePath || undefined}>
              {t("settings.keySource", { source: initial.keySource })}
            </div>)}
          <KeyField apiKeyEnv={apiKeyEnv.trim()} busy={busy || fetchingModels} keySet={initial.keySet} onSet={(env, value) => onSaveKey(env, value)}/>
        </>)}
      <div className="provider-model-fetch-row">
        <button type="button" className="btn btn--small" disabled={busy || fetchingModels || !canFetch || extraBodyInvalid} onClick={() => void fetchModels()}>
          {fetchingModels ? t("settings.fetchingModels") : t("settings.testFetchModels")}
        </button>
        <span>{t("settings.testFetchModelsHint")}</span>
      </div>
      {fetchStatus && <div className="provider-fetch-status provider-fetch-status--ok">{fetchStatus}</div>}
      {fetchFallback && <div className="provider-fetch-status provider-fetch-status--warn">{fetchFallback}</div>}
      <label className="set-label">{t("settings.manualModels")}</label>
      <input className="mem-input" placeholder={t("settings.providerModels")} value={models} onChange={(e) => updateManualModels(e.target.value)}/>
      <div className="mem-hint">{t("settings.manualModelsHint")}</div>
      <ProviderEditorModelPicker candidates={modelCandidateNames} selectedModels={modelNames} visionModels={visionModelNames} visionCapability={effectiveVisionCapability} contextWindows={modelContextWindows} disabled={busy || fetchingModels} onToggleModel={toggleEditorModel} onToggleVision={toggleEditorVisionModel} onContextWindowChange={updateEditorModelContextWindow} onSelectAll={selectAllEditorModels} onClear={clearEditorModels}/>
      <ProviderServiceCapabilities supported={effectiveServerWebSearchCapability} models={modelNames} enabled={webSearch} disabled={busy || fetchingModels} onChange={setWebSearch}/>
      {advancedFields}
      <div className="prov-card__actions">
        <button className="btn btn--small" onClick={onCancel} disabled={busy}>
          {t("common.cancel")}
        </button>
        <button className="btn btn--primary btn--small" onClick={() => void save()} disabled={busy || !name.trim() || !effectiveBaseUrl || !models.trim() || extraBodyInvalid}>
          {t("common.save")}
        </button>
      </div>
    </div>);
}

