import type { ReactElement } from "react";
import { clampWorkspaceSplitTreeWidth } from "../lib/workspaceSplit";
import type { DirEntry, FilePreview } from "../lib/types";
export const WORKSPACE_TREE_MIN_WIDTH = 140;
export const WORKSPACE_TREE_DEFAULT_WIDTH = 300;
export const WORKSPACE_TREE_RAIL_WIDTH = 44;
export const WORKSPACE_PREVIEW_MIN_WIDTH = 140;
const WORKSPACE_PREVIEW_TARGET_WIDTH = 360;
export const WORKSPACE_DUAL_PANEL_TARGET_WIDTH = WORKSPACE_TREE_DEFAULT_WIDTH + WORKSPACE_PREVIEW_TARGET_WIDTH;
export const WORKSPACE_CONTEXT_MENU_SELECTION_HEIGHT = 48;
export const WORKSPACE_MAX_PREVIEW_TABS = 5;
export type WorkspaceRevealRequest = {
    id: number;
    path: string;
};
export type WorkspaceFileListRequest = {
    id: number;
    paths: string[];
};
export type WorkspaceChangeListEntry = {
    key: string;
    path: string;
    meta: string;
    time: string;
    detail: string;
};
export type WorkspaceChangeListRequest = {
    id: number;
    changes: WorkspaceChangeListEntry[];
};
export function clampWorkspaceTreeWidth(width: number, panelWidth?: number): number {
    return clampWorkspaceSplitTreeWidth({
        width,
        panelWidth,
        railWidth: WORKSPACE_TREE_RAIL_WIDTH,
        treeMinWidth: WORKSPACE_TREE_MIN_WIDTH,
        previewMinWidth: WORKSPACE_PREVIEW_MIN_WIDTH,
    });
}
export function entryPath(dir: string, entry: DirEntry): string {
    const prefix = dir === "" || dir.endsWith("/") ? dir : dir + "/";
    return prefix + entry.name + (entry.isDir ? "/" : "");
}
export function basename(path: string): string {
    const parts = path.split("/").filter(Boolean);
    return parts[parts.length - 1] ?? "";
}
export function parentPath(path: string): string {
    const clean = path.replace(/\/$/, "");
    const parts = clean.split("/").filter(Boolean);
    return parts.slice(0, -1).join("/");
}
export function parentDirs(path: string): string[] {
    const parts = path.split("/").filter(Boolean);
    const dirs: string[] = [""];
    let acc = "";
    for (let i = 0; i < parts.length - 1; i++) {
        acc += parts[i] + "/";
        dirs.push(acc);
    }
    return dirs;
}
export function topLevelDirPath(path: string): string {
    const first = path.split("/").find(Boolean);
    return first ? `${first}/` : "";
}
export function renderMediaPreview(preview: FilePreview): ReactElement | null {
    if (!preview.url)
        return null;
    if (preview.kind === "image") {
        return (<div className="workspace-media workspace-media--image">
        <img src={preview.url} alt={basename(preview.path)} decoding="async" draggable={false}/>
      </div>);
    }
    if (preview.kind === "pdf") {
        return (<iframe className="workspace-media workspace-media--pdf" src={preview.url} title={basename(preview.path)}/>);
    }
    return null;
}
export function shortCwd(cwd?: string): string {
    if (!cwd)
        return "";
    const parts = cwd.split("/").filter(Boolean);
    if (parts.length <= 2)
        return cwd;
    return "…/" + parts.slice(-2).join("/");
}
export function formatBytes(n: number): string {
    if (n >= 1024 * 1024)
        return `${(n / (1024 * 1024)).toFixed(1)} MB`;
    if (n >= 1024)
        return `${Math.ceil(n / 1024)} KB`;
    return `${n} B`;
}
export function formatCommitDate(dateStr: string): string {
    const d = new Date(dateStr);
    if (isNaN(d.getTime()))
        return dateStr;
    const day = String(d.getDate()).padStart(2, "0");
    const monthNames = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
    const month = monthNames[d.getMonth()];
    const year = d.getFullYear();
    const hours = String(d.getHours()).padStart(2, "0");
    const minutes = String(d.getMinutes()).padStart(2, "0");
    return `${day} ${month} ${year} ${hours}:${minutes}`;
}
export interface TreeRow {
    key: string;
    path: string;
    depth: number;
    entry: DirEntry;
    active: boolean;
    isOpen?: boolean;
    isSearch?: boolean;
    compactPaths?: string[];
    displayName?: string;
}

