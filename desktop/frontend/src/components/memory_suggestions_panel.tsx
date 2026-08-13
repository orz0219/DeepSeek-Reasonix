import { Check, ChevronDown, ChevronRight, FileText, RefreshCw, Sparkles } from "lucide-react";
import { useT } from "../lib/i18n";
import type { MemorySuggestion, MemorySuggestionsView, SkillSuggestion } from "../lib/types";
import { memoryScopeLabel, MemoryFactScope, memoryTypeLabel, suggestionStamp, suggestionTotal } from "./memory_panel_helpers";

interface MemorySuggestionsSectionProps {
    suggestions: MemorySuggestionsView | null;
    suggestionBusy: boolean;
    busy: boolean;
    error: string | null;
    expandedSuggestion: string | null;
    setExpandedSuggestion: (id: string | null) => void;
    acceptedSuggestions: Record<string, string>;
    acceptMemorySuggestion: (candidate: MemorySuggestion) => void;
    acceptSkillSuggestion: (candidate: SkillSuggestion) => void;
    refreshSuggestions: () => void;
}

export function MemorySuggestionsSection(props: MemorySuggestionsSectionProps) {
    const t = useT();
    const { suggestions, suggestionBusy, busy, error, expandedSuggestion, setExpandedSuggestion, acceptedSuggestions, acceptMemorySuggestion, acceptSkillSuggestion, refreshSuggestions } = props;
    return (
<section className="mem-section">
<div className="mem-section__head">
	<div>
		<div className="mem-section__title">{t("memory.suggestions")}</div>
		<div className="mem-note">{t("memory.suggestionsHint")}</div>
	</div>
	<div className="mem-section__actions">
		<button className="btn btn--small" type="button" disabled={suggestionBusy || busy} onClick={() => void refreshSuggestions()}>
			<RefreshCw size={13}/>
			{suggestions ? t("memory.refreshSuggestions") : t("memory.scanSuggestions")}
		</button>
	</div>
</div>
{error && <div className="mem-error" role="alert">{error}</div>}
{!suggestions ? (<div className="mem-empty mem-empty--cta">
		<strong>{t("memory.suggestionsEmptyTitle")}</strong>
		<span>{t("memory.suggestionsEmptyBody")}</span>
		<button className="btn btn--primary btn--small" type="button" disabled={suggestionBusy || busy} onClick={() => void refreshSuggestions()}>
			<Sparkles size={13}/>
			{t("memory.scanSuggestions")}
		</button>
	</div>) : suggestionTotal(suggestions) === 0 ? (<div className="mem-empty mem-empty--cta">
		<strong>{t("memory.noSuggestionsTitle")}</strong>
		<span>{t("memory.noSuggestionsBody")}</span>
	</div>) : (<div className="mem-suggestions">
		{suggestions.generatedAt && (<div className="mem-suggestions__stamp">
				{t("memory.suggestionsGenerated", { time: suggestionStamp(suggestions.generatedAt) })}
			</div>)}
		{suggestions.memories.length > 0 && (<div className="mem-suggestion-group">
				<div className="mem-suggestion-group__title">{t("memory.memoryCandidates")}</div>
				<div className="mem-facts">
					{suggestions.memories.map((candidate) => {
                        const open = expandedSuggestion === candidate.id;
                        const accepted = acceptedSuggestions[candidate.id];
                        return (<article className="mem-fact mem-suggestion" data-mem-type={candidate.type || "other"} key={candidate.id}>
								<button className="mem-fact__summary" type="button" onClick={() => setExpandedSuggestion(open ? null : candidate.id)}>
								{open ? <ChevronDown size={15}/> : <ChevronRight size={15}/>}
								<span className="mem-fact__main">
									<span className="mem-fact__title">{candidate.title || candidate.name}</span>
									<span className="mem-fact__meta">
										<MemoryFactScope scope={candidate.scope} t={t}/>
										<span className="mem-fact__type" data-mem-type={candidate.type}>{memoryTypeLabel(candidate.type, t)}</span>
										<span className="mem-fact__slug">{candidate.name}</span>
									</span>
										<span className="mem-fact__desc">{candidate.description}</span>
									</span>
								</button>
								{open && (<div className="mem-fact__detail">
										<div className="mem-suggestion__body">{candidate.body}</div>
										{candidate.reason && <div className="mem-suggestion__reason">{candidate.reason}</div>}
										{candidate.evidence.length > 0 && (<ul className="mem-suggestion__evidence">
												{candidate.evidence.map((item) => <li key={item}>{item}</li>)}
											</ul>)}
										<div className="mem-fact__actions">
											<span className="mem-hint mem-hint--inline">{t("memory.confirmBeforeApply")}</span>
											{accepted ? (<span className="mem-suggestion__accepted"><Check size={13}/>{t("memory.savedSuggestion")}</span>) : (<button className="btn btn--primary btn--small" type="button" disabled={busy} onClick={() => void acceptMemorySuggestion(candidate)}>
													<Check size={13}/>
													{t("memory.saveAsMemory")}
												</button>)}
										</div>
									</div>)}
							</article>);
                    })}
				</div>
			</div>)}
		{suggestions.skills.length > 0 && (<div className="mem-suggestion-group">
				<div className="mem-suggestion-group__title">{t("memory.skillCandidates")}</div>
				<div className="mem-facts">
					{suggestions.skills.map((candidate) => {
                        const open = expandedSuggestion === candidate.id;
                        const accepted = acceptedSuggestions[candidate.id];
                        return (<article className="mem-fact mem-suggestion mem-suggestion--skill" data-mem-type="reference" key={candidate.id}>
								<button className="mem-fact__summary" type="button" onClick={() => setExpandedSuggestion(open ? null : candidate.id)}>
									{open ? <ChevronDown size={15}/> : <ChevronRight size={15}/>}
									<span className="mem-doc__icon"><FileText size={15}/></span>
									<span className="mem-fact__main">
										<span className="mem-fact__title">{candidate.name}</span>
										<span className="mem-fact__meta">
											<span className="mem-fact__type">{t("memory.skillCandidate")}</span>
											<span className="mem-fact__slug">{memoryScopeLabel(candidate.scope, t)}</span>
										</span>
										<span className="mem-fact__desc">{candidate.description}</span>
									</span>
								</button>
								{open && (<div className="mem-fact__detail">
										<pre className="mem-suggestion__body mem-suggestion__body--code">{candidate.body}</pre>
										{candidate.reason && <div className="mem-suggestion__reason">{candidate.reason}</div>}
										{candidate.evidence.length > 0 && (<ul className="mem-suggestion__evidence">
												{candidate.evidence.map((item) => <li key={item}>{item}</li>)}
											</ul>)}
										<div className="mem-fact__actions">
											<span className="mem-hint mem-hint--inline">{t("memory.confirmBeforeApply")}</span>
											{accepted ? (<span className="mem-suggestion__accepted"><Check size={13}/>{t("memory.createdSkillSuggestion")}</span>) : (<button className="btn btn--primary btn--small" type="button" disabled={busy} onClick={() => void acceptSkillSuggestion(candidate)}>
													<Check size={13}/>
													{t("memory.createSkill")}
												</button>)}
										</div>
									</div>)}
							</article>);
                    })}
				</div>
			</div>)}
	</div>)}
</section>
    );
}
