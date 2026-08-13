import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Check, Copy, Download, Pencil, Plus, RotateCcw, Trash2, Upload } from "lucide-react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { isThemeStyle } from "../lib/theme";
import { type ThemePackView, type ThemeSaveInput, applyThemePack, beginThemePreview, cancelThemePreview, clearThemePack, commitThemePreview, draftPackView, themePackKind } from "../lib/themePack";
import { useToast } from "../lib/toast";
import { useConfirmDialog } from "./ConfirmDialog";
import { EditorState, slugifyId, packToEditor, emptyEditor } from "./theme_library_helpers";
import { ThemeEditor } from "./theme_library_editor";
export function ThemeLibrarySection() {
    const t = useT();
    const { showToast } = useToast();
    const { confirm, dialog: confirmDialog } = useConfirmDialog();
    const [packs, setPacks] = useState<ThemePackView[]>([]);
    const [loading, setLoading] = useState(true);
    const [editor, setEditor] = useState<EditorState | null>(null);
    const [busy, setBusy] = useState(false);
    const previewTimer = useRef<number | null>(null);
    const reload = useCallback(async () => {
        setLoading(true);
        try {
            const list = await app.ListThemePacks();
            setPacks(list || []);
            const active = await app.GetActiveThemePack();
            if (active?.pack) {
                commitThemePreview(active.pack);
            }
            else {
                const still = (list || []).find((p) => p.active);
                if (!still)
                    clearThemePack();
            }
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setLoading(false);
        }
    }, []);
    useEffect(() => {
        void reload();
        return () => {
            if (previewTimer.current)
                window.clearTimeout(previewTimer.current);
            // Closing settings / leaving the appearance tab must not leave a draft preview applied.
            cancelThemePreview();
        };
    }, [reload]);
    const schedulePreview = useCallback((state: EditorState) => {
        if (previewTimer.current)
            window.clearTimeout(previewTimer.current);
        previewTimer.current = window.setTimeout(() => {
            const bgUrl = state.backgroundDataUrl || state.existingBackgroundUrl || "";
            const draft = draftPackView({
                id: state.id || "preview",
                name: state.name,
                baseStyle: state.baseStyle,
                tokens: state.tokens,
                recipes: state.recipes,
                background: state.background,
                backgroundUrl: bgUrl,
            });
            beginThemePreview(draft);
        }, 80);
    }, []);
    const openCreate = () => {
        const state = emptyEditor();
        setEditor(state);
        schedulePreview(state);
    };
    const openEdit = (pack: ThemePackView) => {
        if (themePackKind(pack) !== "user") {
            // Editing a base style means copying into a user theme.
            const state = packToEditor(pack, "create");
            setEditor(state);
            schedulePreview(state);
            return;
        }
        const state = packToEditor(pack, "edit");
        setEditor(state);
        schedulePreview(state);
    };
    const openCopy = async (pack: ThemePackView) => {
        setBusy(true);
        try {
            const newId = slugifyId(`${pack.id}-copy`);
            const created = await app.CopyThemePack(pack.id, newId, `${pack.name} Copy`);
            showToast(t("settings.themeLibrary.copied", { name: created.name }), "info");
            await reload();
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const activate = async (pack: ThemePackView) => {
        setBusy(true);
        try {
            await app.ActivateThemePack(pack.id);
            const active = await app.GetActiveThemePack();
            commitThemePreview(active.pack ?? null);
            // Sync base style via appearance when activating a pack.
            if (active.pack && isThemeStyle(active.pack.baseStyle)) {
                // Appearance style stays independent in config; pack overlay supplies baseStyle live.
            }
            await reload();
            showToast(t("settings.themeLibrary.activated", { name: pack.name }), "info");
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const resetDefault = async () => {
        setBusy(true);
        try {
            await app.ResetThemePack();
            cancelThemePreview();
            applyThemePack(null);
            await reload();
            showToast(t("settings.themeLibrary.resetDone"), "info");
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const remove = async (pack: ThemePackView) => {
        if (themePackKind(pack) !== "user")
            return;
        const ok = await confirm({
            title: t("settings.themeLibrary.confirmDeleteTitle"),
            message: t("settings.themeLibrary.confirmDelete", { name: packDisplayName(pack, t) }),
            confirmLabel: t("common.delete"),
            cancelLabel: t("common.cancel"),
            tone: "danger",
        });
        if (!ok)
            return;
        setBusy(true);
        try {
            await app.DeleteThemePack(pack.id);
            await reload();
            const active = await app.GetActiveThemePack();
            commitThemePreview(active.pack ?? null);
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const doImport = async (replace = false) => {
        setBusy(true);
        try {
            // First call may open a file dialog. On ID conflict the backend stages the
            // extract and returns needsReplace — confirm then call again with replace=true
            // (empty path) so the staged import is published without re-picking a file.
            const result = await app.ImportThemePack("", replace);
            if (!result)
                return;
            if (result.needsReplace) {
                const ok = await confirm({
                    title: t("settings.themeLibrary.confirmReplaceImportTitle"),
                    message: t("settings.themeLibrary.confirmReplaceImport"),
                    confirmLabel: t("settings.themeLibrary.replaceConfirm"),
                    cancelLabel: t("common.cancel"),
                });
                if (!ok)
                    return;
                const confirmed = await app.ImportThemePack("", true);
                if (!confirmed?.pack?.id)
                    return;
                showToast(t("settings.themeLibrary.imported", { name: confirmed.pack.name }), "info");
                await reload();
                return;
            }
            if (!result.pack?.id) {
                // Cancelled
                return;
            }
            showToast(t("settings.themeLibrary.imported", { name: result.pack.name }), "info");
            await reload();
        }
        catch (err) {
            const msg = err instanceof Error ? err.message : String(err);
            showToast(msg, "error");
        }
        finally {
            setBusy(false);
        }
    };
    const doExport = async (pack: ThemePackView) => {
        if (pack.hasBackground) {
            const ok = await confirm({
                title: t("settings.themeLibrary.exportRightsTitle"),
                message: t("settings.themeLibrary.exportRights"),
                confirmLabel: t("settings.themeLibrary.exportConfirm"),
                cancelLabel: t("common.cancel"),
            });
            if (!ok)
                return;
        }
        setBusy(true);
        try {
            const path = await app.ExportThemePack(pack.id, "");
            if (path)
                showToast(t("settings.themeLibrary.exported"), "info");
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const cancelEditor = () => {
        cancelThemePreview();
        setEditor(null);
    };
    const saveEditor = async (activateAfter: boolean) => {
        if (!editor)
            return;
        setBusy(true);
        try {
            const input: ThemeSaveInput = {
                id: editor.id.trim(),
                name: editor.name.trim(),
                author: editor.author,
                description: editor.description,
                license: editor.license,
                baseStyle: editor.baseStyle,
                tokens: editor.tokens,
                recipes: editor.recipes,
                background: editor.background,
                backgroundDataUrl: editor.backgroundDataUrl || undefined,
                clearBackground: editor.background === null && editor.mode === "edit",
                replace: editor.mode === "edit",
                activate: activateAfter,
            };
            const saved = await app.SaveThemePack(input);
            commitThemePreview(activateAfter ? saved : (await app.GetActiveThemePack()).pack ?? null);
            setEditor(null);
            await reload();
            showToast(t("settings.themeLibrary.saved", { name: saved.name }), "info");
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const updateEditor = (patch: Partial<EditorState>) => {
        setEditor((prev) => {
            if (!prev)
                return prev;
            const next = { ...prev, ...patch };
            schedulePreview(next);
            return next;
        });
    };
    const activeId = useMemo(() => packs.find((p) => p.active)?.id ?? "", [packs]);
    const groups = useMemo(() => {
        const official: ThemePackView[] = [];
        const base: ThemePackView[] = [];
        const user: ThemePackView[] = [];
        const plugin: ThemePackView[] = [];
        for (const p of packs) {
            const kind = themePackKind(p);
            if (kind === "official")
                official.push(p);
            else if (kind === "base")
                base.push(p);
            else if (kind === "plugin")
                plugin.push(p);
            else
                user.push(p);
        }
        return { official, base, user, plugin };
    }, [packs]);
    return (<div className="theme-library">
      <div className="theme-library__toolbar">
        <button type="button" className="btn btn--small" disabled={busy} onClick={openCreate}>
          <Plus size={13}/> {t("settings.themeLibrary.new")}
        </button>
        <button type="button" className="btn btn--small" disabled={busy} onClick={() => void doImport(false)}>
          <Upload size={13}/> {t("settings.themeLibrary.import")}
        </button>
        <button type="button" className="btn btn--small theme-reset-btn" disabled={busy} onClick={() => void resetDefault()}>
          <RotateCcw size={13}/> {t("settings.themeLibrary.reset")}
        </button>
      </div>

      {loading ? (<div className="theme-lib-card__sub">{t("settings.themeLibrary.loading")}</div>) : (<>
          {groups.official.length > 0 && (<section className="theme-library__group" data-group="official">
              <h4 className="theme-library__heading">{t("settings.themeLibrary.groupOfficial")}</h4>
              <div className="theme-library__grid theme-library__grid--official">
                {groups.official.map((pack) => (<OfficialThemeCard key={pack.id} pack={pack} active={pack.id === activeId} busy={busy} onActivate={() => void activate(pack)} onCopy={() => void openCopy(pack)}/>))}
              </div>
            </section>)}

          {groups.base.length > 0 && (<section className="theme-library__group" data-group="base">
              <h4 className="theme-library__heading">{t("settings.themeLibrary.groupBase")}</h4>
              <div className="theme-library__grid theme-library__grid--base">
                {groups.base.map((pack) => (<ThemeLibCard key={pack.id} pack={pack} active={pack.id === activeId} busy={busy} onActivate={() => void activate(pack)} onEdit={() => openEdit(pack)} onCopy={() => void openCopy(pack)} onExport={() => void doExport(pack)} onDelete={() => void remove(pack)}/>))}
              </div>
            </section>)}

          <section className="theme-library__group" data-group="user">
            <h4 className="theme-library__heading">{t("settings.themeLibrary.groupUser")}</h4>
            {groups.user.length === 0 ? (<div className="theme-lib-card__sub">{t("settings.themeLibrary.emptyUser")}</div>) : (<div className="theme-library__grid">
                {groups.user.map((pack) => (<ThemeLibCard key={pack.id} pack={pack} active={pack.id === activeId} busy={busy} onActivate={() => void activate(pack)} onEdit={() => openEdit(pack)} onCopy={() => void openCopy(pack)} onExport={() => void doExport(pack)} onDelete={() => void remove(pack)}/>))}
              </div>)}
          </section>

          {groups.plugin.length > 0 && (<section className="theme-library__group" data-group="plugin">
              <h4 className="theme-library__heading">{t("settings.themeLibrary.groupPlugin")}</h4>
              <div className="theme-library__grid">
                {groups.plugin.map((pack) => (<ThemeLibCard key={pack.id} pack={pack} active={pack.id === activeId} busy={busy} onActivate={() => void activate(pack)} onEdit={() => openEdit(pack)} onCopy={() => void openCopy(pack)} onExport={() => void doExport(pack)} onDelete={() => void remove(pack)}/>))}
              </div>
            </section>)}
        </>)}

      {editor && (<ThemeEditor state={editor} busy={busy} onChange={updateEditor} onCancel={cancelEditor} onSave={(activateAfter) => void saveEditor(activateAfter)}/>)}
      {confirmDialog}
    </div>);
}
function packDisplayName(pack: ThemePackView, t: (key: never, vars?: Record<string, string | number>) => string): string {
    return pack.nameKey ? t(pack.nameKey as never) : pack.name;
}
function packDescription(pack: ThemePackView, t: (key: never, vars?: Record<string, string | number>) => string): string {
    if (pack.descriptionKey)
        return t(pack.descriptionKey as never);
    return pack.description || "";
}
function OfficialThemeCard({ pack, active, busy, onActivate, onCopy, }: {
    pack: ThemePackView;
    active: boolean;
    busy: boolean;
    onActivate: () => void;
    onCopy: () => void;
}) {
    const t = useT();
    const name = packDisplayName(pack, t);
    const desc = packDescription(pack, t);
    const lightBg = pack.tokens?.light?.bg || "#f4f3ef";
    const darkBg = pack.tokens?.dark?.bg || "#0c0d10";
    const accent = pack.tokens?.dark?.accent || pack.tokens?.light?.accent || "#ff6a3d";
    return (<div className={`theme-lib-card theme-lib-card--official${active ? " theme-lib-card--on" : ""}`}>
      <div className="theme-lib-card__thumb theme-lib-card__thumb--img">
        {pack.previewUrl ? (<img src={pack.previewUrl} alt={name} loading="lazy" decoding="async"/>) : (<div className="theme-lib-card__thumb-fallback" style={{ background: `linear-gradient(120deg, ${lightBg} 0%, ${lightBg} 55%, ${accent} 140%)` }}/>)}
      </div>
      <div className="theme-lib-card__meta">
        <div className="theme-lib-card__name">
          {name} {active ? <Check size={12} style={{ display: "inline", verticalAlign: "middle" }}/> : null}
        </div>
        {desc ? <div className="theme-lib-card__desc">{desc}</div> : null}
        <div className="theme-lib-card__sub">
          {pack.license || "MIT"} · {pack.author || "Reasonix Contributors"}
        </div>
      </div>
      <div className="theme-lib-card__swatches" aria-hidden="true">
        <span className="theme-lib-card__swatch" style={{ background: lightBg }}/>
        <span className="theme-lib-card__swatch" style={{ background: darkBg }}/>
        <span className="theme-lib-card__swatch" style={{ background: accent }}/>
      </div>
      <div className="theme-lib-card__actions">
        <button type="button" className="btn btn--small btn--primary" disabled={busy || active} onClick={onActivate}>
          {active ? t("settings.themeLibrary.active") : t("settings.themeLibrary.enable")}
        </button>
        <button type="button" className="btn btn--small" disabled={busy} onClick={onCopy}>
          <Copy size={12}/> {t("settings.themeLibrary.copyFrom")}
        </button>
      </div>
    </div>);
}
function ThemeLibCard({ pack, active, busy, onActivate, onEdit, onCopy, onExport, onDelete, }: {
    pack: ThemePackView;
    active: boolean;
    busy: boolean;
    onActivate: () => void;
    onEdit: () => void;
    onCopy: () => void;
    onExport: () => void;
    onDelete: () => void;
}) {
    const t = useT();
    const kind = themePackKind(pack);
    const lightBg = pack.tokens?.light?.bg || "#f4f3ef";
    const darkBg = pack.tokens?.dark?.bg || "#0c0d10";
    const accent = pack.tokens?.dark?.accent || pack.tokens?.light?.accent || "#ff6a3d";
    const thumbStyle: Record<string, string> = pack.backgroundUrl
        ? { backgroundImage: `url("${pack.backgroundUrl}")`, backgroundSize: "cover" }
        : { ["--thumb-light"]: lightBg, ["--thumb-dark"]: darkBg };
    return (<div className={`theme-lib-card${active ? " theme-lib-card--on" : ""}`}>
      <div className="theme-lib-card__thumb" style={thumbStyle}/>
      <div className="theme-lib-card__meta">
        <div className="theme-lib-card__name">
          {pack.name} {active ? <Check size={12} style={{ display: "inline", verticalAlign: "middle" }}/> : null}
        </div>
        <div className="theme-lib-card__sub">
          {kind === "base"
            ? t("settings.themeLibrary.builtin")
            : kind === "plugin"
                ? pack.pluginName
                    ? t("settings.themeGallery.kindPlugin", { name: pack.pluginName })
                    : t("settings.themeGallery.kindPluginUnknown")
                : pack.author || t("settings.themeLibrary.userTheme")}
          {" · "}
          {pack.baseStyle}
        </div>
      </div>
      <div className="theme-lib-card__swatches" aria-hidden="true">
        <span className="theme-lib-card__swatch" style={{ background: lightBg }}/>
        <span className="theme-lib-card__swatch" style={{ background: darkBg }}/>
        <span className="theme-lib-card__swatch" style={{ background: accent }}/>
      </div>
      <div className="theme-lib-card__actions">
        <button type="button" className="btn btn--small btn--primary" disabled={busy || active} onClick={onActivate}>
          {active ? t("settings.themeLibrary.active") : t("settings.themeLibrary.enable")}
        </button>
        {kind === "user" && (<button type="button" className="btn btn--small" disabled={busy} onClick={onEdit} title={t("settings.themeLibrary.edit")}>
            <Pencil size={12}/>
          </button>)}
        {kind !== "plugin" && (<button type="button" className="btn btn--small" disabled={busy} onClick={onCopy} title={t("settings.themeLibrary.copy")}>
            <Copy size={12}/>
          </button>)}
        {kind === "user" && (<>
            <button type="button" className="btn btn--small" disabled={busy} onClick={onExport} title={t("settings.themeLibrary.export")}>
              <Download size={12}/>
            </button>
            <button type="button" className="btn btn--small" disabled={busy} onClick={onDelete} title={t("settings.themeLibrary.delete")}>
              <Trash2 size={12}/>
            </button>
          </>)}
      </div>
    </div>);
}
export type { EditorState as EditorState } from "./theme_library_helpers";
export { TOKEN_GROUPS as TOKEN_GROUPS } from "./theme_library_helpers";
export { slugifyId as slugifyId } from "./theme_library_helpers";
export { packToEditor as packToEditor } from "./theme_library_helpers";
export { emptyEditor as emptyEditor } from "./theme_library_helpers";
export { ThemeEditor as ThemeEditor } from "./theme_library_editor";

