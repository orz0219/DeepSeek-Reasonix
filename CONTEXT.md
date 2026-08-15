# Reasonix

The AI coding agent desktop app (Wails). This glossary covers the transcript
rendering domain.

## Language

**Streaming tail**:
The plain-text remnant of the in-progress Markdown block shown after the
rendered portion while output streams.
_Avoid_: tail text, pending text, unfinished text

**Commit boundary**:
The point in streamed text up to which blocks are complete and safe to parse;
everything before it renders as Markdown, everything after rides the tail.
_Avoid_: render point, commit point

**Line freeze**:
The tail property that terminated lines stay fixed and only the final line
grows with new tokens, preventing visual re-flow jitter.
_Avoid_: stable window, freeze

**Fold behavior**:
The reasoning panel's expansion policy, set by a three-state brain icon:
fully open (always expanded, the default), half open (stays expanded through
the whole reply — reasoning and body stream side by side — then collapses when
the reply finishes), or closed (expands only while reasoning streams, collapses
the moment reasoning completes). Replaces the former display-mode setting
(hidden / summary / auto).
_Avoid_: expand mode, reasoning display mode, auto mode
