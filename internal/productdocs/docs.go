// Package productdocs provides offline retrieval over the official Reasonix
// documentation embedded in the application binary.
package productdocs

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	productcontent "reasonix/docs"
	"reasonix/internal/retrieval"
	"reasonix/internal/tool"
	releasenotes "reasonix/release-notes"
)

const (
	defaultLimit  = 5
	maxLimit      = 10
	maxSnippet    = 360
	maxQueryRunes = 4096
	scoreFloor    = 0.15
)

type document struct {
	path           string
	source         string
	title          string
	locale         string
	audience       string
	releaseNote    bool
	releaseVersion string
	sections       []*section
}

type section struct {
	id          string
	document    *document
	heading     string
	content     string
	searchText  string
	counts      map[string]int
	headingHits map[string]int
	length      int
	startLine   int
	endLine     int
}

func (d *document) displayPath() string {
	if d.releaseNote {
		return d.path
	}
	return "docs/" + d.path
}

func (d *document) sourceRange(startLine, endLine int) string {
	if d.releaseNote {
		return fmt.Sprintf("%s rendered-lines=%d-%d", d.source, startLine, endLine)
	}
	return fmt.Sprintf("%s:%d-%d", d.source, startLine, endLine)
}

type catalog struct {
	digest       string
	docs         []*document
	byPath       map[string]*document
	byID         map[string]*section
	sections     []*section
	df           map[string]int
	avgLen       float64
	releaseNotes int
}

// Manifest identifies the exact documentation corpus bundled into one build.
// Version and Revision describe the product binary; Digest binds the sorted
// Markdown sources plus the structured release catalog consumed by retrieval.
type Manifest struct {
	Version      string `json:"version"`
	Revision     string `json:"revision"`
	Digest       string `json:"digest"`
	Documents    int    `json:"documents"`
	Sections     int    `json:"sections"`
	ReleaseNotes int    `json:"release_notes"`
}

type searchHit struct {
	section *section
	score   float64
}

type docsTool struct {
	catalog *catalog
	loadErr error
}

var (
	queryVersionRe = regexp.MustCompile(`(?i)\bv?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9a-z.-]+)?)\b`)

	defaultOnce    sync.Once
	defaultCatalog *catalog
	defaultLoadErr error
	// linkedVersion and linkedRevision are stamped by official release builds.
	// Keeping them here gives CLI and Desktop one shared corpus identity.
	linkedVersion  = "dev"
	linkedRevision string
)

// NewTool returns a read-only tool backed by the documentation embedded in the
// current Reasonix build. Loading stays lazy so merely registering the stable
// schema does not add Markdown parsing work to application startup.
func NewTool() tool.Tool {
	return &docsTool{}
}

func loadDefaultCatalog() (*catalog, error) {
	defaultOnce.Do(func() {
		defaultCatalog, defaultLoadErr = loadCatalogWithReleaseNotes(productcontent.Content, releasenotes.Content)
	})
	return defaultCatalog, defaultLoadErr
}

// EmbeddedManifest returns the identity of the corpus compiled into this
// binary. Diagnostics and release verification intentionally share it.
func EmbeddedManifest() (Manifest, error) {
	c, err := loadDefaultCatalog()
	if err != nil {
		return Manifest{}, err
	}
	return c.manifest(), nil
}

// CommandOverview returns the local /docs help text and the identity of the
// exact corpus compiled into this binary. It never calls a model or the network.
func CommandOverview(language string) (string, error) {
	return CommandOverviewFor(language, "/docs")
}

// CommandOverviewFor is CommandOverview with the invocation name selected by
// the runtime resolver (for example /reasonix:docs when /docs is occupied).
func CommandOverviewFor(language, commandName string) (string, error) {
	c, err := loadDefaultCatalog()
	if err != nil {
		return "", fmt.Errorf("load embedded documentation: %w", err)
	}
	commandName = "/" + strings.TrimPrefix(strings.TrimSpace(commandName), "/")
	if commandName == "/" {
		commandName = "/docs"
	}
	m := c.manifest()
	identity := fmt.Sprintf("version=%s revision=%s digest=%s", m.Version, m.Revision, m.Digest)
	stats := fmt.Sprintf("documents=%d sections=%d releases=%d", m.Documents, m.Sections, m.ReleaseNotes)
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "zh", "zh-cn":
		return fmt.Sprintf("内置 Reasonix 文档\n%s\n%s\n\n用法：%s <问题>\n示例：%s 1.19.5 更新日志\n\n搜索在本地完成，命中的版本匹配资料会交给当前配置的 AI 组织答案。", identity, stats, commandName, commandName), nil
	case "zh-tw":
		return fmt.Sprintf("內建 Reasonix 文件\n%s\n%s\n\n用法：%s <問題>\n範例：%s 1.19.5 更新日誌\n\n搜尋在本機完成，命中的版本匹配資料會交給目前設定的 AI 組織答案。", identity, stats, commandName, commandName), nil
	default:
		return fmt.Sprintf("Embedded Reasonix documentation\n%s\n%s\n\nUsage: %s <question>\nExample: %s 1.19.5 changelog\n\nSearch runs locally, then the version-matched evidence is passed to the configured AI to compose the answer.", identity, stats, commandName, commandName), nil
	}
}

// SearchEmbedded searches the exact documentation corpus compiled into this
// binary. It is the host-side retrieval path used by /docs, independent of
// whether the configured model chooses to call the docs tool itself.
func SearchEmbedded(ctx context.Context, query string) (string, error) {
	c, err := loadDefaultCatalog()
	if err != nil {
		return "", fmt.Errorf("load embedded documentation: %w", err)
	}
	return (&docsTool{catalog: c}).search(ctx, query, "auto", "all", defaultLimit)
}

// SourceManifest computes the corpus identity from the source Markdown and
// structured release catalog used by a build.
func SourceManifest(docsFS, releaseNotesFS fs.FS) (Manifest, error) {
	c, err := loadCatalogWithReleaseNotes(docsFS, releaseNotesFS)
	if err != nil {
		return Manifest{}, err
	}
	return c.manifest(), nil
}

func (c *catalog) manifest() Manifest {
	version, revision := buildIdentity()
	return Manifest{
		Version:      version,
		Revision:     revision,
		Digest:       "sha256:" + c.digest,
		Documents:    len(c.docs),
		Sections:     len(c.sections),
		ReleaseNotes: c.releaseNotes,
	}
}

func buildIdentity() (string, string) {
	version := strings.TrimSpace(linkedVersion)
	if version == "" {
		version = "dev"
	}
	revision := strings.TrimSpace(linkedRevision)
	if info, ok := debug.ReadBuildInfo(); ok {
		if version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		if revision == "" {
			modified := false
			for _, setting := range info.Settings {
				switch setting.Key {
				case "vcs.revision":
					revision = strings.TrimSpace(setting.Value)
				case "vcs.modified":
					modified = setting.Value == "true"
				}
			}
			if modified && revision != "" {
				revision += "+dirty"
			}
		}
	}
	if revision == "" {
		revision = "unknown"
	}
	return version, revision
}

func (c *catalog) identityLine() string {
	m := c.manifest()
	return fmt.Sprintf("version=%s revision=%s digest=%s", m.Version, m.Revision, m.Digest)
}

func (*docsTool) Name() string { return "docs" }

func (*docsTool) Description() string {
	return "Search and read the official documentation embedded in this exact Reasonix build. " +
		"Use it before web search or assumptions for Reasonix setup, CLI, Desktop, configuration, permissions, MCP, memory, recovery, provider behavior, and maintainer workflows. " +
		"Search first, then read the returned section_id when the full section is needed."
}

func (*docsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"operation":{"type":"string","enum":["search","read","list"],"description":"search ranks relevant sections; read returns one section or lists a document's sections; list shows the embedded document catalog."},
			"query":{"type":"string","maxLength":4096,"description":"Question, command, configuration key, error phrase, or topic for operation=search."},
			"section_id":{"type":"string","description":"Exact section_id returned by search or by a document section listing. Used by operation=read."},
			"path":{"type":"string","description":"Exact docs/*.md path returned by search/list. With operation=read and no section_id, lists that document's sections."},
			"language":{"type":"string","enum":["auto","all","en","zh-CN"],"description":"Language preference. search defaults to auto from the query; list defaults to all. Explicit en or zh-CN filters results."},
			"audience":{"type":"string","enum":["all","user","developer","maintainer"],"description":"Optional audience filter; defaults to all."},
			"limit":{"type":"integer","minimum":1,"maximum":10,"description":"Maximum search results, default 5, max 10."}
		},
		"required":["operation"]
	}`)
}

func (t *docsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.loadErr != nil {
		return "", fmt.Errorf("load embedded documentation: %w", t.loadErr)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c := t.catalog
	if c == nil {
		var err error
		c, err = loadDefaultCatalog()
		if err != nil {
			return "", fmt.Errorf("load embedded documentation: %w", err)
		}
	}
	if c == nil {
		return "", fmt.Errorf("embedded documentation is unavailable")
	}
	var in struct {
		Operation string `json:"operation"`
		Query     string `json:"query"`
		SectionID string `json:"section_id"`
		Path      string `json:"path"`
		Language  string `json:"language"`
		Audience  string `json:"audience"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	language, err := normalizeLanguage(in.Language)
	if err != nil {
		return "", err
	}
	audience, err := normalizeAudience(in.Audience)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(in.Operation)) {
	case "search":
		return (&docsTool{catalog: c}).search(ctx, in.Query, language, audience, in.Limit)
	case "read":
		return (&docsTool{catalog: c}).read(in.SectionID, in.Path)
	case "list":
		return (&docsTool{catalog: c}).list(language, audience), nil
	case "":
		return "", fmt.Errorf("operation is required")
	default:
		return "", fmt.Errorf("unknown operation %q", in.Operation)
	}
}

func (*docsTool) ReadOnly() bool { return true }

func (*docsTool) SnipHint() tool.SnipHint {
	return tool.SnipHint{Head: 24, Tail: 6, HeadChars: 8000, TailChars: 1500}
}

func (t *docsTool) search(ctx context.Context, query, language, audience string, limit int) (string, error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) > maxQueryRunes {
		return "", fmt.Errorf("query is too long: maximum %d characters", maxQueryRunes)
	}
	queryTerms, err := retrieval.QueryTerms(query)
	if err != nil {
		return "", err
	}
	limit = clamp(limit, defaultLimit, maxLimit)
	preferredLanguage := language
	if preferredLanguage == "auto" {
		preferredLanguage = detectQueryLanguage(query)
	}
	queryLower := strings.ToLower(query)
	queryVersions := queryVersionRe.FindAllStringSubmatch(queryLower, -1)
	hits := make([]searchHit, 0, len(t.catalog.sections))
	for _, section := range t.catalog.sections {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if language != "auto" && language != "all" && section.document.locale != language {
			continue
		}
		if audience != "all" && section.document.audience != audience {
			continue
		}
		score := retrieval.BM25Score(section.counts, section.length, queryTerms, t.catalog.df, len(t.catalog.sections), t.catalog.avgLen)
		exactReleaseVersion := false
		for _, match := range queryVersions {
			if len(match) > 1 && strings.EqualFold(section.document.releaseVersion, match[1]) {
				exactReleaseVersion = true
				break
			}
		}
		if score <= 0 && !exactReleaseVersion {
			continue
		}
		if exactReleaseVersion {
			// Release-note virtual paths carry the exact requested version. Keep
			// that stronger than generic terms such as "changelog" or "更新日志".
			score += 100
		}
		for _, term := range queryTerms {
			if section.headingHits[term] > 0 {
				score += 0.35
			}
		}
		if queryLower != "" && strings.Contains(strings.ToLower(section.searchText), queryLower) {
			score += 1.5
		}
		if preferredLanguage != "all" && section.document.locale == preferredLanguage {
			score *= 1.15
		}
		hits = append(hits, searchHit{section: section, score: score})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return hits[i].section.id < hits[j].section.id
		}
		return hits[i].score > hits[j].score
	})
	hits = retrieval.KeepTopRelativeScore(hits, scoreFloor, func(hit searchHit) float64 { return hit.score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return formatSearchResults(query, t.catalog.identityLine(), hits), nil
}

func (t *docsTool) read(sectionID, documentPath string) (string, error) {
	sectionID = strings.TrimSpace(sectionID)
	documentPath = strings.TrimSpace(documentPath)
	if sectionID != "" {
		section, ok := t.catalog.byID[sectionID]
		if !ok {
			return "", fmt.Errorf("unknown section_id %q; use operation=search or read with an exact path to list section ids", sectionID)
		}
		return fmt.Sprintf("Embedded Reasonix documentation (%s)\nsource: %s\npath: %s\nsection_id: %s\nlocale: %s\naudience: %s\nheading: %s\n\n%s",
			t.catalog.identityLine(), section.document.sourceRange(section.startLine, section.endLine), section.document.displayPath(), section.id,
			section.document.locale, section.document.audience, section.heading, strings.TrimSpace(section.content)), nil
	}
	if documentPath == "" {
		return "", fmt.Errorf("section_id or path is required for operation=read")
	}
	doc, ok := t.catalog.byPath[documentPath]
	if !ok {
		doc, ok = t.catalog.byPath[strings.TrimPrefix(documentPath, "docs/")]
	}
	if !ok {
		return "", fmt.Errorf("unknown documentation path %q; use operation=list for exact paths", documentPath)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s, audience=%s)\npath: %s\nsource: %s\nbuild: %s\n", doc.title, doc.locale, doc.audience, doc.displayPath(), doc.source, t.catalog.identityLine())
	for _, section := range doc.sections {
		fmt.Fprintf(&b, "\n- section_id=%s lines=%d-%d heading=%s", section.id, section.startLine, section.endLine, section.heading)
	}
	b.WriteString("\n\nUse operation=read with section_id to read one complete section.")
	return b.String(), nil
}

func (t *docsTool) list(language, audience string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Embedded Reasonix documentation catalog (%s):\n", t.catalog.identityLine())
	count := 0
	for _, doc := range t.catalog.docs {
		if language != "auto" && language != "all" && doc.locale != language {
			continue
		}
		if audience != "all" && doc.audience != audience {
			continue
		}
		count++
		fmt.Fprintf(&b, "\n- path=%s locale=%s audience=%s sections=%d title=%s", doc.displayPath(), doc.locale, doc.audience, len(doc.sections), doc.title)
	}
	if count == 0 {
		b.WriteString("\n\nNo embedded documents matched the requested filters.")
	} else {
		b.WriteString("\n\nUse operation=read with an exact path to list its section ids, or operation=search to rank relevant sections.")
	}
	return b.String()
}

func clamp(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}
