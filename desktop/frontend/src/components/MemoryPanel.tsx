import { ChevronDown, ChevronRight, FileText, Search, Trash2 } from "lucide-react";
import { useMemo, useRef, useState, type ReactNode } from "react";
import { useT } from "../lib/i18n";
import type { MemoryView } from "../lib/types";
import { ResizableDrawer } from "./ResizableDrawer";
import { Tooltip } from "./Tooltip";
import { ModalCloseButton } from "./ModalCloseButton";
import { displayTitle, memoryFactKey, memoryMatches, ArchivedMemoryList, uniqueLinks, memoryScopeLabel, MemoryFactScope, memoryTypeLabel, memoryDocTitle, errorMessage } from "./memory_panel_helpers";
export { displayTitle as displayTitle } from "./memory_panel_helpers";
export { memoryFactKey as memoryFactKey } from "./memory_panel_helpers";
export { formatMemoryTime as formatMemoryTime } from "./memory_panel_helpers";
export { freshnessLabel as freshnessLabel } from "./memory_panel_helpers";
export { memoryMatches as memoryMatches } from "./memory_panel_helpers";
export { ArchivedMemoryList as ArchivedMemoryList } from "./memory_panel_helpers";
export { uniqueLinks as uniqueLinks } from "./memory_panel_helpers";
export { memoryScopeLabel as memoryScopeLabel } from "./memory_panel_helpers";
export { MemoryFactScope as MemoryFactScope } from "./memory_panel_helpers";
export { memoryTypeLabel as memoryTypeLabel } from "./memory_panel_helpers";
export { memoryDocTitle as memoryDocTitle } from "./memory_panel_helpers";
export { memoryDocHint as memoryDocHint } from "./memory_panel_helpers";
export { errorMessage as errorMessage } from "./memory_panel_helpers";
export { suggestionTotal as suggestionTotal } from "./memory_panel_helpers";
export { suggestionStamp as suggestionStamp } from "./memory_panel_helpers";
// MemoryPanel is the desktop memory manager: a right-side drawer over the loaded
// REASONIX.md hierarchy and saved auto-memories. Unlike Claude Code's /memory
// (which shells out to $EDITOR) it edits docs in place, and unlike Codex (no UI
// at all) it shows the saved facts. Docs are editable; facts are read-only
// (the model owns them via the `remember` tool). Quick-add mirrors the "#"
// shortcut with an explicit scope selector.
export function MemoryPanel({ view, onClose, onRemember, onForget, onSaveDoc, }: {
    view: MemoryView | null;
    onClose: () => void;
    onRemember: (scope: string, note: string) => Promise<void> | void;
    onForget: (name: string) => Promise<void> | void;
    onSaveDoc: (path: string, body: string) => Promise<void> | void;
}) {
    const t = useT();
    const [note, setNote] = useState("");
    const [scope, setScope] = useState("");
    const [editingPath, setEditingPath] = useState<string | null>(null);
    const [draft, setDraft] = useState("");
    const [busy, setBusy] = useState(false);
    const [highlight, setHighlight] = useState<string | null>(null);
    const [query, setQuery] = useState("");
    const [typeFilter, setTypeFilter] = useState("all");
    const [expanded, setExpanded] = useState<string | null>(null);
    const [expandedArchive, setExpandedArchive] = useState<string | null>(null);
    const [confirmForget, setConfirmForget] = useState<string | null>(null);
    const [error, setError] = useState<string | null>(null);
    const factRefs = useRef<Record<string, HTMLElement | null>>({});
    // Filter input — a single substring search across docs and facts. The
    // substring is case-insensitive and matches anywhere in the body or the
    // path; an empty string shows everything. The filter is purely frontend
    // (no kernel round-trip) so it's instant and reversible.
    const [filter, setFilter] = useState("");
    const facts = view?.facts ?? [];
    const archives = view?.archives ?? [];
    const factNames = useMemo(() => new Set(facts.map((f) => f.name)), [facts]);
    const factTypes = useMemo(() => Array.from(new Set([...facts, ...archives].map((f) => f.type).filter(Boolean))).sort(), [facts, archives]);
    const normalizedQuery = query.trim().toLowerCase();
    const normalizedFilter = filter.trim().toLowerCase();
    const filteredFacts = useMemo(() => facts.filter((f) => {
        if (normalizedFilter) {
            const hay = [f.name, f.description, f.body].join(" ").toLowerCase();
            if (!hay.includes(normalizedFilter))
                return false;
        }
        return memoryMatches(f, normalizedQuery, typeFilter);
    }), [facts, normalizedQuery, normalizedFilter, typeFilter]);
    const filteredArchives = useMemo(() => archives.filter((f) => {
        if (normalizedFilter) {
            const hay = [f.name, f.description, f.body, f.path].join(" ").toLowerCase();
            if (!hay.includes(normalizedFilter))
                return false;
        }
        return memoryMatches(f, normalizedQuery, typeFilter);
    }), [archives, normalizedQuery, normalizedFilter, typeFilter]);
    const scrollToFact = (key: string) => {
        const el = factRefs.current[key];
        if (!el)
            return;
        el.scrollIntoView({ block: "center", behavior: "auto" });
        setHighlight(key);
        window.setTimeout(() => setHighlight((h) => (h === key ? null : h)), 1200);
    };
    // Clear active filters when the target is hidden, else the [[link]] is a silent no-op.
    const jumpTo = (name: string) => {
        if (!factNames.has(name))
            return;
        const target = facts.find((f) => f.name === name && f.scope === "project") ?? facts.find((f) => f.name === name);
        if (!target)
            return;
        const key = memoryFactKey(target);
        const visible = filteredFacts.some((f) => memoryFactKey(f) === key);
        setExpanded(key);
        setConfirmForget(null);
        if (!visible) {
            setQuery("");
            setTypeFilter("all");
            window.setTimeout(() => scrollToFact(key), 0);
            return;
        }
        scrollToFact(key);
    };
    // renderWithLinks turns [[name]] tokens into in-panel jumps; a token with no
    // matching saved memory renders as a flagged dead link.
    const renderWithLinks = (text: string): ReactNode[] => {
        const out: ReactNode[] = [];
        const re = /\[\[([^\]]+)\]\]/g;
        let last = 0;
        let k = 0;
        let m: RegExpExecArray | null;
        while ((m = re.exec(text)) !== null) {
            if (m.index > last)
                out.push(text.slice(last, m.index));
            const target = m[1].trim();
            out.push(factNames.has(target) ? (<button key={k++} type="button" className="mem-link" onClick={() => jumpTo(target)}>
            {target}
          </button>) : (<Tooltip key={k++} label={t("memory.deadLink", { name: target })}>
            <span className="mem-link mem-link--dead">{target}</span>
          </Tooltip>));
            last = re.lastIndex;
        }
        if (last < text.length)
            out.push(text.slice(last));
        return out;
    };
    const forgetFact = async (ref: string) => {
        if (busy)
            return;
        setBusy(true);
        setError(null);
        try {
            await onForget(ref);
            if (expanded === ref)
                setExpanded(null);
            setConfirmForget(null);
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    };
    const filteredDocs = useMemo(() => {
        if (!view)
            return [];
        const q = filter.trim().toLowerCase();
        if (!q)
            return view.docs;
        return view.docs.filter((d) => d.body.toLowerCase().includes(q) || d.path.toLowerCase().includes(q));
    }, [view, filter]);
    const scopes = view?.scopes ?? [];
    // Default the scope selector to "project" when present, else the first option.
    const activeScope = scope || scopes.find((s) => s.scope === "project")?.scope || scopes[0]?.scope || "project";
    const submitNote = async () => {
        const trimmed = note.trim();
        if (!trimmed || busy)
            return;
        setBusy(true);
        setError(null);
        try {
            await onRemember(activeScope, trimmed);
            setNote("");
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    };
    const startEdit = (path: string, body: string) => {
        setEditingPath(path);
        setDraft(body);
    };
    const saveEdit = async () => {
        if (editingPath === null || busy)
            return;
        setBusy(true);
        setError(null);
        try {
            await onSaveDoc(editingPath, draft);
            setEditingPath(null);
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    };
    return (<ResizableDrawer onClose={onClose}>
        <header className="drawer__head">
          <div>
            <div className="drawer__title">{t("memory.title")}</div>
            {view?.available && (<div className="drawer__summary">
                {t("memory.summary", { facts: facts.length, archives: archives.length, docs: view.docs.length })}
              </div>)}
          </div>
          <ModalCloseButton label={t("common.close")} onClick={onClose}/>
        </header>

        {!view?.available ? (<div className="empty">{t("memory.unavailable")}</div>) : (<div className="drawer__body">
            {/* Saved auto-memories — the model owns these via remember/forget;
                the panel can delete one and follow [[name]] cross-links. */}
            <section className="mem-section">
              <div className="mem-section__row">
                <div>
                  <div className="mem-section__title">{t("memory.savedMemories")}</div>
                  <div className="mem-note">{t("memory.fallibleNote")}</div>
                </div>
                <span className="mem-count">{facts.length}</span>
              </div>
              <div className="mem-toolbar">
                <label className="mem-search">
                  <Search size={14}/>
                  <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("memory.searchPlaceholder")}/>
                </label>
                <div className="mem-filter" role="tablist" aria-label={t("memory.typeFilter")}>
                  <button className={`mem-filter__item${typeFilter === "all" ? " mem-filter__item--on" : ""}`} onClick={() => setTypeFilter("all")} type="button">
                    {t("memory.allTypes")}
                  </button>
                  {factTypes.map((type) => (<button className={`mem-filter__item${typeFilter === type ? " mem-filter__item--on" : ""}`} onClick={() => setTypeFilter(type)} type="button" key={type}>
                      {memoryTypeLabel(type, t)}
                    </button>))}
                </div>
              </div>
              {error && <div className="mem-error" role="alert">{error}</div>}
              {facts.length === 0 ? (<div className="mem-empty">{t("memory.noFacts")}</div>) : filteredFacts.length === 0 ? (<div className="mem-empty">
                  {t("memory.noMatches")}
                  <button className="mem-empty__action" onClick={() => {
                    setQuery("");
                    setTypeFilter("all");
                }} type="button">
                    {t("memory.clearFilters")}
                  </button>
                </div>) : (<div className="mem-facts">
                  {filteredFacts.map((f) => {
                    const key = memoryFactKey(f);
                    const isOpen = expanded === key;
                    const links = uniqueLinks(f.body, factNames);
                    const missing = links.filter((link) => !link.exists);
                    return (<article className={`mem-fact${highlight === key ? " mem-fact--hl" : ""}`} data-mem-type={f.type || "other"} key={key} ref={(el) => {
                            factRefs.current[key] = el;
                        }}>
                        <button className="mem-fact__summary" onClick={() => {
                            setExpanded(isOpen ? null : key);
                            setConfirmForget(null);
                        }} type="button">
                          {isOpen ? <ChevronDown size={15}/> : <ChevronRight size={15}/>}
                          <span className="mem-fact__main">
                            <span className="mem-fact__title">{displayTitle(f)}</span>
                            <span className="mem-fact__meta">
                              <MemoryFactScope scope={f.scope} t={t}/>
                              {f.type && <span className="mem-fact__type" data-mem-type={f.type}>{memoryTypeLabel(f.type, t)}</span>}
                              <span className="mem-fact__slug">{f.name}</span>
                            </span>
                            <span className="mem-fact__desc">{f.description}</span>
                          </span>
                        </button>
                        {links.length > 0 && (<div className="mem-fact__links" aria-label={t("memory.links")}>
                            {links.map((link) => link.exists ? (<button className="mem-link-chip" key={link.name} onClick={() => jumpTo(link.name)} type="button">
                                  [[{link.name}]]
                                </button>) : (<Tooltip key={link.name} label={t("memory.deadLink", { name: link.name })}>
                                  <span className="mem-link-chip mem-link-chip--dead">[[{link.name}]]</span>
                                </Tooltip>))}
                          </div>)}
                        {isOpen && (<div className="mem-fact__detail">
                            {f.body ? (<div className="mem-fact__body">{renderWithLinks(f.body)}</div>) : (<div className="mem-empty">{t("memory.noBody")}</div>)}
                            {missing.length > 0 && (<div className="mem-deadline">
                                {t("memory.missingLinks", { n: missing.length })}
                              </div>)}
                            <div className="mem-fact__actions">
                              <span className="mem-hint mem-hint--inline">
                                {t("memory.appliesNow")}
                              </span>
                              {confirmForget === key ? (<div className="mem-confirm">
                                  <button className="btn btn--small" onClick={() => setConfirmForget(null)} disabled={busy} type="button">
                                    {t("common.cancel")}
                                  </button>
                                  <button className="btn btn--small mem-danger" onClick={() => void forgetFact(f.id || f.name)} disabled={busy} type="button">
                                    {t("memory.confirmForget")}
                                  </button>
                                </div>) : (<button className="btn btn--small mem-fact__forget" onClick={() => setConfirmForget(key)} disabled={busy} type="button">
                                  <Trash2 size={13}/>
                                  {t("memory.forget")}
                                </button>)}
                            </div>
                          </div>)}
                      </article>);
                })}
                </div>)}
              {(view.storeDir || view.storeGlobalDir) && (<div className="mem-hint">{t("memory.storedUnder", { dir: [view.storeDir, view.storeGlobalDir].filter(Boolean).join(" + ") })}</div>)}
            </section>

            {archives.length > 0 && <section className="mem-section">
              <ArchivedMemoryList archives={filteredArchives} totalArchives={archives.length} expanded={expandedArchive} setExpanded={setExpandedArchive} renderWithLinks={renderWithLinks} t={t}/>
            </section>}

            {/* Quick-add: scope selector + note, mirroring the "#" shortcut. */}
            <section className="mem-section">
              <div className="mem-section__title">{t("memory.quickAdd")}</div>
              <div className="mem-add">
                <Tooltip label={t("memory.whereToSave")}>
                  <select className="mem-select" value={activeScope} onChange={(e) => setScope(e.target.value)}>
                    {scopes.map((s) => (<option key={s.scope} value={s.scope}>
                        {s.scope}
                      </option>))}
                  </select>
                </Tooltip>
                <input className="mem-input" placeholder={t("memory.notePlaceholder")} value={note} onChange={(e) => setNote(e.target.value)} onKeyDown={(e) => {
                if (e.key === "Enter")
                    void submitNote();
            }}/>
                <button className="btn btn--primary btn--small" onClick={() => void submitNote()} disabled={busy || !note.trim()}>
                  {t("memory.remember")}
                </button>
              </div>
              <div className="mem-hint">
                {scopes.find((s) => s.scope === activeScope)?.path}
              </div>
            </section>

            {/* Doc files — editable in place. */}
            <section className="mem-section">
              <div className="mem-section__title">{t("memory.instructionFiles")}</div>
              <input className="mem-input mem-filter" placeholder={t("memory.filterPlaceholder")} value={filter} onChange={(e) => setFilter(e.target.value)} spellCheck={false} aria-label={t("memory.filterPlaceholder")}/>
              {filteredDocs.length === 0 && (<div className="mem-empty">{filter ? t("memory.noFilterMatch") : t("memory.noDocs")}</div>)}
              {filteredDocs.map((d) => {
                const editing = editingPath === d.path;
                return (<div className="mem-doc" data-doc-scope={d.scope || "other"} key={d.path}>
                    <div className="mem-doc__head">
                      <span className="mem-doc__icon"><FileText size={15}/></span>
                      <span className="mem-doc__info">
                        <span className="mem-doc__name">{memoryDocTitle(d.scope, t)}</span>
                        <span className="mem-doc__path">{d.path}</span>
                      </span>
                      <span className={`mem-doc__tag badge--${d.scope}`}>{memoryScopeLabel(d.scope, t)}</span>
                      {!editing && (<button className="btn btn--small" onClick={() => startEdit(d.path, d.body)}>
                          {t("common.edit")}
                        </button>)}
                    </div>
                    {editing ? (<div className="mem-doc__edit">
                        <textarea className="mem-textarea" value={draft} onChange={(e) => setDraft(e.target.value)} spellCheck={false}/>
                        <div className="mem-doc__actions">
                          <button className="btn btn--small" onClick={() => setEditingPath(null)} disabled={busy}>
                            {t("common.cancel")}
                          </button>
                          <button className="btn btn--primary btn--small" onClick={() => void saveEdit()} disabled={busy}>
                            {t("common.save")}
                          </button>
                        </div>
                      </div>) : (<pre className="mem-doc__body">{d.body}</pre>)}
                  </div>);
            })}
            </section>



            {/* Saved auto-memories — read-only; the model owns these. */}
            <section className="mem-section">
              <div className="mem-section__title">{t("memory.savedMemories")}</div>
              {filteredFacts.length === 0 ? (<div className="mem-empty">{filter ? t("memory.noFilterMatch") : t("memory.noFacts")}</div>) : (filteredFacts.map((f) => (<div className="mem-fact" key={memoryFactKey(f)} title={f.body}>
                    <span className={`badge badge--${f.scope}`}>{memoryScopeLabel(f.scope, t)}</span>
                    <span className={`badge badge--${f.type}`}>{memoryTypeLabel(f.type, t)}</span>
                    <div className="mem-fact__text">
                      <div className="mem-fact__name">{f.name}</div>
                      <div className="mem-fact__desc">{f.description}</div>
                    </div>
                  </div>)))}
              {(view.storeDir || view.storeGlobalDir) && (<div className="mem-hint" title={[view.storeDir, view.storeGlobalDir].filter(Boolean).join(" + ")}>
                  {t("memory.storedUnder", { dir: [view.storeDir, view.storeGlobalDir].filter(Boolean).join(" + ") })}
                </div>)}
            </section>
          </div>)}
    </ResizableDrawer>);
}
export { MemorySettingsPage as MemorySettingsPage } from "./memory_settings_page";

