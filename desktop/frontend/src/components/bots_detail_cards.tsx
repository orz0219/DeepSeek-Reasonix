import { ChevronDown, Clipboard, Send } from "lucide-react";
import type { ReactNode } from "react";
import { useT, type DictKey } from "../lib/i18n";
import type { BotAccessView, BotConnectionDiagnostic, BotConnectionView, BotSettingsView } from "../lib/types";
import { toRef, TOOL_APPROVAL_MODES, normalizeBotToolApprovalMode } from "./settings_normalize";
import { BotAccessListField, DEFAULT_QQ_SECRET_ENV, QQ_CONNECTION_ID, botAccessEntryCount, botConnectionLabel, botConnectionScopeLabel, botConnectionCredentialSummary, botConnectionSecretEnv, botConnectionSecretPatch, diagnosticMessage } from "./settings_bot_helpers";
import { settingsModelMeta, SettingsField, ToggleSegment } from "./SettingsPanel";
import { ModelPicker } from "./settings_models";
import { InlineConfirmButton } from "./InlineConfirmButton";
import type { SettingsView } from "../lib/types_settings";

interface BotAccessSectionProps {
    id: string;
    access: BotAccessView;
    updateAccess: (patch: Partial<BotAccessView>) => void;
    persistAccess: (patch: Partial<BotAccessView>) => void;
    busy: boolean;
    accessListText: (id: string, access: BotAccessView, field: BotAccessListField) => string;
    setAccessListText: (id: string, field: BotAccessListField, value: string) => void;
    persistAccessListText: (id: string, access: BotAccessView, field: BotAccessListField, value: string, persistAccess: (patch: Partial<BotAccessView>) => void) => void;
}

function BotAccessSection(props: BotAccessSectionProps) {
    const t = useT();
    const { id, access, updateAccess, persistAccess, busy, accessListText, setAccessListText, persistAccessListText } = props;
        const mode = access.allowAll ? "everyone" : "trusted";
        const setMode = (nextMode: "trusted" | "everyone") => {
            const patch = nextMode === "everyone"
                ? { enabled: false, allowAll: true }
                : { enabled: true, allowAll: false };
            updateAccess(patch);
            persistAccess(patch);
        };
        return (<section className="bot-detail-section bot-detail-section--access">
        <div>
          <div className="bot-detail-section__head">{t("settings.botAccessControl")}</div>
          <p>{access.allowAll ? t("settings.botSimpleAccessEveryoneSummary") : t("settings.botSimpleAccessTrustedSummary", { count: botAccessEntryCount(access) })}</p>
        </div>
        <div className="bot-choice-grid bot-choice-grid--access">
          <button type="button" className={`bot-choice-card${mode === "trusted" ? " bot-choice-card--active" : ""}`} disabled={busy} onClick={() => setMode("trusted")}>
            <strong>{t("settings.botAccessTrusted")}</strong>
            <span>{t("settings.botAccessTrustedHint")}</span>
          </button>
          <button type="button" className={`bot-choice-card${mode === "everyone" ? " bot-choice-card--active" : ""}`} disabled={busy} onClick={() => setMode("everyone")}>
            <strong>{t("settings.botAccessEveryone")}</strong>
            <span>{t("settings.botAccessEveryoneHint")}</span>
          </button>
        </div>
        <div className="bot-pairing-row">
          <div>
            <strong>{t("settings.botAccessPairing")}</strong>
            <span>{t("settings.botAccessPairingHint")}</span>
          </div>
          <ToggleSegment value={access.pairingEnabled} disabled={busy} onChange={(pairingEnabled) => {
                updateAccess({ pairingEnabled });
                persistAccess({ pairingEnabled });
            }}/>
        </div>
        {access.allowAll ? (<div className="bot-access-panel__warning">{t("settings.botAllowAllWarn")}</div>) : (<div className="bot-access-platforms bot-access-platforms--single">
            <div className="bot-access-platform">
              <BotListInput label={t("settings.botListUsers")} value={accessListText(id, access, "users")} disabled={busy} placeholder={t("settings.botListPlaceholder")} onChange={(value) => setAccessListText(id, "users", value)} onBlur={(value) => persistAccessListText(id, access, "users", value, persistAccess)}/>
              <BotListInput label={t("settings.botListGroups")} value={accessListText(id, access, "groups")} disabled={busy} placeholder={t("settings.botListPlaceholder")} onChange={(value) => setAccessListText(id, "groups", value)} onBlur={(value) => persistAccessListText(id, access, "groups", value, persistAccess)}/>
            </div>
          </div>)}
        <details className="bot-access-panel bot-simple-roles">
          <summary className="bot-access-panel__summary">
            <span>
              <strong>{t("settings.botRoleAccess")}</strong>
              <small>{t("settings.botRoleAccessHint")}</small>
            </span>
            <ChevronDown className="bot-access-panel__chevron" size={16} aria-hidden="true"/>
          </summary>
          <div className="bot-access-panel__body">
            <div className="bot-access-platforms bot-access-platforms--single">
              <div className="bot-access-platform">
                <BotListInput label={t("settings.botListApprovers")} value={accessListText(id, access, "approvers")} disabled={busy || access.allowAll} placeholder={t("settings.botListPlaceholder")} onChange={(value) => setAccessListText(id, "approvers", value)} onBlur={(value) => persistAccessListText(id, access, "approvers", value, persistAccess)}/>
                <BotListInput label={t("settings.botListAdmins")} value={accessListText(id, access, "admins")} disabled={busy || access.allowAll} placeholder={t("settings.botListPlaceholder")} onChange={(value) => setAccessListText(id, "admins", value)} onBlur={(value) => persistAccessListText(id, access, "admins", value, persistAccess)}/>
              </div>
            </div>
          </div>
        </details>
      </section>
    );
}


interface BotQQDetailCardProps {
    draft: BotSettingsView;
    busy: boolean;
    refs: string[];
    s: SettingsView;
    qqSecretValue: string;
    setQQSecretValue: (v: string) => void;
    persistQQ: (patch: Partial<BotSettingsView["qq"]>) => void;
    updateQQ: (patch: Partial<BotSettingsView["qq"]>) => void;
    removeQQBot: () => void;
    qqOnline: boolean;
    qqConfigured: boolean;
    qqCanEnableAccess: boolean;
    qqCanSaveAndEnable: boolean;
    saveQQAndEnable: () => void;
    clearQQSecret: () => void;
    focusQQAccessSettings: () => void;
    updateQQAccess: (patch: Partial<BotAccessView>) => void;
    persistQQAccess: (patch: Partial<BotAccessView>) => void;
    accessListText: (id: string, access: BotAccessView, field: BotAccessListField) => string;
    setAccessListText: (id: string, field: BotAccessListField, value: string) => void;
    persistAccessListText: (id: string, access: BotAccessView, field: BotAccessListField, value: string, persistAccess: (patch: Partial<BotAccessView>) => void) => void;
}

export function BotQQDetailCard(props: BotQQDetailCardProps) {
    const t = useT();
    const { draft, busy, refs, s, qqSecretValue, setQQSecretValue, persistQQ, updateQQ, removeQQBot, qqOnline, qqConfigured, qqCanEnableAccess, qqCanSaveAndEnable, saveQQAndEnable, clearQQSecret, focusQQAccessSettings, updateQQAccess, persistQQAccess, accessListText, setAccessListText, persistAccessListText } = props;
    return (<article className="bot-detail-card"     aria-labelledby="bot-detail-title">
      <div className="bot-detail-card__head">
        <div className="bot-detail-card__identity">
          <div className="bot-detail-card__title" id="bot-detail-title">
            QQ Bot
            <span className="badge badge--neutral">QQ</span>
            <span className={`badge ${qqOnline ? "badge--project" : qqConfigured ? "badge--feedback" : "badge--feedback"}`}>
              {qqOnline ? t("settings.botConnectionConnected") : qqConfigured ? t("settings.botConnectionConfigured") : t("settings.botConnectionDisconnected")}
            </span>
          </div>
          <div className="bot-detail-card__desc">{t("settings.botAutoSaveHint")}</div>
        </div>
      </div>

      <section className="bot-detail-section">
        <div className="bot-detail-section__head">{t("settings.botConnectionSummary")}</div>
        <div className="bot-detail-summary">
          <div>
            <span>{t("settings.botConnectionColumnChannel")}</span>
            <strong>QQ</strong>
          </div>
          <div>
            <span>{t("settings.botConnectionColumnRemote")}</span>
            <code title={draft.qq.appId.trim() || undefined}>{draft.qq.appId.trim() || "—"}</code>
          </div>
          <div>
            <span>{t("settings.botConnectionColumnScope")}</span>
            <strong>{t("settings.botScopeGlobal")}</strong>
          </div>
          <div>
            <span>{t("settings.botConnectionColumnStatus")}</span>
            <strong>{qqOnline ? t("settings.botConnectionConnected") : qqConfigured ? t("settings.botConnectionConfigured") : t("settings.botConnectionDisconnected")}</strong>
          </div>
        </div>
      </section>

      <section className="bot-detail-section bot-detail-section--runtime-primary">
        <SettingsField label={t("settings.botEnableBot")} hint={t("settings.botGatewayEnabled")}>
          <ToggleSegment value={draft.qq.enabled} disabled={busy} onChange={(enabled) => {
            if (enabled && !qqCanEnableAccess) {
                focusQQAccessSettings();
                return;
            }
            updateQQ({ enabled });
            void persistQQ({ enabled });
        }}/>
        </SettingsField>
        <SettingsField label={t("settings.botToolApprovalMode")} hint={t("settings.botToolApprovalModeHint")}>
          <div className="provider-add-segmented" role="group" aria-label={t("settings.botToolApprovalMode")}>
            {TOOL_APPROVAL_MODES.map((mode) => (<button key={mode} type="button" className={normalizeBotToolApprovalMode(draft.qq.toolApprovalMode) === mode ? "provider-add-segmented__item provider-add-segmented__item--active" : "provider-add-segmented__item"} disabled={busy} onClick={() => void persistQQ({ toolApprovalMode: mode })}>
                {t(`settings.botToolApprovalMode.${mode}` as DictKey)}
              </button>))}
          </div>
        </SettingsField>
        <SettingsField label={t("settings.botChannelModel")} hint={t("settings.botChannelModelHint")}>
          <ModelPicker s={s} refs={refs} value={toRef(draft.qq.model, s)} disabled={busy} emptyOptionLabel={t("settings.botChannelModelAuto")} emptyOptionHint={settingsModelMeta(s, t)} onPick={(model) => void persistQQ({ model })}/>
        </SettingsField>
      </section>

      {<BotAccessSection id={QQ_CONNECTION_ID} access={draft.qq.access} updateAccess={updateQQAccess} persistAccess={(patch) => void persistQQAccess(patch)} busy={busy} accessListText={accessListText} setAccessListText={setAccessListText} persistAccessListText={persistAccessListText}/>}

      <section className="bot-detail-section">
        <div className="bot-detail-section__head">{t("settings.botRuntimeSettings")}</div>
        <SettingsField label={t("settings.botSandbox")} hint={t("settings.botInstallQQHint")}>
          <ToggleSegment value={draft.qq.sandbox} disabled={busy} onLabel={t("settings.toggleOn")} offLabel={t("settings.toggleOff")} onChange={(sandbox) => {
            updateQQ({ sandbox });
            void persistQQ({ sandbox });
        }}/>
        </SettingsField>
        <SettingsField label={t("settings.botWorkspaceRoot")} hint={t("settings.botWorkspaceRootHint")}>
          <input className="mem-input" value={draft.qq.workspaceRoot} disabled={busy} placeholder={t("settings.botWorkspaceRootPlaceholder")} spellCheck={false} onChange={(event) => updateQQ({ workspaceRoot: event.target.value })} onBlur={(event) => void persistQQ({ workspaceRoot: event.currentTarget.value })}/>
        </SettingsField>
      </section>

      <section className="bot-detail-section">
        <div className="bot-detail-section__head">{t("settings.botCredential")}</div>
        <div className="bot-credential-stack">
          <div className="bot-credential-line">
            <span>{draft.qq.appId.trim() ? t("settings.botCredentialApp", { value: draft.qq.appId.trim() }) : t("settings.botCredentialConfigured")}</span>
            <strong>{draft.qq.secretSet ? t("settings.botSecretSet") : t("settings.botSecretMissing")}</strong>
          </div>
          <div className="bot-secret-row bot-secret-row--qq">
            <input className="mem-input" value={draft.qq.appId} disabled={busy} placeholder={t("settings.botAppId")} spellCheck={false} aria-label={t("settings.botAppId")} onChange={(event) => updateQQ({ appId: event.target.value })} onBlur={(event) => void persistQQ({ appId: event.currentTarget.value })}/>
            <input className="mem-input" value={draft.qq.appSecretEnv || DEFAULT_QQ_SECRET_ENV} disabled={busy} placeholder={DEFAULT_QQ_SECRET_ENV} spellCheck={false} aria-label={t("settings.botSecretEnv")} onChange={(event) => updateQQ({ appSecretEnv: event.target.value })} onBlur={(event) => void persistQQ({ appSecretEnv: event.currentTarget.value || DEFAULT_QQ_SECRET_ENV })}/>
            <input className="mem-input" type="password" value={qqSecretValue} disabled={busy} placeholder={draft.qq.secretSet ? t("settings.botSecretReplace") : t("settings.botSecretPaste")} aria-label={t("settings.botSecretValue")} onChange={(event) => setQQSecretValue(event.target.value)}/>
            <button type="button" className="btn btn--secondary btn--small" disabled={busy || !qqCanSaveAndEnable} onClick={() => void saveQQAndEnable()}>
              {draft.qq.secretSet ? t("settings.saveKey") : t("settings.botSaveAndEnable")}
            </button>
            <button type="button" className="btn btn--secondary btn--small" disabled={busy || !draft.qq.secretSet} onClick={() => void clearQQSecret()}>
              {t("settings.clearKey")}
            </button>
          </div>
          {!qqCanEnableAccess ? <div className="bot-connect-panel__hint bot-connect-panel__hint--warning">{t("settings.botQQAccessRequired")}</div> : null}
        </div>
      </section>

      <section className="bot-detail-section bot-detail-section--danger">
        <div>
          <div className="bot-detail-section__head">{t("settings.botDangerZone")}</div>
          <p>{t("settings.deleteBotHint")}</p>
        </div>
        <InlineConfirmButton label={t("settings.deleteBot")} confirmLabel={t("settings.confirmDeleteBot")} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={() => void removeQQBot()}/>
      </section>
    </article>
    );
}


interface BotConnectionDetailCardProps {
    selectedConnection: BotConnectionView;
    selectedConnectionRemote: string;
    selectedConnectionToolApprovalMode: string;
    selectedDiagnostic: BotConnectionDiagnostic | string | undefined;
    selectedDiagnosticDetail: string;
    busy: boolean;
    connectionSecrets: Record<string, string>;
    setConnectionSecrets: (fn: (prev: Record<string, string>) => Record<string, string> | Record<string, string>) => void;
    diagnoseConnection: (id: string) => void;
    testConnection: (c: BotConnectionView) => void;
    copyConnectionDiagnostic: (c: BotConnectionView) => void;
    reportConnectionDiagnostic: (c: BotConnectionView) => void;
    persistConnection: (id: string, patch: Partial<BotConnectionView>) => void;
    updateConnection: (id: string, patch: Partial<BotConnectionView>) => void;
    persistConnectionAccess: (c: BotConnectionView, patch: Partial<BotAccessView>) => void;
    updateConnectionAccess: (id: string, patch: Partial<BotAccessView>) => void;
    persistConnectionToolApprovalMode: (id: string, mode: string) => void;
    saveConnectionSecret: (c: BotConnectionView) => void;
    updateConnectionCredential: (id: string, patch: Partial<BotConnectionView["credential"]>) => void;
    persistConnectionCredential: (id: string, patch: Partial<BotConnectionView["credential"]>) => void;
    clearConnectionSecret: (c: BotConnectionView) => void;
    removeConnection: (c: BotConnectionView) => void;
    refs: string[];
    s: SettingsView;
    accessListText: (id: string, access: BotAccessView, field: BotAccessListField) => string;
    setAccessListText: (id: string, field: BotAccessListField, value: string) => void;
    persistAccessListText: (id: string, access: BotAccessView, field: BotAccessListField, value: string, persistAccess: (patch: Partial<BotAccessView>) => void) => void;
}

export function BotConnectionDetailCard(props: BotConnectionDetailCardProps) {
    const t = useT();
    const { selectedConnection, selectedConnectionRemote, selectedConnectionToolApprovalMode, selectedDiagnostic, selectedDiagnosticDetail, busy, connectionSecrets, setConnectionSecrets, diagnoseConnection, testConnection, copyConnectionDiagnostic, reportConnectionDiagnostic, persistConnection, updateConnection, persistConnectionAccess, updateConnectionAccess, persistConnectionToolApprovalMode, saveConnectionSecret, updateConnectionCredential, persistConnectionCredential, clearConnectionSecret, removeConnection, refs, s, accessListText, setAccessListText, persistAccessListText } = props;
    return (<article className="bot-detail-card"     aria-labelledby="bot-detail-title">
      <div className="bot-detail-card__head">
        <div className="bot-detail-card__identity">
          <div className="bot-detail-card__title" id="bot-detail-title">
            {selectedConnection.label || botConnectionLabel(selectedConnection, t)}
            <span className="badge badge--neutral">{botConnectionLabel(selectedConnection, t)}</span>
            <span className={`badge ${selectedConnection.status === "connected" ? "badge--project" : "badge--feedback"}`}>
              {selectedConnection.status === "connected" ? t("settings.botConnectionConnected") : selectedConnection.status || t("settings.botConnectionDisconnected")}
            </span>
          </div>
          <div className="bot-detail-card__desc">{t("settings.botAutoSaveHint")}</div>
        </div>
        <div className="bot-detail-card__actions">
          <button type="button" className="btn btn--small" disabled={busy} onClick={() => void diagnoseConnection(selectedConnection.id)}>
            {t("settings.botDiagnose")}
          </button>
          {(selectedConnection.provider === "feishu" || selectedConnection.provider === "weixin") ? (<button type="button" className="btn btn--small" disabled={busy || !selectedConnectionRemote} onClick={() => void testConnection(selectedConnection)}>
              {t("settings.botTest")}
            </button>) : null}
        </div>
      </div>

      {diagnosticMessage(selectedDiagnostic) ? (<div className="bot-detail-notice">
          <span>{diagnosticMessage(selectedDiagnostic)}</span>
          {selectedDiagnosticDetail ? (<div className="bot-diagnostic-actions">
              <button type="button" className="btn btn--secondary btn--small" disabled={busy} onClick={() => void copyConnectionDiagnostic(selectedConnection)}>
                <Clipboard aria-hidden="true"/>
                {t("settings.botCopyDiagnostic")}
              </button>
              <button type="button" className="btn btn--primary btn--small" disabled={busy} onClick={() => void reportConnectionDiagnostic(selectedConnection)}>
                <Send aria-hidden="true"/>
                {t("settings.botSendDiagnostic")}
              </button>
              <small>{t("settings.botDiagnosticPrivacy")}</small>
            </div>) : null}
        </div>) : null}

      <section className="bot-detail-section">
        <div className="bot-detail-section__head">{t("settings.botConnectionSummary")}</div>
        <div className="bot-detail-summary">
          <div>
            <span>{t("settings.botConnectionColumnChannel")}</span>
            <strong>{botConnectionLabel(selectedConnection, t)}</strong>
          </div>
          <div>
            <span>{t("settings.botConnectionColumnRemote")}</span>
            <code title={selectedConnectionRemote || undefined}>{selectedConnectionRemote || "—"}</code>
          </div>
          <div>
            <span>{t("settings.botConnectionColumnScope")}</span>
            <strong>{botConnectionScopeLabel(selectedConnection, t)}</strong>
          </div>
          <div>
            <span>{t("settings.botConnectionColumnStatus")}</span>
            <strong>{selectedConnection.status === "connected" ? t("settings.botConnectionConnected") : selectedConnection.status || t("settings.botConnectionDisconnected")}</strong>
          </div>
        </div>
      </section>

      <section className="bot-detail-section bot-detail-section--runtime-primary">
        <SettingsField label={t("settings.botEnableBot")} hint={t("settings.botGatewayEnabled")}>
          <ToggleSegment value={selectedConnection.enabled} disabled={busy} onChange={(enabled) => void persistConnection(selectedConnection.id, { enabled })}/>
        </SettingsField>
        <SettingsField label={t("settings.botToolApprovalMode")} hint={t("settings.botToolApprovalModeHint")}>
          <div className="provider-add-segmented" role="group" aria-label={t("settings.botToolApprovalMode")}>
            {TOOL_APPROVAL_MODES.map((mode) => (<button key={mode} type="button" className={selectedConnectionToolApprovalMode === mode ? "provider-add-segmented__item provider-add-segmented__item--active" : "provider-add-segmented__item"} disabled={busy} onClick={() => persistConnectionToolApprovalMode(selectedConnection.id, mode)}>
                {t(`settings.botToolApprovalMode.${mode}` as DictKey)}
              </button>))}
          </div>
        </SettingsField>
        <SettingsField label={t("settings.botChannelModel")} hint={t("settings.botChannelModelHint")}>
          <ModelPicker s={s} refs={refs} value={toRef(selectedConnection.model, s)} disabled={busy} emptyOptionLabel={t("settings.botChannelModelAuto")} emptyOptionHint={settingsModelMeta(s, t)} onPick={(model) => void persistConnection(selectedConnection.id, { model })}/>
        </SettingsField>
      </section>

      {<BotAccessSection id={selectedConnection.id} access={selectedConnection.access} updateAccess={(patch) => updateConnectionAccess(selectedConnection.id, patch)} persistAccess={(patch) => void persistConnectionAccess(selectedConnection, patch)} busy={busy} accessListText={accessListText} setAccessListText={setAccessListText} persistAccessListText={persistAccessListText}/>}

      <section className="bot-detail-section">
        <div className="bot-detail-section__head">{t("settings.botRuntimeSettings")}</div>
        <SettingsField label={t("settings.botWorkspaceRoot")} hint={t("settings.botWorkspaceRootHint")}>
          <input className="mem-input" value={selectedConnection.workspaceRoot} disabled={busy} placeholder={t("settings.botWorkspaceRootPlaceholder")} spellCheck={false} onChange={(event) => updateConnection(selectedConnection.id, { workspaceRoot: event.target.value })} onBlur={(event) => void persistConnection(selectedConnection.id, { workspaceRoot: event.currentTarget.value })}/>
        </SettingsField>
      </section>

      <section className="bot-detail-section">
        <div className="bot-detail-section__head">{t("settings.botCredential")}</div>
        <div className="bot-credential-stack">
          <div className="bot-credential-line">
            <span>{botConnectionCredentialSummary(selectedConnection, t)}</span>
            <strong>{selectedConnection.credential.secretSet ? t("settings.botSecretSet") : t("settings.botSecretMissing")}</strong>
          </div>
          {botConnectionSecretEnv(selectedConnection) ? (<div className="bot-secret-row">
              <input className="mem-input" value={botConnectionSecretEnv(selectedConnection)} disabled={busy} spellCheck={false} onChange={(event) => updateConnectionCredential(selectedConnection.id, botConnectionSecretPatch(selectedConnection, event.target.value))} onBlur={(event) => void persistConnectionCredential(selectedConnection.id, botConnectionSecretPatch(selectedConnection, event.currentTarget.value))}/>
              <input className="mem-input" type="password" value={connectionSecrets[selectedConnection.id] ?? ""} disabled={busy} placeholder={selectedConnection.credential.secretSet ? t("settings.botSecretReplace") : t("settings.botSecretPaste")} onChange={(event) => setConnectionSecrets((prev) => ({ ...prev, [selectedConnection.id]: event.target.value }))}/>
              <button type="button" className="btn btn--secondary btn--small" disabled={busy || !(connectionSecrets[selectedConnection.id] ?? "").trim()} onClick={() => void saveConnectionSecret(selectedConnection)}>
                {t("settings.saveKey")}
              </button>
              <button type="button" className="btn btn--secondary btn--small" disabled={busy || !selectedConnection.credential.secretSet} onClick={() => void clearConnectionSecret(selectedConnection)}>
                {t("settings.clearKey")}
              </button>
            </div>) : null}
        </div>
      </section>

      <section className="bot-detail-section bot-detail-section--danger">
        <div>
          <div className="bot-detail-section__head">{t("settings.botDangerZone")}</div>
          <p>{t("settings.deleteBotHint")}</p>
        </div>
        <InlineConfirmButton label={t("settings.deleteBot")} confirmLabel={t("settings.confirmDeleteBot")} cancelLabel={t("common.cancel")} disabled={busy} danger onConfirm={() => removeConnection(selectedConnection)}/>
      </section>
    </article>
    );
}


export function BotListInput({ label, value, disabled, placeholder, onChange, onBlur, }: {
    label: ReactNode;
    value: string;
    disabled?: boolean;
    placeholder?: string;
    onChange: (value: string) => void;
    onBlur: (value: string) => void;
}) {
    return (<label className="bot-list-input">
        <span className="bot-list-input__label">{label}</span>
        <input className="mem-input" value={value} disabled={disabled} placeholder={placeholder} spellCheck={false} onChange={(event) => onChange(event.target.value)} onBlur={(event) => onBlur(event.currentTarget.value)}/>
    </label>);
}
