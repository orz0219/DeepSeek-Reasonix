import { useCallback, useEffect, useId, useRef, useState } from "react";
import { BrainCircuit, Check, ChevronDown, ChevronUp, CircleDollarSign, Languages, ListChecks, Monitor, PanelBottom, Play, Power, ShieldCheck, SlidersHorizontal, Volume2 } from "lucide-react";
import { app } from "../lib/bridge";
import { normalizeLangPref, useI18n, useT, type DictKey, type LangPref } from "../lib/i18n";
import { getDisplayMode, onDisplayModeChange, setDisplayMode as setLocalDisplayMode } from "../lib/displayMode";
import { getProcessFoldPreference, onProcessFoldPreferenceChange, setProcessFoldPreference, type ProcessFoldPreference } from "../lib/processFoldPreference";
import { applyReasoningDisplayMode, useReasoningDisplayMode, type ReasoningDisplayMode } from "../lib/reasoningDisplayPreference";
import { normalizeStatusBarItems, type StatusBarItemId } from "../lib/statusBarItems";
import { normalizeToolApprovalMode } from "../lib/types";
import type { NetworkView } from "../lib/types";
import { Tooltip } from "./Tooltip";
import { AnchoredPopover } from "./AnchoredPopover";
import { getGenerativePreset, setGenerativePreset, generativeMusic, type GenerativePreset } from "../lib/generative-music";
import { SoundSelect } from "./SoundSelect";
import { getSuccessPreference, setSuccessPreference, getAttentionPreference, setAttentionPreference, playSuccessChime, playAttentionChime, type SoundWavPref } from "../lib/sound";
import { StatusBarItemsEditor } from "./StatusBarItemsEditor";
import { SectionProps, SettingsSection, SettingsField, proxyModeLabel } from "./SettingsPanel";
import { normalizeCloseBehavior, DisplayMode, normalizeDisplayMode, normalizeDesktopCurrency, normalizeDesktopLayoutStyle, normalizeStatusBarStyle, desktopLayoutStyleLabel, LANGUAGE_PREFS, DesktopCurrency, closeBehaviorLabel, TOOL_APPROVAL_MODES, statusBarItemLabel, normalizeNetworkView, PROXY_MODES, PROXY_TYPES } from "./settings_normalize";
export function GeneralSection({ s, busy, apply, agentRunning }: SectionProps & {
    agentRunning: boolean;
}) {
    const { t, setPref } = useI18n();
    const closeBehavior = normalizeCloseBehavior(s.closeBehavior);
    const [displayMode, setDisplayMode] = useState<DisplayMode>(() => normalizeDisplayMode(getDisplayMode()));
    const [processFold, setProcessFold] = useState<ProcessFoldPreference>(getProcessFoldPreference);
    const reasoningDisplayMode = useReasoningDisplayMode();
    const soundPanelId = useId();
    useEffect(() => onDisplayModeChange((mode) => setDisplayMode(mode)), []);
    useEffect(() => onProcessFoldPreferenceChange((pref) => setProcessFold(pref)), []);
    const defaultToolApprovalMode = normalizeToolApprovalMode(s.defaultToolApprovalMode);
    const saveReasoningDisplayMode = useCallback(async (mode: ReasoningDisplayMode) => {
        const ok = await apply(() => app.SetReasoningDisplayMode(mode));
        if (ok)
            applyReasoningDisplayMode(mode);
    }, [apply]);
    const languagePref = normalizeLangPref(s.desktopLanguage);
    const desktopCurrency = normalizeDesktopCurrency(s.desktopCurrency);
    const desktopLayoutStyle = normalizeDesktopLayoutStyle(s.desktopLayoutStyle);
    const [genMusicPreset, setGenMusicPreset] = useState<GenerativePreset>(getGenerativePreset());
    const [soundPref, setSoundPref] = useState<SoundWavPref>(getSuccessPreference());
    const [attentionPref, setAttentionPref] = useState<SoundWavPref>(getAttentionPreference());
    const [soundExpanded, setSoundExpanded] = useState(false);
    const statusBarStyle = normalizeStatusBarStyle(s.statusBarStyle);
    const statusBarItems = normalizeStatusBarItems(s.statusBarItems);
    const soundStatus = summarizeSoundStatus(genMusicPreset, soundPref, attentionPref);
    const applyStatusBarItems = (items: StatusBarItemId[]) => {
        const contentScrollTop = document.querySelector<HTMLElement>(".settings-center__content")?.scrollTop ?? 0;
        const navScrollTop = document.querySelector<HTMLElement>(".settings-center__nav")?.scrollTop ?? 0;
        const active = document.activeElement;
        if (active instanceof HTMLElement && active.closest(".status-bar-items-editor"))
            active.blur();
        void apply(() => app.SetStatusBarItems(items)).finally(() => {
            window.scrollTo(0, 0);
            requestAnimationFrame(() => {
                window.scrollTo(0, 0);
                const content = document.querySelector<HTMLElement>(".settings-center__content");
                const nav = document.querySelector<HTMLElement>(".settings-center__nav");
                if (content)
                    content.scrollTop = Math.min(contentScrollTop, Math.max(0, content.scrollHeight - content.clientHeight));
                if (nav)
                    nav.scrollTop = navScrollTop;
            });
        });
    };
    const setLanguage = (next: LangPref) => {
        setPref(next);
        void apply(() => app.SetDesktopLanguage(next));
    };
    return (<>
      <SettingsSection title={t("settings.general.sectionAppearance")} description={t("settings.general.sectionAppearanceHint")}>
      <SettingsField label={t("settings.desktopLayoutStyle")} hint={t("settings.desktopLayoutStyleHint")} icon={<Monitor size={18}/>}>
        <div className="set-seg">
          {(["workbench", "classic", "creation"] as const).map((style) => (<button key={style} className={`set-seg__btn${desktopLayoutStyle === style ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => void apply(() => app.SetDesktopLayoutStyle(style))}>
              {desktopLayoutStyleLabel(style, t)}
            </button>))}
        </div>
      </SettingsField>
      <SettingsField label={t("settings.language")} hint={t("settings.languageHint")} icon={<Languages size={18}/>}>
        <div className="set-seg">
          {LANGUAGE_PREFS.map((pref) => (<button key={pref || "auto"} className={`set-seg__btn${languagePref === pref ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => setLanguage(pref)}>
              {pref === "" ? t("settings.langAuto") : pref === "zh" ? "中文" : "English"}
            </button>))}
        </div>
      </SettingsField>
      <SettingsField label={t("settings.currency")} hint={t("settings.currencyHint")} icon={<CircleDollarSign size={18}/>}>
        <div className="set-seg">
          {(["", "CNY", "USD"] as DesktopCurrency[]).map((currency) => (<button key={currency || "auto"} className={`set-seg__btn${desktopCurrency === currency ? " set-seg__btn--on" : ""}`} disabled={busy || agentRunning} onClick={() => void apply(() => app.SetDesktopCurrency(currency))}>
              {currency === "" ? t("settings.currencyAuto") : currency}
            </button>))}
        </div>
      </SettingsField>
      </SettingsSection>

      <SettingsSection title={t("settings.general.sectionConversation")} description={t("settings.sessionContentDisplayHint")}>
        <SettingsField label={t("settings.displayMode")} hint={t("settings.displayModeHint")} icon={<SlidersHorizontal size={18}/>}>
          <div className="set-seg" role="radiogroup" aria-label={t("settings.displayMode")}>
            {(["standard", "compact"] as const).map((mode) => (<button key={mode} type="button" className={`set-seg__btn${displayMode === mode ? " set-seg__btn--on" : ""}`} aria-pressed={displayMode === mode} disabled={busy} onClick={() => {
                setLocalDisplayMode(mode);
                void apply(() => app.SetDisplayMode(mode));
            }}>
                {t(`settings.displayMode.${mode}`)}
              </button>))}
          </div>
        </SettingsField>
        <SettingsField label={t("settings.reasoningDisplay")} hint={t("settings.reasoningDisplayHint")} icon={<BrainCircuit size={18}/>}>
          <div>
            <div className="set-seg" role="radiogroup" aria-label={t("settings.reasoningDisplay")}>
              {(["hidden", "summary", "auto"] as const).map((mode) => (<button key={mode} type="button" className={`set-seg__btn${reasoningDisplayMode === mode ? " set-seg__btn--on" : ""}`} aria-pressed={reasoningDisplayMode === mode} disabled={busy} onClick={() => void saveReasoningDisplayMode(mode)}>
                  {t(`settings.reasoningDisplay.${mode}`)}
                </button>))}
            </div>
            {reasoningDisplayMode === "legacy-collapsed" && <div className="settings-inline-hint" role="status">{t("settings.reasoningDisplay.legacy")}</div>}
          </div>
        </SettingsField>
        <SettingsField label={t("settings.processFold")} hint={t("settings.processFoldHint")} icon={<ListChecks size={18}/>}>
          <div className="set-seg" role="radiogroup" aria-label={t("settings.processFold")}>
            {(["auto", "expanded"] as const).map((pref) => (<button key={pref} type="button" className={`set-seg__btn${processFold === pref ? " set-seg__btn--on" : ""}`} aria-pressed={processFold === pref} onClick={() => setProcessFoldPreference(pref)}>
                {t(`settings.processFold.${pref}`)}
              </button>))}
          </div>
        </SettingsField>
      </SettingsSection>

      <SettingsSection title={t("settings.general.sectionSystem")} description={t("settings.general.sectionSystemHint")}>
      <SettingsField label={t("settings.closeBehavior")} hint={t("settings.closeBehaviorHint")} icon={<Power size={18}/>}>
        <div className="set-seg">
          {(["background", "quit"] as const).map((mode) => (<button key={mode} className={`set-seg__btn${closeBehavior === mode ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => void apply(() => app.SetCloseBehavior(mode))}>
              {closeBehaviorLabel(mode, t)}
            </button>))}
        </div>
      </SettingsField>
      <SettingsField label={t("settings.defaultToolApprovalMode")} hint={t("settings.defaultToolApprovalModeHint")} icon={<ShieldCheck size={18}/>}>
        <div className="set-seg">
          {TOOL_APPROVAL_MODES.map((mode) => (<button key={mode} className={`set-seg__btn${defaultToolApprovalMode === mode ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => void apply(() => app.SetDefaultToolApprovalMode(mode))}>
              {t(`settings.defaultToolApprovalMode.${mode}`)}
            </button>))}
        </div>
      </SettingsField>
      <SettingsField label={t("settings.sound")} hint={t("settings.soundHint")} icon={<Volume2 size={18}/>} stacked>
        <div className={`settings-sound-editor${soundExpanded ? " settings-sound-editor--expanded" : ""}`}>
          <div className="settings-sound-editor__summary">
            <span className={`settings-sound-editor__status settings-sound-editor__status--${soundStatus}`}>
              {t(`settings.soundStatus.${soundStatus}`)}
            </span>
            <Tooltip label={t(soundExpanded ? "settings.soundCollapse" : "settings.soundExpand")}>
              <button type="button" className="settings-sound-editor__toggle" aria-expanded={soundExpanded} aria-controls={soundPanelId} aria-label={t(soundExpanded ? "settings.soundCollapse" : "settings.soundExpand")} onClick={() => setSoundExpanded((open) => !open)}>
                {soundExpanded ? <ChevronUp size={15} aria-hidden="true"/> : <ChevronDown size={15} aria-hidden="true"/>}
              </button>
            </Tooltip>
          </div>
          {soundExpanded && (<div className="settings-sound-editor__list" id={soundPanelId}>
              <div className="settings-sound-row">
                <span className="settings-sound-row__label">{t("settings.generativeMusic")}</span>
                <GenMusicSelect value={genMusicPreset} onChange={(next) => {
                setGenMusicPreset(next);
                setGenerativePreset(next);
                if (next === "off") {
                    generativeMusic.stop();
                }
                else {
                    if (generativeMusic.isRunning) {
                        generativeMusic.setPreset(next);
                    }
                    else if (agentRunning) {
                        generativeMusic.start(next);
                    }
                    generativeMusic.playPreview(next);
                }
            }} onPreview={() => { if (genMusicPreset !== "off")
            generativeMusic.playPreview(genMusicPreset); }} previewDisabled={genMusicPreset === "off"}/>
              </div>
              <div className="settings-sound-row">
                <span className="settings-sound-row__label">{t("settings.notificationSoundSuccess")}</span>
                <SoundSelect value={soundPref} onChange={(next) => {
                setSoundPref(next);
                setSuccessPreference(next);
                playSuccessChime();
            }} onPreview={playSuccessChime} previewDisabled={soundPref === "off"}/>
              </div>
              <div className="settings-sound-row">
                <span className="settings-sound-row__label">{t("settings.notificationSoundAttention")}</span>
                <SoundSelect value={attentionPref} onChange={(next) => {
                setAttentionPref(next);
                setAttentionPreference(next);
                playAttentionChime();
            }} onPreview={playAttentionChime} previewDisabled={attentionPref === "off"}/>
              </div>
            </div>)}
        </div>
      </SettingsField>
      <SettingsField label={t("settings.statusBarStyle")} hint={t("settings.statusBarStyleHint")} icon={<PanelBottom size={18}/>}>
        <div className="set-seg">
          {(["icon", "text"] as const).map((style) => (<button key={style} className={`set-seg__btn${statusBarStyle === style ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => void apply(() => app.SetStatusBarStyle(style))}>
              {t(`settings.statusBarStyle.${style}`)}
            </button>))}
        </div>
      </SettingsField>
      <SettingsField label={t("settings.statusBarItems")} hint={t("settings.statusBarItemsHint")} icon={<ListChecks size={18}/>} className="status-bar-items-setting" stacked>
        <StatusBarItemsEditor items={statusBarItems} busy={busy} onChange={applyStatusBarItems} itemLabel={(id) => statusBarItemLabel(id, t)}/>
      </SettingsField>
    </SettingsSection>
    </>);
}
const GENRE_OPTIONS: {
    value: GenerativePreset;
    labelKey: DictKey;
}[] = [
    { value: "off", labelKey: "settings.generativeMusic.off" },
    { value: "ethereal", labelKey: "settings.generativeMusic.presets.ethereal" },
    { value: "classic", labelKey: "settings.generativeMusic.presets.classic" },
    { value: "digital", labelKey: "settings.generativeMusic.presets.digital" },
    { value: "retro", labelKey: "settings.generativeMusic.presets.retro" },
];
function summarizeSoundStatus(music: GenerativePreset, success: SoundWavPref, attention: SoundWavPref): "allOff" | "enabled" | "custom" {
    const enabledCount = [music !== "off", success !== "off", attention !== "off"].filter(Boolean).length;
    if (enabledCount === 0)
        return "allOff";
    if (enabledCount === 1)
        return "enabled";
    return "custom";
}
function GenMusicSelect({ value, onChange, onPreview, previewDisabled, }: {
    value: GenerativePreset;
    onChange: (v: GenerativePreset) => void;
    onPreview: () => void;
    previewDisabled?: boolean;
}) {
    const t = useT();
    const [open, setOpen] = useState(false);
    const triggerRef = useRef<HTMLButtonElement>(null);
    const selected = GENRE_OPTIONS.find((o) => o.value === value) ?? GENRE_OPTIONS[0];
    return (<div className="sound-select">
      <button ref={triggerRef} className="sound-select__trigger" type="button" onClick={() => setOpen((v) => !v)}>
        <span className="sound-select__label">{t(selected.labelKey)}</span>
        <ChevronDown size={16} className={`sound-select__chev${open ? " sound-select__chev--open" : ""}`}/>
      </button>
      {!previewDisabled && (<button className="chip chip--icon" type="button" title={t("settings.generativeMusicPreview")} aria-label={t("settings.generativeMusicPreview")} onClick={onPreview}>
          <Play size={13} aria-hidden="true"/>
        </button>)}
      <AnchoredPopover open={open} anchorRef={triggerRef} onClose={() => setOpen(false)} className="sound-select__menu" placement="bottom">
        <div className="sound-select__list" role="listbox">
          {GENRE_OPTIONS.map((opt) => (<button key={opt.value} className={`sound-select__option${opt.value === value ? " sound-select__option--selected" : ""}`} role="option" aria-selected={opt.value === value} type="button" onClick={() => {
                onChange(opt.value);
                setOpen(false);
            }}>
              <span>{t(opt.labelKey)}</span>
              {opt.value === value && <Check size={14} className="sound-select__check"/>}
            </button>))}
        </div>
      </AnchoredPopover>
    </div>);
}
export function NetworkSection({ s, busy, apply }: SectionProps) {
    const t = useT();
    const savedNetwork = normalizeNetworkView(s.network);
    const [draft, setDraft] = useState<NetworkView>(savedNetwork);
    useEffect(() => setDraft(normalizeNetworkView(s.network)), [s.network]);
    const dirty = JSON.stringify(draft) !== JSON.stringify(savedNetwork);
    const setProxy = (next: Partial<NetworkView["proxy"]>) => {
        setDraft({ ...draft, proxy: { ...draft.proxy, ...next } });
    };
    return (<SettingsSection title={t("settings.tab.network")} actions={<button className="btn btn--primary btn--small" disabled={busy || !dirty} onClick={() => void apply(() => app.SetNetwork(draft))}>
          {t("settings.saveNetwork")}
        </button>}>
      <SettingsField label={t("settings.proxyMode")}>
        <div className="set-seg">
          {PROXY_MODES.map((mode) => (<button key={mode} className={`set-seg__btn${draft.proxyMode === mode ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => setDraft({ ...draft, proxyMode: mode })}>
              {proxyModeLabel(mode, t)}
            </button>))}
        </div>
      </SettingsField>

      {draft.proxyMode === "custom" && (<>
          <SettingsField label={t("settings.proxyType")}>
            <div className="set-seg">
              {PROXY_TYPES.map((typ) => (<button key={typ} className={`set-seg__btn${draft.proxy.type === typ ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => setProxy({ type: typ })}>
                  {typ.toUpperCase()}
                </button>))}
            </div>
          </SettingsField>
          <SettingsField label={t("settings.proxyServer")}>
            <div className="settings-inline-controls">
            <input className="mem-input set-grow" placeholder="127.0.0.1" value={draft.proxy.server} disabled={busy || !!draft.proxyUrl.trim()} onChange={(e) => setProxy({ server: e.target.value })}/>
            <label className="set-label">{t("settings.proxyPort")}</label>
            <input className="mem-input set-narrow" placeholder="7890" value={draft.proxy.port ? String(draft.proxy.port) : ""} disabled={busy || !!draft.proxyUrl.trim()} inputMode="numeric" onChange={(e) => setProxy({ port: Number(e.target.value) || 0 })}/>
            </div>
          </SettingsField>
          <SettingsField label={t("settings.proxyUsername")}>
            <div className="settings-inline-controls">
            <input className="mem-input set-grow" value={draft.proxy.username} disabled={busy || !!draft.proxyUrl.trim()} onChange={(e) => setProxy({ username: e.target.value })}/>
            <label className="set-label">{t("settings.proxyPassword")}</label>
            <input className="mem-input set-grow" type="password" value={draft.proxy.password} disabled={busy || !!draft.proxyUrl.trim()} onChange={(e) => setProxy({ password: e.target.value })}/>
            </div>
          </SettingsField>
          <SettingsField label={t("settings.proxyUrl")} hint={t("settings.proxyUrlHint")}>
              <input className="mem-input set-grow" placeholder="socks5://127.0.0.1:7890" value={draft.proxyUrl} disabled={busy} onChange={(e) => setDraft({ ...draft, proxyUrl: e.target.value })}/>
          </SettingsField>
          <SettingsField label={t("settings.noProxy")}>
            <input className="mem-input set-grow" placeholder="localhost,127.0.0.1,.local" value={draft.noProxy} disabled={busy} onChange={(e) => setDraft({ ...draft, noProxy: e.target.value })}/>
          </SettingsField>
        </>)}
    </SettingsSection>);
}

