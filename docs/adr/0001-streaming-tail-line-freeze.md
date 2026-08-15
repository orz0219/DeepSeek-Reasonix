# Streamed message rendering keeps the tail line-frozen

Status: proposed

Streaming assistant output is rendered block-by-block (completed blocks parse
into Markdown, the in-progress block stays as a plain-text tail) and the tail
freezes every terminated line — only its final line grows with new tokens.
An unclosed code fence or display-math block no longer triggers a full-document
re-parse for live highlighting; the whole open block rides the plain-text tail
until it closes, then renders in one swap.

We chose visual stability over live highlighting: streaming a long code block
re-parsed and re-rendered the entire document every commit interval, and a
long unwrapped paragraph kept re-wrapping its tail's last line — both read as
"the last line is violently jittering". Line-freezing means the already-typed
lines never move; only the final line appends. The trade-off is that code
blocks are not syntax-highlighted until they finish, which we judged worth it
because the finished state arrives moments later and renders atomically.

Rejected alternatives: plain-text-only typing with a single end-of-stream
Markdown swap (too much latency for long replies), and keeping live
highlighting with throttled full re-parses (the jitter we set out to kill).
The tail is also dimmed (~60-70% opacity) so the unfinished region reads as
"not yet settled" rather than as a rendering glitch.
