import { useMemo, useRef } from "react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { THEME_STYLES } from "../lib/theme";
import { defaultBackground, isSafeHex, themeTokenKeys } from "../lib/themePack";
import { useToast } from "../lib/toast";
import { EditorState, TOKEN_GROUPS } from "./theme_library_helpers";
export function ThemeEditor({ state, busy, onChange, onCancel, onSave, }: {
    state: EditorState;
    busy: boolean;
    onChange: (patch: Partial<EditorState>) => void;
    onCancel: () => void;
    onSave: (activate: boolean) => void;
}) {
    const t = useT();
    const { showToast } = useToast();
    const previewRef = useRef<HTMLDivElement>(null);
    const dragging = useRef(false);
    const setToken = (key: string, value: string) => {
        const mode = state.tokenMode;
        const nextTokens = {
            ...state.tokens,
            [mode]: { ...(state.tokens[mode] || {}), [key]: value },
        };
        // Allow empty to clear override
        if (!value) {
            const map = { ...(nextTokens[mode] || {}) };
            delete map[key];
            nextTokens[mode] = map;
        }
        else if (!isSafeHex(value) && value.length >= 7) {
            // Keep typing intermediate values without applying invalid hex to preview tokens fully
        }
        onChange({ tokens: nextTokens });
    };
    const pickBackground = async () => {
        try {
            const dataUrl = await app.PickThemeBackground();
            if (!dataUrl)
                return;
            const bg = state.background ? { ...state.background } : defaultBackground();
            onChange({
                background: bg,
                backgroundDataUrl: dataUrl,
                existingBackgroundUrl: "",
            });
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
    };
    const onFocusPointer = (clientX: number, clientY: number) => {
        const el = previewRef.current;
        if (!el || !state.background)
            return;
        const rect = el.getBoundingClientRect();
        const x = Math.min(1, Math.max(0, (clientX - rect.left) / rect.width));
        const y = Math.min(1, Math.max(0, (clientY - rect.top) / rect.height));
        onChange({ background: { ...state.background, focusX: x, focusY: y } });
    };
    const bgUrl = state.backgroundDataUrl || state.existingBackgroundUrl;
    const warnings = useMemo(() => {
        // Client-side soft check mirroring backend pairs.
        const out: string[] = [];
        for (const mode of ["light", "dark"] as const) {
            const fg = state.tokens[mode]?.fg;
            const bg = state.tokens[mode]?.bg;
            if (fg && bg && isSafeHex(fg) && isSafeHex(bg)) {
                const ratio = contrastRatio(fg, bg);
                if (ratio < 4.5)
                    out.push(`${mode} fg/bg ${ratio.toFixed(2)} < 4.5`);
            }
        }
        return out;
    }, [state.tokens]);
    return (<div className="theme-editor">
      <strong>{state.mode === "create" ? t("settings.themeLibrary.editorCreate") : t("settings.themeLibrary.editorEdit")}</strong>

      <div className="theme-editor__row">
        <div className="theme-editor__label">{t("settings.themeLibrary.fieldId")}</div>
        <div className="theme-editor__fields">
          <input value={state.id} disabled={state.mode === "edit" || busy} onChange={(e) => onChange({ id: e.target.value })}/>
          <input value={state.name} disabled={busy} placeholder={t("settings.themeLibrary.fieldName")} onChange={(e) => onChange({ name: e.target.value })}/>
          <input value={state.author} disabled={busy} placeholder={t("settings.themeLibrary.fieldAuthor")} onChange={(e) => onChange({ author: e.target.value })}/>
        </div>
      </div>

      <div className="theme-editor__row">
        <div className="theme-editor__label">{t("settings.themeLibrary.fieldBase")}</div>
        <div className="set-seg">
          {THEME_STYLES.map((s) => (<button key={s} type="button" className={`set-seg__btn${state.baseStyle === s ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => onChange({ baseStyle: s })}>
              {s}
            </button>))}
        </div>
      </div>

      <div className="theme-editor__row">
        <div className="theme-editor__label">{t("settings.themeLibrary.fieldRecipes")}</div>
        <div className="theme-editor__fields">
          <div className="set-seg">
            {(["comfortable", "compact"] as const).map((d) => (<button key={d} type="button" className={`set-seg__btn${state.recipes.density === d ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => onChange({ recipes: { ...state.recipes, density: d } })}>
                {d}
              </button>))}
          </div>
          <div className="set-seg">
            {(["square", "soft", "round"] as const).map((c) => (<button key={c} type="button" className={`set-seg__btn${state.recipes.corners === c ? " set-seg__btn--on" : ""}`} disabled={busy} onClick={() => onChange({ recipes: { ...state.recipes, corners: c } })}>
                {c}
              </button>))}
          </div>
        </div>
      </div>

      <div className="theme-editor__row">
        <div className="theme-editor__label">{t("settings.themeLibrary.fieldTokens")}</div>
        <div className="theme-editor__fields">
          <div className="set-seg">
            {(["dark", "light"] as const).map((m) => (<button key={m} type="button" className={`set-seg__btn${state.tokenMode === m ? " set-seg__btn--on" : ""}`} onClick={() => onChange({ tokenMode: m })}>
                {m}
              </button>))}
          </div>
          {TOKEN_GROUPS.map((group) => (<div key={group.labelKey}>
              <div className="theme-lib-card__sub" style={{ marginBottom: 6 }}>{t(group.labelKey as never)}</div>
              <div className="theme-editor__color-grid">
                {group.keys.filter((k) => themeTokenKeys().includes(k)).map((key) => {
                const val = state.tokens[state.tokenMode]?.[key] || "";
                const colorVal = isSafeHex(val) ? val.slice(0, 7) : "#888888";
                return (<label key={key} className="theme-editor__color">
                      <span>{key}</span>
                      <input type="color" value={colorVal} disabled={busy} onChange={(e) => setToken(key, e.target.value)}/>
                      <input type="text" value={val} placeholder="#RRGGBB" disabled={busy} onChange={(e) => setToken(key, e.target.value.trim())}/>
                    </label>);
            })}
              </div>
            </div>))}
        </div>
      </div>

      <div className="theme-editor__row">
        <div className="theme-editor__label">{t("settings.themeLibrary.fieldBackground")}</div>
        <div className="theme-editor__fields">
          <div className="theme-library__toolbar">
            <button type="button" className="btn btn--small" disabled={busy} onClick={() => void pickBackground()}>
              {t("settings.themeLibrary.pickImage")}
            </button>
            <button type="button" className="btn btn--small" disabled={busy || (!state.background && !bgUrl)} onClick={() => onChange({ background: null, backgroundDataUrl: "", existingBackgroundUrl: "" })}>
              {t("settings.themeLibrary.clearImage")}
            </button>
          </div>
          {state.background && (<>
              <div ref={previewRef} className="theme-editor__bg-preview" style={bgUrl ? { backgroundImage: `url("${bgUrl}")` } : undefined} onPointerDown={(e) => {
                dragging.current = true;
                (e.target as HTMLElement).setPointerCapture?.(e.pointerId);
                onFocusPointer(e.clientX, e.clientY);
            }} onPointerMove={(e) => {
                if (!dragging.current)
                    return;
                onFocusPointer(e.clientX, e.clientY);
            }} onPointerUp={() => {
                dragging.current = false;
            }}>
                <span className="theme-editor__focus" style={{ left: `${(state.background.focusX ?? 0.5) * 100}%`, top: `${(state.background.focusY ?? 0.5) * 100}%` }}/>
              </div>
              <div className="set-seg">
                {(["left", "center", "right"] as const).map((s) => (<button key={s} type="button" className={`set-seg__btn${state.background?.safeArea === s ? " set-seg__btn--on" : ""}`} onClick={() => onChange({ background: { ...state.background!, safeArea: s } })}>
                    {s}
                  </button>))}
              </div>
              <label className="theme-editor__color">
                {t("settings.themeLibrary.homeOpacity")}
                <input type="range" min={0} max={1} step={0.01} value={state.background.homeOpacity} onChange={(e) => onChange({ background: { ...state.background!, homeOpacity: Number(e.target.value) } })}/>
              </label>
              <label className="theme-editor__color">
                {t("settings.themeLibrary.taskOpacity")}
                <input type="range" min={0} max={0.45} step={0.01} value={state.background.taskOpacity} onChange={(e) => onChange({ background: { ...state.background!, taskOpacity: Number(e.target.value) } })}/>
              </label>
              <label className="theme-editor__color">
                {t("settings.themeLibrary.overlayStrength")}
                <input type="range" min={0} max={1} step={0.01} value={state.background.overlayStrength} onChange={(e) => onChange({ background: { ...state.background!, overlayStrength: Number(e.target.value) } })}/>
              </label>
            </>)}
        </div>
      </div>

      {warnings.length > 0 && (<div className="theme-editor__warn">
          {t("settings.themeLibrary.contrastWarn")}
          <ul style={{ margin: "6px 0 0", paddingLeft: 18 }}>
            {warnings.map((w) => (<li key={w}>{w}</li>))}
          </ul>
        </div>)}

      <div className="theme-editor__actions">
        <button type="button" className="btn btn--small" disabled={busy} onClick={onCancel}>
          {t("settings.themeLibrary.cancel")}
        </button>
        <button type="button" className="btn btn--small" disabled={busy} onClick={() => onSave(false)}>
          {t("settings.themeLibrary.save")}
        </button>
        <button type="button" className="btn btn--small btn--primary" disabled={busy} onClick={() => onSave(true)}>
          {t("settings.themeLibrary.saveEnable")}
        </button>
      </div>
    </div>);
}
function contrastRatio(a: string, b: string): number {
    const la = relativeLuminance(a);
    const lb = relativeLuminance(b);
    const lighter = Math.max(la, lb);
    const darker = Math.min(la, lb);
    return (lighter + 0.05) / (darker + 0.05);
}
function relativeLuminance(hex: string): number {
    const n = hex.replace("#", "");
    const r = parseInt(n.slice(0, 2), 16) / 255;
    const g = parseInt(n.slice(2, 4), 16) / 255;
    const b = parseInt(n.slice(4, 6), 16) / 255;
    const lin = (c: number) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
    return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

