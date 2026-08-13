// Heartbeat Panel — Modal for configuring scheduled heartbeat tasks.
//
// Renders a list of tasks with add/edit/delete controls, plus a manual
// "run now" button for each. The panel is opened from the sidebar nav item.
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { ChevronsUpDown, Clock, MessageSquare, Play, Trash2 } from "lucide-react";
import { app } from "../../../lib/bridge";
import { useT } from "../../../lib/i18n";
import type { HeartbeatTask } from "./heartbeat.types";
import type { WorkspaceView } from "../../../lib/types";
import { heartbeatIntervalLabel, heartbeatNextRunAt, defaultHeartbeatCycleDays, heartbeatBuildCycleInterval, WEEKDAYS, normalizeMode } from "./heartbeat_panel_helpers";
// ── Task Card ─────────────────────────────────────────────────────────────────
export function TaskCard({ task, scopeLabel, onToggle, onEdit, onTrigger, onOpenTopic, onClose, }: {
    task: HeartbeatTask;
    scopeLabel: string;
    onToggle: () => void;
    onEdit: () => void;
    onTrigger: () => void;
    onOpenTopic: (scope: string, workspaceRoot: string, topicId: string) => void;
    onClose: () => void;
}) {
    const t = useT();
    const intervalLabel = heartbeatIntervalLabel(task.interval, t);
    const nextRunLabel = (() => {
        if (!task.enabled)
            return t("heartbeat.disabled");
        const now = Date.now();
        const next = heartbeatNextRunAt(task, now);
        if (next === null)
            return task.lastRunAt ? "" : t("heartbeat.neverRun");
        const diff = next - now;
        if (diff <= 0)
            return t("heartbeat.due" as any);
        if (diff < 60000)
            return t("heartbeat.soon" as any);
        if (diff < 3600000)
            return `${Math.floor(diff / 60000)}${t("heartbeat.minLater" as any)}`;
        if (diff < 86400000)
            return `${Math.floor(diff / 3600000)}${t("heartbeat.hourLater" as any)}`;
        const d = new Date(next);
        return `${d.getMonth() + 1}/${d.getDate()} ${d.getHours().toString().padStart(2, "0")}:${d.getMinutes().toString().padStart(2, "0")}`;
    })();
    const lastRunLabel = task.lastRunAt
        ? (() => {
            const d = new Date(task.lastRunAt);
            const now = new Date();
            const diff = now.getTime() - task.lastRunAt;
            if (diff < 60000)
                return t("heartbeat.justNow" as any);
            if (diff < 3600000)
                return `${Math.floor(diff / 60000)}${t("heartbeat.minAgo" as any)}`;
            if (diff < 86400000)
                return `${Math.floor(diff / 3600000)}${t("heartbeat.hourAgo" as any)}`;
            return `${d.getMonth() + 1}/${d.getDate()} ${d.getHours().toString().padStart(2, "0")}:${d.getMinutes().toString().padStart(2, "0")}`;
        })()
        : t("heartbeat.neverRun");
    return (<li className={`heartbeat-card${!task.enabled ? " heartbeat-card--disabled" : ""}`}>
      <div className="heartbeat-card__head">
        <span className={`heartbeat-card__dot${task.enabled ? " heartbeat-card__dot--on" : ""}`}/>
        <span className="heartbeat-card__title">
          <button type="button" className="heartbeat-card__title-btn" onClick={onEdit}>
            <span className="heartbeat-card__title-text">{task.title || t("heartbeat.untitled")}</span>
            <span className="heartbeat-card__title-scope">{scopeLabel}</span>
          </button>
        </span>
        <span className="heartbeat-card__meta-item heartbeat-card__meta-item--compact">
          <Clock size={10}/>
          {intervalLabel}
          <span className="heartbeat-card__meta-sep">·</span>
          {task.enabled ? nextRunLabel : lastRunLabel}
        </span>
        <span className="heartbeat-card__head-actions">
          <button className="heartbeat-card__open-btn heartbeat-card__open-btn--play" onClick={onTrigger} title={t("heartbeat.runNow")}>
            <Play size={12}/>
          </button>
          <button className="heartbeat-card__open-btn" type="button" disabled={!task.topicId} onClick={() => {
            if (task.topicId) {
                onClose();
                onOpenTopic(task.scope || "global", task.workspaceRoot || "", task.topicId);
            }
        }} title={task.topicId ? (t("heartbeat.openTopic" as any)) : ""}>
            <MessageSquare size={13}/>
          </button>
          <button className={`heartbeat-card__toggle${task.enabled ? " heartbeat-card__toggle--on" : ""}`} onClick={onToggle} aria-label={task.enabled ? t("heartbeat.disable") : t("heartbeat.enabled")}>
            <span className="heartbeat-card__toggle-knob"/>
          </button>
        </span>
      </div>
    </li>);
}
function CycleEditor({ draft, setDraft, }: {
    draft: HeartbeatTask;
    setDraft: (field: keyof HeartbeatTask, value: string | boolean) => void;
}) {
    const t = useT();
    const cycleMatch = (draft.interval || "").match(/^(\d+)[smh]\|(daily|weekly|biweekly|monthly|yearly)(?::([^@]*))?(?:@(\d{2}:\d{2}))?$/);
    const [cycleType, setCycleType] = useState<string>(cycleMatch ? cycleMatch[2] : "daily");
    const cycleDays = cycleMatch?.[3] || "";
    const cycleTime = cycleMatch?.[4] || "09:00";
    const [selectedDays, setSelectedDays] = useState<string[]>(cycleDays ? cycleDays.split(",").filter(Boolean) :
        defaultHeartbeatCycleDays(cycleMatch ? cycleMatch[2] : "daily"));
    const [monthDay, setMonthDay] = useState(cycleDays || "1");
    const [yearMonth, setYearMonth] = useState(cycleDays.split("-")[0] || "1");
    const [yearDay, setYearDay] = useState(cycleDays.split("-")[1] || "1");
    const [timeVal, setTimeVal] = useState(cycleTime);
    const hasWeekdays = cycleType === "daily" || cycleType === "weekly" || cycleType === "biweekly";
    // Build interval string when config changes
    const buildInterval = useCallback(heartbeatBuildCycleInterval, []);
    const onCycleTypeChange = useCallback((ct: string) => {
        setCycleType(ct);
        const days = defaultHeartbeatCycleDays(ct);
        setSelectedDays(days);
        setMonthDay("1");
        setYearMonth("1");
        setYearDay("1");
        setDraft("interval", buildInterval(ct, days, timeVal));
    }, [buildInterval, setDraft, timeVal]);
    const onDayToggle = useCallback((day: string) => {
        setSelectedDays((prev) => {
            if (prev.includes(day) && prev.length <= 1)
                return prev;
            const next = prev.includes(day) ? prev.filter((d) => d !== day) : [...prev, day];
            setDraft("interval", buildInterval(cycleType, next, timeVal));
            return next;
        });
    }, [buildInterval, cycleType, setDraft, timeVal]);
    const onMonthDayChange = useCallback((d: string) => {
        setMonthDay(d);
        setDraft("interval", buildInterval(cycleType, [d], timeVal));
    }, [buildInterval, cycleType, setDraft, timeVal]);
    const onYearMonthChange = useCallback((m: string) => {
        setYearMonth(m);
        setDraft("interval", buildInterval(cycleType, [m, yearDay], timeVal));
    }, [buildInterval, cycleType, setDraft, timeVal, yearDay]);
    const onYearDayChange = useCallback((d: string) => {
        setYearDay(d);
        setDraft("interval", buildInterval(cycleType, [yearMonth, d], timeVal));
    }, [buildInterval, cycleType, setDraft, timeVal, yearMonth]);
    const onTimeChange = useCallback((tm: string) => {
        setTimeVal(tm);
        const days = hasWeekdays ? selectedDays
            : cycleType === "monthly" ? [monthDay]
                : cycleType === "yearly" ? [yearMonth, yearDay]
                    : [];
        setDraft("interval", buildInterval(cycleType, days, tm));
    }, [buildInterval, cycleType, selectedDays, monthDay, yearMonth, yearDay, setDraft]);
    const MONTHS = Array.from({ length: 12 }, (_, i) => ({
        value: String(i + 1),
        label: t("heartbeat.monthOption", { n: i + 1 }),
    }));
    const DAYS = Array.from({ length: 31 }, (_, i) => ({
        value: String(i + 1),
        label: t("heartbeat.dayOption", { n: i + 1 }),
    }));
    return (<div className="heartbeat-editor__cycle-wrap">
      <div className="heartbeat-editor__cycle-row">
        <select className="heartbeat-editor__freq-select" value={cycleType} onChange={(e) => onCycleTypeChange(e.target.value)}>
          <option value="daily">{t("heartbeat.cycleDaily")}</option>
          <option value="weekly">{t("heartbeat.cycleWeekly")}</option>
          <option value="biweekly">{t("heartbeat.cycleBiweekly")}</option>
          <option value="monthly">{t("heartbeat.cycleMonthly")}</option>
          <option value="yearly">{t("heartbeat.cycleYearly")}</option>
        </select>

        {cycleType === "monthly" && (<select className="heartbeat-editor__freq-select" value={monthDay} onChange={(e) => onMonthDayChange(e.target.value)}>
            {DAYS.map((d) => (<option key={d.value} value={d.value}>{d.label}</option>))}
          </select>)}

        {cycleType === "yearly" && (<>
            <select className="heartbeat-editor__freq-select" value={yearMonth} onChange={(e) => onYearMonthChange(e.target.value)}>
              {MONTHS.map((m) => (<option key={m.value} value={m.value}>{m.label}</option>))}
            </select>
            <select className="heartbeat-editor__freq-select" value={yearDay} onChange={(e) => onYearDayChange(e.target.value)}>
              {DAYS.map((d) => (<option key={d.value} value={d.value}>{d.label}</option>))}
            </select>
          </>)}

        <input className="heartbeat-editor__freq-input heartbeat-editor__freq-input--time" type="time" value={timeVal} onChange={(e) => onTimeChange(e.target.value)}/>

        {hasWeekdays && (<div className="set-seg">
            {WEEKDAYS.map((wd) => (<button key={wd.key} type="button" className={`set-seg__btn${selectedDays.includes(wd.key) ? " set-seg__btn--on" : ""}`} onClick={() => onDayToggle(wd.key)} aria-pressed={selectedDays.includes(wd.key)}>
                {t(wd.labelKey)}
              </button>))}
          </div>)}
      </div>
    </div>);
}
export function TaskEditor({ task, onSave, onCancel, onDelete, onDirtyChange, }: {
    task: HeartbeatTask;
    onSave: (t: HeartbeatTask) => void;
    onCancel: () => void;
    onDelete: () => void;
    onDirtyChange?: (dirty: boolean) => void;
}) {
    const t = useT();
    const titleRef = useRef<HTMLInputElement>(null);
    const [workspaces, setWorkspaces] = useState<WorkspaceView[]>([]);
    const [projectOpen, setProjectOpen] = useState(false);
    const [confirmingDelete, setConfirmingDelete] = useState(false);
    const projectRef = useRef<HTMLDivElement>(null);
    useEffect(() => {
        titleRef.current?.focus();
        app.ListWorkspaces().then((list) => setWorkspaces(list ?? [])).catch(() => { });
    }, []);
    useEffect(() => {
        if (!projectOpen)
            return;
        const close = (e: MouseEvent) => {
            if (projectRef.current && !projectRef.current.contains(e.target as Node)) {
                setProjectOpen(false);
            }
        };
        document.addEventListener("click", close);
        return () => document.removeEventListener("click", close);
    }, [projectOpen]);
    const [draft, setDraft] = useState(task);
    const initialTaskRef = useRef(task);
    const isDirty = draft.title !== initialTaskRef.current.title
        || draft.prompt !== initialTaskRef.current.prompt
        || draft.interval !== initialTaskRef.current.interval
        || draft.enabled !== initialTaskRef.current.enabled
        || draft.approvalMode !== initialTaskRef.current.approvalMode
        || draft.newConversationEachRun !== initialTaskRef.current.newConversationEachRun
        || draft.notifyChannels !== initialTaskRef.current.notifyChannels
        || draft.scope !== initialTaskRef.current.scope
        || draft.workspaceRoot !== initialTaskRef.current.workspaceRoot
        || draft.timeWindowStart !== initialTaskRef.current.timeWindowStart
        || draft.timeWindowEnd !== initialTaskRef.current.timeWindowEnd;
    useEffect(() => {
        onDirtyChange?.(isDirty);
    }, [isDirty, onDirtyChange]);
    const intervalBeforeCycle = useRef<string | null>(null);
    const promptRef = useRef<HTMLTextAreaElement>(null);
    // Auto-grow prompt textarea: shrink-to-fit then cap at 180px
    const autoGrowPrompt = useCallback(() => {
        const el = promptRef.current;
        if (!el)
            return;
        el.style.height = "auto";
        el.style.height = Math.min(el.scrollHeight, 180) + "px";
    }, []);
    useLayoutEffect(() => {
        autoGrowPrompt();
    }, [draft.prompt, autoGrowPrompt]);
    const set = useCallback((field: keyof HeartbeatTask, value: string | boolean) => {
        setDraft((prev) => ({ ...prev, [field]: value }));
    }, []);
    // Detect frequency type from interval value
    const [freqType, setFreqType] = useState<"cycle" | "interval">((task.interval && task.interval.includes("|")) ? "cycle" : "interval");
    const isNew = !task.createdAt;
    const selectedWorkspace = draft.scope === "project" && draft.workspaceRoot
        ? workspaces.find((w) => w.path === draft.workspaceRoot)
        : null;
    return (<div className="heartbeat-editor">
      <div className="heartbeat-editor__fields">
        {/* Title */}
        <div className="heartbeat-editor__field">
        <label>{t("heartbeat.fieldTitle")}</label>
        <input ref={titleRef} className="heartbeat-editor__input" value={draft.title} onChange={(e) => set("title", e.target.value)} placeholder={t("heartbeat.titlePlaceholder")}/>
      </div>

      {/* Scope */}
      <div className="heartbeat-editor__field">
        <label>{t("heartbeat.fieldScope")} <span className="heartbeat-editor__optional">{t("heartbeat.optional")}</span></label>
        <div className="heartbeat-editor__scope-row">
          <button className={`heartbeat-scope-btn${draft.scope !== "project" ? " heartbeat-scope-btn--active" : ""}`} onClick={() => setDraft((prev) => ({ ...prev, scope: "global", workspaceRoot: "" }))}>
            {t("heartbeat.scopeGlobal")}
          </button>
          <div className="heartbeat-project-wrap" ref={projectRef}>
            <button className={`heartbeat-scope-btn${draft.scope === "project" ? " heartbeat-scope-btn--active" : ""}`} onClick={() => setProjectOpen((v) => !v)}>
              {selectedWorkspace ? selectedWorkspace.name : t("heartbeat.scopeProject")}
              <ChevronsUpDown size={12}/>
            </button>
            {projectOpen && (<div className="heartbeat-project-menu">
                {workspaces.length === 0 ? (<div className="heartbeat-project-menu__empty">{t("heartbeat.noProjects")}</div>) : (workspaces.map((ws) => (<button key={ws.path} className={`heartbeat-project-menu__item${draft.workspaceRoot === ws.path ? " heartbeat-project-menu__item--active" : ""}`} onClick={() => {
                    setDraft((prev) => ({ ...prev, scope: "project", workspaceRoot: ws.path }));
                    setProjectOpen(false);
                }}>
                      {ws.name}
                      {ws.current && <span className="heartbeat-project-menu__current">{t("heartbeat.currentWorkspace")}</span>}
                    </button>)))}
              </div>)}
          </div>
        </div>
      </div>

      {/* Prompt */}
      <div className="heartbeat-editor__field">
        <label>{t("heartbeat.fieldPrompt")}</label>
        <textarea ref={promptRef} className="heartbeat-editor__textarea" value={draft.prompt} onChange={(e) => {
            set("prompt", e.target.value);
            // autoGrowPrompt is called via useEffect watching draft.prompt
        }} placeholder={t("heartbeat.promptPlaceholder")}/>
      </div>

      {/* Approval Mode + Push to bot (side by side) */}
      <div style={{ display: "flex", gap: "16px", flexWrap: "wrap" }}>
        <div className="heartbeat-editor__field" style={{ flex: "1 1 45%", minWidth: "200px" }}>
          <label>{t("heartbeat.fieldApprovalMode")}</label>
          <div className="set-seg" style={{ alignSelf: "flex-start" }}>
            <button className={`set-seg__btn${normalizeMode(draft.approvalMode) === "ask" ? " set-seg__btn--on" : ""}`} onClick={() => setDraft((prev) => ({ ...prev, approvalMode: "ask" }))} title={t("heartbeat.approvalModeAskTooltip")}>
              {t("heartbeat.approvalModeAsk")}
            </button>
            <button className={`set-seg__btn${normalizeMode(draft.approvalMode) === "auto" ? " set-seg__btn--on" : ""}`} onClick={() => setDraft((prev) => ({ ...prev, approvalMode: "auto" }))} title={t("heartbeat.approvalModeAutoTooltip")}>
              {t("heartbeat.approvalModeAuto")}
            </button>
            <button className={`set-seg__btn${normalizeMode(draft.approvalMode) === "yolo" ? " set-seg__btn--on" : ""}`} onClick={() => setDraft((prev) => ({ ...prev, approvalMode: "yolo" }))} title={t("heartbeat.approvalModeYoloTooltip")}>
              {t("heartbeat.approvalModeYolo")}
            </button>
          </div>
          <span className="heartbeat-editor__mode-hint">
            {normalizeMode(draft.approvalMode) === "yolo" ? t("heartbeat.approvalModeYoloHint") :
            normalizeMode(draft.approvalMode) === "auto" ? t("heartbeat.approvalModeAutoHint") :
                t("heartbeat.approvalModeAskHint")}
          </span>
        </div>

        {/* Push to bot channels */}
        <div className="heartbeat-editor__field" style={{ flex: "1 1 45%", minWidth: "200px", textAlign: "left" }}>
          <label>{t("heartbeat.notifyChannels")} <span className="heartbeat-editor__optional">{t("heartbeat.optional")}</span></label>
          <div className="set-seg" style={{ alignSelf: "flex-start" }}>
            <button className={`set-seg__btn${draft.notifyChannels === true ? " set-seg__btn--on" : ""}`} onClick={() => setDraft((prev) => ({ ...prev, notifyChannels: true }))}>
              {t("heartbeat.notifyChannelsOn")}
            </button>
            <button className={`set-seg__btn${draft.notifyChannels !== true ? " set-seg__btn--on" : ""}`} onClick={() => setDraft((prev) => ({ ...prev, notifyChannels: false }))}>
              {t("heartbeat.notifyChannelsOff")}
            </button>
          </div>
          <span className="heartbeat-editor__mode-hint">
            {draft.notifyChannels === true
            ? t("heartbeat.notifyChannelsOnHint")
            : t("heartbeat.notifyChannelsOffHint")}
          </span>
        </div>
      </div>

      {/* New conversation per run */}
      <div className="heartbeat-editor__field">
        <label>{t("heartbeat.fieldNewConversation")}</label>
        <div className="set-seg" style={{ alignSelf: "flex-start" }}>
          <button className={`set-seg__btn${!draft.newConversationEachRun ? " set-seg__btn--on" : ""}`} onClick={() => setDraft((prev) => ({ ...prev, newConversationEachRun: false }))}>
            {t("heartbeat.newConversationEachRunOff")}
          </button>
          <button className={`set-seg__btn${draft.newConversationEachRun ? " set-seg__btn--on" : ""}`} onClick={() => setDraft((prev) => ({ ...prev, newConversationEachRun: true }))}>
            {t("heartbeat.newConversationEachRunOn")}
          </button>
        </div>
      </div>

      {/* Frequency */}
      <div className="heartbeat-editor__field">
        <label>{t("heartbeat.fieldInterval")}</label>
        <div className="set-seg" style={{ alignSelf: "flex-start" }}>
          <button className={`set-seg__btn${freqType === "cycle" ? " set-seg__btn--on" : ""}`} onClick={() => {
            setFreqType("cycle");
            // Save the original interval so switching back can restore it
            const cur = draft.interval || "";
            const nextInterval = cur.includes("|") ? cur : "24h|daily@09:00";
            if (!cur.includes("|")) {
                intervalBeforeCycle.current = cur;
            }
            setDraft((prev) => ({ ...prev, interval: nextInterval, timeWindowStart: undefined, timeWindowEnd: undefined }));
        }}>
            {t("heartbeat.freqCycle")}
          </button>
          <button className={`set-seg__btn${freqType === "interval" ? " set-seg__btn--on" : ""}`} onClick={() => {
            setFreqType("interval");
            // Restore original interval if user toggled cycle and back without saving
            if (intervalBeforeCycle.current !== null) {
                setDraft((prev) => ({ ...prev, interval: intervalBeforeCycle.current! }));
                intervalBeforeCycle.current = null;
            }
            else if ((draft.interval || "").includes("|")) {
                // Fallback: strip cycle suffix
                setDraft((prev) => ({ ...prev, interval: (prev.interval || "").replace(/\|.*$/, "") }));
            }
        }}>
            {t("heartbeat.freqInterval")}
          </button>
        </div>

        {freqType === "cycle" ? <CycleEditor draft={draft} setDraft={set}/> : (<div className="heartbeat-editor__freq-interval">
            <span className="heartbeat-editor__freq-label">{t("heartbeat.freqEvery")}</span>
            <input className="heartbeat-editor__freq-input" value={(() => {
                const m = (draft.interval || "").match(/^(\d+)/);
                return m ? m[1] : "1";
            })()} onChange={(e) => {
                const num = e.target.value.replace(/\D/g, "");
                const mUnit = (draft.interval || "").match(/^(\d+)([smh])/);
                const unit = mUnit ? mUnit[2] : "h";
                // Guard: never save a bare unit string like "h" or "m"
                setDraft((prev) => ({ ...prev, interval: num ? num + unit : "1" + unit }));
            }} placeholder="1"/>
            <select className="heartbeat-editor__freq-select" value={(() => {
                const m = (draft.interval || "").match(/^(\d+)([smh])/);
                return m ? m[2] : "h";
            })()} onChange={(e) => {
                const num = (draft.interval || "").match(/^(\d+)/)?.[1] || "1";
                setDraft((prev) => ({ ...prev, interval: num + e.target.value }));
            }}>
              <option value="m">{t("heartbeat.unitMin")}</option>
              <option value="h">{t("heartbeat.unitHour")}</option>
            </select>
            <span className="heartbeat-editor__freq-label" style={{ marginLeft: "6px" }}>
              {draft.timeWindowStart || draft.timeWindowEnd ? (<>{t("heartbeat.timeWindow")}</>) : (<span className="heartbeat-editor__tw-add" onClick={() => setDraft((prev) => ({ ...prev, timeWindowStart: "09:00", timeWindowEnd: "17:00" }))}>
                  + {t("heartbeat.timeWindow")}
                </span>)}
            </span>
            {(draft.timeWindowStart || draft.timeWindowEnd) && (<>
                <input className="heartbeat-editor__freq-input heartbeat-editor__freq-input--time" type="time" value={draft.timeWindowStart || ""} onChange={(e) => setDraft((prev) => ({ ...prev, timeWindowStart: e.target.value || undefined }))} placeholder="09:00"/>
                <span className="heartbeat-editor__freq-label heartbeat-editor__tw-sep">—</span>
                <input className="heartbeat-editor__freq-input heartbeat-editor__freq-input--time" type="time" value={draft.timeWindowEnd || ""} onChange={(e) => setDraft((prev) => ({ ...prev, timeWindowEnd: e.target.value || undefined }))} placeholder="17:00"/>
                <button className="heartbeat-card__open-btn heartbeat-editor__tw-clear" onClick={() => setDraft((prev) => ({ ...prev, timeWindowStart: undefined, timeWindowEnd: undefined }))} title={t("heartbeat.clearTimeWindow")}>
                  ×
                </button>
              </>)}
          </div>)}
      </div>

      </div>

      {/* Actions */}
      <div className="heartbeat-editor__actions">
        {!isNew && !confirmingDelete && (<button className="heartbeat-btn heartbeat-btn--danger" onClick={() => setConfirmingDelete(true)} style={{ marginRight: "auto" }}>
            <Trash2 size={13}/>
            {t("heartbeat.delete")}
          </button>)}
        {!isNew && confirmingDelete && (<span className="heartbeat-editor__confirm-del" style={{ marginRight: "auto" }}>
            <span>{t("heartbeat.confirmDelete")}</span>
            <button className="heartbeat-btn heartbeat-btn--danger" onClick={onDelete}>
              {t("common.delete")}
            </button>
            <button className="heartbeat-btn" onClick={() => setConfirmingDelete(false)}>
              {t("common.cancel")}
            </button>
          </span>)}
        <button className="heartbeat-btn heartbeat-btn--primary" onClick={() => onSave(draft)} disabled={!draft.title.trim() || !draft.prompt.trim() || !isDirty} title={!draft.title.trim() || !draft.prompt.trim() ? t("heartbeat.requiredFields") : !isDirty ? t("heartbeat.noChanges") : undefined}>
          {isNew ? t("heartbeat.add") : t("heartbeat.save")}
        </button>
        <button className="heartbeat-btn" onClick={onCancel}>
          {t("common.cancel")}
        </button>
      </div>
    </div>);
}

