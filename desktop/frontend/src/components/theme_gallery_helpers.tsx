import { type KeyboardEvent as ReactKeyboardEvent } from "react";
import { CircleHelp } from "lucide-react";
import { useT } from "../lib/i18n";
import { type ThemeStyle } from "../lib/theme";
import { type ThemePackView, type ThemePackBackground, type ThemePackSceneBackground, type ThemePackRecipes, type ThemePackTokens, emptyThemeTokens, themePackKind } from "../lib/themePack";
import { Tooltip } from "./Tooltip";
export type EditorState = {
    mode: "create" | "edit";
    id: string;
    name: string;
    author: string;
    description: string;
    license: string;
    baseStyle: ThemeStyle;
    tokens: ThemePackTokens;
    recipes: ThemePackRecipes;
    background: ThemePackBackground | null;
    backgroundDataUrl: string;
    existingBackgroundUrl: string;
    taskBackground: ThemePackSceneBackground | null;
    taskBackgroundDataUrl: string;
    existingTaskBackgroundUrl: string;
    tokenMode: "light" | "dark";
    originalId: string;
};
export const TOKEN_GROUPS: {
    labelKey: string;
    keys: string[];
}[] = [
    { labelKey: "settings.themeTokens.surfaces", keys: ["bg", "bgSoft", "bgElev", "panel", "sidebar", "chat", "workspace", "workspaceFiles"] },
    { labelKey: "settings.themeTokens.borderText", keys: ["border", "borderSoft", "fg", "fgDim", "fgFaint"] },
    { labelKey: "settings.themeTokens.accentStatus", keys: ["accent", "accentFg", "ok", "warn", "err"] },
];
export function slugifyId(name: string): string {
    const s = name
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "")
        .slice(0, 48);
    if (!s)
        return "my-theme";
    if (/^[a-z][a-z0-9-]*[a-z0-9]$|^[a-z]$/.test(s))
        return s;
    return `t-${s}`.slice(0, 48);
}
export function packDisplayName(pack: ThemePackView, t: (key: never, vars?: Record<string, string | number>) => string): string {
    return pack.nameKey ? t(pack.nameKey as never) : pack.name;
}
export function packDescription(pack: ThemePackView, t: (key: never, vars?: Record<string, string | number>) => string): string {
    if (pack.descriptionKey)
        return t(pack.descriptionKey as never);
    return pack.description || "";
}
export function packKindBadge(pack: ThemePackView, t: ReturnType<typeof useT>): string {
    const kind = themePackKind(pack);
    if (kind === "official")
        return t("settings.themeGallery.kindOfficial");
    if (kind === "base")
        return t("settings.themeGallery.kindBase");
    if (kind === "plugin") {
        return pack.pluginName
            ? t("settings.themeGallery.kindPlugin", { name: pack.pluginName })
            : t("settings.themeGallery.kindPluginUnknown");
    }
    return t("settings.themeGallery.kindUser");
}
export function createEditorState(baseStyle: ThemeStyle): EditorState {
    return {
        mode: "create",
        id: "my-theme",
        name: "My Theme",
        author: "",
        description: "",
        license: "",
        baseStyle,
        tokens: emptyThemeTokens(),
        recipes: { density: "comfortable", corners: "soft" },
        background: null,
        backgroundDataUrl: "",
        existingBackgroundUrl: "",
        taskBackground: null,
        taskBackgroundDataUrl: "",
        existingTaskBackgroundUrl: "",
        tokenMode: "dark",
        originalId: "",
    };
}
function handlePreviewRadioKey<T extends string>(event: ReactKeyboardEvent<HTMLButtonElement>, values: readonly T[], current: T, onChange: (value: T) => void) {
    const direction = event.key === "ArrowRight" || event.key === "ArrowDown"
        ? 1
        : event.key === "ArrowLeft" || event.key === "ArrowUp"
            ? -1
            : 0;
    if (!direction)
        return;
    event.preventDefault();
    const currentIndex = values.indexOf(current);
    const nextIndex = (currentIndex + direction + values.length) % values.length;
    onChange(values[nextIndex]);
    const radios = event.currentTarget
        .closest('[role="radiogroup"]')
        ?.querySelectorAll<HTMLButtonElement>('[role="radio"]');
    radios?.[nextIndex]?.focus();
}
export function ThemePreviewControls({ mode, scene, onModeChange, onSceneChange, }: {
    mode: "light" | "dark";
    scene: "home" | "task";
    onModeChange: (mode: "light" | "dark") => void;
    onSceneChange: (scene: "home" | "task") => void;
}) {
    const t = useT();
    return (<div className="theme-gallery__preview-controls">
      <div className="theme-gallery__preview-control">
        <span className="theme-gallery__preview-label">{t("settings.themeGallery.appearancePreview")}</span>
        <div className="set-seg" role="radiogroup" aria-label={t("settings.themeGallery.appearancePreview")}>
          <button type="button" role="radio" aria-checked={mode === "light"} tabIndex={mode === "light" ? 0 : -1} className={`set-seg__btn${mode === "light" ? " set-seg__btn--on" : ""}`} onClick={() => onModeChange("light")} onKeyDown={(event) => handlePreviewRadioKey(event, ["light", "dark"], mode, onModeChange)}>
            {t("settings.themeLight")}
          </button>
          <button type="button" role="radio" aria-checked={mode === "dark"} tabIndex={mode === "dark" ? 0 : -1} className={`set-seg__btn${mode === "dark" ? " set-seg__btn--on" : ""}`} onClick={() => onModeChange("dark")} onKeyDown={(event) => handlePreviewRadioKey(event, ["light", "dark"], mode, onModeChange)}>
            {t("settings.themeDark")}
          </button>
        </div>
      </div>
      <div className="theme-gallery__preview-control">
        <span className="theme-gallery__preview-label">
          {t("settings.themeGallery.scenePreview")}
          <Tooltip label={t("settings.themeGallery.scenePreviewHint")} side="top">
            <button type="button" className="theme-gallery__preview-help" aria-label={t("settings.themeGallery.scenePreviewHint")}>
              <CircleHelp size={13} aria-hidden="true"/>
            </button>
          </Tooltip>
        </span>
        <div className="set-seg" role="radiogroup" aria-label={t("settings.themeGallery.scenePreview")}>
          <button type="button" role="radio" aria-checked={scene === "home"} tabIndex={scene === "home" ? 0 : -1} className={`set-seg__btn${scene === "home" ? " set-seg__btn--on" : ""}`} onClick={() => onSceneChange("home")} onKeyDown={(event) => handlePreviewRadioKey(event, ["home", "task"], scene, onSceneChange)}>
            {t("settings.themeGallery.sceneHome")}
          </button>
          <button type="button" role="radio" aria-checked={scene === "task"} tabIndex={scene === "task" ? 0 : -1} className={`set-seg__btn${scene === "task" ? " set-seg__btn--on" : ""}`} onClick={() => onSceneChange("task")} onKeyDown={(event) => handlePreviewRadioKey(event, ["home", "task"], scene, onSceneChange)}>
            {t("settings.themeGallery.sceneTask")}
          </button>
        </div>
      </div>
    </div>);
}
export function themeContrastRatio(a: string, b: string): number {
    const luminance = (hex: string) => {
        const raw = hex.slice(1, 7);
        const channels = [0, 2, 4].map((index) => parseInt(raw.slice(index, index + 2), 16) / 255)
            .map((value) => (value <= 0.03928 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4));
        return 0.2126 * channels[0] + 0.7152 * channels[1] + 0.0722 * channels[2];
    };
    const la = luminance(a);
    const lb = luminance(b);
    return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

