package serve

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// events streams the controller's event flow as SSE until the client
// disconnects. Each event is one `data:` frame of the JSON wire form.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var ch <-chan []byte
	var unsubscribe func()

	s.ctl().ReplayPendingPromptsWith(func() event.Sink {
		ch, unsubscribe = s.bc.Subscribe()
		return event.FuncSink(func(e event.Event) {
			s.bc.EmitTo(ch, e)
		})
	})
	defer unsubscribe()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:

			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// submit runs raw user input as a turn (slash commands and @-references
// resolved by the controller). Returns 202 — output arrives on the event stream.
// An optional "format":"json_object" asks the model for structured JSON output
// on this turn (text.format on the wire).
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input  string `json:"input"`
		Format string `json:"format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Input == "" {
		http.Error(w, "missing input", http.StatusBadRequest)
		return
	}
	body.Format = strings.TrimSpace(body.Format)
	switch body.Format {
	case "", "json_object":

	default:
		http.Error(w, `unsupported format (supported: "json_object")`, http.StatusBadRequest)
		return
	}
	trimmed := strings.TrimSpace(body.Input)
	if strings.HasPrefix(trimmed, "!") {
		http.Error(w, "shell commands are unavailable over HTTP", http.StatusForbidden)
		return
	}

	if strings.HasPrefix(trimmed, "/model ") {
		ref := strings.TrimSpace(strings.TrimPrefix(trimmed, "/model"))
		if ref != "" {
			if err := s.switchModel(r.Context(), ref); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	if strings.HasPrefix(trimmed, "/effort ") {
		level := strings.TrimSpace(strings.TrimPrefix(trimmed, "/effort"))
		if level != "" {
			if err := s.switchEffort(r.Context(), level); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	s.bindMu.Lock()
	ctrl := s.ctl()

	if ctrl.Running() {
		s.bindMu.Unlock()
		http.Error(w, "session is busy; use POST /inbox/items for durable follow-up", http.StatusConflict)
		return
	}
	ctrl.SubmitHTTPFormat(body.Input, body.Format)

	if !ctrl.Running() && !ctrl.RuntimeStatus().PendingPrompt {
		s.bindMu.Unlock()
		http.Error(w, "input was not admitted; session is rotating, closed, or finishing — use POST /inbox/items", http.StatusConflict)
		return
	}
	s.bindMu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) cancel(w http.ResponseWriter, _ *http.Request) {
	s.ctl().Cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Allow   bool   `json:"allow"`
		Session bool   `json:"session"`
		Persist bool   `json:"persist"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	s.ctl().Approve(body.ID, body.Allow, body.Session, body.Persist)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.ctl().SetPlanMode(body.On)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	if err := s.ctl().Compact(r.Context(), ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.ctl().Snapshot(); err != nil {
		slog.Warn("serve: snapshot after compact", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) newSession(w http.ResponseWriter, _ *http.Request) {

	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	if err := s.ctl().NewSession(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.bc.ResetSession()

	if err := s.rebindSessionLease(s.ctl().SessionPath()); err != nil {
		http.Error(w, sessionInUseError(err), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type historyToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type historyMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Reasoning  string            `json:"reasoning,omitempty"`
	ToolCalls  []historyToolCall `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
	ToolName   string            `json:"toolName,omitempty"`
}

func historyMessages(msgs []provider.Message) []historyMessage {
	out := make([]historyMessage, 0, len(msgs))
	for _, m := range msgs {

		if m.Role == provider.RoleUser {
			if steerText, isSteer := agent.SteerText(m.Content); isSteer {
				out = append(out, historyMessage{Role: "notice", Content: "↪ " + steerText})
				continue
			}
		}
		hm := historyMessage{Role: string(m.Role), Content: m.Content}
		if m.Role == provider.RoleAssistant {
			hm.Reasoning = m.ReasoningContent
			if len(m.ToolCalls) > 0 {
				hm.ToolCalls = make([]historyToolCall, len(m.ToolCalls))
				for i, tc := range m.ToolCalls {
					hm.ToolCalls[i] = historyToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
				}
			}
		}
		if m.Role == provider.RoleTool {
			hm.ToolCallID = m.ToolCallID
			hm.ToolName = m.Name
		}
		out = append(out, hm)
	}
	return out
}

// history returns the session's message log so a reconnecting client can
// repopulate its transcript, including historical tool cards. Supports ETag caching:
// if the client sends If-None-Match with the current ETag, the server returns
// 304 Not Modified with no body, saving bandwidth on reconnects.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	writeJSONCached(w, r, historyMessages(s.ctl().History()))
}

// context returns the prompt-vs-window gauge numbers. Supports ETag caching
// so reconnecting clients avoid re-fetching unchanged context data.
func (s *Server) context(w http.ResponseWriter, r *http.Request) {
	used, window := s.ctl().ContextSnapshot()
	writeJSONCached(w, r, map[string]int{"used": used, "window": window})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("serve: writeJSON encode failed", "err", err)
	}
}

// writeJSONCached encodes v as JSON, computes a weak ETag from the body, and
// returns 304 Not Modified if the client's If-None-Match matches. This avoids
// re-sending unchanged history/context payloads on every reconnect.
func writeJSONCached(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Warn("serve: writeJSONCached marshal failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(body))
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	_, _ = w.Write(body)
}

// corsMiddleware adds CORS headers for a specific allowed origin. Only use for
// local development — the server has no auth, so broad CORS would let any site
// drive the agent. origin is the exact origin to allow (e.g.
// "http://localhost:5173"); empty origin skips CORS entirely.
func corsMiddleware(next http.Handler, origin string) http.Handler {
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logMiddleware logs each request's method, path, and status.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		slog.Info("serve: request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start).String(),
		)
	})
}

// responseWriter captures the status code for logging.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying ResponseWriter if it supports flushing
// (required for SSE /events). Without this the type assertion in the events
// handler fails and the stream endpoint returns 500.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rewind rewinds the session to a checkpoint.
func (s *Server) rewind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Turn  int    `json:"turn"`
		Scope string `json:"scope"` // "code", "conversation", "both"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Turn < 0 {
		http.Error(w, "missing turn", http.StatusBadRequest)
		return
	}
	scope := control.RewindBoth
	switch body.Scope {
	case "code":
		scope = control.RewindCode
	case "conversation":
		scope = control.RewindConversation
	}
	if err := s.ctl().Rewind(body.Turn, scope); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
