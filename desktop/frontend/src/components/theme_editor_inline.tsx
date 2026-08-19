import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ImagePlus, X } from "lucide-react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { THEME_STYLES } from "../lib/theme";
import { beginThemePreview, cancelThemePreview, defaultBackground, defaultTaskBackground, draftPackView, isSafeHex, themeTokenKeys } from "../lib/themePack";
import { useToast } from "../lib/toast";
import { ThemePreviewSurface } from "./ThemePreviewSurface";
import { EditorState, themeContrastRatio, TOKEN_GROUPS, ThemePreviewControls } from "./theme_gallery_helpers";
export function ThemeEditorInline({ state, busy, onChange, onCancel, onSave, }: {
    state: EditorState;
    busy: boolean;
    onChange: (patch: Partial<EditorState>) => void;
    onCancel: () => void;
    onSave: (activate: boolean) => void;
}) {
    const t = useT();
    const { showToast } = useToast();
    const [previewMode, setPreviewMode] = useState<"light" | "dark">("dark");
    const [previewScene, setPreviewScene] = useState<"home" | "task">("home");
    const titleId = useId();
    const editorRef = useRef<HTMLDivElement>(null);
    const initialFocusRef = useRef<HTMLInputElement>(null);
    const restoreFocusRef = useRef<HTMLElement | null>(null);
    const busyRef = useRef(busy);
    const onCancelRef = useRef(onCancel);
    busyRef.current = busy;
    onCancelRef.current = onCancel;
    const homeUrl = state.backgroundDataUrl || state.existingBackgroundUrl;
    const taskUrl = state.taskBackgroundDataUrl || state.existingTaskBackgroundUrl;
    const draft = useMemo(() => draftPackView({
        id: state.id || "preview",
        name: state.name || "Preview",
        baseStyle: state.baseStyle,
        tokens: state.tokens,
        recipes: state.recipes,
        background: state.background,
        backgroundUrl: homeUrl,
        taskBackground: state.taskBackground,
        taskBackgroundUrl: taskUrl,
    }), [homeUrl, state.baseStyle, state.background, state.id, state.name, state.recipes, state.taskBackground, state.tokens, taskUrl]);
    useEffect(() => {
        beginThemePreview(draft);
    }, [draft]);
    useEffect(() => () => cancelThemePreview(), []);
    useLayoutEffect(() => {
        restoreFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
        const initialFocus = initialFocusRef.current && !initialFocusRef.current.disabled
            ? initialFocusRef.current
            : editorRef.current?.querySelector<HTMLElement>('input:not([disabled]), textarea:not([disabled]), button:not([disabled])');
        (initialFocus || editorRef.current)?.focus();
        return () => {
            if (restoreFocusRef.current?.isConnected)
                restoreFocusRef.current.focus();
        };
    }, []);
    useEffect(() => {
        const onKeyDown = (event: KeyboardEvent) => {
            if (event.key === "Escape") {
                event.preventDefault();
                event.stopPropagation();
                if (!busyRef.current)
                    onCancelRef.current();
                return;
            }
            if (event.key !== "Tab")
                return;
            const focusable = Array.from(editorRef.current?.querySelectorAll<HTMLElement>('button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])') || []).filter((element) => !element.hidden && element.getAttribute("aria-hidden") !== "true");
            if (focusable.length === 0)
                return;
            const first = focusable[0];
            const last = focusable[focusable.length - 1];
            if (event.shiftKey && document.activeElement === first) {
                event.preventDefault();
                last.focus();
            }
            else if (!event.shiftKey && document.activeElement === last) {
                event.preventDefault();
                first.focus();
            }
        };
        document.addEventListener("keydown", onKeyDown, { capture: true });
        return () => document.removeEventListener("keydown", onKeyDown, { capture: true });
    }, []);
    const setToken = (key: string, value: string) => {
        const next = {
            ...state.tokens,
            [state.tokenMode]: { ...(state.tokens[state.tokenMode] || {}) },
        };
        if (value)
            next[state.tokenMode]![key] = value;
        else
            delete next[state.tokenMode]![key];
        onChange({ tokens: next });
    };
    const pickBackground = async (scene: "home" | "task") => {
        try {
            const dataUrl = await app.PickThemeBackground();
            if (!dataUrl)
                return;
            if (scene === "home") {
                onChange({
                    background: state.background ? { ...state.background } : defaultBackground(),
                    backgroundDataUrl: dataUrl,
                    existingBackgroundUrl: "",
                });
            }
            else {
                onChange({
                    taskBackground: state.taskBackground ? { ...state.taskBackground } : defaultTaskBackground(),
                    taskBackgroundDataUrl: dataUrl,
                    existingTaskBackgroundUrl: "",
                });
            }
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
    };
    const warnings = useMemo(() => {
        const out: string[] = [];
        for (const mode of ["light", "dark"] as const) {
            const fg = state.tokens[mode]?.fg;
            const bg = state.tokens[mode]?.bg;
            if (fg && bg && isSafeHex(fg) && isSafeHex(bg)) {
                const ratio = themeContrastRatio(fg, bg);
                if (ratio < 4.5)
                    out.push(`${mode} fg/bg ${ratio.toFixed(2)} < 4.5`);
            }
        }
        return out;
    }, [state.tokens]);
    const appLayoutClass = ["app--workbench"]
        .find((className) => document.querySelector(`.${className}`)) || "";
    return createPortal(<div className="theme-gallery__editor-overlay">
      <div ref={editorRef} className={`theme-editor theme-gallery__editor${appLayoutClass ? ` ${appLayoutClass}` : ""}`} role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1}>
        <header className="theme-editor__header">
          <div>
            <strong id={titleId}>{state.mode === "create" ? t("settings.themeLibrary.editorCreate") : t("settings.themeLibrary.editorEdit")}</strong>
            <p>{t("settings.themeEditor.subtitle")}</p>
          </div>
          <button type="button" className="btn btn--icon" aria-label={t("common.close")} disabled={busy} onClick={onCancel}>
            <X size={16}/>
          </button>
        </header>

        <div className="theme-editor__body">
          <div className="theme-editor__controls">
            <section className="theme-editor__section">
              <h3>{t("settings.themeEditor.metadata")}</h3>
              <div className="theme-editor__fields theme-editor__fields--grid">
                <label><span>{t("settings.themeLibrary.fieldId")}</span><input ref={initialFocusRef} value={state.id} disabled={state.mode === "edit" || busy} onChange={(e) => onChange({ id: e.target.value })}/></label>
                <label><span>{t("settings.themeLibrary.fieldName")}</span><input value={state.name} disabled={busy} onChange={(e) => onChange({ name: e.target.value })}/></label>
                <label><span>{t("settings.themeLibrary.fieldAuthor")}</span><input value={state.author} disabled={busy} onChange={(e) => onChange({ author: e.target.value })}/></label>
                <label><span>{t("settings.themeEditor.license")}</span><input value={state.license} disabled={busy} placeholder="MIT" onChange={(e) => onChange({ license: e.target.value })}/></label>
                <label className="theme-editor__wide"><span>{t("settings.themeEditor.description")}</span><textarea value={state.description} disabled={busy} rows={2} onChange={(e) => onChange({ description: e.target.value })}/></label>
              </div>
            </section>

            <section className="theme-editor__section">
              <h3>{t("settings.themeEditor.layout")}</h3>
              <div className="theme-editor__setting-row">
                <span>{t("settings.themeLibrary.fieldBase")}</span>
                <div className="set-seg theme-editor__base-styles">
                  {THEME_STYLES.map((s) => (<button key={s} type="button" className={`set-seg__btn${state.baseStyle === s ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => onChange({ baseStyle: s })}>{t(`settings.style.${s}.zh` as never)}</button>))}
                </div>
              </div>
              <div className="theme-editor__setting-row">
                <span>{t("settings.themeLibrary.fieldRecipes")}</span>
                <div className="theme-editor__recipe-groups">
                  <div className="set-seg">
                    {(["comfortable", "compact"] as const).map((density) => (<button key={density} type="button" className={`set-seg__btn${state.recipes.density === density ? " set-seg__btn--on" : ""}`} onClick={() => onChange({ recipes: { ...state.recipes, density } })}>{t(`settings.themeEditor.density.${density}` as never)}</button>))}
                  </div>
                  <div className="set-seg">
                    {(["square", "soft", "round"] as const).map((corners) => (<button key={corners} type="button" className={`set-seg__btn${state.recipes.corners === corners ? " set-seg__btn--on" : ""}`} onClick={() => onChange({ recipes: { ...state.recipes, corners } })}>{t(`settings.themeEditor.corners.${corners}` as never)}</button>))}
                  </div>
                </div>
              </div>
            </section>

            <section className="theme-editor__section">
              <div className="theme-editor__section-head">
                <h3>{t("settings.themeEditor.colors")}</h3>
                <div className="set-seg">
                  {(["light", "dark"] as const).map((mode) => (<button key={mode} type="button" className={`set-seg__btn${state.tokenMode === mode ? " set-seg__btn--on" : ""}`} onClick={() => onChange({ tokenMode: mode })}>{mode === "light" ? t("settings.themeLight") : t("settings.themeDark")}</button>))}
                </div>
              </div>
              {TOKEN_GROUPS.map((group) => (<div key={group.labelKey} className="theme-editor__token-group">
                  <h4>{t(group.labelKey as never)}</h4>
                  <div className="theme-editor__color-grid">
                    {group.keys.filter((key) => themeTokenKeys().includes(key)).map((key) => {
                const value = state.tokens[state.tokenMode]?.[key] || "";
                return (<label key={key} className="theme-editor__color">
                          <span className="theme-editor__token-label"><span>{t(`settings.themeTokens.key.${key}` as never)}</span><code>{key}</code></span>
                          <div className="theme-editor__color-control">
                            <input type="color" value={isSafeHex(value) ? value.slice(0, 7) : "#888888"} disabled={busy} onChange={(e) => setToken(key, e.target.value)}/>
                            <input type="text" value={value} placeholder="#RRGGBB" disabled={busy} onChange={(e) => setToken(key, e.target.value.trim())}/>
                          </div>
                        </label>);
            })}
                  </div>
                </div>))}
            </section>

            <section className="theme-editor__section">
              <h3>{t("settings.themeEditor.scenes")}</h3>
              <p className="theme-editor__section-help">{t("settings.themeEditor.scenesHint")}</p>
              <div className="theme-editor__scene-grid">
                <SceneImageEditor title={t("settings.themeEditor.homeBackground")} hint={t("settings.themeEditor.homeBackgroundHint")} url={homeUrl} present={Boolean(state.background)} focusX={state.background?.focusX ?? 0.5} focusY={state.background?.focusY ?? 0.5} safeArea={state.background?.safeArea || "center"} opacity={state.background?.homeOpacity ?? 1} opacityMax={1} overlayStrength={state.background?.overlayStrength ?? 0.62} paneOpacity={state.background?.paneOpacity ?? 0.72} busy={busy} onPick={() => void pickBackground("home")} onClear={() => onChange({ background: null, backgroundDataUrl: "", existingBackgroundUrl: "" })} onPatch={(patch) => {
            const { opacity: nextOpacity, ...rest } = patch;
            onChange({ background: { ...(state.background || defaultBackground()), ...rest, homeOpacity: nextOpacity ?? state.background?.homeOpacity ?? 1 } });
        }}/>
                <SceneImageEditor title={t("settings.themeEditor.taskBackground")} hint={state.taskBackground ? t("settings.themeEditor.taskBackgroundHint") : t("settings.themeEditor.taskBackgroundFallback")} url={taskUrl || homeUrl} present={Boolean(state.taskBackground)} focusX={state.taskBackground?.focusX ?? state.background?.focusX ?? 0.5} focusY={state.taskBackground?.focusY ?? state.background?.focusY ?? 0.5} safeArea={state.taskBackground?.safeArea || state.background?.safeArea || "center"} opacity={state.taskBackground?.opacity ?? state.background?.taskOpacity ?? 0.28} opacityMax={1} overlayStrength={state.taskBackground?.overlayStrength ?? state.background?.overlayStrength ?? 0.62} paneOpacity={state.taskBackground?.paneOpacity ?? state.background?.paneOpacity ?? 0.80} busy={busy} onPick={() => void pickBackground("task")} onClear={() => onChange({ taskBackground: null, taskBackgroundDataUrl: "", existingTaskBackgroundUrl: "" })} onPatch={(patch) => onChange({ taskBackground: { ...(state.taskBackground || defaultTaskBackground()), ...patch, opacity: patch.opacity ?? state.taskBackground?.opacity ?? 0.28 } })}/>
              </div>
            </section>

            {warnings.length > 0 ? (<div className="theme-editor__warn">
                {t("settings.themeLibrary.contrastWarn")}
                <ul>{warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul>
              </div>) : null}
          </div>

          <aside className="theme-editor__live">
            <div className="theme-editor__live-head">
              <strong>{t("settings.themeEditor.livePreview")}</strong>
              <span>{previewScene === "home" ? t("settings.themeGallery.sceneHome") : t("settings.themeGallery.sceneTask")}</span>
            </div>
            <ThemePreviewControls mode={previewMode} scene={previewScene} onModeChange={setPreviewMode} onSceneChange={setPreviewScene}/>
            <ThemePreviewSurface pack={draft} mode={previewMode} scene={previewScene}/>
            <p>{t("settings.themeEditor.livePreviewHint")}</p>
          </aside>
        </div>

        <div className="theme-editor__actions">
          <button type="button" className="btn" disabled={busy} onClick={onCancel}>
            {t("common.cancel")}
          </button>
          <button type="button" className="btn" disabled={busy} onClick={() => onSave(false)}>
            {t("common.save")}
          </button>
          <button type="button" className="btn btn--primary" disabled={busy} onClick={() => onSave(true)}>
            {t("settings.themeGallery.saveAndApply")}
          </button>
        </div>
      </div>
    </div>, document.body);
}
type ScenePatch = {
    focusX?: number;
    focusY?: number;
    safeArea?: "left" | "center" | "right";
    opacity?: number;
    overlayStrength?: number;
    paneOpacity?: number;
};
function SceneImageEditor({ title, hint, url, present, focusX, focusY, safeArea, opacity, opacityMax, overlayStrength, paneOpacity, busy, onPick, onClear, onPatch, }: {
    title: string;
    hint: string;
    url: string;
    present: boolean;
    focusX: number;
    focusY: number;
    safeArea: string;
    opacity: number;
    opacityMax: number;
    overlayStrength: number;
    paneOpacity: number;
    busy: boolean;
    onPick: () => void;
    onClear: () => void;
    onPatch: (patch: ScenePatch) => void;
}) {
    const t = useT();
    const previewRef = useRef<HTMLDivElement>(null);
    const dragging = useRef(false);
    const updateFocus = (clientX: number, clientY: number) => {
        const rect = previewRef.current?.getBoundingClientRect();
        if (!rect || !present)
            return;
        onPatch({
            focusX: Math.min(1, Math.max(0, (clientX - rect.left) / rect.width)),
            focusY: Math.min(1, Math.max(0, (clientY - rect.top) / rect.height)),
        });
    };
    return (<div className={`theme-editor__scene${present ? " theme-editor__scene--ready" : ""}`}>
      <div className="theme-editor__scene-head">
        <div><strong>{title}</strong><p>{hint}</p></div>
        <div className="theme-editor__scene-actions">
          <button type="button" className="btn btn--small" disabled={busy} onClick={onPick}><ImagePlus size={13}/> {t("settings.themeLibrary.pickImage")}</button>
          {present ? <button type="button" className="btn btn--small" disabled={busy} onClick={onClear}>{t("settings.themeLibrary.clearImage")}</button> : null}
        </div>
      </div>
      <div ref={previewRef} className="theme-editor__bg-preview" style={url ? { backgroundImage: `url("${url}")`, opacity } : undefined} onPointerDown={(event) => {
            dragging.current = true;
            event.currentTarget.setPointerCapture?.(event.pointerId);
            updateFocus(event.clientX, event.clientY);
        }} onPointerMove={(event) => dragging.current && updateFocus(event.clientX, event.clientY)} onPointerUp={() => { dragging.current = false; }}>
        {!url ? <div className="theme-editor__scene-empty"><ImagePlus size={22}/><span>{t("settings.themeEditor.uploadPrompt")}</span></div> : null}
        {present ? <span className="theme-editor__focus" style={{ left: `${focusX * 100}%`, top: `${focusY * 100}%` }}/> : null}
      </div>
      {present ? (<div className="theme-editor__scene-controls">
          <div className="theme-editor__setting-block">
            <div className="theme-editor__setting-row">
              <span>{t("settings.themeEditor.safeArea")}</span>
              <div className="set-seg" role="radiogroup" aria-label={t("settings.themeEditor.safeArea")}>
                {(["left", "center", "right"] as const).map((area) => <button key={area} type="button" role="radio" aria-checked={safeArea === area} className={`set-seg__btn${safeArea === area ? " set-seg__btn--on" : ""}`} onClick={() => onPatch({ safeArea: area })}>{t(`settings.themeEditor.safeArea.${area}` as never)}</button>)}
              </div>
            </div>
            <p className="theme-editor__setting-hint">{t("settings.themeEditor.safeAreaHint")}</p>
          </div>
          <label className="theme-editor__range"><span>{t("settings.themeEditor.opacity")} <b>{Math.round(opacity * 100)}%</b></span><input type="range" min={0} max={opacityMax} step={0.01} value={opacity} onChange={(e) => onPatch({ opacity: Number(e.target.value) })}/></label>
          <label className="theme-editor__range"><span>{t("settings.themeLibrary.overlayStrength")} <b>{Math.round(overlayStrength * 100)}%</b></span><input type="range" min={0} max={1} step={0.01} value={overlayStrength} onChange={(e) => onPatch({ overlayStrength: Number(e.target.value) })}/></label>
          <label className="theme-editor__range"><span>{t("settings.themeEditor.paneOpacity")} <b>{Math.round(paneOpacity * 100)}%</b></span><input type="range" min={0} max={1} step={0.01} value={paneOpacity} onChange={(e) => onPatch({ paneOpacity: Number(e.target.value) })}/></label>
        </div>) : null}
    </div>);
}

