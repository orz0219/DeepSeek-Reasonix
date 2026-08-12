package config

import (
	"fmt"
	"strings"
)

// replaceTOMLSection replaces the content of a named TOML section (including
// its header line) with newContent. It handles both [section] and [[section]]
// array-of-tables headers. If the section doesn't exist, newContent is appended
// at the end.
func replaceTOMLSection(body, sectionName, newContent string) string {
	spans := tomlLineSpans(body)
	structural := tomlStructuralLineMask(spans)
	arrayIdx := -1
	for i, span := range spans {
		if !structural[i] {
			continue
		}
		name, isArray, ok := tomlEditSectionHeader(span.text)
		if ok && isArray && name == sectionName {
			arrayIdx = i
			break
		}
	}
	if arrayIdx >= 0 {
		start := spans[arrayIdx].start
		end := len(body)
		for i := arrayIdx + 1; i < len(spans); i++ {
			if !structural[i] {
				continue
			}
			name, isArray, ok := tomlEditSectionHeader(spans[i].text)
			if !ok {
				continue
			}
			if (isArray && name == sectionName) || strings.HasPrefix(name, sectionName+".") {
				continue
			}
			end = spans[i].start
			break
		}
		return body[:start] + strings.TrimRight(newContent, "\n") + "\n" + body[end:]
	}

	for i, span := range spans {
		if !structural[i] {
			continue
		}
		name, isArray, ok := tomlEditSectionHeader(span.text)
		if !ok || isArray || name != sectionName {
			continue
		}
		end := len(body)
		for nextIdx, next := range spans {
			if !structural[nextIdx] {
				continue
			}
			if next.start <= span.start {
				continue
			}
			if _, _, ok := tomlEditSectionHeader(next.text); ok {
				end = next.start
				break
			}
		}
		return body[:span.start] + newContent + body[end:]
	}
	return strings.TrimRight(body, "\n") + "\n\n" + newContent
}

func upsertTOMLSectionKey(body, sectionName, key, line string) string {
	line = strings.TrimRight(line, "\r\n") + "\n"
	spans := tomlLineSpans(body)
	structural := tomlStructuralLineMask(spans)
	sectionIdx := -1
	sectionEnd := len(body)
	for i, span := range spans {
		if !structural[i] {
			continue
		}
		name, isArray, ok := tomlEditSectionHeader(span.text)
		if ok {
			if sectionIdx >= 0 {
				sectionEnd = span.start
				break
			}
			if !isArray && name == sectionName {
				sectionIdx = i
			}
			continue
		}
		if sectionIdx >= 0 {
			if got, _, ok := tomlKeyValue(span.text); ok && got == key {
				endIdx := tomlValueEndSpan(spans, i)
				end := spans[endIdx].end
				if endIdx > i {
					if comments := tomlCommentsInSpans(spans, i, endIdx); len(comments) > 0 {
						line = strings.Join(comments, "\n") + "\n" + line
					}
				} else if comment := tomlInlineComment(spans[endIdx].text); comment != "" {
					line = strings.TrimRight(line, "\r\n") + " " + comment + "\n"
				}
				return body[:span.start] + line + body[end:]
			}
		}
	}
	if sectionIdx < 0 {
		block := fmt.Sprintf("[%s]\n%s", sectionName, line)
		return replaceTOMLSection(body, sectionName, block)
	}
	prefix := body[:sectionEnd]
	if prefix != "" && !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	return prefix + line + body[sectionEnd:]
}

type tomlLexState struct {
	stringKind tomlStringKind
	escaped    bool
}

const (
	tomlStringNone tomlStringKind = iota
	tomlStringBasic
	tomlStringLiteral
	tomlStringMultilineBasic
	tomlStringMultilineLiteral
)

func (s tomlLexState) inMultilineString() bool {
	return s.stringKind == tomlStringMultilineBasic || s.stringKind == tomlStringMultilineLiteral
}

func scanTOMLLine(line string, state *tomlLexState, outsideString func(byte)) int {
	for i := 0; i < len(line); {
		ch := line[i]
		switch state.stringKind {
		case tomlStringBasic:
			if state.escaped {
				state.escaped = false
				i++
				continue
			}
			switch ch {
			case '\\':
				state.escaped = true
			case '"':
				state.stringKind = tomlStringNone
			}
			i++
			continue
		case tomlStringLiteral:
			if ch == '\'' {
				state.stringKind = tomlStringNone
			}
			i++
			continue
		case tomlStringMultilineBasic:
			if state.escaped {
				state.escaped = false
				i++
				continue
			}
			if ch == '\\' {
				state.escaped = true
				i++
				continue
			}
			if ch == '"' {
				run := tomlQuoteRun(line, i, '"')
				if run >= 3 {
					state.stringKind = tomlStringNone
				}
				i += run
				continue
			}
			i++
			continue
		case tomlStringMultilineLiteral:
			if ch == '\'' {
				run := tomlQuoteRun(line, i, '\'')
				if run >= 3 {
					state.stringKind = tomlStringNone
				}
				i += run
				continue
			}
			i++
			continue
		}

		switch ch {
		case '#':
			return i
		case '"':
			run := tomlQuoteRun(line, i, '"')
			switch {
			case run == 1:
				state.stringKind = tomlStringBasic
			case run >= 3 && run < 6:
				state.stringKind = tomlStringMultilineBasic
			}
			i += run
			continue
		case '\'':
			run := tomlQuoteRun(line, i, '\'')
			switch {
			case run == 1:
				state.stringKind = tomlStringLiteral
			case run >= 3 && run < 6:
				state.stringKind = tomlStringMultilineLiteral
			}
			i += run
			continue
		default:
			if outsideString != nil {
				outsideString(ch)
			}
		}
		i++
	}
	return -1
}

func tomlQuoteRun(line string, start int, quote byte) int {
	end := start
	for end < len(line) && line[end] == quote {
		end++
	}
	return end - start
}

func tomlStructuralLineMask(spans []tomlLineSpan) []bool {
	structural := make([]bool, len(spans))
	state := tomlLexState{}
	for i, span := range spans {
		structural[i] = !state.inMultilineString()
		scanTOMLLine(span.text, &state, nil)
	}
	return structural
}

func tomlValueEndSpan(spans []tomlLineSpan, start int) int {
	if start < 0 || start >= len(spans) {
		return start
	}
	_, value, ok := tomlKeyValue(spans[start].text)
	if !ok || !strings.HasPrefix(strings.TrimSpace(value), "[") {
		return start
	}
	depth := 0
	seenArray := false
	state := tomlLexState{}
	for i := start; i < len(spans); i++ {
		closed := false
		scanTOMLLine(spans[i].text, &state, func(ch byte) {
			switch ch {
			case '[':
				seenArray = true
				depth++
			case ']':
				if seenArray {
					depth--
					closed = depth == 0
				}
			}
		})
		if closed {
			return i
		}
	}
	return start
}

func tomlInlineComment(line string) string {
	state := tomlLexState{}
	if i := scanTOMLLine(line, &state, nil); i >= 0 {
		return strings.TrimRight(line[i:], "\r\n")
	}
	return ""
}

func tomlCommentsInSpans(spans []tomlLineSpan, start, end int) []string {
	state := tomlLexState{}
	var comments []string
	for i := start; i <= end; i++ {
		line := spans[i].text
		commentAt := scanTOMLLine(line, &state, nil)
		if commentAt < 0 {
			continue
		}
		indentEnd := 0
		for indentEnd < len(line) && (line[indentEnd] == ' ' || line[indentEnd] == '\t') {
			indentEnd++
		}
		comment := strings.TrimRight(line[commentAt:], "\r\n")
		comments = append(comments, line[:indentEnd]+comment)
	}
	return comments
}

func removeTOMLSection(body, sectionName string) string {
	spans := tomlLineSpans(body)
	for i, span := range spans {
		name, isArray, ok := tomlEditSectionHeader(span.text)
		if !ok || name != sectionName {
			continue
		}
		end := len(body)
		for j := i + 1; j < len(spans); j++ {
			nextName, nextIsArray, ok := tomlEditSectionHeader(spans[j].text)
			if !ok {
				continue
			}
			if (isArray && nextIsArray && nextName == sectionName) || strings.HasPrefix(nextName, sectionName+".") {
				continue
			}
			end = spans[j].start
			break
		}
		return strings.TrimRight(body[:span.start], "\n") + "\n" + body[end:]
	}
	return body
}

func removeTOMLSectionKey(body, sectionName, key string) string {
	spans := tomlLineSpans(body)
	sectionIdx := -1
	keyIdx := -1
	endIdx := len(spans)
	for i, span := range spans {
		name, isArray, ok := tomlEditSectionHeader(span.text)
		if ok {
			if sectionIdx >= 0 {
				endIdx = i
				break
			}
			if !isArray && name == sectionName {
				sectionIdx = i
			}
			continue
		}
		if sectionIdx >= 0 && keyIdx < 0 {
			if got, _, ok := tomlKeyValue(span.text); ok && got == key {
				keyIdx = i
			}
		}
	}
	if sectionIdx < 0 || keyIdx < 0 {
		return body
	}
	keyEndIdx := tomlValueEndSpan(spans, keyIdx)
	for i := sectionIdx + 1; i < endIdx; i++ {
		if i >= keyIdx && i <= keyEndIdx {
			continue
		}
		trimmed := strings.TrimSpace(spans[i].text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return body[:spans[keyIdx].start] + body[spans[keyEndIdx].end:]
	}
	sectionStart := spans[sectionIdx].start
	sectionEnd := len(body)
	if endIdx < len(spans) {
		sectionEnd = spans[endIdx].start
	}
	return strings.TrimRight(body[:sectionStart], "\n") + "\n" + body[sectionEnd:]
}

type tomlLineSpan struct {
	start int
	end   int
	text  string
}

func tomlLineSpans(body string) []tomlLineSpan {
	if body == "" {
		return nil
	}
	var spans []tomlLineSpan
	for start := 0; start < len(body); {
		end := len(body)
		if idx := strings.IndexByte(body[start:], '\n'); idx >= 0 {
			end = start + idx + 1
		}
		spans = append(spans, tomlLineSpan{start: start, end: end, text: body[start:end]})
		start = end
	}
	return spans
}

func tomlEditSectionHeader(line string) (string, bool, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false, false
	}
	if before, _, ok := strings.Cut(trimmed, "#"); ok {
		trimmed = strings.TrimSpace(before)
	}
	if strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]") {
		name := strings.TrimSpace(trimmed[2 : len(trimmed)-2])
		return name, true, name != ""
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		return name, false, name != ""
	}
	return "", false, false
}

func replaceTOMLTopLevelField(body, key, newLine string) string {
	spans := tomlLineSpans(body)
	insertAt := len(body)
	for _, span := range spans {
		if _, _, ok := tomlEditSectionHeader(span.text); ok {
			insertAt = span.start
			break
		}
		if got, ok := tomlTopLevelKey(span.text); ok && got == key {
			return body[:span.start] + newLine + body[span.end:]
		}
	}
	return body[:insertAt] + newLine + body[insertAt:]
}

func tomlTopLevelKey(line string) (string, bool) {
	key, _, ok := tomlKeyValue(line)
	return key, ok
}

func tomlKeyValue(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	if before, _, ok := strings.Cut(trimmed, "#"); ok {
		trimmed = strings.TrimSpace(before)
	}
	key, value, ok := strings.Cut(trimmed, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.Contains(key, ".") {
		return "", "", false
	}
	return key, strings.TrimSpace(value), true
}

func tomlSectionKeyValue(body, sectionName, key string) (string, bool) {
	inSection := false
	for _, span := range tomlLineSpans(body) {
		if name, isArray, ok := tomlEditSectionHeader(span.text); ok {
			inSection = !isArray && name == sectionName
			continue
		}
		if !inSection {
			continue
		}
		got, value, ok := tomlKeyValue(span.text)
		if ok && got == key {
			return value, true
		}
	}
	return "", false
}

func tomlStringLiteralEquals(value, want string) bool {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		quote := value[0]
		if (quote == '"' || quote == '\'') && value[len(value)-1] == quote {
			return value[1:len(value)-1] == want
		}
	}
	return value == want
}

func tomlBodyHasTopLevelKey(body, key string) bool {
	for _, span := range tomlLineSpans(body) {
		if _, _, ok := tomlEditSectionHeader(span.text); ok {
			return false
		}
		if got, ok := tomlTopLevelKey(span.text); ok && got == key {
			return true
		}
	}
	return false
}

func tomlBodyHasSection(body, sectionName string) bool {
	for _, span := range tomlLineSpans(body) {
		name, _, ok := tomlEditSectionHeader(span.text)
		if ok && name == sectionName {
			return true
		}
	}
	return false
}
