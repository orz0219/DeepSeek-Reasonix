import { useEffect, useMemo, useRef, useState } from "react";
import { Check, ChevronDown } from "lucide-react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { mergedFetchedProviderModels, providerDefaultModel, providerIsConfigured, providerRequiresKey } from "../lib/providerModels";
import { cachedFetchProviderModels, shouldSkipAutoRefresh } from "../lib/providerModelCache";
import type { ProviderModelCatalogUpdate, ProviderView, SettingsView } from "../lib/types";
import { AnchoredPopover } from "./AnchoredPopover";
import { allRefs, toRef, EFFORT_PRESETS, COMPACT_RATIO_PRESETS, ProxyMode } from "./settings_normalize";
import { ModelsSectionProps, providerAccessGroups, ProviderAccessGroup, SettingsSection, SettingsField, ProvidersSection, providerGroupID, providerGroupLabel } from "./SettingsPanel";
export function ModelsSection({ s, busy, apply, backgroundApply, initialFocus }: ModelsSectionProps) {
    const t = useT();
    const [subtab, setSubtab] = useState<"usage" | "access">(initialFocus?.target === "model-access"
        ? "access"
        : "usage");
    // The command palette may re-target this section while the settings panel is
    // already open (the subtab state is not remounted by a tab change). Each
    // freshly allocated focus request runs this effect once, including repeated
    // requests for the same target after the user changes subtabs.
    useEffect(() => {
        if (initialFocus?.target !== "model-access")
            return;
        setSubtab("access");
    }, [initialFocus?.target, initialFocus?.requestId]);
    const autoRefreshKeyRef = useRef("");
    const autoRefreshGenerationRef = useRef(0);
    const refs = useMemo(() => allRefs(s), [s.providers]);
    const defaultRef = toRef(s.defaultModel, s);
    const plannerRef = toRef(s.plannerModel, s);
    const subagentRef = toRef(s.subagentModel, s);
    const plannerSelectRef = plannerRef === defaultRef ? "" : plannerRef;
    const [defaultProvider] = defaultRef.split("/");
    const defaultProviderView = s.providers.find((p) => p.name === defaultProvider);
    const modelIssue = !defaultProviderView
        ? t("settings.modelUnavailable", { ref: defaultRef || t("common.none") })
        : !providerIsConfigured(defaultProviderView)
            ? t("settings.modelNeedsKey", { provider: modelProviderLabel(defaultProvider, defaultProviderView, t) })
            : "";
    const agent = s.agent ?? { temperature: 0, maxSteps: 0, plannerMaxSteps: 0, maxSubagentDepth: 2, maxSubagentConcurrency: 6, maxParallelWriters: 3, systemPrompt: "", reasoningLanguage: "auto", compactRatio: 0.85 };
    const compactRatio = agent.compactRatio ?? 0.85;
    const compactRatioPercent = Math.round(compactRatio * 1000) / 10;
    const [compactRatioDraft, setCompactRatioDraft] = useState(() => String(compactRatioPercent));
    const [compactRatioCustomOpen, setCompactRatioCustomOpen] = useState(false);
    const compactRatioCustomInputRef = useRef<HTMLInputElement>(null);
    const compactRatioPreset = COMPACT_RATIO_PRESETS.find(([ratio]) => Math.abs(compactRatio - ratio) < 0.0001);
    const compactRatioDraftPercent = Number(compactRatioDraft);
    const compactRatioDraftValid = compactRatioDraft !== ""
        && Number.isFinite(compactRatioDraftPercent)
        && compactRatioDraftPercent >= 65
        && compactRatioDraftPercent <= 85;
    const compactRatioDraftDirty = compactRatioDraftValid
        && Math.abs(compactRatioDraftPercent / 100 - compactRatio) > 0.0001;
    const defaultModel = defaultRef.startsWith(`${defaultProvider}/`) ? defaultRef.slice(defaultProvider.length + 1) : "";
    const modelContextWindow = defaultProviderView?.modelOverrides?.find((override) => override.model === defaultModel)?.contextWindow ?? 0;
    const effectiveContextWindow = modelContextWindow > 0 ? modelContextWindow : (defaultProviderView?.contextWindow ?? 0);
    const compactTokens = effectiveContextWindow > 0 ? Math.round(effectiveContextWindow * compactRatio) : 0;
    const compactRatioImpact = compactTokens > 0
        ? t("settings.compactRatioImpactWithTokens", { percent: compactRatioPercent, tokens: compactTokens.toLocaleString() })
        : t("settings.compactRatioImpact", { percent: compactRatioPercent });
    const compactRatioSelection = compactRatioPreset
        ? t(compactRatioPreset[1])
        : t("settings.compactRatioCustomValue", { percent: compactRatioPercent });
    const compactRatioOverrideHint = agent.compactRatioOverridden
        ? t("settings.compactRatioProjectOverride", { percent: Math.round((agent.effectiveCompactRatio ?? compactRatio) * 100) })
        : "";
    const subagentDepth = Number.isFinite(agent.maxSubagentDepth) && agent.maxSubagentDepth <= 1 ? 1 : 2;
    const subagentConcurrency = Number.isFinite(agent.maxSubagentConcurrency) && agent.maxSubagentConcurrency > 0
        ? Math.max(1, Math.min(32, Math.floor(agent.maxSubagentConcurrency)))
        : 6;
    const parallelWriters = Number.isFinite(agent.maxParallelWriters) && agent.maxParallelWriters > 0
        ? Math.max(1, Math.min(subagentConcurrency, Math.floor(agent.maxParallelWriters)))
        : Math.min(3, subagentConcurrency);
    useEffect(() => {
        setCompactRatioDraft(String(compactRatioPercent));
    }, [compactRatioPercent]);
    useEffect(() => {
        if (compactRatioCustomOpen)
            compactRatioCustomInputRef.current?.focus();
    }, [compactRatioCustomOpen]);
    const persistCompactRatio = async (ratio: number) => {
        if (await apply(() => app.SetCompactRatio(ratio)))
            setCompactRatioCustomOpen(false);
    };
    const openCompactRatioCustom = () => {
        setCompactRatioDraft(String(compactRatioPercent));
        setCompactRatioCustomOpen(true);
    };
    const closeCompactRatioCustom = () => {
        setCompactRatioDraft(String(compactRatioPercent));
        setCompactRatioCustomOpen(false);
    };
    const selectCompactRatioPreset = async (ratio: number) => {
        if (Math.abs(compactRatio - ratio) < 0.0001) {
            closeCompactRatioCustom();
            return;
        }
        await persistCompactRatio(ratio);
    };
    const saveCompactRatioDraft = async () => {
        if (!compactRatioDraftValid || !compactRatioDraftDirty || busy)
            return;
        await persistCompactRatio(compactRatioDraftPercent / 100);
    };
    useEffect(() => {
        const generation = ++autoRefreshGenerationRef.current;
        let cancelled = false;
        const stale = () => cancelled || autoRefreshGenerationRef.current !== generation;
        if (subtab !== "usage")
            return;
        const groups = providerAccessGroups(s.providers.filter((p) => p.added), t);
        const candidates = groups
            .map((group) => {
            const provider = group.providers.find((p) => providerIsConfigured(p) && p.baseUrl);
            return provider ? { group, provider } : null;
        })
            .filter((item): item is {
            group: ProviderAccessGroup;
            provider: ProviderView;
        } => Boolean(item));
        // The backend token covers provider identity, current catalog, headers,
        // and credential revision without persisting sensitive header values in
        // sessionStorage. Older payloads without the token simply skip this
        // opportunistic background refresh; manual refresh remains available.
        if (candidates.some(({ provider }) => !provider.modelCatalogFingerprint?.trim()))
            return;
        const refreshKey = candidates.map(({ group, provider }) => JSON.stringify([
            group.id,
            provider.modelCatalogFingerprint!.trim(),
        ])).join("|");
        if (!refreshKey || autoRefreshKeyRef.current === refreshKey)
            return;
        // Session-level cooldown per provider set: reopening the panel does not
        // refetch the same providers, while a changed set refreshes immediately.
        const autoRefreshStorageKey = `settings-auto-refresh-at:${refreshKey}`;
        const lastAutoRefresh = sessionStorage.getItem(autoRefreshStorageKey);
        if (lastAutoRefresh && Date.now() - Number(lastAutoRefresh) < 30000)
            return;
        // Respect slow network hints; background model-list refresh can wait.
        if (shouldSkipAutoRefresh())
            return;
        autoRefreshKeyRef.current = refreshKey;
        sessionStorage.setItem(autoRefreshStorageKey, String(Date.now()));
        void backgroundApply(async () => {
            // Batch-fetch models for all candidates in one round-trip.
            const providersToFetch = candidates.map((c) => c.provider).filter((p) => p.models && p.models.length > 0);
            let batchResults: Record<string, string[]> = {};
            try {
                batchResults = await app.FetchAllProviderModels(providersToFetch) as Record<string, string[]>;
            }
            catch {
                // Batch failed entirely — fall back to per-provider cached calls below.
            }
            if (stale())
                return;
            const updates: ProviderModelCatalogUpdate[] = [];
            for (const { provider } of candidates) {
                if (stale())
                    return;
                if (!provider.models || provider.models.length === 0)
                    continue;
                try {
                    const fetched = batchResults[provider.name]
                        ?? await cachedFetchProviderModels((p) => app.FetchProviderModels(p), provider);
                    if (stale())
                        return;
                    if (!fetched || fetched.length === 0)
                        continue;
                    const models = mergedFetchedProviderModels(provider.models, fetched, { preserveCurated: true });
                    const currentDefault = providerDefaultModel(provider.default, models);
                    const visionModels = provider.visionModels.filter((model) => models.includes(model));
                    if (sameStringList(provider.models, models) && provider.default === currentDefault && sameStringList(provider.visionModels, visionModels))
                        continue;
                    const expectedFingerprint = provider.modelCatalogFingerprint?.trim() ?? "";
                    if (!expectedFingerprint)
                        continue;
                    updates.push({ name: provider.name, expectedFingerprint, models, default: currentDefault, visionModels });
                }
                catch {
                    // Background discovery is opportunistic; manual refresh shows errors.
                }
            }
            if (updates.length > 0) {
                try {
                    if (stale())
                        return;
                    // Compare and apply narrow catalog updates in one transaction.
                    await app.SaveProviderModelCatalogs(updates);
                }
                catch {
                    // Background discovery is opportunistic; explicit edits show errors.
                }
            }
        });
        return () => {
            cancelled = true;
            if (autoRefreshGenerationRef.current === generation)
                autoRefreshGenerationRef.current += 1;
        };
    }, [backgroundApply, s.providers, subtab, t]);
    return (<>
      <div className="settings-subtabs">
        <button type="button" className={`settings-subtab${subtab === "usage" ? " settings-subtab--active" : ""}`} aria-selected={subtab === "usage"} onClick={() => setSubtab("usage")}>
          {t("settings.modelTab.usage")}
        </button>
        <button type="button" className={`settings-subtab${subtab === "access" ? " settings-subtab--active" : ""}`} aria-selected={subtab === "access"} onClick={() => setSubtab("access")}>
          {t("settings.modelTab.access")}
        </button>
      </div>

      {subtab === "usage" ? (<>
          <SettingsSection title={t("settings.modelUsage")}>
            <SettingsField label={t("settings.defaultModel")} hint={t("settings.defaultModelHint")}>
              <ModelPicker s={s} refs={refs} value={toRef(s.defaultModel, s)} disabled={busy} onPick={(ref) => void apply(() => app.SetDefaultModel(ref))}/>
            </SettingsField>

            <SettingsField label={t("settings.plannerModel")}>
              <ModelPicker s={s} refs={refs} value={plannerSelectRef} disabled={busy} includeSameDefault onPick={(ref) => void apply(() => app.SetPlannerModel(ref))}/>
            </SettingsField>

            <SettingsField label={t("settings.subagentModel")}>
              <ModelPicker s={s} refs={refs} value={subagentRef} disabled={busy} emptyOptionLabel={t("settings.subagentModelDefault")} emptyOptionHint={t("common.auto")} onPick={(ref) => void apply(() => app.SetSubagentModel(ref))}/>
            </SettingsField>

            <SettingsField label={t("settings.subagentEffort")} hint={t("settings.subagentHint")}>
              <select className="mem-select set-grow" value={s.subagentEffort || ""} disabled={busy} onChange={(e) => void apply(() => app.SetSubagentEffort(e.target.value))}>
                <option value="">{t("settings.subagentEffortDefault")}</option>
                {EFFORT_PRESETS.map((level) => (<option key={level} value={level}>
                    {level}
                  </option>))}
              </select>
            </SettingsField>

            <SettingsField label={t("settings.subagentDepth")} hint={t("settings.subagentDepthHint")}>
              <div className="provider-add-segmented" role="group" aria-label={t("settings.subagentDepth")}>
                {[1, 2].map((depth) => (<button key={depth} type="button" className={subagentDepth === depth ? "provider-add-segmented__item provider-add-segmented__item--active" : "provider-add-segmented__item"} disabled={busy} aria-pressed={subagentDepth === depth} onClick={() => void apply(() => app.SetMaxSubagentDepth(depth))}>
                    {depth === 1 ? t("settings.subagentDepthOne") : t("settings.subagentDepthTwo")}
                  </button>))}
              </div>
            </SettingsField>

            <SettingsField label={t("settings.subagentConcurrency")} hint={t("settings.subagentConcurrencyHint")}>
              <input className="mem-input" type="number" min={1} max={32} value={subagentConcurrency} disabled={busy} onChange={(e) => {
                const n = Number(e.target.value);
                if (!Number.isFinite(n))
                    return;
                void apply(() => app.SetMaxSubagentConcurrency(n));
            }}/>
            </SettingsField>

            <SettingsField label={t("settings.parallelWriters")} hint={t("settings.parallelWritersHint")}>
              <input className="mem-input" type="number" min={1} max={subagentConcurrency} value={parallelWriters} disabled={busy} onChange={(e) => {
                const n = Number(e.target.value);
                if (!Number.isFinite(n))
                    return;
                void apply(() => app.SetMaxParallelWriters(n));
            }}/>
            </SettingsField>

            {modelIssue && <div className="provider-fetch-banner provider-fetch-banner--warn">{modelIssue}</div>}
          </SettingsSection>
          <SettingsSection title={t("settings.agentRuntime")} description={t("settings.agentRuntimeHint")}>
            <SettingsField label={t("settings.reasoningLanguage")} hint={t("settings.reasoningLanguageHint")}>
              <div className="set-seg">
                {(["auto", "zh", "en"] as const).map((lang) => (<button key={lang} className={`set-seg__btn${agent.reasoningLanguage === lang ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => void apply(() => app.SetReasoningLanguage(lang))}>
                    {t(`settings.reasoningLanguage.${lang}`)}
                  </button>))}
              </div>
            </SettingsField>
            <SettingsField label={t("settings.compactRatio")} hint={t("settings.compactRatioHint")} stacked>
              <div className="compact-ratio-controls">
                <div className="set-seg compact-ratio-presets" role="group" aria-label={t("settings.compactRatio")}>
                  {COMPACT_RATIO_PRESETS.map(([ratio, labelKey]) => (<button key={ratio} type="button" className={`set-seg__btn${Math.abs(compactRatio - ratio) < 0.0001 ? " set-seg__btn--on" : ""}`} disabled={busy} aria-label={t(labelKey)} aria-pressed={Math.abs(compactRatio - ratio) < 0.0001} onClick={() => void selectCompactRatioPreset(ratio)}>
                      <span className="compact-ratio-preset__percent" aria-hidden="true">{Math.round(ratio * 100)}%</span>
                      <span className="compact-ratio-preset__caption" aria-hidden="true">{t(labelKey).split(" · ")[1]}</span>
                    </button>))}
                </div>
                <div className="compact-ratio-summary">
                  <div className="compact-ratio-current">{t("settings.compactRatioCurrent", { value: compactRatioSelection })}</div>
                  <button type="button" className="btn btn--small compact-ratio-custom-toggle" disabled={busy} aria-expanded={compactRatioCustomOpen} aria-controls="settings-compact-ratio-custom-panel" onClick={compactRatioCustomOpen ? closeCompactRatioCustom : openCompactRatioCustom}>
                    {t("settings.compactRatioCustomOption")}
                  </button>
                </div>
                <div className="compact-ratio-impact">{compactRatioImpact}</div>
                {compactRatioCustomOpen && (<div id="settings-compact-ratio-custom-panel" className="compact-ratio-custom-panel">
                    <div className="settings-inline-controls compact-ratio-custom">
                      <label className="set-label" htmlFor="settings-compact-ratio-custom">{t("settings.compactRatioCustom")}</label>
                      <input ref={compactRatioCustomInputRef} id="settings-compact-ratio-custom" className="mem-input set-narrow" type="number" min={65} max={85} step={0.1} inputMode="decimal" value={compactRatioDraft} disabled={busy} aria-label={t("settings.compactRatioCustomAria")} aria-describedby="settings-compact-ratio-custom-hint" aria-invalid={!compactRatioDraftValid} onInput={(event) => setCompactRatioDraft(event.currentTarget.value)} onKeyDown={(event) => {
                    if (event.key === "Enter") {
                        event.preventDefault();
                        void saveCompactRatioDraft();
                    }
                    if (event.key === "Escape") {
                        event.preventDefault();
                        closeCompactRatioCustom();
                    }
                }}/>
                      <span className="compact-ratio-custom__suffix" aria-hidden="true">%</span>
                      <button type="button" className="btn btn--small" disabled={busy || !compactRatioDraftValid || !compactRatioDraftDirty} onClick={() => void saveCompactRatioDraft()}>
                        {t("settings.compactRatioApply")}
                      </button>
                      <button type="button" className="btn btn--small" disabled={busy} onClick={closeCompactRatioCustom}>
                        {t("common.cancel")}
                      </button>
                    </div>
                    <div id="settings-compact-ratio-custom-hint" className={`compact-ratio-custom__hint${compactRatioDraftValid ? "" : " compact-ratio-custom__hint--invalid"}`}>
                      {t("settings.compactRatioCustomHint")}
                    </div>
                  </div>)}
              </div>
            </SettingsField>
            {compactRatioOverrideHint && <div className="provider-fetch-banner provider-fetch-banner--warn">{compactRatioOverrideHint}</div>}
          </SettingsSection>
        </>) : (<ProvidersSection s={s} busy={busy} apply={apply}/>)}
    </>);
}
type ModelPickerOption = {
    ref: string;
    provider: string;
    model: string;
    providerView?: ProviderView;
};
export function ModelPicker({ s, refs, value, disabled, includeSameDefault = false, ariaLabel, emptyOptionLabel, emptyOptionHint, onPick, }: {
    s: SettingsView;
    refs: string[];
    value: string;
    disabled: boolean;
    includeSameDefault?: boolean;
    ariaLabel?: string;
    emptyOptionLabel?: string;
    emptyOptionHint?: string;
    onPick: (ref: string) => void;
}) {
    const t = useT();
    const [open, setOpen] = useState(false);
    const [query, setQuery] = useState("");
    const [debouncedQuery, setDebouncedQuery] = useState("");
    const triggerRef = useRef<HTMLButtonElement>(null);
    // Debounce search to avoid expensive filtering on every keystroke
    useEffect(() => {
        const timer = setTimeout(() => setDebouncedQuery(query), 150);
        return () => clearTimeout(timer);
    }, [query]);
    const q = debouncedQuery.trim().toLowerCase();
    const emptyLabel = includeSameDefault ? t("settings.plannerNone") : emptyOptionLabel;
    const emptyHint = includeSameDefault ? t("settings.plannerNoneHint") : emptyOptionHint;
    const emptyMeta = includeSameDefault ? t("settings.plannerNoneHintShort") : emptyOptionHint;
    const selected = refs.includes(value) ? modelOptionFromRef(value, s) : null;
    const selectedLabel = value === "" && emptyLabel
        ? emptyLabel
        : selected?.model || value || t("common.none");
    const selectedMeta = value === "" && emptyLabel
        ? emptyMeta || ""
        : selected
            ? modelOptionMeta(selected, t)
            : t("settings.noModelsConfigured");
    const emptyOptionVisible = Boolean(emptyLabel) && (!q || `${emptyLabel} ${emptyHint || ""}`.toLowerCase().includes(q));
    const groups = useMemo(() => {
        const providerOrder: string[] = [];
        const providerSeen = new Set<string>();
        for (const p of s.providers) {
            const id = providerGroupID(p);
            if (!providerSeen.has(id)) {
                providerOrder.push(id);
                providerSeen.add(id);
            }
        }
        const options = refs
            .map((ref) => modelOptionFromRef(ref, s))
            .filter((opt): opt is ModelPickerOption => Boolean(opt))
            .filter((opt) => !q || `${opt.ref} ${opt.provider} ${modelProviderLabel(opt.provider, opt.providerView, t)} ${opt.model}`.toLowerCase().includes(q));
        for (const opt of options) {
            const groupID = modelOptionGroupID(opt);
            if (!providerSeen.has(groupID)) {
                providerOrder.push(groupID);
                providerSeen.add(groupID);
            }
        }
        return providerOrder
            .map((groupID) => {
            const providerViews = s.providers.filter((p) => providerGroupID(p) === groupID);
            const firstProvider = providerViews[0];
            return {
                groupID,
                label: firstProvider ? providerGroupLabel(firstProvider, t) : groupID,
                keySet: providerViews.some((p) => p.keySet),
                requiresKey: providerViews.every((p) => providerRequiresKey(p)),
                options: uniqueModelOptions(options.filter((opt) => modelOptionGroupID(opt) === groupID)),
            };
        })
            .filter((group) => group.options.length > 0);
    }, [q, refs, s, t]);
    useEffect(() => {
        if (!open)
            setQuery("");
    }, [open]);
    const pick = (ref: string) => {
        setOpen(false);
        if (ref !== value)
            onPick(ref);
    };
    return (<div className="settings-model-picker">
      <button ref={triggerRef} type="button" className="settings-model-picker__trigger" disabled={disabled || (!includeSameDefault && !emptyOptionLabel && refs.length === 0)} aria-label={ariaLabel} aria-haspopup="listbox" aria-expanded={open} onClick={() => setOpen((next) => !next)}>
        <span className="settings-model-picker__selected">
          <span>{selectedLabel}</span>
          <small>{selectedMeta}</small>
        </span>
        <ChevronDown size={16} className={`settings-model-picker__chev${open ? " settings-model-picker__chev--open" : ""}`}/>
      </button>
      <AnchoredPopover open={open && !disabled} anchorRef={triggerRef} onClose={() => setOpen(false)} className="settings-model-picker__menu" placement="bottom" style={{ width: triggerRef.current?.getBoundingClientRect().width }}>
        <div className="settings-model-picker__search">
          <input value={query} placeholder={t("settings.searchModels")} onChange={(e) => setQuery(e.target.value)} autoFocus/>
        </div>
        <div className="settings-model-picker__list" role="listbox">
          {emptyOptionVisible && (<button type="button" role="option" aria-selected={value === ""} className={`settings-model-picker__option settings-model-picker__option--pinned${value === "" ? " settings-model-picker__option--selected" : ""}`} onClick={() => pick("")}>
              <span>
                <strong>{emptyLabel}</strong>
                {emptyHint && <small>{emptyHint}</small>}
              </span>
              {value === "" && <Check size={14}/>}
            </button>)}
          {groups.map((group) => (<div className="settings-model-picker__group" key={group.groupID}>
              <div className="settings-model-picker__group-title">
                <span>{group.label}</span>
                <small>{providerKeyStatusLabel(group, t)}</small>
              </div>
              {group.options.map((opt) => (<button key={opt.ref} type="button" role="option" aria-selected={opt.ref === value} className={`settings-model-picker__option${opt.ref === value ? " settings-model-picker__option--selected" : ""}`} onClick={() => pick(opt.ref)}>
                  <span>
                    <strong>{opt.model}</strong>
                    <small>{modelOptionMeta(opt, t)}</small>
                  </span>
                  {opt.ref === value && <Check size={14}/>}
                </button>))}
            </div>))}
          {!emptyOptionVisible && groups.length === 0 && <div className="settings-model-picker__empty">{t("settings.noMatchingModels")}</div>}
        </div>
      </AnchoredPopover>
    </div>);
}
function modelOptionFromRef(ref: string, s: SettingsView): ModelPickerOption | null {
    if (!ref)
        return null;
    const [provider, ...modelParts] = ref.split("/");
    const model = modelParts.join("/") || ref;
    return {
        ref,
        provider,
        model,
        providerView: s.providers.find((p) => p.name === provider),
    };
}
function modelOptionMeta(option: ModelPickerOption, t: ReturnType<typeof useT>): string {
    const key = option.providerView ? providerKeyStatusLabel(option.providerView, t) : t("settings.noKey");
    return `${modelProviderLabel(option.provider, option.providerView, t)} · ${key}`;
}
export function providerKeyStatusLabel(provider: {
    keySet: boolean;
    requiresKey?: boolean;
    apiKeyEnv?: string;
}, t: ReturnType<typeof useT>): string {
    if (!providerRequiresKey(provider))
        return t("settings.noKeyRequired");
    return provider.keySet ? t("settings.keySet") : t("settings.noKey");
}
export function modelProviderLabel(provider: string, providerView: ProviderView | undefined, t: ReturnType<typeof useT>): string {
    return providerView ? providerGroupLabel(providerView, t) : provider;
}
function modelOptionGroupID(option: ModelPickerOption): string {
    return option.providerView ? providerGroupID(option.providerView) : `custom:${option.provider}`;
}
function uniqueModelOptions(options: ModelPickerOption[]): ModelPickerOption[] {
    const seen = new Set<string>();
    const out: ModelPickerOption[] = [];
    for (const option of options) {
        if (seen.has(option.model))
            continue;
        seen.add(option.model);
        out.push(option);
    }
    return out;
}
function sameStringList(a: string[], b: string[]): boolean {
    if (a.length !== b.length)
        return false;
    return a.every((value, i) => value === b[i]);
}
export function proxyModeLabel(mode: ProxyMode, t: ReturnType<typeof useT>): string {
    switch (mode) {
        case "auto":
            return t("settings.proxyMode.auto");
        case "custom":
            return t("settings.proxyMode.custom");
        case "off":
            return t("settings.proxyMode.off");
    }
}

