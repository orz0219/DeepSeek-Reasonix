package config

import (
	"errors"
	"fmt"
	"strings"
)

type promptFileSource uint8

const (
	promptFileSourceUnknown promptFileSource = iota
	promptFileSourceUser
	promptFileSourceProject
)

type systemPromptFileError struct {
	configured string
	candidates []string
	errors     []error
	allMissing bool
}

func (e *systemPromptFileError) Error() string {
	detail := "could not be read from any configured location"
	if e.allMissing {
		detail = "not found at any configured location"
	}
	message := fmt.Sprintf("system_prompt_file %q %s: %s", e.configured, detail, strings.Join(e.candidates, ", "))
	if !e.allMissing && len(e.errors) > 0 {
		message += ": " + errors.Join(e.errors...).Error()
	}
	return message
}

func (e *systemPromptFileError) Unwrap() error { return errors.Join(e.errors...) }

// IsMissingSystemPromptFile reports whether every allowed location for a
// configured prompt file was absent. Permission, containment, and other I/O
// failures deliberately return false so callers do not start without an
// explicitly configured prompt.
func IsMissingSystemPromptFile(err error) bool {
	var target *systemPromptFileError
	return errors.As(err, &target) && target.allMissing
}
