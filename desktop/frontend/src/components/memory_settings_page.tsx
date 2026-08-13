import { Activity, AlertTriangle, ArchiveRestore, Check, ChevronDown, ChevronRight, FileText, History, Pencil, Plus, Search, Sparkles, Trash2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import type { MemoryArchive, MemoryFact, MemorySuggestion, MemorySuggestionsView, MemoryView, SkillSuggestion, TabMeta } from "../lib/types";
import { AnchoredPopover } from "./AnchoredPopover";
import { Tooltip } from "./Tooltip";
import { MemorySuggestionsSection } from "./memory_suggestions_panel";
import { displayTitle, memoryFactKey, formatMemoryTime, freshnessLabel, memoryMatches, ArchivedMemoryList, uniqueLinks, memoryScopeLabel, MemoryFactScope, memoryTypeLabel, memoryDocTitle, memoryDocHint, errorMessage, suggestionTotal } from "./memory_panel_helpers";
// MemorySettingsPage is a self-contained memory management page embedded inside
// the settings centre. It loads its own data and handles all memory operations.
export function MemorySettingsPage() {
    const t = useT();
    const [view, setView] = useState<MemoryView | null>(null);
    const [tabs, setTabs] = useState<TabMeta[]>([]);
    const [selectedTabId, setSelectedTabId] = useState<string | null>(null);
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
    const [expandedDoc, setExpandedDoc] = useState<string | null>(null);
    const [confirmForget, setConfirmForget] = useState<string | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [tab, setTab] = useState<"saved" | "archived" | "docs" | "activity" | "suggestions">("saved");
    const [showAdd, setShowAdd] = useState(false);
    const [showStorage, setShowStorage] = useState(false);
    const [suggestions, setSuggestions] = useState<MemorySuggestionsView | null>(null);
    const [suggestionBusy, setSuggestionBusy] = useState(false);
    const [expandedSuggestion, setExpandedSuggestion] = useState<string | null>(null);
    const [acceptedSuggestions, setAcceptedSuggestions] = useState<Record<string, string>>({});
    const [revisions, setRevisions] = useState<Record<string, MemoryFact[]>>({});
    const [revisionBusy, setRevisionBusy] = useState<string | null>(null);
    const factRefs = useRef<Record<string, HTMLElement | null>>({});
    useEffect(() => {
        app.ListTabs().then((tabList) => {
            setTabs(tabList);
            if (!selectedTabId) {
                const active = tabList.find((tb) => tb.active);
                if (active)
                    setSelectedTabId(active.id);
            }
        }).catch(() => { });
    }, []);
    // Deduplicate tabs by workspace: multiple conversations in the same project
    // should appear as a single entry in the memory workspace selector.
    const uniqueWorkspaceTabs = useMemo(() => {
        const byWorkspace = new Map<string, TabMeta>();
        for (const tb of tabs) {
            const key = tb.workspaceRoot || `${tb.scope}:global`;
            if (!byWorkspace.has(key))
                byWorkspace.set(key, tb);
        }
        return [...byWorkspace.values()];
    }, [tabs]);
    // Ensure selectedTabId always points to a valid entry in uniqueWorkspaceTabs.
    // On initial load the active tab is picked; if dedup removed it, fall back to first.
    const effectiveTabId = useMemo(() => {
        if (uniqueWorkspaceTabs.some((tb) => tb.id === selectedTabId))
            return selectedTabId;
        return uniqueWorkspaceTabs[0]?.id ?? null;
    }, [selectedTabId, uniqueWorkspaceTabs]);
    // Sync effectiveTabId back to selectedTabId when it changes
    useEffect(() => {
        if (effectiveTabId && effectiveTabId !== selectedTabId) {
            setSelectedTabId(effectiveTabId);
        }
    }, [effectiveTabId]);
    const reload = useCallback(async () => {
        const tabId = effectiveTabId;
        // Clear view immediately so stale data from the previous workspace
        // doesn't persist while the new workspace loads.
        setView((prev) => {
            if (prev && tabId)
                return {
                    ...prev,
                    facts: [], archives: [], docs: [], conflicts: [], instructionDiagnostics: [],
                    lastRecall: { query: "", hits: [], omitted: 0, charBudget: 0, usedChars: 0 },
                };
            return prev;
        });
        setView(tabId ? await app.MemoryForTab(tabId).catch(() => null) : await app.Memory().catch(() => null));
    }, [effectiveTabId]);
    useEffect(() => { void reload(); }, [reload]);
    useEffect(() => {
        setRevisions({});
        setExpanded(null);
        setExpandedArchive(null);
        setExpandedDoc(null);
        setSuggestions(null);
    }, [effectiveTabId]);
    // Workspace selector: custom styled dropdown matching settings-subtab height
    const wsTriggerRef = useRef<HTMLButtonElement>(null);
    const [wsOpen, setWsOpen] = useState(false);
    const selectedWs = uniqueWorkspaceTabs.find((tb) => tb.id === effectiveTabId);
    const wsSelector = uniqueWorkspaceTabs.length > 0 ? (<div className="mem-ws-select">
			{uniqueWorkspaceTabs.length > 1 ? (<>
					<button ref={wsTriggerRef} type="button" className="mem-ws-select__trigger" onClick={() => setWsOpen((v) => !v)}>
						<span className="mem-ws-select__label">{selectedWs?.workspaceName || selectedWs?.label || ""}</span>
						<ChevronDown size={13} className={"mem-ws-select__chev" + (wsOpen ? " mem-ws-select__chev--open" : "")}/>
					</button>
					<AnchoredPopover open={wsOpen} anchorRef={wsTriggerRef} onClose={() => setWsOpen(false)} className="mem-ws-select__menu" placement="bottom">
						<div className="mem-ws-select__list" role="listbox">
							{uniqueWorkspaceTabs.map((tb) => (<button key={tb.id} type="button" role="option" aria-selected={tb.id === effectiveTabId} className={"mem-ws-select__option" + (tb.id === effectiveTabId ? " mem-ws-select__option--selected" : "")} onClick={() => { setSelectedTabId(tb.id); setWsOpen(false); }}>
									<span>{tb.workspaceName || tb.label || tb.scope || tb.id}</span>
									{tb.id === effectiveTabId && <Check size={13}/>}
								</button>))}
						</div>
					</AnchoredPopover>
				</>) : (<span className="mem-ws-select__label mem-ws-select__label--single">{selectedWs?.workspaceName || selectedWs?.label || ""}</span>)}
		</div>) : null;
    const refreshSuggestions = useCallback(async () => {
        if (suggestionBusy)
            return;
        setSuggestionBusy(true);
        setError(null);
        try {
            const next = effectiveTabId
                ? await app.MemorySuggestionsForTab(effectiveTabId)
                : await app.MemorySuggestions();
            setSuggestions({
                memories: next.memories ?? [],
                skills: next.skills ?? [],
                generatedAt: next.generatedAt || "",
                available: !!next.available,
                source: next.source || "",
            });
            setAcceptedSuggestions({});
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setSuggestionBusy(false);
        }
    }, [effectiveTabId, suggestionBusy]);
    useEffect(() => {
        if (tab !== "suggestions" || suggestions || suggestionBusy)
            return;
        void refreshSuggestions();
    }, [refreshSuggestions, suggestionBusy, suggestions, tab]);
    const facts = view?.facts ?? [];
    const archives = view?.archives ?? [];
    const factNames = useMemo(() => new Set(facts.map((f) => f.name)), [facts]);
    const factTypes = useMemo(() => Array.from(new Set([...facts, ...archives].map((f) => f.type).filter(Boolean))).sort(), [facts, archives]);
    const normalizedQuery = query.trim().toLowerCase();
    const filteredFacts = useMemo(() => facts.filter((f) => memoryMatches(f, normalizedQuery, typeFilter)), [facts, normalizedQuery, typeFilter]);
    const filteredArchives = useMemo(() => archives.filter((f) => {
        if (typeFilter !== "all" && f.type !== typeFilter)
            return false;
        if (!normalizedQuery)
            return true;
        return memoryMatches(f, normalizedQuery, "all") || [f.path, f.archivedAt].join(" ").toLowerCase().includes(normalizedQuery);
    }), [archives, normalizedQuery, typeFilter]);
    const scrollToFact = useCallback((key: string) => {
        const el = factRefs.current[key];
        if (!el)
            return;
        el.scrollIntoView({ block: "center", behavior: "auto" });
        setHighlight(key);
        window.setTimeout(() => setHighlight((h) => (h === key ? null : h)), 1200);
    }, []);
    const jumpTo = useCallback((name: string) => {
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
    }, [factNames, facts, filteredFacts, scrollToFact]);
    const renderWithLinks = useCallback((text: string): ReactNode[] => {
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
    }, [factNames, jumpTo, t]);
    const forgetFact = useCallback(async (ref: string, key: string) => {
        if (busy)
            return;
        setBusy(true);
        setError(null);
        try {
            if (effectiveTabId)
                await app.ForgetForTab(effectiveTabId, ref);
            else
                await app.Forget(ref);
            await reload();
            if (expanded === key)
                setExpanded(null);
            setConfirmForget(null);
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    }, [busy, expanded, reload, effectiveTabId]);
    const loadRevisions = useCallback(async (fact: MemoryFact) => {
        const ref = fact.id || fact.name;
        const key = memoryFactKey(fact);
        if (!ref || revisions[key] || revisionBusy === key)
            return;
        setRevisionBusy(key);
        try {
            const items = effectiveTabId
                ? await app.MemoryRevisionsForTab(effectiveTabId, ref)
                : await app.MemoryRevisions(ref);
            setRevisions((prev) => ({ ...prev, [key]: items ?? [] }));
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setRevisionBusy(null);
        }
    }, [effectiveTabId, revisionBusy, revisions]);
    const restoreRevision = useCallback(async (fact: MemoryFact, revision: number) => {
        const ref = fact.id || fact.name;
        const key = memoryFactKey(fact);
        if (!ref || busy)
            return;
        setBusy(true);
        setError(null);
        try {
            if (effectiveTabId)
                await app.RestoreMemoryRevisionForTab(effectiveTabId, ref, revision);
            else
                await app.RestoreMemoryRevision(ref, revision);
            setRevisions((prev) => {
                const next = { ...prev };
                delete next[key];
                return next;
            });
            await reload();
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    }, [busy, effectiveTabId, reload]);
    const restoreArchive = useCallback(async (archive: MemoryArchive) => {
        if (busy)
            return;
        setBusy(true);
        setError(null);
        try {
            const restored = effectiveTabId
                ? await app.RestoreArchivedMemoryForTab(effectiveTabId, archive.path)
                : await app.RestoreArchivedMemory(archive.path);
            await reload();
            setExpandedArchive(null);
            setExpanded(restored.name || archive.name);
            setHighlight(restored.name || archive.name);
            setTab("saved");
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    }, [busy, effectiveTabId, reload]);
    const scopes = view?.scopes ?? [];
    const activeScope = scope || scopes.find((s) => s.scope === "project")?.scope || scopes[0]?.scope || "project";
    const submitNote = useCallback(async () => {
        const trimmed = note.trim();
        if (!trimmed || busy)
            return;
        setBusy(true);
        setError(null);
        try {
            if (effectiveTabId)
                await app.RememberForTab(effectiveTabId, activeScope, trimmed);
            else
                await app.Remember(activeScope, trimmed);
            await reload();
            setNote("");
            setShowAdd(false);
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    }, [note, busy, activeScope, reload, effectiveTabId]);
    const startEdit = useCallback((path: string, body: string) => {
        setEditingPath(path);
        setDraft(body);
    }, []);
    const saveEdit = useCallback(async () => {
        if (editingPath === null || busy)
            return;
        setBusy(true);
        setError(null);
        try {
            if (effectiveTabId)
                await app.SaveDocForTab(effectiveTabId, editingPath, draft);
            else
                await app.SaveDoc(editingPath, draft);
            await reload();
            setEditingPath(null);
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    }, [editingPath, busy, draft, reload, effectiveTabId]);
    const acceptMemorySuggestion = useCallback(async (candidate: MemorySuggestion) => {
        if (busy)
            return;
        setBusy(true);
        setError(null);
        try {
            const path = effectiveTabId
                ? await app.AcceptMemorySuggestionForTab(effectiveTabId, candidate)
                : await app.AcceptMemorySuggestion(candidate);
            setAcceptedSuggestions((prev) => ({ ...prev, [candidate.id]: path || candidate.name }));
            await reload();
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    }, [busy, reload, effectiveTabId]);
    const acceptSkillSuggestion = useCallback(async (candidate: SkillSuggestion) => {
        if (busy)
            return;
        setBusy(true);
        setError(null);
        try {
            const path = effectiveTabId
                ? await app.AcceptSkillSuggestionForTab(effectiveTabId, candidate)
                : await app.AcceptSkillSuggestion(candidate);
            setAcceptedSuggestions((prev) => ({ ...prev, [candidate.id]: path || candidate.name }));
        }
        catch (err) {
            setError(errorMessage(err));
        }
        finally {
            setBusy(false);
        }
    }, [busy, effectiveTabId]);
    if (!view?.available) {
        return (<>
				{wsSelector}
				<div className="empty">{t("memory.unavailable")}</div>
			</>);
    }
    const hasSavedFilters = facts.length > 0;
    const hasArchivedFilters = archives.length > 0;
    return (<>
			<div className="memory-overview" aria-label={t("memory.title")}>
				<div className="memory-overview__copy">
					<span>{t("memory.summarySettings", { facts: facts.length, archives: archives.length, docs: view.docs.length })}</span>
				</div>
				{view.storeDir && (<button className="memory-storage-toggle" type="button" onClick={() => setShowStorage((v) => !v)}>
						{showStorage ? t("memory.hideStorage") : t("memory.showStorage")}
					</button>)}
			</div>
			{showStorage && view.storeDir && (<div className="memory-storage-path">
					<span>{t("memory.storagePathLabel")}</span>
					<code>{view.storeDir}</code>
				</div>)}
			<div className="memory-tabs-row" role="tablist" aria-label={t("settings.tab.memory")}>
				<div className="settings-subtabs memory-tabs-row__primary" role="presentation">
					<button className={"settings-subtab" + (tab === "saved" ? " settings-subtab--active" : "")} role="tab" aria-selected={tab === "saved"} type="button" onClick={() => setTab("saved")}>
						<span>{t("memory.savedMemories")}</span>
					</button>
					<button className={"settings-subtab" + (tab === "archived" ? " settings-subtab--active" : "")} role="tab" aria-selected={tab === "archived"} type="button" onClick={() => setTab("archived")}>
						<span>{t("memory.archivedMemories")}</span>
					</button>
					<button className={"settings-subtab" + (tab === "docs" ? " settings-subtab--active" : "")} role="tab" aria-selected={tab === "docs"} type="button" onClick={() => setTab("docs")}>
						<span>{t("memory.instructionFiles")}</span>
					</button>
					<button className={"settings-subtab" + (tab === "activity" ? " settings-subtab--active" : "")} role="tab" aria-selected={tab === "activity"} type="button" onClick={() => setTab("activity")}>
						<span>{t("memory.activity")}</span>
					</button>
				</div>
				<div className="memory-tabs-row__spacer"/>
				<div className="memory-tabs-row__tail" role="presentation">
					{wsSelector}
					<button className={"memory-suggestion-tab" + (tab === "suggestions" ? " memory-suggestion-tab--active" : "")} role="tab" aria-selected={tab === "suggestions"} type="button" onClick={() => setTab("suggestions")}>
						<Sparkles size={14} aria-hidden="true"/>
						<span>{t("memory.suggestions")}</span>
						{suggestionTotal(suggestions) > 0 && <span className="settings-subtab__count">{suggestionTotal(suggestions)}</span>}
					</button>
				</div>
			</div>

			{tab === "saved" && <section className="mem-section">
				<div className="mem-section__head">
					<div>
						<div className="mem-section__title">{t("memory.savedMemories")}</div>
						<div className="mem-note">{t("memory.fallibleNote")}</div>
					</div>
				</div>
				{view.conflicts.length > 0 && (<div className="mem-context-notice" role="status">
						<AlertTriangle size={15}/>
						<div>
							<strong>{t("memory.overridesTitle", { count: view.conflicts.length })}</strong>
							{view.conflicts.map((conflict) => (<span key={`${conflict.projectId}:${conflict.globalId}:${conflict.key}`}>
									{t("memory.overrideExplanation", { project: conflict.projectName, global: conflict.globalName })}
								</span>))}
						</div>
					</div>)}
				{hasSavedFilters && <div className="mem-toolbar">
					<label className="mem-search">
						<Search size={14}/>
						<input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("memory.searchPlaceholder")}/>
					</label>
					<div className="mem-filter" role="tablist" aria-label={t("memory.typeFilter")}>
						<button className={"mem-filter__item" + (typeFilter === "all" ? " mem-filter__item--on" : "")} onClick={() => setTypeFilter("all")} type="button">
							{t("memory.allTypes")}
						</button>
						{factTypes.map((type) => (<button className={"mem-filter__item" + (typeFilter === type ? " mem-filter__item--on" : "")} onClick={() => setTypeFilter(type)} type="button" key={type}>
								{memoryTypeLabel(type, t)}
							</button>))}
					</div>
				</div>}
				{error && <div className="mem-error" role="alert">{error}</div>}
				{facts.length === 0 ? (<div className="mem-empty mem-empty--cta">
						<strong>{t("memory.emptySavedTitle")}</strong>
						<span>{t("memory.emptySavedBody")}</span>
					</div>) : filteredFacts.length === 0 ? (<div className="mem-empty">
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
                    const factRevisions = revisions[key];
                    return (<article className={"mem-fact" + (highlight === key ? " mem-fact--hl" : "")} data-mem-type={f.type || "other"} key={key} ref={(el) => {
                            factRefs.current[key] = el;
                        }}>
									<button className="mem-fact__summary" onClick={() => {
                            setExpanded(isOpen ? null : key);
                            setConfirmForget(null);
                            if (!isOpen)
                                void loadRevisions(f);
                        }} type="button">
										{isOpen ? <ChevronDown size={15}/> : <ChevronRight size={15}/>}
										<span className="mem-fact__main">
											<span className="mem-fact__title">{displayTitle(f)}</span>
											<span className="mem-fact__meta">
												<MemoryFactScope scope={f.scope} t={t}/>
												{f.type && <span className="mem-fact__type" data-mem-type={f.type}>{memoryTypeLabel(f.type, t)}</span>}
												<span className={`mem-freshness mem-freshness--${f.freshness || "current"}`}>{freshnessLabel(f.freshness, t)}</span>
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
										<div className="mem-fact__provenance">
											<span>{t("memory.factId")}: <code>{f.id || f.name}</code></span>
											<span>{t("memory.revision", { revision: f.revision || 1 })}</span>
											{f.updatedAt && <span>{t("memory.updatedAt", { time: formatMemoryTime(f.updatedAt) })}</span>}
										</div>
										<div className="mem-revisions">
											<div className="mem-revisions__head"><History size={13}/><strong>{t("memory.revisionHistory")}</strong></div>
											{revisionBusy === key ? (<span className="mem-note">{t("memory.loadingRevisions")}</span>) : !factRevisions || factRevisions.length === 0 ? (<span className="mem-note">{t("memory.noRevisions")}</span>) : factRevisions.map((revision) => (<div className="mem-revision" key={`${key}:${revision.revision}`}>
													<div>
														<strong>{t("memory.revision", { revision: revision.revision || 1 })}</strong>
														<span>{formatMemoryTime(revision.updatedAt || revision.createdAt)}</span>
													</div>
													<button className="btn btn--small" type="button" disabled={busy} onClick={() => void restoreRevision(f, revision.revision || 1)}>
														<ArchiveRestore size={13}/>{t("memory.restoreRevision")}
													</button>
												</div>))}
										</div>
										<div className="mem-fact__actions">
												<span className="mem-hint mem-hint--inline">
													{t("memory.appliesNow")}
												</span>
											{confirmForget === key ? (<div className="mem-confirm">
														<button className="btn btn--small" onClick={() => setConfirmForget(null)} disabled={busy} type="button">
															{t("common.cancel")}
														</button>
														<button className="btn btn--small mem-danger" onClick={() => void forgetFact(f.id || f.name, key)} disabled={busy} type="button">
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
			</section>}

			{tab === "suggestions" && <MemorySuggestionsSection
				suggestions={suggestions}
				suggestionBusy={suggestionBusy}
				busy={busy}
				error={error}
				expandedSuggestion={expandedSuggestion}
				setExpandedSuggestion={setExpandedSuggestion}
				acceptedSuggestions={acceptedSuggestions}
				acceptMemorySuggestion={acceptMemorySuggestion}
				acceptSkillSuggestion={acceptSkillSuggestion}
				refreshSuggestions={refreshSuggestions}
			/>}

			{tab === "activity" && <section className="mem-section">
				<div className="mem-section__head">
					<div>
						<div className="mem-section__title">{t("memory.recallTitle")}</div>
						<div className="mem-note">{t("memory.recallHint")}</div>
					</div>
				</div>
				<div className="mem-recall-summary">
					<Activity size={16}/>
					<div>
						<strong>{view.lastRecall.query || t("memory.noRecallQuery")}</strong>
						<span>{t("memory.recallBudget", { used: view.lastRecall.usedChars, budget: view.lastRecall.charBudget, omitted: view.lastRecall.omitted })}</span>
					</div>
				</div>
				{view.lastRecall.suppressed && (<div className="mem-context-notice mem-context-notice--muted">
						<AlertTriangle size={15}/>
						<span>{t("memory.recallSuppressed", { reason: view.lastRecall.suppressed })}</span>
					</div>)}
				{view.lastRecall.hits.length === 0 ? (<div className="mem-empty">{t("memory.noRecallHits")}</div>) : (<div className="mem-recall-hits">
						{view.lastRecall.hits.map((hit) => (<div className="mem-recall-hit" key={`${hit.id}:${hit.revision}`}>
								<div className="mem-recall-hit__head">
									<strong>{hit.title || hit.name}</strong>
									<span>{Math.round(hit.score * 100)}%</span>
								</div>
								<div className="mem-fact__meta">
									<MemoryFactScope scope={hit.scope} t={t}/>
									<span className="mem-fact__type" data-mem-type={hit.type}>{memoryTypeLabel(hit.type, t)}</span>
									<span className={`mem-freshness mem-freshness--${hit.freshness}`}>{freshnessLabel(hit.freshness, t)}</span>
									<span>{t("memory.revision", { revision: hit.revision })}</span>
								</div>
								<p>{hit.snippet}</p>
								<small>{hit.reason}</small>
							</div>))}
					</div>)}
			</section>}

			{tab === "archived" && <section className="mem-section">
				<div className="mem-section__head">
					<div>
						<div className="mem-section__title">{t("memory.archivedMemories")}</div>
						<div className="mem-note">{t("memory.archivedHint")}</div>
					</div>
				</div>
				{hasArchivedFilters && <div className="mem-toolbar">
					<label className="mem-search">
						<Search size={14}/>
						<input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("memory.searchPlaceholder")}/>
					</label>
					<div className="mem-filter" role="tablist" aria-label={t("memory.typeFilter")}>
						<button className={"mem-filter__item" + (typeFilter === "all" ? " mem-filter__item--on" : "")} onClick={() => setTypeFilter("all")} type="button">
							{t("memory.allTypes")}
						</button>
						{factTypes.map((type) => (<button className={"mem-filter__item" + (typeFilter === type ? " mem-filter__item--on" : "")} onClick={() => setTypeFilter(type)} type="button" key={type}>
								{memoryTypeLabel(type, t)}
							</button>))}
					</div>
				</div>}
				{archives.length === 0 ? (<div className="mem-empty mem-empty--cta">
						<strong>{t("memory.emptyArchivedTitle")}</strong>
						<span>{t("memory.emptyArchivedBody")}</span>
					</div>) : (<ArchivedMemoryList archives={filteredArchives} totalArchives={archives.length} expanded={expandedArchive} setExpanded={setExpandedArchive} renderWithLinks={renderWithLinks} t={t} hideHeader busy={busy} onRestore={restoreArchive}/>)}
			</section>}

			{tab === "docs" && <section className="mem-section">
				<div className="mem-section__head">
					<div>
						<div className="mem-section__title">{t("memory.instructionFiles")}</div>
						<div className="mem-note">{t("memory.instructionFilesHint")}</div>
					</div>
					<div className="mem-section__actions">
						<button className="btn btn--small" type="button" disabled={busy} onClick={() => setShowAdd((v) => !v)}>
							{showAdd ? t("common.collapse") : <><Plus size={13}/>{t("memory.addMemory")}</>}
						</button>
					</div>
				</div>
				{view.instructionDiagnostics.length > 0 && (<div className="mem-instruction-diagnostics">
						<div className="mem-instruction-diagnostics__title"><AlertTriangle size={14}/>{t("memory.instructionDiagnostics")}</div>
						{view.instructionDiagnostics.map((diagnostic, index) => (<div className="mem-instruction-diagnostic" key={`${diagnostic.code}:${diagnostic.path}:${diagnostic.line || index}`}>
								<strong>{diagnostic.code}</strong>
								<span>{diagnostic.message}</span>
								<code>{diagnostic.path}{diagnostic.line ? `:${diagnostic.line}` : ""}</code>
							</div>))}
					</div>)}
				{showAdd && (<div className="mem-add-card">
						<div className="mem-add-card__head">
							<div>
								<strong>{t("memory.addMemory")}</strong>
								<span>{t("memory.addMemoryHint")}</span>
							</div>
						</div>
						<div className="mem-add">
							<Tooltip label={t("memory.whereToSave")}>
								<select className="mem-select" value={activeScope} onChange={(e) => setScope(e.target.value)}>
									{scopes.map((s) => (<option key={s.scope} value={s.scope}>
											{memoryScopeLabel(s.scope, t)}
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
					</div>)}
				{view.docs.length === 0 && (<div className="mem-empty">{t("memory.noDocs")}</div>)}
				{view.docs.map((d) => {
                const editing = editingPath === d.path;
                const open = expandedDoc === d.path || editing;
                return (<div className="mem-doc" data-doc-scope={d.scope || "other"} key={d.path}>
							<div className="mem-doc__head">
								<button className="mem-doc__identity mem-doc__toggle" type="button" aria-expanded={open} onClick={() => {
                        if (!editing)
                            setExpandedDoc(open ? null : d.path);
                    }} disabled={editing}>
									<span className="mem-doc__chevron">
										{open ? <ChevronDown size={15}/> : <ChevronRight size={15}/>}
									</span>
									<span className="mem-doc__icon"><FileText size={15}/></span>
									<div>
										<strong>{memoryDocTitle(d.scope, t)}</strong>
										<span className="mem-doc__path">{d.path}</span>
										<small>{memoryDocHint(d.scope, t)}</small>
										<small>{t("memory.instructionPrecedence", { precedence: d.precedence + 1, directory: d.directory || t("memory.globalDirectory") })}</small>
									</div>
								</button>
								<div className="mem-doc__head-actions">
									<span className={"mem-doc__tag badge--" + d.scope}>{memoryScopeLabel(d.scope, t)}</span>
									{!editing && (<button className="btn btn--small" onClick={() => startEdit(d.path, d.body)}>
										<Pencil size={13}/>
										{t("common.edit")}
									</button>)}
								</div>
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
								</div>) : open ? (<div className="mem-doc__expanded">
									<pre className="mem-doc__body">{d.body}</pre>
									{d.imports.length > 0 && (<div className="mem-doc__imports">
											<strong>{t("memory.instructionImports")}</strong>
											{d.imports.map((item) => <code key={`${item.sourcePath}:${item.path}`}>{item.sourcePath} → {item.path}</code>)}
										</div>)}
								</div>) : null}
						</div>);
            })}
			</section>}
		</>);
}

