package agent

import (
	"encoding/json"
)

// parsedToolArgs caches the result of unmarshaling a tool call's JSON arguments.
// It is populated once per tool call in parseToolCall and reused by evidence
// recording, permission classification, and mutation analysis to avoid 4-5
// redundant json.Unmarshal calls on the same bytes.
type parsedToolArgs struct {
	raw json.RawMessage
	// fields is the lazily-unmarshaled map. Nil until first access.
	fields map[string]json.RawMessage
	// err records an unmarshal failure so callers don't retry.
	err error
}

func newParsedToolArgs(raw json.RawMessage) *parsedToolArgs {
	return &parsedToolArgs{raw: raw}
}

// Fields returns the unmarshaled argument map, parsing on first call.
func (p *parsedToolArgs) Fields() (map[string]json.RawMessage, error) {
	if p == nil {
		return nil, nil
	}
	if p.fields != nil {
		return p.fields, p.err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(p.raw, &fields); err != nil {
		p.err = err
		return nil, err
	}
	p.fields = fields
	return fields, nil
}

// StringField returns the string value of a key, or "" if missing/not a string.
func (p *parsedToolArgs) StringField(key string) string {
	fields, err := p.Fields()
	if err != nil || fields == nil {
		return ""
	}
	if raw, ok := fields[key]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return ""
}
