import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ArrowLeft, Check, Copy, Download, MoreHorizontal, Pencil, Plus, Trash2, Upload } from "lucide-react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { type ThemeStyle, isThemeStyle } from "../lib/theme";
import { type ThemePackView, type ThemeSaveInput, cancelThemePreview, draftPackView, emptyThemeTokens, themePackKind } from "../lib/themePack";
import { type GalleryTab, type ThemeExperienceView, type ThemeSelection, activateBaseStyle, activateThemePack, cancelGlobalPreview, commitGlobalPreview, groupThemePacks, isSelectionActive, selectionFromPack, startGlobalPreview } from "../lib/themeExperience";
import { useToast } from "../lib/toast";
import { themePreviewPalette } from "../lib/themePreviewPalette";
import { useConfirmDialog } from "./ConfirmDialog";
import { ThemePreviewSurface } from "./ThemePreviewSurface";
import { EditorState, slugifyId, packDisplayName, packDescription, packKindBadge, createEditorState, ThemePreviewControls } from "./theme_gallery_helpers";
import { ThemeEditorInline } from "./theme_editor_inline";
export function ThemeGallery({ experience, initialCreateBaseStyle, onExperienceChange, onBack, }: {
    experience: ThemeExperienceView;
    initialCreateBaseStyle?: ThemeStyle;
    onExperienceChange: (view: ThemeExperienceView) => void;
    onBack: () => void;
}) {
    const t = useT();
    const { showToast } = useToast();
    const { confirm, dialog: confirmDialog } = useConfirmDialog();
    const [packs, setPacks] = useState<ThemePackView[]>([]);
    const [loading, setLoading] = useState(true);
    const [busy, setBusy] = useState(false);
    const [tab, setTab] = useState<GalleryTab>("catalog");
    const [selected, setSelected] = useState<ThemeSelection | null>(null);
    const [detailMode, setDetailMode] = useState<"light" | "dark">("dark");
    const [detailScene, setDetailScene] = useState<"home" | "task">("home");
    const [immersive, setImmersive] = useState(false);
    const [previewingId, setPreviewingId] = useState<string | null>(null);
    const [editor, setEditor] = useState<EditorState | null>(() => initialCreateBaseStyle ? createEditorState(initialCreateBaseStyle) : null);
    const [menuOpen, setMenuOpen] = useState(false);
    const selectionSeeded = useRef(false);
    const moreActionsRef = useRef<HTMLButtonElement>(null);
    const reload = useCallback(async () => {
        setLoading(true);
        try {
            const list = await app.ListThemePacks();
            setPacks(list || []);
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setLoading(false);
        }
    }, [showToast]);
    useEffect(() => {
        void reload();
        return () => {
            cancelGlobalPreview();
            cancelThemePreview();
        };
    }, [reload]);
    const groups = useMemo(() => groupThemePacks(packs), [packs]);
    const catalogPacks = useMemo(() => [...groups.official, ...groups.plugin, ...groups.base], [groups.official, groups.plugin, groups.base]);
    const visible = tab === "catalog" ? catalogPacks : groups.user;
    const visibleSections = tab === "catalog"
        ? [
            { id: "official", label: t("settings.themeGallery.sectionFlagship"), packs: groups.official },
            { id: "plugin", label: t("settings.themeLibrary.groupPlugin"), packs: groups.plugin },
            { id: "base", label: t("settings.themeGallery.tabBase"), packs: groups.base },
        ].filter((section) => section.packs.length > 0)
        : [{ id: "user", label: "", packs: groups.user }];
    const changeTab = (nextTab: GalleryTab) => {
        const nextPacks = nextTab === "catalog" ? catalogPacks : groups.user;
        if (nextTab !== tab) {
            cancelGlobalPreview();
            setPreviewingId(null);
        }
        setTab(nextTab);
        setMenuOpen(false);
        if (selected && nextPacks.some((pack) => pack.id === selected.id))
            return;
        const nextSelection = nextPacks.find((pack) => isSelectionActive(selectionFromPack(pack), experience)) || nextPacks[0] || null;
        setSelected(nextSelection ? selectionFromPack(nextSelection) : null);
    };
    // Seed selection from active experience.
    useEffect(() => {
        if (selectionSeeded.current || packs.length === 0)
            return;
        selectionSeeded.current = true;
        if (experience.activePack) {
            setSelected(selectionFromPack(experience.activePack));
            setTab(themePackKind(experience.activePack) === "user" ? "user" : "catalog");
            return;
        }
        const base = groups.base.find((p) => p.id === experience.baseStyle) || groups.base[0] || groups.official[0];
        if (base) {
            setSelected(selectionFromPack(base));
            setTab("catalog");
        }
    }, [packs, experience, selected, groups.base, groups.official]);
    const selectedPack = selected?.pack || (selected?.kind === "base" ? groups.base.find((p) => p.id === selected.id) : null) || null;
    const isActive = isSelectionActive(selected, experience);
    const previewPackGlobally = useCallback((pack: ThemePackView) => {
        if (themePackKind(pack) === "base") {
            const draft = draftPackView({
                id: pack.id,
                name: pack.name,
                baseStyle: pack.id,
                tokens: emptyThemeTokens(),
                recipes: { density: "comfortable", corners: "soft" },
            });
            startGlobalPreview(draft);
            return;
        }
        startGlobalPreview(pack);
    }, []);
    // Entering immersive mode is the explicit opt-in to a live, reversible
    // preview. Every rail selection replaces that preview until Back restores it
    // or Apply persists it.
    useEffect(() => {
        if (!immersive || !selectedPack)
            return;
        previewPackGlobally(selectedPack);
    }, [immersive, selectedPack, previewPackGlobally]);
    const onSelectPack = (pack: ThemePackView) => {
        setSelected(selectionFromPack(pack));
        setMenuOpen(false);
        if (!immersive) {
            previewPackGlobally(pack);
            setPreviewingId(pack.id);
        }
    };
    const applySelected = async () => {
        if (!selected)
            return;
        setBusy(true);
        try {
            if (selected.kind === "base") {
                const view = await activateBaseStyle(selected.id);
                onExperienceChange(view);
                showToast(t("settings.themeGallery.appliedBase", { name: selected.id }), "info");
            }
            else {
                const view = await activateThemePack(selected.id);
                onExperienceChange(view);
                commitGlobalPreview(view.activePack ?? null);
                showToast(t("settings.themeGallery.applied", { name: packDisplayName(selected.pack, t) }), "info");
            }
            setPreviewingId(null);
            await reload();
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const closeImmersivePreview = () => {
        cancelGlobalPreview();
        setPreviewingId(null);
        setImmersive(false);
    };
    const copySelected = async () => {
        if (!selectedPack)
            return;
        setBusy(true);
        try {
            const newId = slugifyId(`${selectedPack.id}-copy`);
            const created = await app.CopyThemePack(selectedPack.id, newId, `${packDisplayName(selectedPack, t)} Copy`);
            showToast(t("settings.themeLibrary.copied", { name: created.name }), "info");
            await reload();
            cancelGlobalPreview();
            setPreviewingId(null);
            setTab("user");
            setSelected(selectionFromPack(created));
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const removeSelected = async () => {
        if (!selectedPack || themePackKind(selectedPack) !== "user")
            return;
        const ok = await confirm({
            title: t("settings.themeLibrary.confirmDeleteTitle"),
            message: t("settings.themeLibrary.confirmDelete", { name: packDisplayName(selectedPack, t) }),
            confirmLabel: t("common.delete"),
            cancelLabel: t("common.cancel"),
            tone: "danger",
        });
        setMenuOpen(false);
        if (!ok) {
            requestAnimationFrame(() => moreActionsRef.current?.focus());
            return;
        }
        setBusy(true);
        try {
            await app.DeleteThemePack(selectedPack.id);
            showToast(t("settings.themeGallery.deleted", { name: packDisplayName(selectedPack, t) }), "info");
            setSelected(null);
            await reload();
            // Experience may have changed if we deleted the active pack.
            const { loadThemeExperience, applyExperienceToDOM } = await import("../lib/themeExperience");
            const view = await loadThemeExperience();
            applyExperienceToDOM(view);
            onExperienceChange(view);
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
            setMenuOpen(false);
        }
    };
    const exportSelected = async () => {
        if (!selectedPack || themePackKind(selectedPack) !== "user")
            return;
        try {
            const ok = await confirm({
                title: t("settings.themeLibrary.exportRightsTitle"),
                message: t("settings.themeLibrary.exportRights"),
                confirmLabel: t("settings.themeLibrary.exportConfirm"),
                cancelLabel: t("common.cancel"),
            });
            if (!ok)
                return;
            const path = await app.ExportThemePack(selectedPack.id, "");
            if (path)
                showToast(t("settings.themeLibrary.exported"), "info");
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        setMenuOpen(false);
    };
    const openCreate = () => {
        cancelGlobalPreview();
        setPreviewingId(null);
        setEditor(createEditorState(isThemeStyle(experience.baseStyle) ? experience.baseStyle : "graphite"));
    };
    const openEdit = () => {
        if (!selectedPack || themePackKind(selectedPack) !== "user")
            return;
        cancelGlobalPreview();
        setPreviewingId(null);
        setEditor({
            mode: "edit",
            id: selectedPack.id,
            name: selectedPack.name,
            author: selectedPack.author || "",
            description: selectedPack.description || "",
            license: selectedPack.license || "",
            baseStyle: isThemeStyle(selectedPack.baseStyle) ? selectedPack.baseStyle : "graphite",
            tokens: { light: { ...(selectedPack.tokens?.light || {}) }, dark: { ...(selectedPack.tokens?.dark || {}) } },
            recipes: {
                density: selectedPack.recipes?.density === "compact" ? "compact" : "comfortable",
                corners: selectedPack.recipes?.corners === "square" || selectedPack.recipes?.corners === "round"
                    ? selectedPack.recipes.corners
                    : "soft",
            },
            background: selectedPack.background ? { ...selectedPack.background } : null,
            backgroundDataUrl: "",
            existingBackgroundUrl: selectedPack.backgroundUrl || "",
            taskBackground: selectedPack.taskBackground ? { ...selectedPack.taskBackground } : null,
            taskBackgroundDataUrl: "",
            existingTaskBackgroundUrl: selectedPack.taskBackgroundUrl || "",
            tokenMode: "dark",
            originalId: selectedPack.id,
        });
        setMenuOpen(false);
    };
    const doImport = async () => {
        setBusy(true);
        try {
            const result = await app.ImportThemePack("", false);
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
                if (confirmed?.pack) {
                    showToast(t("settings.themeLibrary.imported", { name: confirmed.pack.name }), "info");
                }
            }
            else if (result.pack) {
                showToast(t("settings.themeLibrary.imported", { name: result.pack.name }), "info");
            }
            await reload();
            cancelGlobalPreview();
            setPreviewingId(null);
            setTab("user");
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    const saveEditor = async (activate: boolean) => {
        if (!editor)
            return;
        setBusy(true);
        try {
            const input: ThemeSaveInput = {
                id: editor.id,
                name: editor.name,
                author: editor.author,
                description: editor.description,
                license: editor.license,
                baseStyle: editor.baseStyle,
                tokens: editor.tokens,
                recipes: editor.recipes,
                background: editor.background || undefined,
                backgroundDataUrl: editor.backgroundDataUrl || undefined,
                clearBackground: !editor.background && !editor.backgroundDataUrl && !editor.existingBackgroundUrl,
                taskBackground: editor.taskBackground || undefined,
                taskBackgroundDataUrl: editor.taskBackgroundDataUrl || undefined,
                clearTaskBackground: !editor.taskBackground && !editor.taskBackgroundDataUrl && !editor.existingTaskBackgroundUrl,
                replace: editor.mode === "edit",
                // Keep save and activation separate. The activation path below is the
                // sole owner of the active-theme pointer, so a failed activation leaves
                // the previous theme selected and the preview snapshot reversible.
                activate: false,
            };
            const saved = await app.SaveThemePack(input);
            showToast(t("settings.themeLibrary.saved", { name: saved.name }), "info");
            if (activate) {
                // Commit activation before unmounting the editor. ThemeEditorInline's
                // cleanup cancels any remaining preview, so closing it earlier would
                // briefly restore the old snapshot while reload() is in flight.
                const view = await activateThemePack(saved.id);
                onExperienceChange(view);
            }
            else {
                cancelThemePreview();
            }
            setEditor(null);
            await reload();
            setTab("user");
            setSelected(selectionFromPack(saved));
        }
        catch (err) {
            showToast(err instanceof Error ? err.message : String(err), "error");
        }
        finally {
            setBusy(false);
        }
    };
    if (immersive && selectedPack) {
        return (<div className="theme-gallery theme-gallery--immersive">
        <header className="theme-gallery__top">
          <button type="button" className="btn btn--small" onClick={closeImmersivePreview}>
            <ArrowLeft size={14}/> {t("settings.themeGallery.back")}
          </button>
          <h2 className="theme-gallery__title">{t("settings.themeGallery.previewTitle")}</h2>
          <div className="theme-gallery__top-actions">
            <button type="button" className="btn btn--small" onClick={() => void doImport()} disabled={busy}>
              <Upload size={13}/> {t("settings.themeLibrary.import")}
            </button>
            <button type="button" className="btn btn--small" onClick={openCreate} disabled={busy}>
              <Plus size={13}/> {t("settings.themeLibrary.new")}
            </button>
          </div>
        </header>
        <div className="theme-gallery__immersive-toolbar">
          <ThemePreviewControls mode={detailMode} scene={detailScene} onModeChange={setDetailMode} onSceneChange={setDetailScene}/>
        </div>
        <div className="theme-gallery__immersive-body">
          <ThemePreviewSurface pack={selectedPack} mode={detailMode} scene={detailScene}/>
          <aside className="theme-gallery__immersive-rail">
            <div className="theme-gallery__detail-head">
              <div className="theme-gallery__detail-title-row">
                <h3>{packDisplayName(selectedPack, t)}</h3>
                {isActive ? (<span className="theme-gallery__detail-status">
                    <Check size={12} strokeWidth={3}/> {t("settings.themeGallery.current")}
                  </span>) : null}
              </div>
              <span className="theme-gallery__badge">
                {packKindBadge(selectedPack, t)}
              </span>
            </div>
            <p className="theme-gallery__detail-desc">{packDescription(selectedPack, t)}</p>
            {!isActive ? (<button type="button" className="btn btn--primary" disabled={busy} onClick={() => void applySelected()}>
                {t("settings.themeGallery.apply")}
              </button>) : null}
            <div className="theme-gallery__rail-list" role="listbox" aria-label={t("settings.themeGallery.title")}>
              {[
                { id: "official", label: t("settings.themeLibrary.groupOfficial"), packs: groups.official },
                { id: "user", label: t("settings.themeLibrary.groupUser"), packs: groups.user },
                { id: "plugin", label: t("settings.themeLibrary.groupPlugin"), packs: groups.plugin },
                { id: "base", label: t("settings.themeGallery.tabBase"), packs: groups.base },
            ]
                .filter((section) => section.packs.length > 0)
                .map((section) => (<div key={section.id} className="theme-gallery__rail-section" role="group" aria-label={section.label}>
                    <div className="theme-gallery__rail-section-head" aria-hidden="true">
                      <span>{section.label}</span>
                      <span className="theme-gallery__rail-section-count">{section.packs.length}</span>
                    </div>
                    <div className="theme-gallery__rail-section-items">
                      {section.packs.map((p) => (<button key={p.id} type="button" role="option" aria-selected={selected?.id === p.id} className={`theme-gallery__rail-card${selected?.id === p.id ? " theme-gallery__rail-card--on" : ""}`} onClick={() => onSelectPack(p)}>
                          {themePackKind(p) === "base" ? (<ThemePreviewSurface pack={p} mode={detailMode} scene={detailScene} variant="thumbnail"/>) : p.previewUrl || p.backgroundUrl ? (<img src={p.previewUrl || p.backgroundUrl} alt="" loading="lazy"/>) : (<span className="theme-gallery__rail-fallback"/>)}
                          <span>{packDisplayName(p, t)}</span>
                        </button>))}
                    </div>
                  </div>))}
            </div>
          </aside>
        </div>
        {confirmDialog}
      </div>);
    }
    return (<div className="theme-gallery">
      <header className="theme-gallery__top">
        <div className="theme-gallery__crumbs">
          <button type="button" className="theme-gallery__back" onClick={onBack}>
            <ArrowLeft size={14}/>
            <span>
              {t("settings.appearance")} / {t("settings.themeGallery.title")}
            </span>
          </button>
          <h2 className="theme-gallery__title">{t("settings.themeGallery.title")}</h2>
          <p className="theme-gallery__sub">{t("settings.themeGallery.subtitle")}</p>
        </div>
        <div className="theme-gallery__top-actions">
          <button type="button" className="btn btn--small" onClick={openCreate} disabled={busy}>
            <Plus size={13}/> {t("settings.themeLibrary.new")}
          </button>
          <button type="button" className="btn btn--small" onClick={() => void doImport()} disabled={busy}>
            <Upload size={13}/> {t("settings.themeLibrary.import")}
          </button>
        </div>
      </header>

      <div className="theme-gallery__tabs" role="tablist">
        {([
            ["catalog", t("settings.themeGallery.tabAll"), catalogPacks.length],
            ["user", t("settings.themeLibrary.groupUser"), groups.user.length],
        ] as const).map(([id, label, count]) => (<button key={id} type="button" role="tab" aria-selected={tab === id} className={`theme-gallery__tab${tab === id ? " theme-gallery__tab--on" : ""}`} onClick={() => changeTab(id)}>
            {label} <span className="theme-gallery__tab-count">{count}</span>
          </button>))}
      </div>

      <div className="theme-gallery__body">
        <div className="theme-gallery__grid" role="listbox" aria-label={t("settings.themeGallery.title")}>
          {loading ? (<div className="theme-lib-card__sub">{t("settings.themeLibrary.loading")}</div>) : visible.length === 0 ? (<div className="theme-lib-card__sub">{tab === "user" ? t("settings.themeLibrary.emptyUser") : t("settings.themeGallery.empty")}</div>) : (visibleSections.map((section) => (<div key={section.id} className="theme-gallery__grid-section" role="group" aria-label={section.label || undefined}>
                {section.label ? (<div className="theme-gallery__section-head">
                    <h3>{section.label}</h3>
                    <span>{section.packs.length}</span>
                  </div>) : null}
                {section.packs.map((pack) => {
                const name = packDisplayName(pack, t);
                const active = isSelectionActive(selectionFromPack(pack), experience);
                const sel = selected?.id === pack.id;
                return (<button key={pack.id} type="button" role="option" aria-selected={sel} className={`theme-gallery-card${sel ? " theme-gallery-card--selected" : ""}${active ? " theme-gallery-card--active" : ""}`} onClick={() => onSelectPack(pack)} onKeyDown={(e) => {
                        if (e.key === "Enter" || e.key === " ") {
                            e.preventDefault();
                            onSelectPack(pack);
                        }
                    }}>
                      <div className="theme-gallery-card__thumb">
                        {themePackKind(pack) === "base" ? (<ThemePreviewSurface pack={pack} mode={detailMode} scene={detailScene} variant="thumbnail"/>) : pack.previewUrl || pack.backgroundUrl ? (<img src={pack.previewUrl || pack.backgroundUrl} alt={name} loading="lazy" decoding="async"/>) : (<div className="theme-gallery-card__swatches" style={{ background: `linear-gradient(120deg, ${pack.tokens?.light?.bg || "#f4f3ef"}, ${pack.tokens?.dark?.accent || pack.tokens?.light?.accent || "#ff6a3d"})` }}/>)}
                        {active ? (<span className="theme-gallery-card__check" aria-hidden="true">
                            <Check size={14} strokeWidth={3}/>
                          </span>) : null}
                      </div>
                      <div className="theme-gallery-card__name">{name}</div>
                      {active ? <div className="theme-gallery-card__status">{t("settings.themeGallery.current")}</div> : null}
                    </button>);
            })}
              </div>)))}
        </div>

        <aside className="theme-gallery__detail" aria-live="polite">
          {selectedPack ? (<>
              <div className="theme-gallery__detail-preview">
                <ThemePreviewSurface pack={selectedPack} mode={detailMode} scene={detailScene}/>
              </div>
              <div className="theme-gallery__detail-meta">
                <div className="theme-gallery__detail-head">
                  <div className="theme-gallery__detail-title-row">
                    <h3 className="theme-gallery__detail-name">{packDisplayName(selectedPack, t)}</h3>
                    {isActive ? (<span className="theme-gallery__detail-status">
                        <Check size={12} strokeWidth={3}/> {t("settings.themeGallery.current")}
                      </span>) : previewingId === selectedPack.id ? (<span className="theme-gallery__detail-status theme-gallery__detail-status--preview">
                        {t("settings.themeGallery.previewing")}
                      </span>) : null}
                  </div>
                </div>
                <div className="theme-gallery__detail-tags">
                  <span className="theme-gallery__badge">
                    {packKindBadge(selectedPack, t)}
                  </span>
                  {selectedPack.license ? <span className="theme-gallery__badge theme-gallery__badge--muted">{selectedPack.license}</span> : null}
                </div>
                <p className="theme-gallery__detail-desc">{packDescription(selectedPack, t)}</p>
                <div className="theme-gallery__detail-palette">
                  <span className="theme-gallery__preview-label">{t("settings.themeGallery.paletteLabel")}</span>
                  <div className="theme-gallery__detail-swatches" aria-hidden="true">
                    <span style={{ background: themePreviewPalette(selectedPack, detailMode).bg }}/>
                    <span style={{ background: themePreviewPalette(selectedPack, detailMode).panel }}/>
                    <span style={{ background: themePreviewPalette(selectedPack, detailMode).accent }}/>
                  </div>
                </div>
                <ThemePreviewControls mode={detailMode} scene={detailScene} onModeChange={setDetailMode} onSceneChange={setDetailScene}/>
                {!isActive ? (<button type="button" className="btn btn--primary theme-gallery__apply" disabled={busy} onClick={() => void applySelected()}>
                    {t("settings.themeGallery.apply")}
                  </button>) : null}
                <div className="theme-gallery__detail-actions">
                  <button type="button" className="btn btn--small theme-gallery__open-preview" disabled={busy} onClick={() => setImmersive(true)}>
                    {t("settings.themeGallery.openPreview")}
                  </button>
                  {themePackKind(selectedPack) === "user" ? (<div className="theme-gallery__detail-user-actions">
                      <button type="button" className="btn btn--small" disabled={busy} onClick={openEdit}>
                        <Pencil size={12}/> {t("settings.themeLibrary.edit")}
                      </button>
                      <button type="button" className="btn btn--small" disabled={busy} onClick={() => void exportSelected()}>
                        <Download size={12}/> {t("settings.themeLibrary.export")}
                      </button>
                    </div>) : null}
                  {themePackKind(selectedPack) !== "plugin" ? (<button type="button" className="btn btn--small theme-gallery__detail-copy" disabled={busy} onClick={() => void copySelected()}>
                      <Copy size={12}/> {t("settings.themeLibrary.copyFrom")}
                    </button>) : null}
                  {themePackKind(selectedPack) === "user" ? (<div className="theme-gallery__more theme-gallery__detail-more">
                      <button ref={moreActionsRef} type="button" className="btn btn--small" onClick={() => setMenuOpen((v) => !v)} aria-expanded={menuOpen}>
                        <MoreHorizontal size={14}/> {t("settings.themeGallery.moreActions")}
                      </button>
                      {menuOpen ? (<div className="theme-gallery__menu" role="menu">
                          <button type="button" role="menuitem" className="theme-gallery__menu-danger" onClick={() => void removeSelected()}>
                            <Trash2 size={12}/> {t("settings.themeLibrary.delete")}
                          </button>
                        </div>) : null}
                    </div>) : null}
                </div>
              </div>
            </>) : (<div className="theme-lib-card__sub">{t("settings.themeGallery.selectHint")}</div>)}
        </aside>
      </div>

      {editor ? (<ThemeEditorInline state={editor} busy={busy} onChange={(patch) => setEditor((s) => (s ? { ...s, ...patch } : s))} onCancel={() => {
                cancelThemePreview();
                setEditor(null);
            }} onSave={(activate) => void saveEditor(activate)}/>) : null}
      {confirmDialog}
    </div>);
}
export type { EditorState as EditorState } from "./theme_gallery_helpers";
export { TOKEN_GROUPS as TOKEN_GROUPS } from "./theme_gallery_helpers";
export { slugifyId as slugifyId } from "./theme_gallery_helpers";
export { packDisplayName as packDisplayName } from "./theme_gallery_helpers";
export { packDescription as packDescription } from "./theme_gallery_helpers";
export { packKindBadge as packKindBadge } from "./theme_gallery_helpers";
export { createEditorState as createEditorState } from "./theme_gallery_helpers";
export { ThemePreviewControls as ThemePreviewControls } from "./theme_gallery_helpers";
export { themeContrastRatio as themeContrastRatio } from "./theme_gallery_helpers";
export { ThemeEditorInline as ThemeEditorInline } from "./theme_editor_inline";

