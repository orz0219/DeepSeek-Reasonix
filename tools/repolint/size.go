package main

import (
	"fmt"
	"regexp"
	"strings"
)

const maxFileLines = 800

// A single-component file's body is essentially one exported function — a
// React component, hook, or factory, plus at most one small companion — so
// the 800-line ceiling would only scatter one responsibility across files.
const maxSingleComponentLines = 5000

var (
	exportFnRe      = regexp.MustCompile(`^export (?:async )?function [A-Za-z_$]`)
	exportDefaultRe = regexp.MustCompile(`^export default (?:async )?function `)
)

// singleComponentFile reports whether a TypeScript file is dominated by one
// exported function declaration (the body runs from the last one to the next
// later top-level export and covers at least half the file).
func singleComponentFile(s *sourceFile) bool {
	if s.isTest() || strings.HasSuffix(s.rel, ".go") {
		return false
	}
	var starts []int
	for i, line := range s.src {
		trimmed := strings.TrimSpace(line)
		if exportFnRe.MatchString(trimmed) || exportDefaultRe.MatchString(trimmed) {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 || len(starts) > 2 {
		return false
	}
	end := len(s.src)
	for i := starts[len(starts)-1] + 1; i < len(s.src); i++ {
		trimmed := strings.TrimSpace(s.src[i])
		if strings.HasPrefix(trimmed, "export ") &&
			!exportFnRe.MatchString(trimmed) && !exportDefaultRe.MatchString(trimmed) {
			end = i
			break
		}
	}
	body := end - starts[len(starts)-1]
	return body*2 >= len(s.src)
}

// Translation tables are data, not code: they grow one entry per UI string, so
// a ceiling there fires on every new label without ever pointing at something
// worth splitting.
var sizeExemptPrefixes = []string{
	"desktop/frontend/src/locales/",
}

func sizeExempt(rel string) bool {
	for _, prefix := range sizeExemptPrefixes {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

func checkSize(s *sourceFile) []Finding {
	if s.lines <= maxFileLines || sizeExempt(s.rel) {
		return nil
	}
	if s.lines <= maxSingleComponentLines && singleComponentFile(s) {
		return nil
	}
	rule := ruleFileSize
	if s.isTest() {
		rule = ruleTestSize
	}
	return []Finding{{s.rel, 1, rule,
		fmt.Sprintf("%d lines exceeds the %d-line ceiling", s.lines, maxFileLines), s.lines - maxFileLines}}
}
