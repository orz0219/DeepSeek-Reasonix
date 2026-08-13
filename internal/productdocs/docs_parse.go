package productdocs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	goldmarktext "github.com/yuin/goldmark/text"

	"reasonix/internal/retrieval"
)

func loadCatalogWithReleaseNotes(docsFS, releaseNotesFS fs.FS) (*catalog, error) {
	entries, err := fs.ReadDir(docsFS, ".")
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	hash := sha256.New()
	_, _ = hash.Write([]byte("reasonix-product-docs-v1\x00"))
	markdownParser := goldmark.DefaultParser()
	c := &catalog{byPath: map[string]*document{}, byID: map[string]*section{}}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		data, err := fs.ReadFile(docsFS, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("read %s: Markdown is not valid UTF-8", entry.Name())
		}
		writeDigestRecord(hash, entry.Name(), data)
		doc := parseDocumentWithParser(entry.Name(), string(data), markdownParser)
		doc.source = "docs/" + entry.Name()
		if len(doc.sections) == 0 {
			continue
		}
		if err := c.addDocument(doc); err != nil {
			return nil, err
		}
	}
	if releaseNotesFS != nil {
		data, err := fs.ReadFile(releaseNotesFS, "releases.json")
		if err != nil {
			return nil, fmt.Errorf("read release-notes/releases.json: %w", err)
		}
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("read release-notes/releases.json: JSON is not valid UTF-8")
		}
		writeDigestRecord(hash, "release-notes/releases.json", data)
		rendered, releaseCount, err := renderReleaseDocuments(data)
		if err != nil {
			return nil, err
		}
		for _, virtual := range rendered {
			doc := parseDocumentWithParser(virtual.path, virtual.content, markdownParser)
			doc.source = virtual.source
			doc.locale = virtual.locale
			doc.audience = "user"
			doc.releaseNote = true
			doc.releaseVersion = virtual.version
			if len(doc.sections) == 0 {
				return nil, fmt.Errorf("rendered release note %s contains no sections", virtual.path)
			}
			if err := c.addDocument(doc); err != nil {
				return nil, err
			}
		}
		c.releaseNotes = releaseCount
	}
	if len(c.docs) == 0 || len(c.sections) == 0 {
		return nil, fmt.Errorf("embedded documentation corpus is empty")
	}
	c.digest = hex.EncodeToString(hash.Sum(nil))
	counts := make([]map[string]int, 0, len(c.sections))
	totalLength := 0
	for _, section := range c.sections {
		counts = append(counts, section.counts)
		totalLength += section.length
	}
	c.df = retrieval.DocumentFrequency(counts)
	c.avgLen = float64(totalLength) / float64(len(c.sections))
	if c.avgLen <= 0 {
		c.avgLen = 1
	}
	return c, nil
}

func writeDigestRecord(destination hash.Hash, name string, data []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(name)))
	_, _ = destination.Write(size[:])
	_, _ = destination.Write([]byte(name))
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	_, _ = destination.Write(size[:])
	_, _ = destination.Write(data)
}

func (c *catalog) addDocument(doc *document) error {
	if _, exists := c.byPath[doc.path]; exists {
		return fmt.Errorf("duplicate embedded documentation path %q", doc.path)
	}
	c.docs = append(c.docs, doc)
	c.byPath[doc.path] = doc
	c.byPath[doc.displayPath()] = doc
	for _, section := range doc.sections {
		if _, exists := c.byID[section.id]; exists {
			return fmt.Errorf("duplicate embedded documentation section %q", section.id)
		}
		c.sections = append(c.sections, section)
		c.byID[section.id] = section
	}
	return nil
}

func parseDocumentWithParser(name, content string, markdownParser parser.Parser) *document {
	source := []byte(content)
	doc := &document{
		path:     path.Clean(name),
		title:    strings.TrimSuffix(strings.TrimSuffix(name, ".md"), ".zh-CN"),
		locale:   detectDocumentLanguage(name, content),
		audience: documentAudience(name),
	}
	type headingNode struct {
		start int
		level int
		text  string
	}
	var headingNodes []headingNode
	root := markdownParser.Parse(goldmarktext.NewReader(source))
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || node.Kind() != ast.KindHeading || node.Parent() == nil || node.Parent().Kind() != ast.KindDocument {
			return ast.WalkContinue, nil
		}
		heading := node.(*ast.Heading)
		if heading.Level > 4 || heading.Pos() < 0 {
			return ast.WalkContinue, nil
		}
		text := strings.TrimSpace(markdownHeadingText(heading, source))
		if text == "" {
			return ast.WalkContinue, nil
		}
		headingNodes = append(headingNodes, headingNode{start: heading.Pos(), level: heading.Level, text: text})
		return ast.WalkContinue, nil
	})
	for _, heading := range headingNodes {
		if heading.level == 1 {
			doc.title = heading.text
			break
		}
	}
	lineStarts := sourceLineStarts(source)
	var headings [4]string
	sectionNumber := 0
	appendSection := func(start, end int, currentHeading string) {
		start, end = trimSourceBounds(source, start, end)
		if end <= start {
			return
		}
		raw := string(source[start:end])
		sectionNumber++
		searchText := strings.Join([]string{doc.title, currentHeading, raw}, "\n")
		terms := retrieval.Tokens(searchText)
		id := fmt.Sprintf("%s::s%03d", doc.path, sectionNumber)
		section := &section{
			id:          id,
			document:    doc,
			heading:     currentHeading,
			content:     raw,
			searchText:  searchText,
			counts:      retrieval.Counts(terms),
			headingHits: retrieval.Counts(retrieval.Tokens(doc.title + " " + currentHeading)),
			length:      len(terms),
			startLine:   sourceLineNumber(lineStarts, start),
			endLine:     sourceLineNumber(lineStarts, end-1),
		}
		doc.sections = append(doc.sections, section)
	}
	if len(headingNodes) == 0 {
		appendSection(0, len(source), doc.title)
		return doc
	}
	if headingNodes[0].start > 0 {
		appendSection(0, headingNodes[0].start, doc.title)
	}
	for i, heading := range headingNodes {
		end := len(source)
		if i+1 < len(headingNodes) {
			end = headingNodes[i+1].start
		}
		headings[heading.level-1] = heading.text
		for j := heading.level; j < len(headings); j++ {
			headings[j] = ""
		}
		var trail []string
		for _, value := range headings {
			if value != "" {
				trail = append(trail, value)
			}
		}
		appendSection(heading.start, end, strings.Join(trail, " > "))
	}
	return doc
}

func markdownHeadingText(heading *ast.Heading, source []byte) string {
	var text strings.Builder
	_ = ast.Walk(heading, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.Text:
			text.Write(node.Value(source))
			if node.SoftLineBreak() {
				text.WriteByte('\n')
			}
		case *ast.String:
			text.Write(node.Value)
		case *ast.AutoLink:
			text.Write(node.Label(source))
		case *ast.RawHTML:
			text.Write(node.Segments.Value(source))
		}
		return ast.WalkContinue, nil
	})
	return text.String()
}

func trimSourceBounds(source []byte, start, end int) (int, int) {
	if start < 0 {
		start = 0
	}
	if end > len(source) {
		end = len(source)
	}
	if end <= start {
		return start, start
	}
	trimmedLeft := bytes.TrimLeft(source[start:end], " \t\r\n")
	start = end - len(trimmedLeft)
	trimmed := bytes.TrimRight(trimmedLeft, " \t\r\n")
	return start, start + len(trimmed)
}

func sourceLineStarts(source []byte) []int {
	starts := []int{0}
	for i, value := range source {
		if value == '\n' && i+1 < len(source) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func sourceLineNumber(starts []int, offset int) int {
	index := sort.Search(len(starts), func(i int) bool { return starts[i] > offset })
	if index == 0 {
		return 1
	}
	return index
}

func normalizeLanguage(language string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "auto":
		return "auto", nil
	case "all":
		return "all", nil
	case "en", "en-us", "english":
		return "en", nil
	case "zh", "zh-cn", "cn", "chinese":
		return "zh-CN", nil
	default:
		return "", fmt.Errorf("unknown language %q; use auto, all, en, or zh-CN", language)
	}
}

func normalizeAudience(audience string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(audience)) {
	case "", "all":
		return "all", nil
	case "user", "developer", "maintainer":
		return strings.ToLower(strings.TrimSpace(audience)), nil
	default:
		return "", fmt.Errorf("unknown audience %q; use all, user, developer, or maintainer", audience)
	}
}

func detectQueryLanguage(query string) string {
	for _, r := range query {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return "zh-CN"
		}
	}
	return "en"
}

func detectDocumentLanguage(name, content string) string {
	if strings.HasSuffix(strings.ToLower(name), ".zh-cn.md") {
		return "zh-CN"
	}
	han, latin := 0, 0
	for _, r := range content {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			han++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}
	if han > 100 && han*4 > latin {
		return "zh-CN"
	}
	return "en"
}

func documentAudience(name string) string {
	stem := strings.TrimSuffix(strings.TrimSuffix(name, ".md"), ".zh-CN")
	switch strings.ToUpper(stem) {
	case "RELEASING", "SIGNPATH_WINDOWS_ADMIN_SOP", "PRODUCTION_CHECKLIST", "THEME_ASSETS":
		return "maintainer"
	case "CHECKPOINTS", "GOAL_ENFORCEMENT", "SESSION_REFERENCE_ARCHITECTURE", "SPEC", "TASK_CONTRACT", "TOOL_CONTRACT":
		return "developer"
	default:
		return "user"
	}
}

func formatSearchResults(query, identity string, hits []searchHit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No embedded Reasonix documentation matched %q (%s). Try fewer terms, an exact command/configuration key, language=all, or audience=all.", query, identity)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Embedded Reasonix documentation results for %q (%s):\n", query, identity)
	for i, hit := range hits {
		section := hit.section
		fmt.Fprintf(&b, "\n%d. score=%.3f source=%s path=%s section_id=%s locale=%s audience=%s\n   heading: %s\n   snippet: %s\n",
			i+1, hit.score, section.document.sourceRange(section.startLine, section.endLine), section.document.displayPath(), section.id,
			section.document.locale, section.document.audience, section.heading,
			retrieval.MakeSnippet(section.searchText, query, retrieval.Unique(retrieval.Tokens(query)), maxSnippet))
	}
	b.WriteString("\nUse operation=read with section_id to read the complete embedded section. Cite the source path and line range in the answer.")
	return strings.TrimSpace(b.String())
}
