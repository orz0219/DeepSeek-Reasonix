// Heartbeat Panel — Modal for configuring scheduled heartbeat tasks.
//
// Renders a list of tasks with add/edit/delete controls, plus a manual
// "run now" button for each. The panel is opened from the sidebar nav item.
import { useCallback, useEffect, useRef, useState } from "react";
import { Activity, ChevronLeft, ChevronsUpDown, Check, Heart, Plus, Search, X } from "lucide-react";
import { app } from "../../../lib/bridge";
import { useT } from "../../../lib/i18n";
import { AnchoredPopover } from "../../../components/AnchoredPopover";
import { heartbeatListTasks, heartbeatSaveTasks, heartbeatTriggerNow, heartbeatGenerateID } from "./heartbeat.bridge";
import type { HeartbeatTask } from "./heartbeat.types";
import { TaskCard, TaskEditor } from "./heartbeat_panel_cards";
import "./heartbeat.css";
interface HeartbeatPanelProps {
    open: boolean;
    onClose: () => void;
    startNew?: boolean;
    onOpenTopic: (scope: string, workspaceRoot: string, topicId: string) => void;
}
export function HeartbeatPanel({ open, onClose, startNew, onOpenTopic }: HeartbeatPanelProps) {
    const t = useT();
    const [tasks, setTasks] = useState<HeartbeatTask[]>([]);
    const [loading, setLoading] = useState(false);
    const [editing, setEditing] = useState<HeartbeatTask | null>(null);
    const [searchQuery, setSearchQuery] = useState("");
    const [statusFilter, setStatusFilter] = useState<"all" | "enabled" | "disabled">("all");
    const [scopeFilter, setScopeFilter] = useState<string>("all");
    const [scopeFilterOpen, setScopeFilterOpen] = useState(false);
    const scopeFilterRef = useRef<HTMLButtonElement>(null);
    const [statusFilterOpen, setStatusFilterOpen] = useState(false);
    const statusFilterRef = useRef<HTMLButtonElement>(null);
    const [workspaceMap, setWorkspaceMap] = useState<Record<string, string>>({});
    const backdropRef = useRef<HTMLDivElement>(null);
    const dirtyRef = useRef(false);
    const startedRef = useRef(false);
    // Reset dirty ref when leaving edit mode
    useEffect(() => {
        if (!editing)
            dirtyRef.current = false;
    }, [editing]);
    const loadTasks = useCallback(async () => {
        setLoading(true);
        try {
            const [taskList, wsList] = await Promise.all([
                heartbeatListTasks(),
                app.ListWorkspaces(),
            ]);
            setTasks(taskList);
            const map: Record<string, string> = {};
            if (wsList) {
                wsList.forEach((ws) => { if (ws.path)
                    map[ws.path] = ws.name; });
            }
            setWorkspaceMap(map);
        }
        catch {
            // ignore
        }
        finally {
            setLoading(false);
        }
    }, []);
    useEffect(() => {
        if (open) {
            setEditing(null);
            setSearchQuery("");
            setStatusFilter("all");
            setScopeFilter("all");
            startedRef.current = false;
            void loadTasks();
        }
    }, [open, loadTasks]);
    // Open directly in add mode when startNew is true
    useEffect(() => {
        if (open && startNew && !startedRef.current) {
            startedRef.current = true;
            void heartbeatGenerateID().then((id) => {
                setEditing({
                    id,
                    title: "",
                    prompt: "",
                    interval: "30m",
                    enabled: true,
                    approvalMode: "yolo",
                    newConversationEachRun: false,
                    createdAt: Date.now(),
                });
            }).catch(() => { });
        }
    }, [open, startNew]);
    const save = useCallback(async (next: HeartbeatTask[]) => {
        setTasks(next);
        try {
            await heartbeatSaveTasks(next);
        }
        catch {
            // A concurrent external edit wins; reload the authoritative config so
            // the panel cannot continue editing a stale task list.
            await loadTasks();
        }
    }, [loadTasks]);
    const handleAdd = useCallback(async () => {
        try {
            const id = await heartbeatGenerateID();
            setEditing({
                id,
                title: "",
                prompt: "",
                interval: "30m",
                enabled: true,
                approvalMode: "yolo",
                newConversationEachRun: false,
                createdAt: Date.now(),
            });
        }
        catch {
            // ignore
        }
    }, []);
    const handleEdit = useCallback((task: HeartbeatTask) => {
        setEditing({ ...task });
    }, []);
    const handleDelete = useCallback(async (id: string) => {
        const next = tasks.filter((t) => t.id !== id);
        await save(next);
    }, [tasks, save]);
    const handleTrigger = useCallback(async (id: string) => {
        try {
            await heartbeatTriggerNow(id);
            void loadTasks();
        }
        catch {
            // ignore
        }
    }, [loadTasks]);
    const handleSaveEdit = useCallback(async (task: HeartbeatTask) => {
        const idx = tasks.findIndex((t) => t.id === task.id);
        const next = [...tasks];
        if (idx >= 0) {
            next[idx] = task;
        }
        else {
            next.push(task);
        }
        await save(next);
        setEditing(null);
    }, [tasks, save]);
    const handleBackdrop = useCallback((e: React.MouseEvent) => {
        if (e.target === backdropRef.current && !dirtyRef.current)
            onClose();
    }, [onClose]);
    useEffect(() => {
        if (!open)
            return;
        const onKey = (e: globalThis.KeyboardEvent) => {
            if (e.key === "Escape" && !dirtyRef.current && !document.querySelector("[data-anchored-popover='active']"))
                onClose();
        };
        window.addEventListener("keydown", onKey);
        return () => window.removeEventListener("keydown", onKey);
    }, [open, onClose]);
    if (!open)
        return null;
    const scopeFilterLabel = (filter: string, map: Record<string, string>): string => {
        if (filter === "all")
            return t("heartbeat.filterAllProjects");
        if (filter === "global")
            return t("heartbeat.scopeGlobal");
        return map[filter] || filter.split("/").pop() || filter;
    };
    const statusFilterLabel = (filter: string): string => {
        if (filter === "all")
            return t("heartbeat.filterAll" as any);
        if (filter === "enabled")
            return t("heartbeat.filterEnabled" as any);
        return t("heartbeat.filterDisabled" as any);
    };
    return (<div ref={backdropRef} className="heartbeat-backdrop" onMouseDown={handleBackdrop}>
      <div className="heartbeat-modal">
        <header className="heartbeat-modal__header">
          {editing ? (<button className="heartbeat-modal__back" onClick={() => setEditing(null)}>
              <ChevronLeft size={16}/>
            </button>) : (<Activity size={16}/>)}
          <span>{editing ? t("heartbeat.editTask") : t("heartbeat.scheduler")}</span>
          <button className="heartbeat-modal__close" onClick={onClose} aria-label={t("common.close")}>
            <X size={16}/>
          </button>
        </header>

        {editing ? (<TaskEditor key={editing.id} task={editing} onSave={handleSaveEdit} onCancel={() => setEditing(null)} onDelete={() => { handleDelete(editing.id); setEditing(null); }} onDirtyChange={(d) => { dirtyRef.current = d; }}/>) : (<div className="heartbeat-modal__body">
            <div className="heartbeat-toolbar">
              <div className="heartbeat-toolbar__search">
                <Search size={13} className="heartbeat-toolbar__search-icon"/>
                <input className="heartbeat-toolbar__search-input" value={searchQuery} onChange={(e) => setSearchQuery(e.target.value)} placeholder={t("heartbeat.searchPlaceholder" as any)}/>
                {searchQuery && (<button className="heartbeat-toolbar__search-clear" onClick={() => setSearchQuery("")}>
                    <X size={12}/>
                  </button>)}
              </div>
              <div className="heartbeat-scope-filter">
                <button ref={statusFilterRef} className="heartbeat-toolbar__btn heartbeat-toolbar__btn--select" type="button" onClick={() => setStatusFilterOpen((v) => !v)}>
                  <span>{statusFilterLabel(statusFilter)}</span>
                  <ChevronsUpDown size={12}/>
                </button>
                <AnchoredPopover open={statusFilterOpen} anchorRef={statusFilterRef} onClose={() => setStatusFilterOpen(false)} className="heartbeat-filter-menu" placement="bottom">
                  <div className="heartbeat-filter-menu__list" role="listbox">
                    {(["all", "enabled", "disabled"] as const).map((key) => (<button key={key} className={`heartbeat-filter-menu__option${statusFilter === key ? " heartbeat-filter-menu__option--selected" : ""}`} role="option" aria-selected={statusFilter === key} type="button" onClick={() => { setStatusFilter(key); setStatusFilterOpen(false); }}>
                        <span>{key === "all" ? t("heartbeat.filterAll" as any) : key === "enabled" ? t("heartbeat.filterEnabled" as any) : t("heartbeat.filterDisabled" as any)}</span>
                        {statusFilter === key && <Check size={12} className="heartbeat-filter-menu__check"/>}
                      </button>))}
                  </div>
                </AnchoredPopover>
              </div>
              <div className="heartbeat-scope-filter">
                <button ref={scopeFilterRef} className="heartbeat-toolbar__btn heartbeat-toolbar__btn--select" type="button" onClick={() => setScopeFilterOpen((v) => !v)}>
                  <span>{scopeFilterLabel(scopeFilter, workspaceMap)}</span>
                  <ChevronsUpDown size={12}/>
                </button>
                <AnchoredPopover open={scopeFilterOpen} anchorRef={scopeFilterRef} onClose={() => setScopeFilterOpen(false)} className="heartbeat-filter-menu" placement="bottom">
                  <div className="heartbeat-filter-menu__list" role="listbox">
                    <button className={`heartbeat-filter-menu__option${scopeFilter === "all" ? " heartbeat-filter-menu__option--selected" : ""}`} role="option" aria-selected={scopeFilter === "all"} type="button" onClick={() => { setScopeFilter("all"); setScopeFilterOpen(false); }}>
                      <span>{t("heartbeat.filterAllProjects")}</span>
                      {scopeFilter === "all" && <Check size={12} className="heartbeat-filter-menu__check"/>}
                    </button>
                    <button className={`heartbeat-filter-menu__option${scopeFilter === "global" ? " heartbeat-filter-menu__option--selected" : ""}`} role="option" aria-selected={scopeFilter === "global"} type="button" onClick={() => { setScopeFilter("global"); setScopeFilterOpen(false); }}>
                      <span>{t("heartbeat.scopeGlobal")}</span>
                      {scopeFilter === "global" && <Check size={12} className="heartbeat-filter-menu__check"/>}
                    </button>
                    {(() => {
                const seen = new Set<string>();
                const items: {
                    value: string;
                    label: string;
                }[] = [];
                for (const task of tasks) {
                    const key = task.scope !== "project" || !task.workspaceRoot ? "global" : task.workspaceRoot;
                    if (seen.has(key))
                        continue;
                    seen.add(key);
                    if (key !== "global") {
                        items.push({
                            value: key,
                            label: workspaceMap[key] || key.split("/").pop() || key,
                        });
                    }
                }
                return items.map((item) => (<button key={item.value} className={`heartbeat-filter-menu__option${scopeFilter === item.value ? " heartbeat-filter-menu__option--selected" : ""}`} role="option" aria-selected={scopeFilter === item.value} type="button" onClick={() => { setScopeFilter(item.value); setScopeFilterOpen(false); }}>
                          <span>{item.label}</span>
                          {scopeFilter === item.value && <Check size={12} className="heartbeat-filter-menu__check"/>}
                        </button>));
            })()}
                  </div>
                </AnchoredPopover>
              </div>
              <button className="heartbeat-toolbar__btn heartbeat-toolbar__btn--primary" style={{ marginLeft: "auto" }} onClick={handleAdd}>
                <Plus size={14}/>
                {t("heartbeat.addTask")}
              </button>
            </div>

            {(() => {
                const filtered = tasks
                    .filter((task) => {
                    if (statusFilter === "enabled" && !task.enabled)
                        return false;
                    if (statusFilter === "disabled" && task.enabled)
                        return false;
                    if (searchQuery && !task.title.toLowerCase().includes(searchQuery.toLowerCase()))
                        return false;
                    if (scopeFilter === "global" && (task.scope === "project" && task.workspaceRoot))
                        return false;
                    if (scopeFilter !== "all" && scopeFilter !== "global") {
                        if (task.scope !== "project" || task.workspaceRoot !== scopeFilter)
                            return false;
                    }
                    return true;
                })
                    .sort((a, b) => {
                    if (a.enabled && !b.enabled)
                        return -1;
                    if (!a.enabled && b.enabled)
                        return 1;
                    return 0;
                });
                const scopeLabel = (task: HeartbeatTask): string => {
                    if (task.scope !== "project" || !task.workspaceRoot)
                        return t("heartbeat.scopeGlobal");
                    return workspaceMap[task.workspaceRoot] || task.workspaceRoot.split("/").pop() || task.workspaceRoot;
                };
                return loading ? (<div className="heartbeat-empty">
                  <Heart size={24} className="heartbeat-pulse"/>
                  <span>{t("workspace.loading")}</span>
                </div>) : filtered.length === 0 ? (<div className="heartbeat-empty">
                  <Heart size={24}/>
                  <span>{tasks.length === 0 ? t("heartbeat.noTasks") : t("heartbeat.noMatchingTasks")}</span>
                </div>) : (<ul className="heartbeat-tasklist">
                  {filtered.map((task) => (<TaskCard key={task.id} task={task} scopeLabel={scopeLabel(task)} onToggle={() => {
                            const next = tasks.map((t) => t.id === task.id ? { ...t, enabled: !t.enabled } : t);
                            save(next);
                        }} onEdit={() => handleEdit(task)} onTrigger={() => void handleTrigger(task.id)} onOpenTopic={onOpenTopic} onClose={onClose}/>))}
                </ul>);
            })()}
          </div>)}
      </div>
    </div>);
}
export { heartbeatNextRunAt as heartbeatNextRunAt } from "./heartbeat_panel_helpers";
export { heartbeatBuildCycleInterval as heartbeatBuildCycleInterval } from "./heartbeat_panel_helpers";
export { heartbeatIntervalLabel as heartbeatIntervalLabel } from "./heartbeat_panel_helpers";
export { WEEKDAYS as WEEKDAYS } from "./heartbeat_panel_helpers";
export { defaultHeartbeatCycleDays as defaultHeartbeatCycleDays } from "./heartbeat_panel_helpers";
export { normalizeMode as normalizeMode } from "./heartbeat_panel_helpers";
export { TaskCard as TaskCard } from "./heartbeat_panel_cards";
export { TaskEditor as TaskEditor } from "./heartbeat_panel_cards";

