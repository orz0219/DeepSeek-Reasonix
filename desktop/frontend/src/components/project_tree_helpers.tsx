import type { CSSProperties } from "react";
import { Check } from "lucide-react";
import { asArray } from "../lib/array";
import { isTopicNode, topicIsActive, type ProjectTreeReadActivity, type ProjectTreeVariant } from "../lib/projectTreeTopic";
import type { ProjectNode } from "../lib/types";
import { topicActivityTime } from "../lib/session";
import { type Translator } from "../lib/i18n";
import { projectColorValue } from "../lib/projectColors";
import { type TopicShortcutEntry } from "../lib/topicShortcuts";
import type { ShortcutPlatform } from "../lib/keyboardShortcuts";
export interface ProjectTreeProps {
    activeScope?: string;
    activeWorkspaceRoot?: string;
    activeTopicId?: string;
    activeSessionPath?: string;
    variant?: ProjectTreeVariant;
    onOpenTopic: (scope: string, workspaceRoot: string, topicId: string, sessionPath?: string) => Promise<void> | void;
    onAddProject: () => Promise<void>;
    onCreateTopic?: (scope: string, workspaceRoot: string) => Promise<void> | void;
    onCreateDeliveryWorktree?: (workspaceRoot: string) => Promise<void> | void;
    onRenameTopic?: (topicId: string, title: string) => Promise<void> | void;
    onTopicsChanged?: () => Promise<void> | void;
    refreshSignal?: number;
    timeFilter: "all" | "10" | "20" | "1h" | "3h" | "5h" | "1d";
    onTimeFilterChange: (filter: "all" | "10" | "20" | "1h" | "3h" | "5h" | "1d") => void;
    searchExpanded?: boolean;
    searchFocusSignal?: number;
    showShortcutBadges?: boolean;
    shortcutPlatform?: ShortcutPlatform;
    onVisibleTopicsChange?: (topics: TopicShortcutEntry[]) => void;
}
export function projectNodeKey(node: ProjectNode, depth: number): string {
    return node.key || `${node.kind}-${node.root ?? ""}-${node.topicId ?? ""}-${depth}`;
}
export type ProjectDropPosition = "before" | "after";
export type WorkbenchHeaderMenu = "more" | "add" | null;
export type WorkbenchOrganizeMode = "project" | "recent" | "time";
export type WorkbenchSortMode = "created" | "updated";
export type CollapseSnapshot = {
    expanded: Set<string>;
    manuallyCollapsed: Set<string>;
};
export type PinnedTreeSections = {
    pinned: ProjectNode[];
    projects: ProjectNode[];
};
export const GLOBAL_PROJECT_ORDER_KEY = "__global__";
export const WORKBENCH_ORGANIZE_KEY = "projectTree:workbenchOrganize";
// Shared by classic and workbench; key string kept for existing saved choices.
export const WORKBENCH_SORT_KEY = "projectTree:workbenchSort";
const READ_ACTIVITY_KEY = "projectTree:readActivity";
export const READ_ACTIVITY_INIT_KEY = "projectTree:readActivityInitialized";
export function loadReadActivity(): ProjectTreeReadActivity {
    try {
        const raw = localStorage.getItem(READ_ACTIVITY_KEY);
        if (!raw)
            return {};
        const parsed = JSON.parse(raw) as Record<string, unknown>;
        const out: ProjectTreeReadActivity = {};
        for (const [key, value] of Object.entries(parsed)) {
            if (typeof value === "number" && Number.isFinite(value))
                out[key] = value;
        }
        return out;
    }
    catch {
        return {};
    }
}
export function saveReadActivity(readActivity: ProjectTreeReadActivity) {
    try {
        localStorage.setItem(READ_ACTIVITY_KEY, JSON.stringify(readActivity));
    }
    catch {
        /* localStorage unavailable */
    }
}
export function loadWorkbenchOrganizeMode(): WorkbenchOrganizeMode {
    try {
        const value = localStorage.getItem(WORKBENCH_ORGANIZE_KEY);
        if (value === "recent" || value === "time")
            return value;
    }
    catch {
        /* localStorage unavailable */
    }
    return "project";
}
export function loadWorkbenchSortMode(): WorkbenchSortMode {
    try {
        const value = localStorage.getItem(WORKBENCH_SORT_KEY);
        if (value === "created")
            return "created";
    }
    catch {
        /* localStorage unavailable */
    }
    return "updated";
}
function projectOrderKey(node: ProjectNode): string {
    if (node.kind === "global_folder")
        return GLOBAL_PROJECT_ORDER_KEY;
    if (node.kind === "project" && node.root)
        return node.root;
    return "";
}
export function projectRoots(nodes: ProjectNode[]): string[] {
    return nodes
        .map(projectOrderKey)
        .filter((key) => key !== "");
}
export function collapsibleFolderKeys(nodes: ProjectNode[], depth = 0): string[] {
    const keys: string[] = [];
    for (const node of nodes) {
        if (!node)
            continue;
        const children = asArray(node.children);
        if ((node.kind === "project" || node.kind === "global_folder") && children.length > 0) {
            keys.push(projectNodeKey(node, depth));
        }
        keys.push(...collapsibleFolderKeys(children, depth + 1));
    }
    return keys;
}
export function activeSessionAncestorKeys(nodes: ProjectNode[], activeScope?: string, activeWorkspaceRoot?: string, activeTopicId?: string, activeSessionPath?: string): string[] {
    const walk = (nodeList: ProjectNode[], ancestors: string[]): string[] | null => {
        for (const node of nodeList) {
            if (!node)
                continue;
            if (topicIsActive(node, activeScope, activeWorkspaceRoot, activeTopicId, activeSessionPath))
                return ancestors;
            const children = asArray(node.children);
            if (children.length > 0) {
                const next = walk(children, [...ancestors, projectNodeKey(node, ancestors.length)]);
                if (next)
                    return next;
            }
        }
        return null;
    };
    const found = walk(nodes, []);
    if (found)
        return found;
    // Shell-only snapshots no longer embed topics. Expand the matching project or
    // Global folder by workspace identity so the first lazy page can load.
    const scope = (activeScope ?? "").trim();
    const root = (activeWorkspaceRoot ?? "").trim();
    for (const node of nodes) {
        if (!node)
            continue;
        if (scope === "global" && node.kind === "global_folder") {
            return [projectNodeKey(node, 0)];
        }
        if (node.kind === "project" && root && (node.root === root || node.root === activeWorkspaceRoot)) {
            return [projectNodeKey(node, 0)];
        }
        if (!scope && !root && activeTopicId && (node.kind === "project" || node.kind === "global_folder")) {
            // Active topic without resolved scope still needs a folder open path.
            return [projectNodeKey(node, 0)];
        }
    }
    return [];
}
export function defaultExpandedProjectTreeKeys(nodes: ProjectNode[], activeScope?: string, activeWorkspaceRoot?: string, activeTopicId?: string, activeSessionPath?: string): string[] {
    return activeSessionAncestorKeys(nodes, activeScope, activeWorkspaceRoot, activeTopicId, activeSessionPath);
}
export function reorderedProjectRoots(nodes: ProjectNode[], draggedRoot: string, targetRoot: string, position: ProjectDropPosition): string[] {
    const roots = projectRoots(nodes);
    if (draggedRoot === targetRoot || !roots.includes(draggedRoot) || !roots.includes(targetRoot))
        return roots;
    const next = roots.filter((root) => root !== draggedRoot);
    const targetIndex = next.indexOf(targetRoot);
    if (targetIndex < 0)
        return roots;
    next.splice(position === "before" ? targetIndex : targetIndex + 1, 0, draggedRoot);
    return next;
}
export function applyProjectOrder(nodes: ProjectNode[], roots: string[]): ProjectNode[] {
    const projectEntries = nodes
        .map((node): [
        string,
        ProjectNode
    ] => [projectOrderKey(node), node])
        .filter(([key]) => key !== "");
    const byRoot = new Map<string, ProjectNode>(projectEntries);
    const orderedProjects = roots.map((root) => byRoot.get(root)).filter((node): node is ProjectNode => Boolean(node));
    const orderedKeys = new Set(roots);
    const nonProjects = nodes.filter((node) => !orderedKeys.has(projectOrderKey(node)));
    return [...nonProjects, ...orderedProjects];
}
function topicSortValue(node: ProjectNode, sortMode: WorkbenchSortMode): number {
    if (sortMode === "created")
        return node.createdAt || node.lastActivityAt || 0;
    return topicActivityTime(node);
}
function projectSortValue(node: ProjectNode, sortMode: WorkbenchSortMode): number {
    return asArray(node.children).reduce((max, child) => {
        if (!isTopicNode(child))
            return max;
        return Math.max(max, topicSortValue(child, sortMode));
    }, 0);
}
function sortWorkbenchChildren(children: ProjectNode[], sortMode: WorkbenchSortMode): ProjectNode[] {
    return [...children].sort((a, b) => {
        if (!isTopicNode(a) || !isTopicNode(b))
            return 0;
        if (Boolean(a.pinned) !== Boolean(b.pinned))
            return a.pinned ? -1 : 1;
        return topicSortValue(b, sortMode) - topicSortValue(a, sortMode);
    });
}
export function arrangeWorkbenchTree(nodes: ProjectNode[], organizeMode: WorkbenchOrganizeMode, sortMode: WorkbenchSortMode): ProjectNode[] {
    const arranged = nodes.map((node) => {
        if (node.kind !== "project" && node.kind !== "global_folder")
            return node;
        return { ...node, children: sortWorkbenchChildren(asArray(node.children), sortMode) };
    });
    if (organizeMode === "project")
        return arranged;
    const mode = organizeMode === "recent" ? "updated" : sortMode;
    return [...arranged].sort((a, b) => {
        if (Boolean(a.pinned) !== Boolean(b.pinned))
            return a.pinned ? -1 : 1;
        return projectSortValue(b, mode) - projectSortValue(a, mode);
    });
}
// Classic keeps the user's manual project order but sorts topics inside each
// folder, so row order matches the activity time shown in the meta line
// instead of the persisted insertion order.
export function arrangeClassicProjectTree(nodes: ProjectNode[], sortMode: WorkbenchSortMode): ProjectNode[] {
    return arrangeWorkbenchTree(nodes, "project", sortMode);
}
// Classic folders preview only the first few topics; the rest sit behind a
// show-more toggle so one busy project cannot push the others out of view.
export const CLASSIC_TOPIC_PREVIEW_LIMIT = 5;
export function classicTopicWindow(children: ProjectNode[], showAll: boolean): {
    visible: ProjectNode[];
    hiddenCount: number;
} {
    if (showAll || children.length <= CLASSIC_TOPIC_PREVIEW_LIMIT)
        return { visible: children, hiddenCount: 0 };
    return {
        visible: children.slice(0, CLASSIC_TOPIC_PREVIEW_LIMIT),
        hiddenCount: children.length - CLASSIC_TOPIC_PREVIEW_LIMIT,
    };
}
export function splitPinnedProjectTree(nodes: ProjectNode[], sortMode: WorkbenchSortMode, includePinnedProjects = true): PinnedTreeSections {
    const pinnedProjects: ProjectNode[] = [];
    const projects: ProjectNode[] = [];
    for (const node of nodes) {
        if (!node)
            continue;
        const isFolder = node.kind === "project" || node.kind === "global_folder";
        if (includePinnedProjects && isFolder && node.pinned && node.kind === "project") {
            pinnedProjects.push(node);
            continue;
        }
        projects.push(node);
    }
    pinnedProjects.sort((a, b) => projectSortValue(b, sortMode) - projectSortValue(a, sortMode));
    return {
        pinned: pinnedProjects,
        projects,
    };
}
// Global rows use the same project tree recipe; the fallback supplies their non-workspace accent.
export function projectAccentStyle(color?: string, fallbackValue?: string): CSSProperties | undefined {
    const value = projectColorValue(color) || fallbackValue;
    if (!value)
        return undefined;
    return { "--project-accent": value } as CSSProperties;
}
export function colorMenuLabel(label: string, color?: string, active = false) {
    const value = projectColorValue(color);
    return (<span className="project-tree__color-option">
      <span className="project-tree__color-swatch" style={value ? ({ "--project-accent": value } as CSSProperties) : undefined} aria-hidden="true"/>
      <span>{label}</span>
      {active && <Check className="project-tree__color-check" size={12}/>}
    </span>);
}
export function menuLabelWithCheck(label: string, checked: boolean) {
    return (<span className="context-menu__label-with-check">
      <span className="context-menu__label-text">{label}</span>
      {checked && <Check className="context-menu__check" size={13} aria-hidden="true"/>}
    </span>);
}
export function revealLabelKey(platform: string): "projectTree.revealInFinder" | "projectTree.revealInExplorer" | "projectTree.revealInFileManager" {
    if (platform === "darwin")
        return "projectTree.revealInFinder";
    if (platform === "windows")
        return "projectTree.revealInExplorer";
    return "projectTree.revealInFileManager";
}
export function projectColorLabel(t: Translator, color?: string): string {
    switch (color) {
        case "red": return t("projectTree.colorRed");
        case "orange": return t("projectTree.colorOrange");
        case "amber": return t("projectTree.colorAmber");
        case "green": return t("projectTree.colorGreen");
        case "teal": return t("projectTree.colorTeal");
        case "blue": return t("projectTree.colorBlue");
        case "purple": return t("projectTree.colorPurple");
        case "pink": return t("projectTree.colorPink");
        default: return t("projectTree.colorDefault");
    }
}

