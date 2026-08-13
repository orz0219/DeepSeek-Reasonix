import { useState } from "react";
import { ChevronDown, Plus, RefreshCw } from "lucide-react";
import { useT } from "../lib/i18n";
import type { SkillRootSkillView, SkillRootView } from "../lib/types";
import { skillScopeLabel } from "./CapabilitiesPanel";
import { skillSourceSummary } from "./capabilities_helpers";
export function SkillSources({ roots, busy, onAdd, onRefresh, onToggle, }: {
    roots: SkillRootView[];
    busy: boolean;
    onAdd: () => void;
    onRefresh: () => void;
    onToggle: (path: string, enabled: boolean) => void;
}) {
    const t = useT();
    // Sources are a core part of the Skills page, so expose them on first visit.
    // Users can still collapse the section when they need more room for the list.
    const [expanded, setExpanded] = useState(true);
    const [expandedRootSkills, setExpandedRootSkills] = useState<Set<string>>(() => new Set());
    const [fullRootSkills, setFullRootSkills] = useState<Set<string>>(() => new Set());
    const primaryRoots = roots.filter(isPrimarySkillRoot);
    const enabledRoots = primaryRoots.filter((root) => root.enabled !== false && root.status !== "disabled");
    const disabledRoots = primaryRoots.filter((root) => root.enabled === false || root.status === "disabled");
    const shownRoots = [
        ...enabledRoots,
        ...disabledRoots,
    ];
    const summaryRoots = roots;
    const active = summaryRoots.filter((root) => root.skills > 0).length;
    const missing = summaryRoots.filter((root) => root.status === "missing").length;
    const empty = summaryRoots.filter((root) => root.status === "ok" && root.skills === 0).length;
    const disabled = summaryRoots.filter((root) => root.enabled === false || root.status === "disabled").length;
    const toggleRootSkills = (key: string) => {
        setExpandedRootSkills((prev) => {
            const next = new Set(prev);
            if (next.has(key))
                next.delete(key);
            else
                next.add(key);
            return next;
        });
    };
    const toggleRootSkillFull = (key: string) => {
        setFullRootSkills((prev) => {
            const next = new Set(prev);
            if (next.has(key))
                next.delete(key);
            else
                next.add(key);
            return next;
        });
    };
    return (<div className={`cap-sources${expanded ? " cap-sources--expanded" : ""}`}>
      <button className="cap-sources__head" type="button" onClick={() => setExpanded((value) => !value)} aria-expanded={expanded}>
        <span className="cap-sources__copy">
          <span className="cap-sources__title">{t("caps.sources")}</span>
          <span className="cap-sources__summary">{skillSourceSummary(active, missing, empty, disabled, t)}</span>
        </span>
        <ChevronDown className={`cap-sources__chevron${expanded ? " cap-sources__chevron--expanded" : ""}`} aria-hidden size={16}/>
      </button>
      {expanded && (<>
          <div className="cap-sources__manage">
            <div className="cap-sources__manage-actions">
              <button className="btn btn--small" disabled={busy} onClick={onRefresh}>
                <RefreshCw aria-hidden size={13}/>
                {t("caps.refreshSkills")}
              </button>
              <button className="btn btn--small" disabled={busy} onClick={onAdd}>
                <Plus aria-hidden size={13}/>
                {t("caps.addSkillFolder")}
              </button>
            </div>
            <button className="btn btn--small" type="button" onClick={() => {
                setExpanded(false);
            }} aria-expanded={expanded}>
              {t("common.collapse")}
            </button>
          </div>
          {roots.length === 0 ? (<div className="mem-empty">{t("caps.noSkillRoots")}</div>) : shownRoots.length > 0 ? (<div className="cap-source-list">
              {shownRoots.map((root) => {
                    const key = skillRootKey(root);
                    const rootSkills = root.skillItems ?? [];
                    const rootSkillsExpanded = expandedRootSkills.has(key);
                    const rootSkillsFull = fullRootSkills.has(key);
                    const canShowRootSkills = rootSkills.length > 0;
                    return (<div className={`cap-source cap-source--${skillRootTone(root)}`} key={key}>
                    <span className={`cap-dot cap-dot--${skillRootDot(root)}`}/>
                    <div className="cap-source__text">
                      <div className="cap-source__head">
                        <div className="cap-source__label" title={root.dir}>
                          {skillRootLabel(root)}
                        </div>
                        <div className="cap-source__badges">
                          {skillRootBadges(root, t).map((badge) => (<span className={`cap-source-badge cap-source-badge--${badge.tone}`} key={badge.label}>
                              {badge.label}
                            </span>))}
                        </div>
                      </div>
                      <div className="cap-source__meta">
                        <span>{skillRootStatus(root, t)}</span>
                        <span>{t("caps.skillRootCount", { skills: root.skills })}</span>
                        {root.configured && <span>{t("caps.skillRootConfigured")}</span>}
                      </div>
                      {canShowRootSkills && (<div className="cap-source-actions">
                          <button className="btn btn--small" disabled={busy} type="button" aria-expanded={rootSkillsExpanded} onClick={() => toggleRootSkills(key)}>
                            {rootSkillsExpanded ? t("caps.hideSkills") : t("caps.showSkills")}
                          </button>
                        </div>)}
                      {rootSkillsExpanded && rootSkills.length > 0 && (<SkillRootSkillsList skills={rootSkills} showAll={rootSkillsFull} onToggleAll={() => toggleRootSkillFull(key)}/>)}
                      {root.warning && <div className="cap-source__warning">{root.warning}</div>}
                    </div>
                    <div className="cap-source__side">
                      {root.removable && (<input className="provider-capability-row__switch cap-source__switch" type="checkbox" role="switch" checked={root.enabled !== false} disabled={busy} aria-label={`${root.enabled === false ? t("caps.skillRootEnable") : t("caps.skillRootDisable")} ${root.dir}`} onChange={(event) => onToggle(root.dir, event.currentTarget.checked)}/>)}
                    </div>
                  </div>);
                })}
            </div>) : null}
        </>)}
    </div>);
}
const skillRootPreviewLimit = 5;
function SkillRootSkillsList({ skills, showAll, onToggleAll, }: {
    skills: SkillRootSkillView[];
    showAll: boolean;
    onToggleAll: () => void;
}) {
    const t = useT();
    const visible = showAll ? skills : skills.slice(0, skillRootPreviewLimit);
    return (<div className="cap-source-skills">
      {visible.map((skill) => (<div className="cap-source-skill" key={`${skill.scope}:${skill.invocation || skill.name}`}>
          <div className="cap-source-skill__head">
            <span className="cap-source-skill__name">{skill.invocation || `/${skill.name}`}</span>
            <span className="cap-source-skill__badges">
              <span className={`cap-skill-badge cap-skill-badge--${skill.scope}`}>{skillScopeLabel(skill.scope, t)}</span>
              {skill.plugin && <span className="cap-skill-badge">{t("slash.plugin", { name: skill.plugin })}</span>}
              {skill.runAs === "subagent" && <span className="cap-skill-badge cap-skill-badge--run">{t("caps.subagent")}</span>}
            </span>
          </div>
          {skill.description && <div className="cap-source-skill__desc">{skill.description}</div>}
        </div>))}
      {skills.length > skillRootPreviewLimit && (<button className="cap-source-skills__more" type="button" onClick={onToggleAll}>
          {showAll ? t("common.collapse") : t("caps.skillRootShowAllSkills", { count: skills.length })}
        </button>)}
    </div>);
}
function skillRootKey(root: SkillRootView): string {
    return `${root.scope}:${root.priority}:${root.dir}`;
}
function isPrimarySkillRoot(root: SkillRootView): boolean {
    return root.skills > 0 || root.configured || root.status === "disabled" || Boolean(root.warning);
}
function skillRootTone(root: SkillRootView): "active" | "empty" | "problem" {
    if (root.warning || root.status === "inactive" || root.status === "missing" || root.status === "unreadable")
        return "problem";
    if (root.skills > 0)
        return "active";
    return "empty";
}
function skillRootDot(root: SkillRootView): "connected" | "disabled" | "failed" {
    const tone = skillRootTone(root);
    if (tone === "active")
        return "connected";
    if (tone === "empty")
        return "disabled";
    return "failed";
}
function skillRootStatus(root: SkillRootView, t: ReturnType<typeof useT>): string {
    if (root.status === "disabled")
        return t("caps.skillRootDisabled");
    if (root.status === "ok" && root.skills > 0)
        return t("caps.skillRootActive");
    if (root.status === "ok")
        return t("caps.skillRootEmpty");
    if (root.status === "missing")
        return t("caps.skillRootMissing");
    return root.status;
}
function skillRootLabel(root: SkillRootView): string {
    return root.dir;
}
function skillRootBadges(root: SkillRootView, t: ReturnType<typeof useT>): Array<{
    label: string;
    tone: "scope" | "builtin" | "configured" | "missing";
}> {
    const badges: Array<{
        label: string;
        tone: "scope" | "builtin" | "configured" | "missing";
    }> = [
        { label: skillScopeLabel(root.scope, t), tone: "scope" },
        root.scope === "custom"
            ? { label: root.configured ? t("caps.skillRootUserConfigured") : t("caps.skillRootConfiguredPath"), tone: "configured" }
            : { label: t("caps.skillRootBuiltinPath"), tone: "builtin" },
    ];
    if (root.status === "missing") {
        badges.push({ label: t("caps.skillRootMissing"), tone: "missing" });
    }
    return badges;
}

