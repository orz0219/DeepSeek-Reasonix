import { ArchiveRestore, ChevronDown, ChevronRight } from "lucide-react";
import { type ReactNode } from "react";
import { useT } from "../lib/i18n";
import type { MemoryArchive, MemoryFact, MemorySuggestionsView } from "../lib/types";
type LinkInfo = {
    name: string;
    exists: boolean;
};
export function displayTitle(fact: MemoryFact): string {
    return fact.title || fact.name.replaceAll("-", " ");
}
export function memoryFactKey(fact: MemoryFact): string {
    return fact.id || `${fact.scope}:${fact.name}`;
}
export function formatMemoryTime(value?: string): string {
    if (!value)
        return "";
    const date = new Date(value);
    if (Number.isNaN(date.getTime()))
        return value;
    return date.toLocaleString();
}
export function freshnessLabel(value: string, t: ReturnType<typeof useT>): string {
    switch (value) {
        case "fresh": return t("memory.freshness.fresh");
        case "current": return t("memory.freshness.current");
        case "stale": return t("memory.freshness.stale");
        default: return value;
    }
}
export function memoryMatches(fact: MemoryFact, normalizedQuery: string, typeFilter: string): boolean {
    if (typeFilter !== "all" && fact.type !== typeFilter)
        return false;
    if (!normalizedQuery)
        return true;
    return [displayTitle(fact), fact.name, fact.description, fact.type, fact.scope, fact.body]
        .join(" ")
        .toLowerCase()
        .includes(normalizedQuery);
}
function archiveKey(fact: MemoryArchive): string {
    return `${fact.path || fact.name}:${fact.archivedAt || ""}`;
}
function formatArchivedAt(value?: string): string {
    if (!value)
        return "";
    const date = new Date(value);
    if (Number.isNaN(date.getTime()))
        return value;
    return date.toLocaleString();
}
export function ArchivedMemoryList({ archives, totalArchives, expanded, setExpanded, renderWithLinks, t, hideHeader = false, busy = false, onRestore, }: {
    archives: MemoryArchive[];
    totalArchives: number;
    expanded: string | null;
    setExpanded: (key: string | null) => void;
    renderWithLinks: (text: string) => ReactNode[];
    t: ReturnType<typeof useT>;
    hideHeader?: boolean;
    busy?: boolean;
    onRestore?: (archive: MemoryArchive) => Promise<void> | void;
}) {
    if (totalArchives === 0)
        return null;
    return (<div className="mem-archive-block">
      {!hideHeader && <div className="mem-section__row">
        <div>
          <div className="mem-section__title">{t("memory.archivedMemories")}</div>
          <div className="mem-note">{t("memory.archivedHint")}</div>
        </div>
        <span className="mem-count">{totalArchives}</span>
      </div>}
      {archives.length === 0 ? (<div className="mem-empty">{t("memory.noArchivedMatches")}</div>) : (<div className="mem-facts mem-facts--archive">
          {archives.map((f) => {
                const key = archiveKey(f);
                const isOpen = expanded === key;
                return (<article className="mem-fact mem-fact--archived" data-mem-type={f.type || "other"} key={key}>
                <button className="mem-fact__summary" onClick={() => setExpanded(isOpen ? null : key)} type="button">
                  {isOpen ? <ChevronDown size={15}/> : <ChevronRight size={15}/>}
                  <span className="mem-fact__main">
                    <span className="mem-fact__title">{displayTitle(f)}</span>
                    <span className="mem-fact__meta">
                      <MemoryFactScope scope={f.scope} t={t}/>
                      {f.type && <span className="mem-fact__type" data-mem-type={f.type}>{memoryTypeLabel(f.type, t)}</span>}
                      <span className="mem-fact__slug">{f.name}</span>
                      {f.archivedAt && (<span className="mem-fact__archived">
                          {t("memory.archivedAt", { time: formatArchivedAt(f.archivedAt) })}
                        </span>)}
                    </span>
                    <span className="mem-fact__desc">{f.description}</span>
                  </span>
                </button>
                {isOpen && (<div className="mem-fact__detail">
                    {f.body ? (<div className="mem-fact__body">{renderWithLinks(f.body)}</div>) : (<div className="mem-empty">{t("memory.noBody")}</div>)}
                    <div className="mem-archive__path">{f.path}</div>
                    {onRestore && (<div className="mem-fact__actions">
                        <span className="mem-hint mem-hint--inline">{t("memory.restoreArchivedHint")}</span>
                        <button className="btn btn--small" type="button" disabled={busy} onClick={() => void onRestore(f)}>
                          <ArchiveRestore size={13}/>
                          {t("memory.restoreArchived")}
                        </button>
                      </div>)}
                  </div>)}
              </article>);
            })}
        </div>)}
    </div>);
}
export function uniqueLinks(body: string, names: Set<string>): LinkInfo[] {
    const links: LinkInfo[] = [];
    const seen = new Set<string>();
    const re = /\[\[([^\]]+)\]\]/g;
    let match: RegExpExecArray | null;
    while ((match = re.exec(body)) !== null) {
        const name = match[1].trim();
        if (!name || seen.has(name))
            continue;
        seen.add(name);
        links.push({ name, exists: names.has(name) });
    }
    return links;
}
export function memoryScopeLabel(scope: string, t: ReturnType<typeof useT>): string {
    switch (scope) {
        case "project":
            return t("memory.scope.project");
        case "global":
            return t("memory.scope.global");
        case "user":
            return t("memory.scope.user");
        case "local":
            return t("memory.scope.local");
        case "ancestor":
            return t("memory.scope.ancestor");
        default:
            return scope;
    }
}
export function MemoryFactScope({ scope, t }: {
    scope: string;
    t: ReturnType<typeof useT>;
}) {
    if (!scope)
        return null;
    return <span className="mem-fact__scope" data-mem-scope={scope}>{memoryScopeLabel(scope, t)}</span>;
}
export function memoryTypeLabel(type: string, t: ReturnType<typeof useT>): string {
    switch ((type || "").toLowerCase()) {
        case "project":
            return t("memory.type.project");
        case "user":
            return t("memory.type.user");
        case "feedback":
            return t("memory.type.feedback");
        case "reference":
            return t("memory.type.reference");
        default:
            return type || t("memory.type.other");
    }
}
export function memoryDocTitle(scope: string, t: ReturnType<typeof useT>): string {
    switch (scope) {
        case "project":
            return t("memory.doc.projectTitle");
        case "user":
            return t("memory.doc.userTitle");
        case "local":
            return t("memory.doc.localTitle");
        case "ancestor":
            return t("memory.doc.ancestorTitle");
        default:
            return t("memory.doc.customTitle");
    }
}
export function memoryDocHint(scope: string, t: ReturnType<typeof useT>): string {
    switch (scope) {
        case "project":
            return t("memory.doc.projectHint");
        case "user":
            return t("memory.doc.userHint");
        case "local":
            return t("memory.doc.localHint");
        case "ancestor":
            return t("memory.doc.ancestorHint");
        default:
            return t("memory.doc.customHint");
    }
}
export function errorMessage(err: unknown): string {
    if (err instanceof Error)
        return err.message;
    return String(err || "Unknown error");
}
export function suggestionTotal(view: MemorySuggestionsView | null): number {
    return (view?.memories?.length ?? 0) + (view?.skills?.length ?? 0);
}
export function suggestionStamp(value?: string): string {
    if (!value)
        return "";
    const date = new Date(value);
    if (Number.isNaN(date.getTime()))
        return value;
    return date.toLocaleString();
}

