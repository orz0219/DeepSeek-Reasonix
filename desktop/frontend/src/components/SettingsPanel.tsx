import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { RefreshCw } from "lucide-react";
import { useDeferredClose } from "../lib/useMountTransition";
import { app } from "../lib/bridge";
import { useT, type DictKey } from "../lib/i18n";
import { applyTheme, getTheme, getThemeStyle, normalizeThemePreference, normalizeThemeStyleForTheme, type Theme, type ThemeStyle } from "../lib/theme";
import { applyTerminalThemePreference, createTerminalThemeSaveQueue, getTerminalThemePreference, normalizeTerminalThemePreference, type TerminalThemePreference } from "../lib/terminalTheme";
import { applyConversationWidth, getCachedConversationWidth, type ConversationWidth } from "../lib/conversationWidth";
import { applyTextSize, getTextSize, type TextSize } from "../lib/textSize";
import { snapZoom, zoomToPercent, saveRestartZoom, getRestartZoom, type ZoomLevel } from "../lib/dpiScale";
import { applyFontFamily, applyMonoFontFamily, getFontFamily, getMonoFontFamily, getCustomFontName, getCustomMonoFontName, setCustomFontName, setCustomMonoFontName, type FontFamily, type MonoFontFamily } from "../lib/fontFamily";
import { comboFromKeyboardEvent, detectShortcutPlatform, formatShortcutCombo, onShortcutsChanged, resetCustomShortcuts, resolvedShortcutCombo, saveCustomShortcut, shortcutAcceptsCombo, shortcutConflict, shortcutDefinitions, type ShortcutAction } from "../lib/keyboardShortcuts";
import type { SettingsTab, SettingsView } from "../lib/types";
import { AppearanceOverview } from "./AppearanceOverview";
import { applyConfiguredBaseAppearance, setBaseAppearance } from "../lib/themePack";
import { ModalCloseButton } from "./ModalCloseButton";
import { ShortcutComboDisplay } from "./ShortcutComboDisplay";
import { SettingsNavigation, SETTINGS_NAV_TABS } from "./SettingsNavigation";
import { formatSettingsError, normalizeSettingsView } from "./settings_normalize";
import { GeneralSection, NetworkSection } from "./settings_general";
import { ModelsSection } from "./settings_models";
import { PermissionsSection, SandboxSection, AboutSection } from "./settings_permissions";
import { SettingsSection, SettingsPageShell, settingsTabLabel, settingsTabMeta } from "./settings_shell";
export { allRefs as allRefs } from "./settings_normalize";
export { toRef as toRef } from "./settings_normalize";
export { EFFORT_PRESETS as EFFORT_PRESETS } from "./settings_normalize";
export { providerEditorEffectiveKind as providerEditorEffectiveKind } from "./settings_normalize";
export { formatProviderExtraBody as formatProviderExtraBody } from "./settings_normalize";
export { parseProviderExtraBody as parseProviderExtraBody } from "./settings_normalize";
export { providerExtraBodyParseError as providerExtraBodyParseError } from "./settings_normalize";
export { normalizeProviderView as normalizeProviderView } from "./settings_normalize";
export { PROXY_MODES as PROXY_MODES } from "./settings_normalize";
export { COMPACT_RATIO_PRESETS as COMPACT_RATIO_PRESETS } from "./settings_normalize";
export { REASONING_PROTOCOLS as REASONING_PROTOCOLS } from "./settings_normalize";
export { THINKING_MODES as THINKING_MODES } from "./settings_normalize";
export { PROXY_TYPES as PROXY_TYPES } from "./settings_normalize";
export { LANGUAGE_PREFS as LANGUAGE_PREFS } from "./settings_normalize";
export { TOOL_APPROVAL_MODES as TOOL_APPROVAL_MODES } from "./settings_normalize";
export { BOT_TOOL_APPROVAL_MODES as BOT_TOOL_APPROVAL_MODES } from "./settings_normalize";
export type { ProxyMode as ProxyMode } from "./settings_normalize";
export { normalizeProxyMode as normalizeProxyMode } from "./settings_normalize";
export { normalizeNetworkView as normalizeNetworkView } from "./settings_normalize";
export { normalizeReasoningProtocol as normalizeReasoningProtocol } from "./settings_normalize";
export { normalizeThinkingMode as normalizeThinkingMode } from "./settings_normalize";
export { formatProviderHeaders as formatProviderHeaders } from "./settings_normalize";
export { parseProviderHeaders as parseProviderHeaders } from "./settings_normalize";
export { formatSettingsError as formatSettingsError } from "./settings_normalize";
export { providerModelFetchFallbackMessage as providerModelFetchFallbackMessage } from "./settings_normalize";
export type { ProviderPresetStatus as ProviderPresetStatus } from "./settings_normalize";
export { normalizeProviderPresetStatus as normalizeProviderPresetStatus } from "./settings_normalize";
export { normalizeSettingsView as normalizeSettingsView } from "./settings_normalize";
export type { DesktopCurrency as DesktopCurrency } from "./settings_normalize";
export { normalizeDesktopCurrency as normalizeDesktopCurrency } from "./settings_normalize";
export { normalizeCloseBehavior as normalizeCloseBehavior } from "./settings_normalize";
export type { DisplayMode as DisplayMode } from "./settings_normalize";
export { normalizeDisplayMode as normalizeDisplayMode } from "./settings_normalize";
export { normalizeStatusBarStyle as normalizeStatusBarStyle } from "./settings_normalize";
export { statusBarItemLabel as statusBarItemLabel } from "./settings_normalize";
export { closeBehaviorLabel as closeBehaviorLabel } from "./settings_normalize";
export { permissionModeLabel as permissionModeLabel } from "./settings_normalize";
export { sandboxModeLabel as sandboxModeLabel } from "./settings_normalize";
export { providerKindLabel as providerKindLabel } from "./settings_normalize";
export { providerKindHint as providerKindHint } from "./settings_normalize";
export { reasoningProtocolLabel as reasoningProtocolLabel } from "./settings_normalize";
export { thinkingModeLabel as thinkingModeLabel } from "./settings_normalize";
export { GeneralSection as GeneralSection } from "./settings_general";
export { NetworkSection as NetworkSection } from "./settings_general";
export { ModelPicker as ModelPicker } from "./settings_models";
export { proxyModeLabel as proxyModeLabel } from "./settings_models";
export { ModelsSection as ModelsSection } from "./settings_models";
export { providerKeyStatusLabel as providerKeyStatusLabel } from "./settings_models";
export { modelProviderLabel as modelProviderLabel } from "./settings_models";
export { KeyField as KeyField } from "./settings_permissions";
export { PermissionsSection as PermissionsSection } from "./settings_permissions";
export { SandboxSection as SandboxSection } from "./settings_permissions";
export { AboutSection as AboutSection } from "./settings_permissions";
export type { ProviderAccessGroup as ProviderAccessGroup } from "./settings_provider_helpers";
export { providerAccessGroups as providerAccessGroups } from "./settings_provider_helpers";
export { providerSupportsServerWebSearch as providerSupportsServerWebSearch } from "./settings_provider_helpers";
export { providerSupportsServerWebSearchForView as providerSupportsServerWebSearchForView } from "./settings_provider_helpers";
export { providerVisionCapabilityForView as providerVisionCapabilityForView } from "./settings_provider_helpers";
export { providerGroupID as providerGroupID } from "./settings_provider_helpers";
export { providerGroupLabel as providerGroupLabel } from "./settings_provider_helpers";
export { uniqueStrings as uniqueStrings } from "./settings_provider_helpers";
export type { ProviderFetchResult as ProviderFetchResult } from "./settings_provider_helpers";
export type { ProviderModelDraft as ProviderModelDraft } from "./settings_provider_helpers";
export type { AddProviderMode as AddProviderMode } from "./settings_provider_helpers";
export type { OfficialProviderKind as OfficialProviderKind } from "./settings_provider_helpers";
export { OFFICIAL_PROVIDER_CHOICES as OFFICIAL_PROVIDER_CHOICES } from "./settings_provider_helpers";
export type { ProviderTemplateChoice as ProviderTemplateChoice } from "./settings_provider_helpers";
export { providerTemplateCanAdd as providerTemplateCanAdd } from "./settings_provider_helpers";
export { providerTemplateStatusBadge as providerTemplateStatusBadge } from "./settings_provider_helpers";
export { providerTemplateActionLabel as providerTemplateActionLabel } from "./settings_provider_helpers";
export { providerTemplateStatusClass as providerTemplateStatusClass } from "./settings_provider_helpers";
export { providerTemplateConflictProviderName as providerTemplateConflictProviderName } from "./settings_provider_helpers";
export { providerPresetDescription as providerPresetDescription } from "./settings_provider_helpers";
export { providerPresetLabel as providerPresetLabel } from "./settings_provider_helpers";
export { ProviderModelSummary as ProviderModelSummary } from "./settings_provider_helpers";
export { ProviderTechnicalDetails as ProviderTechnicalDetails } from "./settings_provider_helpers";
export { ProviderModelDraftPicker as ProviderModelDraftPicker } from "./settings_provider_helpers";
export { ProviderServiceCapabilities as ProviderServiceCapabilities } from "./settings_provider_helpers";
export type { ProviderVisionCapability as ProviderVisionCapability } from "./settings_provider_helpers";
export { canonicalOfficialProviderName as canonicalOfficialProviderName } from "./settings_provider_helpers";
export { officialProviderKind as officialProviderKind } from "./settings_provider_helpers";
export { parseProviderListInput as parseProviderListInput } from "./settings_provider_helpers";
export { ProvidersSection as ProvidersSection } from "./settings_providers";
export { AddProviderPanel as AddProviderPanel } from "./settings_providers";
export { ProviderAccessCard as ProviderAccessCard } from "./settings_providers";
export { ProviderEditorModelPicker as ProviderEditorModelPicker } from "./settings_providers";
export { ProviderEditor as ProviderEditor } from "./settings_providers";
export { ToggleSegment as ToggleSegment } from "./ToggleSegment";
export type SettingsInitialFocus = {
    target: "model-access";
    requestId?: number;
};
type DesktopPlatform = "darwin" | "windows" | "linux";
const MCPServersSettingsPage = lazy(() => import("./CapabilitiesPanel").then((module) => ({ default: module.MCPServersSettingsPage })));
const SkillsSettingsPage = lazy(() => import("./CapabilitiesPanel").then((module) => ({ default: module.SkillsSettingsPage })));
const PluginsSettingsPage = lazy(() => import("./CapabilitiesPanel").then((module) => ({ default: module.PluginsSettingsPage })));
const SubagentsSettingsPage = lazy(() => import("./SubagentsPanel").then((module) => ({ default: module.SubagentsSettingsPage })));
const DiagnosticsSettingsPage = lazy(() => import("./DiagnosticsSettingsPage").then((module) => ({ default: module.DiagnosticsSettingsPage })));
const StorageSettingsPage = lazy(() => import("./StorageSettingsPage").then((module) => ({ default: module.StorageSettingsPage })));
export const QRCodeSVG = lazy(() => import("qrcode.react").then((module) => ({ default: module.QRCodeSVG })));
// SettingsPanel is the desktop settings centre: a modal hosting settings pages and capability management.
export function SettingsPanel({ onClose, onChanged, initialTab, initialFocus, agentRunning = false, desktopPlatform, onUseSubagent, activeWorkspaceKey = "", }: {
    onClose: () => void;
    onChanged: (settings?: SettingsView | null) => void;
    initialTab?: SettingsTab;
    initialFocus?: SettingsInitialFocus;
    agentRunning?: boolean;
    desktopPlatform: DesktopPlatform;
    onUseSubagent: (command: string) => void;
    activeWorkspaceKey?: string;
}) {
    const t = useT();
    const [s, setS] = useState<SettingsView | null>(null);
    const [loadingSettings, setLoadingSettings] = useState(true);
    const [settingsLoadFailed, setSettingsLoadFailed] = useState(false);
    const [busy, setBusy] = useState(false);
    const [err, setErr] = useState<string | null>(null);
    const [warning, setWarning] = useState<string | null>(null);
    const [theme, setThemeState] = useState<Theme>(getTheme());
    const [themeStyle, setThemeStyleState] = useState<ThemeStyle>(() => getThemeStyle(getTheme()));
    const [terminalTheme, setTerminalThemeState] = useState<TerminalThemePreference>(getTerminalThemePreference());
    const [conversationWidth, setConversationWidth] = useState<ConversationWidth>(() => getCachedConversationWidth());
    const [textSize, setTextSizeState] = useState<TextSize>(getTextSize());
    const [zoomPct, setZoomPct] = useState<number>(zoomToPercent(getRestartZoom()));
    const [fontFamily, setFontFamilyState] = useState<FontFamily>(getFontFamily());
    const [monoFontFamily, setMonoFontFamilyState] = useState<MonoFontFamily>(getMonoFontFamily());
    const [customFontName, setCustomFontNameState] = useState<string>(getCustomFontName());
    const [customMonoFontName, setCustomMonoFontNameState] = useState<string>(getCustomMonoFontName());
    const [tab, setTab] = useState<SettingsTab>(initialTab === "providers" ? "models" : initialTab ?? "general");
    const settingsContentRef = useRef<HTMLElement>(null);
    const pendingSubagentCommandRef = useRef<string | null>(null);
    // Play the modal exit animation, then let the parent unmount us and focus
    // the composer with the selected slash command.
    const { status, requestClose } = useDeferredClose(() => {
        const command = pendingSubagentCommandRef.current;
        pendingSubagentCommandRef.current = null;
        onClose();
        if (command)
            onUseSubagent(command);
    }, 240);
    const zoomSaveSeq = useRef(0);
    const terminalThemeSaveSeq = useRef(0);
    const terminalThemeSavePending = useRef(false);
    const terminalThemeSaveQueue = useRef<ReturnType<typeof createTerminalThemeSaveQueue> | null>(null);
    if (!terminalThemeSaveQueue.current) {
        terminalThemeSaveQueue.current = createTerminalThemeSaveQueue((next) => app.SetDesktopTerminalTheme(next));
    }
    const reload = useCallback(async () => {
        setLoadingSettings(true);
        setSettingsLoadFailed(false);
        try {
            const next = normalizeSettingsView(await app.Settings());
            setS(next);
            return next;
        }
        catch {
            setS(null);
            setSettingsLoadFailed(true);
            return null;
        }
        finally {
            setLoadingSettings(false);
        }
    }, []);
    useEffect(() => {
        void reload();
        if (initialTab)
            setTab(initialTab === "providers" ? "models" : initialTab);
    }, [initialTab, reload]);
    useEffect(() => {
        const content = settingsContentRef.current;
        if (!content)
            return;
        content.scrollTop = 0;
        content.scrollLeft = 0;
    }, [tab]);
    useEffect(() => {
        if (!s)
            return;
        const nextTheme = normalizeThemePreference(s.desktopTheme);
        const nextStyle = normalizeThemeStyleForTheme(s.desktopThemeStyle, nextTheme);
        setThemeState(nextTheme);
        setThemeStyleState(nextStyle);
        if (!terminalThemeSavePending.current) {
            setTerminalThemeState(applyTerminalThemePreference(s.desktopTerminalTheme));
        }
        setConversationWidth(applyConversationWidth(s.conversationWidth));
    }, [s?.conversationWidth, s?.desktopTheme, s?.desktopThemeStyle, s?.desktopTerminalTheme]);
    useEffect(() => {
        if (desktopPlatform !== "windows")
            return;
        let cancelled = false;
        void (async () => {
            try {
                const persisted = await app.GetDesktopZoomFactor();
                if (cancelled || typeof persisted !== "number" || !Number.isFinite(persisted))
                    return;
                const snapped = snapZoom(persisted);
                saveRestartZoom(snapped);
                setZoomPct(zoomToPercent(snapped));
            }
            catch {
                // Older mocks or startup races can lack the binding; keep the local fallback.
            }
        })();
        return () => {
            cancelled = true;
        };
    }, [desktopPlatform]);
    // apply runs a mutation, re-reads settings, and refreshes the topbar/model.
    const apply = useCallback(async (fn: () => Promise<unknown>) => {
        setBusy(true);
        setErr(null);
        setWarning(null);
        try {
            const result = await fn();
            const next = await reload();
            onChanged(next);
            window.dispatchEvent(new Event("reasonix:model-catalog-changed"));
            if (typeof result === "string" && result.trim()) {
                setWarning(result.trim());
            }
            return true;
        }
        catch (e) {
            // Settings writes can be two-phase: persistence may succeed before a
            // runtime refresh reports a real boot error. Re-read the authoritative
            // state even on failure so the UI never offers an action that already
            // committed (for example, a DeepSeek protocol upgrade).
            try {
                const next = await reload();
                onChanged(next);
                window.dispatchEvent(new Event("reasonix:model-catalog-changed"));
            }
            catch {
                // Keep the original mutation error; it is the actionable failure.
            }
            setErr(formatSettingsError(e, t));
            return false;
        }
        finally {
            setBusy(false);
        }
    }, [reload, onChanged, t]);
    const backgroundApply = useCallback(async (fn: () => Promise<void>) => {
        setErr(null);
        setWarning(null);
        try {
            await fn();
            const next = await reload();
            onChanged(next);
            window.dispatchEvent(new Event("reasonix:model-catalog-changed"));
        }
        catch (e) {
            setErr(formatSettingsError(e, t));
        }
    }, [reload, onChanged, t]);
    const setTerminalThemePreference = useCallback((next: TerminalThemePreference) => {
        const seq = ++terminalThemeSaveSeq.current;
        const previous = getTerminalThemePreference();
        terminalThemeSavePending.current = true;
        setErr(null);
        setWarning(null);
        applyTerminalThemePreference(next);
        setTerminalThemeState(next);
        void terminalThemeSaveQueue.current!(next)
            .then(async () => {
            if (seq !== terminalThemeSaveSeq.current)
                return;
            const refreshed = await reload();
            if (seq !== terminalThemeSaveSeq.current)
                return;
            terminalThemeSavePending.current = false;
            onChanged(refreshed);
        })
            .catch(async (error) => {
            if (seq !== terminalThemeSaveSeq.current)
                return;
            const refreshed = await reload();
            if (seq !== terminalThemeSaveSeq.current)
                return;
            const restored = normalizeTerminalThemePreference(refreshed?.desktopTerminalTheme ?? previous);
            applyTerminalThemePreference(restored);
            setTerminalThemeState(restored);
            terminalThemeSavePending.current = false;
            setErr(formatSettingsError(error, t));
            onChanged(refreshed);
        });
    }, [onChanged, reload, t]);
    const setRestartZoom = useCallback(async (zoom: ZoomLevel) => {
        const snapped = snapZoom(zoom);
        const seq = ++zoomSaveSeq.current;
        setErr(null);
        setWarning(null);
        setZoomPct(zoomToPercent(snapped));
        try {
            await app.SetDesktopZoomFactor(snapped);
            if (seq === zoomSaveSeq.current)
                saveRestartZoom(snapped);
        }
        catch (e) {
            if (seq !== zoomSaveSeq.current)
                return;
            setErr(formatSettingsError(e, t));
            setZoomPct(zoomToPercent(getRestartZoom()));
        }
    }, [t]);
    // Close on Esc
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => {
            if (e.key === "Escape" && !document.querySelector("[data-anchored-popover='active']"))
                requestClose();
        };
        document.addEventListener("keydown", onKey);
        return () => document.removeEventListener("keydown", onKey);
    }, [requestClose]);
    // These pages need SettingsView; capability pages load their own data.
    const needsSettings = tab === "general" || tab === "models" || tab === "subagents" || tab === "network" || tab === "permissions" || tab === "sandbox" || tab === "appearance" || tab === "about";
    const lazySettingsPageFallback = <div className="empty">{t("settings.loading")}</div>;
    const settingsNavigationItems = useMemo(() => SETTINGS_NAV_TABS.map((id) => ({
        id,
        label: settingsTabLabel(id, t),
        meta: s ? settingsTabMeta(id, s, t) : "",
        searchTerms: id === "general" ? [
            "settings.language", "settings.currency", "settings.displayMode",
            "settings.reasoningDisplay", "settings.processFold", "settings.closeBehavior",
            "settings.defaultToolApprovalMode", "settings.sound", "settings.statusBarStyle", "settings.statusBarItems",
        ].map((key) => t(key as DictKey)).join(" ") : "",
    })), [s, t]);
    return (<div className="management-modal-backdrop settings-modal-backdrop" data-state={status} onMouseDown={(e) => {
            if (e.target === e.currentTarget)
                requestClose();
        }}>
      <div className="management-modal settings-modal" data-state={status}>
        <header className="management-modal__head settings-modal__head">
          <div className="management-modal__title settings-modal__title">{t("settings.title")}</div>
          <ModalCloseButton label={t("common.close")} onClick={requestClose}/>
        </header>

        <div className="settings-center">
          <SettingsNavigation items={settingsNavigationItems} activeTab={tab} onSelect={setTab}/>
          <main ref={settingsContentRef} className="settings-center__content">
            {needsSettings && settingsLoadFailed && (<div className="banner banner--error settings-load-error" role="alert">
                <span>{t("settings.loadFailed")}</span>
                <button className="btn btn--small" type="button" onClick={() => void reload()}>{t("common.retry")}</button>
              </div>)}
            {needsSettings && err && <div className="banner banner--error">{err}</div>}
            {needsSettings && warning && <div className="banner banner--warning">{warning}</div>}
            {needsSettings && !s ? (loadingSettings ? <div className="empty">{t("settings.loading")}</div> : null) : (<>
                {tab === "general" && s && <SettingsPageShell key={tab} s={s} tab={tab} busy={busy} apply={apply}><GeneralSection s={s} busy={busy} apply={apply} agentRunning={agentRunning}/></SettingsPageShell>}
                {tab === "models" && s && <SettingsPageShell key={tab} s={s} tab={tab} busy={busy} apply={apply}><ModelsSection s={s} busy={busy} apply={apply} backgroundApply={backgroundApply} initialFocus={initialFocus}/></SettingsPageShell>}
                {tab === "mcp" && <SettingsPageShell key={tab} s={s} tab={tab} busy={false} apply={apply}><Suspense fallback={lazySettingsPageFallback}><MCPServersSettingsPage /></Suspense></SettingsPageShell>}
                {tab === "skills" && <SettingsPageShell key={tab} s={s} tab={tab} busy={false} apply={apply}><Suspense fallback={lazySettingsPageFallback}><SkillsSettingsPage activeWorkspaceKey={activeWorkspaceKey}/></Suspense></SettingsPageShell>}
                {tab === "subagents" && s && <SettingsPageShell key={tab} s={s} tab={tab} busy={busy} apply={apply}><Suspense fallback={lazySettingsPageFallback}><SubagentsSettingsPage s={s} onUseInChat={(command) => {
                    pendingSubagentCommandRef.current = command;
                    requestClose();
                }}/></Suspense></SettingsPageShell>}
                {tab === "plugins" && <SettingsPageShell key={tab} s={s} tab={tab} busy={false} apply={apply}><Suspense fallback={lazySettingsPageFallback}><PluginsSettingsPage /></Suspense></SettingsPageShell>}
                {tab === "diagnostics" && <SettingsPageShell key={tab} s={s} tab={tab} busy={false} apply={apply}><Suspense fallback={lazySettingsPageFallback}><DiagnosticsSettingsPage onNavigate={setTab}/></Suspense></SettingsPageShell>}
                {tab === "shortcuts" && <SettingsPageShell key={tab} s={s} tab={tab} busy={false} apply={apply}><ShortcutsSection /></SettingsPageShell>}
                {tab === "permissions" && s && <SettingsPageShell key={tab} s={s} tab={tab} busy={busy} apply={apply}><PermissionsSection s={s} busy={busy} apply={apply}/></SettingsPageShell>}
                {tab === "sandbox" && s && <SettingsPageShell key={tab} s={s} tab={tab} busy={busy} apply={apply}><SandboxSection s={s} busy={busy} apply={apply} windows={desktopPlatform === "windows"}/></SettingsPageShell>}
                {tab === "network" && s && <SettingsPageShell key={tab} s={s} tab={tab} busy={busy} apply={apply}><NetworkSection s={s} busy={busy} apply={apply}/></SettingsPageShell>}
                {tab === "appearance" && s && (<SettingsPageShell key={tab} s={s} tab={tab} busy={busy} apply={apply}>
                    <AppearanceOverview theme={theme} themeStyle={themeStyle} terminalTheme={terminalTheme} conversationWidth={conversationWidth} textSize={textSize} showDisplayZoom={desktopPlatform === "windows"} zoomPct={zoomPct} fontFamily={fontFamily} monoFontFamily={monoFontFamily} customFontName={customFontName} customMonoFontName={customMonoFontName} onTheme={(nextTheme) => {
                    applyConfiguredBaseAppearance(nextTheme, themeStyle);
                    setThemeState(nextTheme);
                    void apply(() => app.SetDesktopAppearance(nextTheme, themeStyle));
                }} onConversationWidth={(width) => {
                    applyConversationWidth(width);
                    setConversationWidth(width);
                    void apply(() => app.SetDesktopConversationWidth(width));
                }} onThemeStyle={(style) => {
                    // AppearanceOverview already persists via ActivateBaseStyle /
                    // experience APIs. Parent only mirrors React + DOM state.
                    applyTheme(getTheme(), style, { persist: false });
                    setThemeStyleState(style);
                    setBaseAppearance(getTheme(), style);
                }} onTerminalTheme={setTerminalThemePreference} onTextSize={(size) => {
                    applyTextSize(size);
                    setTextSizeState(size);
                }} onRestartZoom={setRestartZoom} onFontFamily={(font) => {
                    applyFontFamily(font);
                    setFontFamilyState(font);
                }} onMonoFontFamily={(font) => {
                    applyMonoFontFamily(font);
                    setMonoFontFamilyState(font);
                }} onCustomFontNameChange={(name) => {
                    setCustomFontNameState(name);
                    setCustomFontName(name);
                    applyFontFamily("custom");
                }} onCustomMonoFontNameChange={(name) => {
                    setCustomMonoFontNameState(name);
                    setCustomMonoFontName(name);
                    applyMonoFontFamily("custom");
                }}/>
                  </SettingsPageShell>)}
                {tab === "storage" && <SettingsPageShell key={tab} s={s} tab={tab} busy={false} apply={apply}><Suspense fallback={lazySettingsPageFallback}><StorageSettingsPage /></Suspense></SettingsPageShell>}
                {tab === "about" && s && (<SettingsPageShell key={tab} s={s} tab={tab} busy={busy} apply={apply}>
                    <AboutSection configPath={s.configPath} shadowedByPath={s.shadowedByPath}/>
                  </SettingsPageShell>)}
              </>)}
          </main>
        </div>
      </div>
    </div>);
}
export type SectionProps = {
    s: SettingsView;
    busy: boolean;
    apply: (fn: () => Promise<unknown>) => Promise<boolean>;
};
export type ModelsSectionProps = SectionProps & {
    backgroundApply: (fn: () => Promise<void>) => Promise<void>;
    initialFocus?: SettingsInitialFocus;
};
export function ShortcutsSection() {
    const t = useT();
    const [platform] = useState(() => detectShortcutPlatform());
    const [revision, setRevision] = useState(0);
    const [recording, setRecording] = useState<ShortcutAction | null>(null);
    const [conflict, setConflict] = useState<{
        action: ShortcutAction;
        conflictAction: ShortcutAction;
    } | null>(null);
    const [unsupportedAction, setUnsupportedAction] = useState<ShortcutAction | null>(null);
    useEffect(() => onShortcutsChanged(() => setRevision((value) => value + 1)), []);
    const definitions = shortcutDefinitions();
    const commitShortcut = (action: ShortcutAction, event: ReactKeyboardEvent<HTMLButtonElement>) => {
        if (event.key === "Escape") {
            event.preventDefault();
            event.stopPropagation();
            setConflict(null);
            setUnsupportedAction(null);
            setRecording(null);
            return;
        }
        const combo = comboFromKeyboardEvent(event.nativeEvent);
        if (!combo)
            return;
        if (!shortcutAcceptsCombo(action, combo)) {
            // Let the browser move focus before onBlur cancels recording. Updating
            // recording state synchronously here can keep focus on the re-rendered
            // button in WebKit.
            if (event.key === "Tab") {
                const recorder = event.currentTarget;
                queueMicrotask(() => {
                    // Native Tab normally moves focus first. If this WebView does not,
                    // release focus so the recorder cannot become a keyboard trap.
                    if (document.activeElement === recorder)
                        recorder.blur();
                });
                return;
            }
            event.preventDefault();
            event.stopPropagation();
            setConflict(null);
            setUnsupportedAction(action);
            return;
        }
        event.preventDefault();
        event.stopPropagation();
        const conflictDefinition = shortcutConflict(action, combo, platform);
        if (conflictDefinition) {
            setUnsupportedAction(null);
            setConflict({ action, conflictAction: conflictDefinition.action });
            return;
        }
        saveCustomShortcut(action, combo);
        setConflict(null);
        setUnsupportedAction(null);
        setRecording(null);
        setRevision((value) => value + 1);
    };
    return (<SettingsSection title={t("settings.shortcutsTitle")} description={t("settings.shortcutsHint")} actions={<button className="chip chip--icon" type="button" title={t("settings.shortcutsResetAll")} aria-label={t("settings.shortcutsResetAll")} onClick={() => {
                resetCustomShortcuts();
                setConflict(null);
                setUnsupportedAction(null);
                setRecording(null);
                setRevision((value) => value + 1);
            }}>
          <RefreshCw size={14}/>
        </button>}>
      <div className="shortcuts-settings" data-revision={revision}>
        {conflict && (<div className="shortcuts-settings__conflict" role="alert">
            {t("settings.shortcutsConflict", {
                action: t(definitions.find((definition) => definition.action === conflict.action)?.labelKey ?? "settings.tab.shortcuts"),
                conflict: t(definitions.find((definition) => definition.action === conflict.conflictAction)?.labelKey ?? "settings.tab.shortcuts"),
            })}
          </div>)}
        {unsupportedAction && (<div className="shortcuts-settings__conflict" role="alert">
            {t("settings.shortcutsEnterOnly", {
                action: t(definitions.find((definition) => definition.action === unsupportedAction)?.labelKey ?? "settings.tab.shortcuts"),
            })}
          </div>)}
        {definitions.map((definition) => {
            const resolved = resolvedShortcutCombo(definition.action, platform);
            const defaultCombo = definition.defaults[platform];
            const display = formatShortcutCombo(resolved, platform);
            const isCustom = formatShortcutCombo(resolved, platform) !== formatShortcutCombo(defaultCombo, platform);
            const isRecording = recording === definition.action;
            return (<div className="shortcuts-settings__row" key={definition.action}>
              <div className="shortcuts-settings__copy">
                <div className="shortcuts-settings__label">{t(definition.labelKey)}</div>
                <div className="shortcuts-settings__desc">{t(definition.descriptionKey)}</div>
              </div>
              <div className="shortcuts-settings__control">
                <button className={`shortcuts-settings__key${isRecording ? " shortcuts-settings__key--recording" : ""}${definition.configurable === false ? " shortcuts-settings__key--locked" : ""}`} type="button" data-shortcut-action={definition.action} disabled={definition.configurable === false} aria-label={isRecording ? t("settings.shortcutsRecording") : display} aria-pressed={isRecording} onClick={(event) => {
                    setRecording(definition.action);
                    setConflict(null);
                    setUnsupportedAction(null);
                    // WebKit (the desktop WKWebView) does not focus buttons on
                    // click, and the recorder listens for keys on the button —
                    // without this the recorder never receives any keydown.
                    event.currentTarget.focus();
                }} onBlur={() => {
                    if (!isRecording)
                        return;
                    setConflict(null);
                    setUnsupportedAction(null);
                    setRecording(null);
                }} onKeyDown={(event) => isRecording && commitShortcut(definition.action, event)}>
                  {isRecording ? t("settings.shortcutsRecording") : <ShortcutComboDisplay combo={resolved} platform={platform}/>}
                </button>
                <button className="chip" type="button" disabled={!isCustom} onClick={() => {
                    saveCustomShortcut(definition.action, null);
                    setConflict(null);
                    setUnsupportedAction(null);
                    setRecording(null);
                    setRevision((value) => value + 1);
                }}>
                  {t("settings.shortcutsReset")}
                </button>
              </div>
            </div>);
        })}
      </div>
    </SettingsSection>);
}
export { SettingsSection as SettingsSection } from "./settings_shell";
export { SettingsField as SettingsField } from "./settings_shell";
export { settingsModelMeta as settingsModelMeta } from "./settings_shell";
export { SettingsPageShell as SettingsPageShell } from "./settings_shell";
export { settingsTabLabel as settingsTabLabel } from "./settings_shell";
export { settingsTabMeta as settingsTabMeta } from "./settings_shell";

