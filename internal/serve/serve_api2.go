package serve

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/store"
)

// fork creates a new branch at a checkpoint.
func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Turn int    `json:"turn"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Turn < 0 {
		http.Error(w, "missing turn", http.StatusBadRequest)
		return
	}

	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	path, err := s.ctl().ForkNamed(body.Turn, body.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.bc.ResetSession()

	if err := s.rebindSessionLease(s.ctl().SessionPath()); err != nil {
		http.Error(w, sessionInUseError(err), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]string{"path": path})
}

// summarize runs summarize-from or summarize-up-to on a turn.
func (s *Server) summarize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Turn int    `json:"turn"`
		Mode string `json:"mode"` // "from" or "upto"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Turn < 0 {
		http.Error(w, "missing turn", http.StatusBadRequest)
		return
	}
	var err error
	switch body.Mode {
	case "from":
		err = s.ctl().SummarizeFrom(r.Context(), body.Turn)
	case "upto":
		err = s.ctl().SummarizeUpTo(r.Context(), body.Turn)
	default:
		http.Error(w, "mode must be 'from' or 'upto'", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// autoApproveTools toggles YOLO/full-access tool auto-approval.
func (s *Server) autoApproveTools(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.ctl().SetAutoApproveTools(body.On)
	w.WriteHeader(http.StatusNoContent)
}

// toolApprovalMode selects ask, auto, or yolo approval behavior for interactive
// frontends. Plan remains a separate workflow governed by the selected mode.
func (s *Server) toolApprovalMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	switch strings.ToLower(strings.TrimSpace(body.Mode)) {
	case control.ToolApprovalAsk, control.ToolApprovalAuto, control.ToolApprovalYolo:
		s.ctl().SetToolApprovalMode(body.Mode)
	default:
		http.Error(w, "mode must be ask, auto, or yolo", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bypass is the legacy HTTP endpoint for YOLO/full-access tool auto-approval.
func (s *Server) bypass(w http.ResponseWriter, r *http.Request) {
	s.autoApproveTools(w, r)
}

// goal sets or clears the active goal. An empty goal string clears it.
// Setting a non-empty goal disables plan mode (matching the desktop behavior).
func (s *Server) goal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Goal string `json:"goal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	goal := strings.TrimSpace(body.Goal)
	if goal == "" {
		s.ctl().ClearGoal()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.ctl().SetPlanMode(false)
	s.ctl().SetGoal(goal)
	w.WriteHeader(http.StatusNoContent)
}

// answer responds to an ask_request.
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string            `json:"id"`
		Answers []event.AskAnswer `json:"answers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	s.ctl().AnswerQuestion(body.ID, body.Answers)
	w.WriteHeader(http.StatusNoContent)
}

// resume loads a previous session from a JSONL file.
func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	dir := s.ctl().SessionDir()
	if dir == "" {
		http.Error(w, "sessions disabled", http.StatusBadRequest)
		return
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		http.Error(w, "invalid session dir", http.StatusBadRequest)
		return
	}
	realDir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		http.Error(w, "invalid session dir", http.StatusBadRequest)
		return
	}
	absPath, err := filepath.Abs(strings.TrimSpace(body.Path))
	if err != nil || !store.IsSessionTranscriptName(filepath.Base(absPath)) {
		http.Error(w, "invalid session path", http.StatusBadRequest)
		return
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		http.Error(w, "invalid session path", http.StatusBadRequest)
		return
	}
	if realPath == realDir || !strings.HasPrefix(realPath, realDir+string(os.PathSeparator)) {
		http.Error(w, "path outside session dir", http.StatusForbidden)
		return
	}
	if agent.IsCleanupPending(realPath) {
		http.Error(w, "session is pending cleanup", http.StatusBadRequest)
		return
	}

	s.bindMu.Lock()
	defer s.bindMu.Unlock()

	if err := s.ctl().Snapshot(); err != nil {
		slog.Warn("serve: snapshot before resume", "err", err)
	}

	if s.leases != nil {
		if err := s.leases.Rebind(realPath); err != nil {
			if errors.Is(err, agent.ErrSessionLeaseHeld) {
				http.Error(w, sessionInUseError(err), http.StatusConflict)
			} else {
				http.Error(w, "session lease: "+err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	loaded, err := agent.LoadSession(realPath)
	if err != nil {

		_ = s.rebindSessionLease(s.ctl().SessionPath())
		http.Error(w, "load session: "+err.Error(), http.StatusBadRequest)
		return
	}
	if hook := resumeBindHookForTest; hook != nil {
		hook()
	}
	s.ctl().Resume(loaded, realPath)
	if ctrl, ok := s.ctl().(*control.Controller); ok && s.leases != nil {
		if err := s.leases.BindControllerAuthority(ctrl); err != nil {
			http.Error(w, "session authority: unable to bind resumed session", http.StatusInternalServerError)
			return
		}
	}
	s.bc.ResetSession()
	w.WriteHeader(http.StatusNoContent)
}

// checkpoints returns the session's checkpoint list for the rewind picker.
func (s *Server) checkpoints(w http.ResponseWriter, _ *http.Request) {
	type cp struct {
		Turn   int    `json:"turn"`
		Prompt string `json:"prompt"`
		Files  int    `json:"files"`
	}
	raw := s.ctl().Checkpoints()
	out := make([]cp, len(raw))
	for i, c := range raw {
		out[i] = cp{Turn: c.Turn, Prompt: c.Prompt, Files: len(c.Paths)}
	}
	writeJSON(w, out)
}

// branches returns the branch list and tree text.
func (s *Server) branches(w http.ResponseWriter, _ *http.Request) {
	branches, err := s.ctl().Branches()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tree := s.ctl().BranchTreeText()
	writeJSON(w, map[string]any{"branches": branches, "tree": tree})
}

// models lists configured chat models for the browser model picker.
func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type modelEntry struct {
		Ref      string `json:"ref"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Kind     string `json:"kind,omitempty"`
		Active   bool   `json:"active,omitempty"`
		Default  bool   `json:"default,omitempty"`
	}
	ctrl := s.ctl()
	current := currentModelRef(ctrl)
	label := ctrl.Label()
	modelCounts := make(map[string]int)
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Configured() {
			continue
		}
		models := p.ChatModelList()
		if len(models) == 0 {
			models = p.ModelList()
		}
		for _, model := range models {
			modelCounts[model]++
		}
	}
	var out []modelEntry
	seen := make(map[string]struct{})
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Configured() {
			continue
		}
		models := p.ChatModelList()
		if len(models) == 0 {
			models = p.ModelList()
		}
		for _, model := range models {
			ref := p.Name + "/" + model
			seen[ref] = struct{}{}
			active := ref == current || p.Name == current
			if !active && current == label && model == label {
				if modelCounts[model] == 1 {
					active = true
				} else {
					active = ref == cfg.DefaultModel
				}
			}
			out = append(out, modelEntry{
				Ref:      ref,
				Provider: p.Name,
				Model:    model,
				Kind:     p.Kind,
				Active:   active,
				Default:  ref == cfg.DefaultModel || p.Name == cfg.DefaultModel,
			})
		}
	}

	for _, d := range ctrl.ProviderCatalog() {
		ref := strings.TrimSpace(d.Ref)
		if ref == "" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		parts := strings.Split(ref, "/")
		if len(parts) < 4 || parts[0] != "plugin" {

			continue
		}
		providerName := strings.Join(parts[:3], "/")
		model := strings.TrimSpace(d.Model)
		if model == "" {
			model = parts[len(parts)-1]
		}
		out = append(out, modelEntry{
			Ref:      ref,
			Provider: providerName,
			Model:    model,
			Kind:     "extension",
			Active:   ref == current,
		})
	}
	if out == nil {
		out = []modelEntry{}
	}
	writeJSON(w, map[string]any{"current": current, "label": label, "default": cfg.DefaultModel, "models": out})
}

func currentModelRef(c control.SessionAPI) string {
	ref := strings.TrimSpace(c.ModelRef())
	if ref != "" {
		return ref
	}
	return strings.TrimSpace(c.Label())
}

// status returns a combined status snapshot.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	used, window := s.ctl().ContextSnapshot()
	hit, miss := s.ctl().SessionCache()
	sess := map[string]any{
		"label":            s.ctl().Label(),
		"running":          s.ctl().Running(),
		"plan":             s.ctl().PlanMode(),
		"autoApproveTools": s.ctl().AutoApproveTools(),
		"bypass":           s.ctl().AutoApproveTools(),
		"toolApprovalMode": s.ctl().ToolApprovalMode(),
		"goal":             s.ctl().Goal(),
		"goalStatus":       s.ctl().GoalStatus(),
		"cwd":              s.ctl().SessionDir(),
		"used":             used,
		"window":           window,
		"cacheHit":         hit,
		"cacheMiss":        miss,
	}
	if u := s.ctl().LastUsage(); u != nil {
		sess["lastUsage"] = u
	}
	if b, err := s.ctl().Balance(r.Context()); err == nil && b != nil {
		if cfg, loadErr := config.Load(); loadErr == nil && cfg.DisplayCurrencyPref() == "" {

			s.bc.SetDisplayCurrency(b.PrimaryCurrency())
		}
		sess["balance"] = map[string]any{
			"display":   b.Display(),
			"available": b.Available,
			"infos":     b.Infos,
		}
	} else if err != nil {
		slog.Warn("serve: balance fetch failed", "err", err)
	}
	sess["sessionCostQuote"] = s.bc.SessionCostQuote()
	if j := s.ctl().Jobs(); len(j) > 0 {
		sess["jobs"] = j
	}
	writeJSON(w, sess)
}
