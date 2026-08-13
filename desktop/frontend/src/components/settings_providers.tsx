import { startTransition, useEffect, useMemo, useRef, useState } from "react";
import { ArrowRight, MoreHorizontal, Trash2 } from "lucide-react";
import { asArray } from "../lib/array";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { createLatestRequestGate, inferredVisionModels, mergedFetchedProviderModels, providerDefaultModel, providerIsConfigured, providerModelCandidates } from "../lib/providerModels";
import { cachedFetchProviderModels, invalidateProviderCacheByAPIKeyEnv } from "../lib/providerModelCache";
import type { ProviderPresetView, ProviderView } from "../lib/types";
import { InlineConfirmButton } from "./InlineConfirmButton";
import { Tooltip } from "./Tooltip";
import { AnchoredPopover } from "./AnchoredPopover";
import { toRef, normalizeProviderPresetStatus } from "./settings_normalize";
import { providerKeyStatusLabel } from "./settings_models";
import { SectionProps, SettingsSection } from "./SettingsPanel";
import { AddProviderMode, ProviderFetchResult, ProviderModelDraft, providerAccessGroups, providerVisionCapabilityForView, ProviderAccessGroup, uniqueStrings, OfficialProviderKind, ProviderTemplateChoice, OFFICIAL_PROVIDER_CHOICES, officialProviderKind, providerPresetLabel, providerPresetDescription, providerTemplateCanAdd, providerTemplateStatusBadge, providerTemplateConflictProviderName, providerTemplateStatusClass, providerTemplateActionLabel, providerSupportsServerWebSearchForView, canonicalOfficialProviderName, ProviderModelSummary, ProviderModelDraftPicker, ProviderServiceCapabilities, ProviderTechnicalDetails } from "./settings_provider_helpers";
import { ProviderEditor } from "./settings_provider_editor";
export function ProvidersSection({ s, busy, apply }: SectionProps) {
    const t = useT();
    const defaultProvider = toRef(s.defaultModel, s).split("/")[0];
    const [editing, setEditing] = useState<string | null>(null);
    const [adding, setAdding] = useState<AddProviderMode>(null);
    const [revealedProvider, setRevealedProvider] = useState<string | null>(null);
    const [fetchingProviders, setFetchingProviders] = useState<Set<string>>(() => new Set());
    const fetchGate = useMemo(createLatestRequestGate, []);
    const [fetchResults, setFetchResults] = useState<Record<string, ProviderFetchResult>>({});
    const [modelDrafts, setModelDrafts] = useState<Record<string, ProviderModelDraft>>({});
    const visibleProviders = useMemo(() => s.providers.filter((p) => p.added || p.name === revealedProvider), [s.providers, revealedProvider]);
    const groups = useMemo(() => providerAccessGroups(visibleProviders, t), [visibleProviders, t]);
    useEffect(() => {
        if (revealedProvider && !s.providers.some((p) => p.name === revealedProvider)) {
            setRevealedProvider(null);
            if (editing === revealedProvider)
                setEditing(null);
        }
    }, [editing, revealedProvider, s.providers]);
    const setGroupFetchResult = (groupID: string, result: ProviderFetchResult | null) => {
        setFetchResults((prev) => {
            const next = { ...prev };
            if (result)
                next[groupID] = result;
            else
                delete next[groupID];
            return next;
        });
    };
    const setGroupModelDraft = (groupID: string, draft: ProviderModelDraft | null) => {
        setModelDrafts((prev) => {
            const next = { ...prev };
            if (draft)
                next[groupID] = draft;
            else
                delete next[groupID];
            return next;
        });
    };
    const beginGroupFetch = (groupID: string): number => {
        const generation = fetchGate.begin(groupID);
        setFetchingProviders((current) => {
            if (current.has(groupID))
                return current;
            const next = new Set(current);
            next.add(groupID);
            return next;
        });
        return generation;
    };
    const groupFetchIsCurrent = (groupID: string, generation: number): boolean => (fetchGate.isCurrent(groupID, generation));
    const finishGroupFetch = (groupID: string, generation: number) => {
        if (!groupFetchIsCurrent(groupID, generation))
            return;
        setFetchingProviders((current) => {
            if (!current.has(groupID))
                return current;
            const next = new Set(current);
            next.delete(groupID);
            return next;
        });
    };
    const cancelGroupFetch = (groupID: string) => {
        fetchGate.cancel(groupID);
        setFetchingProviders((current) => {
            if (!current.has(groupID))
                return current;
            const next = new Set(current);
            next.delete(groupID);
            return next;
        });
    };
    const modelDraftForFetch = (p: ProviderView, fetched: string[]): ProviderModelDraft => {
        const candidates = providerModelCandidates(p.models, fetched);
        const selected = mergedFetchedProviderModels(p.models, fetched, { preserveCurated: true });
        const visionCapability = providerVisionCapabilityForView(p);
        const visionSource = visionCapability === "unsupported"
            ? []
            : (p.visionModelsConfigured ? p.visionModels : inferredVisionModels(candidates));
        return {
            providerName: p.name,
            candidates,
            selected: candidates.filter((model) => selected.includes(model)),
            visionModels: candidates.filter((model) => visionSource.includes(model)),
            visionCapability,
        };
    };
    const updateModelDraftSelection = (groupID: string, nextSelected: (draft: ProviderModelDraft) => string[]) => {
        setModelDrafts((prev) => {
            const draft = prev[groupID];
            if (!draft)
                return prev;
            const selectedSet = new Set(nextSelected(draft));
            return {
                ...prev,
                [groupID]: {
                    ...draft,
                    selected: draft.candidates.filter((model) => selectedSet.has(model)),
                },
            };
        });
    };
    const toggleModelDraftVision = (groupID: string, model: string) => {
        setModelDrafts((prev) => {
            const draft = prev[groupID];
            if (!draft)
                return prev;
            return {
                ...prev,
                [groupID]: {
                    ...draft,
                    visionModels: draft.visionModels.includes(model)
                        ? draft.visionModels.filter((candidate) => candidate !== model)
                        : draft.candidates.filter((candidate) => candidate === model || draft.visionModels.includes(candidate)),
                },
            };
        });
    };
    const refreshModels = async (group: ProviderAccessGroup, p: ProviderView) => {
        const generation = beginGroupFetch(group.id);
        setGroupFetchResult(group.id, null);
        setGroupModelDraft(group.id, null);
        try {
            let fetched: string[];
            try {
                fetched = await cachedFetchProviderModels((provider) => app.FetchProviderModels(provider), p, true);
            }
            catch (e) {
                if (!groupFetchIsCurrent(group.id, generation))
                    return;
                setGroupFetchResult(group.id, {
                    kind: "warn",
                    text: t("settings.fetchModelsFailedForProvider", { provider: group.label, err: String((e as Error)?.message ?? e) }),
                });
                return;
            }
            if (!groupFetchIsCurrent(group.id, generation))
                return;
            if (fetched.length === 0) {
                setGroupFetchResult(group.id, {
                    kind: "warn",
                    text: t("settings.fetchModelsEmptyForProvider", { provider: group.label }),
                });
                return;
            }
            const draft = modelDraftForFetch(p, fetched);
            startTransition(() => {
                setGroupModelDraft(group.id, draft);
                setGroupFetchResult(group.id, {
                    kind: "ok",
                    text: t("settings.fetchModelsReadyForProvider", { provider: group.label, n: draft.candidates.length }),
                });
            });
        }
        finally {
            finishGroupFetch(group.id, generation);
        }
    };
    const saveKeyEnvAndAutoRefresh = async (group: ProviderAccessGroup, apiKeyEnv: string, value: string) => {
        const probe = group.providers[0];
        if (!probe || !apiKeyEnv)
            return;
        const generation = beginGroupFetch(group.id);
        setGroupFetchResult(group.id, null);
        setGroupModelDraft(group.id, null);
        try {
            await apply(async () => {
                await app.SaveProviderKey(apiKeyEnv, value);
                invalidateProviderCacheByAPIKeyEnv(apiKeyEnv);
                try {
                    const fetched = await cachedFetchProviderModels((provider) => app.FetchProviderModels(provider), { ...probe, apiKeyEnv });
                    if (!groupFetchIsCurrent(group.id, generation))
                        return;
                    if (fetched.length > 0) {
                        const draft = modelDraftForFetch({ ...probe, apiKeyEnv }, fetched);
                        setGroupModelDraft(group.id, draft);
                        setGroupFetchResult(group.id, {
                            kind: "ok",
                            text: t("settings.fetchModelsReadyForProvider", { provider: group.label, n: draft.candidates.length }),
                        });
                        return;
                    }
                    setGroupFetchResult(group.id, {
                        kind: "warn",
                        text: t("settings.fetchModelsEmptyForProvider", { provider: group.label }),
                    });
                }
                catch (e) {
                    if (!groupFetchIsCurrent(group.id, generation))
                        return;
                    setGroupFetchResult(group.id, {
                        kind: "warn",
                        text: t("settings.fetchModelsAfterKeyFailedForProvider", { provider: group.label, err: String((e as Error)?.message ?? e) }),
                    });
                }
            });
        }
        finally {
            finishGroupFetch(group.id, generation);
        }
    };
    const saveProviderKey = async (group: ProviderAccessGroup, apiKeyEnv: string, value: string) => {
        if (!apiKeyEnv)
            return;
        cancelGroupFetch(group.id);
        setGroupFetchResult(group.id, null);
        setGroupModelDraft(group.id, null);
        await apply(async () => {
            const warning = await app.SetProviderKey(apiKeyEnv, value);
            invalidateProviderCacheByAPIKeyEnv(apiKeyEnv);
            return warning;
        });
    };
    const clearProviderKey = async (group: ProviderAccessGroup, apiKeyEnv: string) => {
        if (!apiKeyEnv)
            return;
        cancelGroupFetch(group.id);
        await apply(async () => {
            await app.ClearProviderKey(apiKeyEnv);
            invalidateProviderCacheByAPIKeyEnv(apiKeyEnv);
        });
    };
    const saveProvider = async (provider: ProviderView, key: string) => {
        if (key) {
            const warning = await app.SaveProviderWithKey(provider, key);
            invalidateProviderCacheByAPIKeyEnv(provider.apiKeyEnv);
            return warning;
        }
        await app.SaveProvider(provider);
    };
    const saveModelDraft = async (group: ProviderAccessGroup) => {
        const draft = modelDrafts[group.id];
        const provider = draft ? group.providers.find((p) => p.name === draft.providerName) : null;
        const models = uniqueStrings(draft?.selected ?? []);
        const visionModels = uniqueStrings(draft?.visionModels ?? []).filter((model) => models.includes(model));
        if (!draft || !provider || models.length === 0)
            return;
        let saved = false;
        await apply(async () => {
            await app.SaveProvider({
                ...provider,
                models,
                visionModels: draft.visionCapability === "unsupported" ? [] : visionModels,
                visionModelsConfigured: true,
                default: providerDefaultModel(provider.default, models),
            });
            saved = true;
        });
        if (!saved)
            return;
        setGroupModelDraft(group.id, null);
        setGroupFetchResult(group.id, {
            kind: "ok",
            text: t("settings.enabledModelsSavedForProvider", { provider: group.label, n: models.length }),
        });
    };
    return (<SettingsSection title={t("settings.providerAccess")} description={t("settings.providerAccessHint")} actions={<button className="btn btn--small" disabled={busy || adding !== null} onClick={() => setAdding("official")}>
          {t("settings.addProvider")}
        </button>}>
      <div className="provider-access-grid">
        {groups.length === 0 && adding === null && (<div className="provider-empty">
            <strong>{t("settings.providerAccessEmptyTitle")}</strong>
            <span>{t("settings.providerAccessEmptyHint")}</span>
            <div className="provider-empty__actions">
              <button type="button" className="btn btn--small" disabled={busy} onClick={() => setAdding("official")}>
                {t("settings.addProvider.officialChoice")}
              </button>
              <button type="button" className="btn btn--small" disabled={busy} onClick={() => setAdding("custom")}>
                {t("settings.addProvider.customChoice")}
              </button>
            </div>
          </div>)}
        {adding !== null && (<AddProviderPanel mode={adding} kinds={s.providerKinds} officialProviders={s.officialProviders} providerPresets={s.providerPresets} busy={busy} onMode={setAdding} onCancel={() => setAdding(null)} onAddOfficial={(kind, key) => apply(() => app.AddOfficialProviderAccess(kind, key)).then(() => setAdding(null))} onAddPreset={(id, key) => apply(() => app.AddProviderPresetAccess(id, key)).then(() => setAdding(null))} onViewPresetConflict={(providerName) => {
                setRevealedProvider(providerName);
                setEditing(providerName);
                setAdding(null);
            }} onResetPreset={(id) => apply(() => app.ResetProviderPresetAccess(id)).then(() => setAdding(null))} onAddCustom={(pv, key) => apply(() => saveProvider(pv, key ?? "")).then(() => setAdding(null))}/>)}
        {adding === null && groups.map((group) => (<ProviderAccessCard key={group.id} group={group} busy={busy} fetching={fetchingProviders.has(group.id)} fetchResult={fetchResults[group.id]} modelDraft={modelDrafts[group.id]} defaultProvider={defaultProvider} editing={editing} kinds={s.providerKinds} onEdit={setEditing} onCancelEdit={() => setEditing(null)} onSave={(pv, key) => {
                cancelGroupFetch(group.id);
                return apply(() => saveProvider(pv, key ?? "")).then(() => {
                    setEditing(null);
                    setGroupModelDraft(group.id, null);
                });
            }} onRefresh={(provider) => void refreshModels(group, provider)} onToggleDraftModel={(model) => updateModelDraftSelection(group.id, (draft) => (draft.selected.includes(model)
                ? draft.selected.filter((candidate) => candidate !== model)
                : [...draft.selected, model]))} onToggleDraftVision={(model) => toggleModelDraftVision(group.id, model)} onSelectAllDraftModels={() => updateModelDraftSelection(group.id, (draft) => draft.candidates)} onClearDraftModels={() => updateModelDraftSelection(group.id, () => [])} onCancelDraftModels={() => {
                setGroupModelDraft(group.id, null);
                setGroupFetchResult(group.id, null);
            }} onSaveDraftModels={() => void saveModelDraft(group)} onToggleWebSearch={(enabled) => {
                const providerNames = group.providers.map((provider) => provider.name);
                if (providerNames.length === 0)
                    return;
                void apply(() => app.SetProviderWebSearch(providerNames, enabled));
            }} onUpgradeRecommended={(name) => {
                cancelGroupFetch(group.id);
                return apply(() => app.UpgradeDeepSeekProviderAccess(name)).then((upgraded) => {
                    if (upgraded) {
                        setEditing(null);
                        setGroupModelDraft(group.id, null);
                    }
                });
            }} onSaveEditorKey={(env, value) => group.builtIn ? saveProviderKey(group, env, value) : saveKeyEnvAndAutoRefresh(group, env, value)} onClearEditorKey={(env) => clearProviderKey(group, env)} onDelete={(providers) => {
                cancelGroupFetch(group.id);
                const providerNames = providers.map(({ name }) => name);
                return apply(() => app.RemoveProviderAccesses(providerNames)).then(() => {
                    if (revealedProvider && providerNames.includes(revealedProvider)) {
                        setRevealedProvider(null);
                        setEditing(null);
                    }
                });
            }}/>))}
      </div>
    </SettingsSection>);
}
export function AddProviderPanel({ mode, kinds, officialProviders, providerPresets, busy, onMode, onCancel, onAddOfficial, onAddPreset, onViewPresetConflict, onResetPreset, onAddCustom, }: {
    mode: AddProviderMode;
    kinds: string[];
    officialProviders: ProviderView[];
    providerPresets: ProviderPresetView[];
    busy: boolean;
    onMode: (mode: AddProviderMode) => void;
    onCancel: () => void;
    onAddOfficial: (kind: OfficialProviderKind, key: string) => Promise<void>;
    onAddPreset: (id: string, key: string) => Promise<void>;
    onViewPresetConflict: (providerName: string) => void;
    onResetPreset: (id: string) => Promise<void>;
    onAddCustom: (p: ProviderView, key?: string) => void | Promise<void>;
}) {
    const t = useT();
    const templateChoices = useMemo<ProviderTemplateChoice[]>(() => [
        ...OFFICIAL_PROVIDER_CHOICES.map((choice) => {
            const state = officialProviders.find((provider) => officialProviderKind(provider) === choice.kind);
            return {
                id: `official:${choice.kind}`,
                source: "official" as const, kind: choice.kind,
                label: t(choice.labelKey), description: t(choice.descKey),
                keyEnv: state?.apiKeyEnv || choice.keyEnv,
                added: Boolean(state?.added), keySet: Boolean(state?.keySet),
            };
        }),
        ...providerPresets.map((preset) => ({
            id: `preset:${preset.id}`,
            source: "preset" as const,
            presetID: preset.id,
            label: providerPresetLabel(preset, t),
            description: providerPresetDescription(preset, t),
            keyEnv: preset.keyEnv,
            added: preset.added,
            status: normalizeProviderPresetStatus(preset.status, preset.added),
            statusProviderNames: asArray(preset.statusProviderNames),
            keySet: preset.keySet,
        })),
    ], [officialProviders, providerPresets, t]);
    const [templateID, setTemplateID] = useState("official:deepseek");
    const [key, setKey] = useState("");
    const firstAvailableTemplateID = templateChoices.find(providerTemplateCanAdd)?.id ?? templateChoices[0]?.id ?? "";
    const selected = templateChoices.find((choice) => choice.id === templateID) ?? templateChoices.find((choice) => choice.id === firstAvailableTemplateID) ?? templateChoices[0];
    useEffect(() => {
        const current = templateChoices.find((choice) => choice.id === templateID);
        if (firstAvailableTemplateID && (!current || (!providerTemplateCanAdd(current) && firstAvailableTemplateID !== templateID))) {
            setTemplateID(firstAvailableTemplateID);
        }
    }, [firstAvailableTemplateID, templateChoices, templateID]);
    const header = (<div className="provider-add-panel__head">
      <div>
        <strong>{t("settings.addProvider.chooseTitle")}</strong>
        <span>{t("settings.addProvider.chooseHint")}</span>
      </div>
      <button type="button" className="btn btn--small" disabled={busy} onClick={onCancel}>
        {t("common.cancel")}
      </button>
    </div>);
    const modeSwitch = (<div className="provider-add-segmented" role="tablist" aria-label={t("settings.addProvider.chooseTitle")}>
      <button type="button" role="tab" aria-selected={mode === "official"} className={mode === "official" ? "provider-add-segmented__item provider-add-segmented__item--active" : "provider-add-segmented__item"} disabled={busy} onClick={() => onMode("official")}>
        {t("settings.addProvider.officialChoice")}
      </button>
      <button type="button" role="tab" aria-selected={mode === "custom"} className={mode === "custom" ? "provider-add-segmented__item provider-add-segmented__item--active" : "provider-add-segmented__item"} disabled={busy} onClick={() => onMode("custom")}>
        {t("settings.addProvider.customChoice")}
      </button>
    </div>);
    if (mode === "official") {
        return (<div className="provider-add-panel">
        {header}
        {modeSwitch}
        <div className="provider-add-panel__hint">{t("settings.addProvider.officialHint")}</div>
        <div className="provider-template-grid">
          {templateChoices.map((choice) => {
                const canAdd = providerTemplateCanAdd(choice);
                const badge = providerTemplateStatusBadge(choice, t);
                const conflictProviderName = providerTemplateConflictProviderName(choice);
                if (choice.source === "preset" && (choice.status === "name_conflict" || choice.status === "installed_modified")) {
                    return (<div key={choice.id} className={`provider-template-card${providerTemplateStatusClass(choice)}`}>
                  <strong>
                    {choice.label}
                    {badge ? ` · ${badge}` : ""}
                  </strong>
                  <span>{choice.description}</span>
                  <div className="provider-template-card__actions">
                    <button type="button" className="btn btn--small" disabled={busy || !conflictProviderName} onClick={() => onViewPresetConflict(conflictProviderName)}>
                      {choice.status === "installed_modified" ? t("settings.addProvider.viewPresetProvider") : t("settings.addProvider.viewConflictProvider")}
                    </button>
                    <InlineConfirmButton label={t("settings.addProvider.resetPreset")} confirmLabel={t("settings.addProvider.confirmResetPreset")} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={() => onResetPreset(choice.presetID)}/>
                  </div>
                </div>);
                }
                return (<button key={choice.id} type="button" className={`provider-template-card${selected?.id === choice.id ? " provider-template-card--active" : ""}${providerTemplateStatusClass(choice)}`} disabled={busy || !canAdd} onClick={() => setTemplateID(choice.id)}>
                <strong>
                  {choice.label}
                  {badge ? ` · ${badge}` : ""}
                </strong>
                <span>{choice.description}</span>
              </button>);
            })}
        </div>
        <label className="set-label">{t("settings.providerKeyOptional")}</label>
        <input className="mem-input" type="password" placeholder={selected ? t("settings.setKey", { env: selected.keyEnv }) : ""} value={key} disabled={busy || !providerTemplateCanAdd(selected)} onChange={(e) => setKey(e.target.value)}/>
        <div className="prov-card__actions">
          <button type="button" className="btn btn--small" disabled={busy} onClick={onCancel}>
            {t("common.cancel")}
          </button>
          <button type="button" className="btn btn--primary btn--small" disabled={busy || !providerTemplateCanAdd(selected)} onClick={() => {
                if (!providerTemplateCanAdd(selected))
                    return;
                if (selected.source === "official")
                    void onAddOfficial(selected.kind, key.trim());
                else
                    void onAddPreset(selected.presetID, key.trim());
            }}>
            {providerTemplateActionLabel(selected, t)}
          </button>
        </div>
      </div>);
    }
    if (mode === "custom") {
        return (<div className="provider-add-panel">
        {header}
        {modeSwitch}
        <div className="provider-add-panel__hint">{t("settings.addProvider.customHint")}</div>
        <ProviderEditor kinds={kinds} busy={busy} onCancel={onCancel} onSave={onAddCustom}/>
      </div>);
    }
    return null;
}
export function ProviderAccessCard({ group, busy, fetching, fetchResult, modelDraft, defaultProvider, editing, kinds, onEdit, onCancelEdit, onSave, onRefresh, onToggleDraftModel, onToggleDraftVision, onSelectAllDraftModels, onClearDraftModels, onCancelDraftModels, onSaveDraftModels, onToggleWebSearch, onUpgradeRecommended, onSaveEditorKey, onClearEditorKey, onDelete, }: {
    group: ProviderAccessGroup;
    busy: boolean;
    fetching: boolean;
    fetchResult?: ProviderFetchResult;
    modelDraft?: ProviderModelDraft;
    defaultProvider: string;
    editing: string | null;
    kinds: string[];
    onEdit: (name: string) => void;
    onCancelEdit: () => void;
    onSave: (p: ProviderView, key?: string) => void | Promise<void>;
    onRefresh: (p: ProviderView) => void;
    onToggleDraftModel: (model: string) => void;
    onToggleDraftVision: (model: string) => void;
    onSelectAllDraftModels: () => void;
    onClearDraftModels: () => void;
    onCancelDraftModels: () => void;
    onSaveDraftModels: () => void;
    onToggleWebSearch: (enabled: boolean) => void;
    onUpgradeRecommended: (name: string) => void | Promise<void>;
    onSaveEditorKey: (apiKeyEnv: string, value: string) => Promise<void>;
    onClearEditorKey?: (apiKeyEnv: string) => Promise<void>;
    onDelete?: (providers: ProviderView[]) => Promise<void>;
}) {
    const t = useT();
    const editableProvider = group.providers[0];
    const isDefault = group.providers.some((p) => p.name === defaultProvider);
    const editingProvider = group.providers.find((p) => editing === p.name);
    const upgradeProvider = group.providers.find((p) => p.recommendedUpgradeAvailable);
    const primaryProviderExpanded = Boolean(editableProvider && editing === editableProvider.name);
    const supportsServerWebSearch = group.providers.length > 0 && group.providers.every(providerSupportsServerWebSearchForView);
    const webSearchEnabled = supportsServerWebSearch && group.providers.every((provider) => Boolean(provider.webSearch));
    const visibleModels = group.models.slice(0, 6);
    const hiddenModelCount = Math.max(0, group.models.length - visibleModels.length);
    return (<article className={`provider-access-card${group.builtIn ? " provider-access-card--builtin" : ""}`}>
      <div className="provider-access-card__head">
        <div className="provider-access-card__identity">
          <div className="provider-access-card__title">
            {group.label}
            <span className={`badge ${group.builtIn ? "badge--project" : "badge--neutral"}`}>
              {group.builtIn ? t("settings.builtinProviderBadge") : t("settings.customProviderBadge")}
            </span>
            <span className={`badge ${group.keySet ? "badge--project" : "badge--feedback"}`}>
              {providerKeyStatusLabel(group, t)}
            </span>
          </div>
        </div>
        <div className="provider-access-card__actions">
          {editableProvider && (<button className="btn btn--small" disabled={busy} aria-expanded={primaryProviderExpanded} onClick={() => primaryProviderExpanded ? onCancelEdit() : onEdit(editableProvider.name)}>
              {primaryProviderExpanded ? t("common.collapse") : t("settings.configureProvider")}
            </button>)}
          {editableProvider && group.providers.length === 1 && (<button className="btn btn--small" disabled={busy || fetching || !editableProvider.baseUrl || !group.configured} onClick={() => onRefresh(editableProvider)}>
              {fetching ? t("settings.fetchingModels") : t("settings.fetchModels")}
            </button>)}
          {editableProvider && onDelete && (<ProviderAccessMoreMenu busy={busy} removeDisabled={isDefault && !group.builtIn} builtIn={group.builtIn} onRemove={() => onDelete(group.providers)}/>)}
        </div>
      </div>
      {group.description && <div className="provider-access-card__desc">{group.description}</div>}

      {upgradeProvider && (<div className="provider-protocol-upgrade">
          <div className="provider-protocol-upgrade__copy">
            <div className="provider-protocol-upgrade__title">
              {t("settings.providerProtocol")}: OpenAI Chat Completions
            </div>
            <div className="provider-protocol-upgrade__desc">{t("settings.addProvider.official.deepseekDesc")}</div>
          </div>
          <div className="provider-protocol-upgrade__actions">
            <InlineConfirmButton label={<>{t("settings.upgradeRecommendedProtocol")}<ArrowRight size={13} aria-hidden="true"/></>} confirmLabel={t("common.confirm")} cancelLabel={t("common.cancel")} disabled={busy} primary onConfirm={() => onUpgradeRecommended(canonicalOfficialProviderName(upgradeProvider.name))}/>
          </div>
        </div>)}

      {!supportsServerWebSearch && (<ProviderModelSummary configured={group.configured} models={visibleModels} hiddenModelCount={hiddenModelCount}/>)}

      {!group.configured && group.requiresKey && (<div className="provider-card-status provider-card-status--warn">
          {t("settings.modelsRequireKey")}
        </div>)}
      {fetchResult && (<div className={`provider-card-status provider-card-status--${fetchResult.kind}`}>
          {fetchResult.text}
        </div>)}

      {modelDraft && (<ProviderModelDraftPicker draft={modelDraft} busy={busy} fetching={fetching} onToggle={onToggleDraftModel} onToggleVision={onToggleDraftVision} onSelectAll={onSelectAllDraftModels} onClear={onClearDraftModels} onCancel={onCancelDraftModels} onSave={onSaveDraftModels}/>)}

      {editableProvider && (<ProviderServiceCapabilities supported={supportsServerWebSearch} configured={group.configured} models={visibleModels} hiddenModelCount={hiddenModelCount} showModelSummary enabled={webSearchEnabled} disabled={busy} onChange={onToggleWebSearch}/>)}

      <ProviderTechnicalDetails group={group}/>

      {group.providers.length > 1 && (<div className="provider-profiles">
          {group.providers.map((p) => {
                const profileExpanded = editing === p.name;
                return (<div className="provider-profile-row" key={p.name}>
                <span>{p.name}</span>
                <span>{p.models.join(", ") || t("common.none")}</span>
                <button className="btn btn--small provider-profile-row__refresh" disabled={busy || fetching || !p.baseUrl || !providerIsConfigured(p)} onClick={() => onRefresh(p)}>
                  {fetching ? t("settings.fetchingModels") : t("settings.fetchModels")}
                </button>
                <button className="btn btn--small provider-profile-row__configure" disabled={busy} aria-expanded={profileExpanded} onClick={() => profileExpanded ? onCancelEdit() : onEdit(p.name)}>
                  {profileExpanded ? t("common.collapse") : t("settings.configureProfile")}
                </button>
              </div>);
            })}
        </div>)}

      {editingProvider && (<ProviderEditor key={editingProvider.name} initial={editingProvider} kinds={kinds} busy={busy} onCancel={onCancelEdit} onSave={onSave} onSaveKey={onSaveEditorKey} onClearKey={onClearEditorKey}/>)}
    </article>);
}
function ProviderAccessMoreMenu({ busy, removeDisabled, builtIn, onRemove, }: {
    busy: boolean;
    removeDisabled: boolean;
    builtIn: boolean;
    onRemove: () => void | Promise<void>;
}) {
    const t = useT();
    const [open, setOpen] = useState(false);
    const triggerRef = useRef<HTMLButtonElement>(null);
    const disabled = busy || removeDisabled;
    const tooltip = removeDisabled ? t("settings.cantDeleteDefault") : t("settings.themeGallery.moreActions");
    return (<div className="provider-access-more">
      <Tooltip label={tooltip}>
        <button ref={triggerRef} type="button" className="btn btn--small provider-access-more__trigger" aria-label={t("settings.themeGallery.moreActions")} aria-haspopup="menu" aria-expanded={open} disabled={disabled} onClick={() => setOpen((current) => !current)}>
          <MoreHorizontal size={16} aria-hidden="true"/>
        </button>
      </Tooltip>
      <AnchoredPopover open={open && !disabled} anchorRef={triggerRef} onClose={() => setOpen(false)} className="provider-access-more__menu" align="end" placement="bottom">
        <div className="provider-access-more__items" role="menu" aria-label={t("settings.themeGallery.moreActions")}>
          <InlineConfirmButton label={<><Trash2 size={14} aria-hidden="true"/>{t("settings.removeProviderAccess")}</>} confirmLabel={builtIn ? t("settings.confirmRemoveProviderAccess") : t("settings.confirmDeleteProvider")} cancelLabel={t("common.cancel")} danger={!builtIn} buttonRole="menuitem" onConfirm={async () => {
            setOpen(false);
            await onRemove();
        }}/>
        </div>
      </AnchoredPopover>
    </div>);
}
export { ProviderEditorModelPicker as ProviderEditorModelPicker } from "./settings_provider_editor";
export { ProviderEditor as ProviderEditor } from "./settings_provider_editor";

