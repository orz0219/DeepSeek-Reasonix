import { type ThemeStyle, isThemeStyle } from "../lib/theme";
import { type ThemePackBackground, type ThemePackRecipes, type ThemePackTokens, type ThemePackView, emptyThemeTokens } from "../lib/themePack";
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
export function packToEditor(pack: ThemePackView, mode: "create" | "edit"): EditorState {
    return {
        mode,
        id: mode === "create" ? slugifyId(`${pack.id}-copy`) : pack.id,
        name: mode === "create" ? `${pack.name} Copy` : pack.name,
        author: pack.author || "",
        description: pack.description || "",
        license: pack.license || "",
        baseStyle: isThemeStyle(pack.baseStyle) ? pack.baseStyle : "graphite",
        tokens: {
            light: { ...(pack.tokens?.light || {}) },
            dark: { ...(pack.tokens?.dark || {}) },
        },
        recipes: {
            density: pack.recipes?.density === "compact" ? "compact" : "comfortable",
            corners: pack.recipes?.corners === "square" || pack.recipes?.corners === "round" ? pack.recipes.corners : "soft",
        },
        background: pack.background ? { ...pack.background } : null,
        backgroundDataUrl: "",
        existingBackgroundUrl: pack.backgroundUrl || "",
        tokenMode: "dark",
        originalId: pack.id,
    };
}
export function emptyEditor(): EditorState {
    return {
        mode: "create",
        id: "my-theme",
        name: "My Theme",
        author: "",
        description: "",
        license: "",
        baseStyle: "graphite",
        tokens: emptyThemeTokens(),
        recipes: { density: "comfortable", corners: "soft" },
        background: null,
        backgroundDataUrl: "",
        existingBackgroundUrl: "",
        tokenMode: "dark",
        originalId: "",
    };
}

