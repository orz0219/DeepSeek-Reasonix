package agent

import (
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
)

// mid-turn steer marker.
// MidTurnSteerPrefix marks user messages that were injected mid-turn as
// guidance (via Steer). The model sees them as instructions; frontends
// display them as a notice, not a regular user bubble.
const MidTurnSteerPrefix = "[Mid-turn steer queued by the user. Do not treat this as a new task; use it only as additional guidance for the current task after completing the current step.]"

func midTurnSteerMessage(text string) string {
	return MidTurnSteerPrefix + "\n" + text
}

// SteerText checks whether content is a mid-turn steer message and, if so,
// returns the original user text without the wrapper prefix. The returned
// text preserves the user's exact input — it only strips the prefix and the
// "\n" separator that midTurnSteerMessage inserts between the prefix and the
// user text; it does not trim spaces so the history replay matches the live
// Steer event rendering character-for-character.
//
// Steers are persisted through withTurnPreferences, which can prepend
// transient language blocks (for Chinese text even in auto mode) and append
// the delivery-runtime marker. Both are transport framing, not steer text:
// leading blocks are skipped before matching the prefix and a trailing
// marker is cut from the returned text, so replay recognizes steers
// regardless of the session's language and profile settings.
func SteerText(content string) (string, bool) {
	s := content
	for {
		if after, found := strings.CutPrefix(s, MidTurnSteerPrefix); found {

			after = strings.TrimPrefix(after, "\n")
			if trimmed, cut := strings.CutSuffix(after, "\n\n"+DeliveryRuntimeMarker); cut {
				after = trimmed
			}
			return after, true
		}
		next, ok := trimLeadingSteerWrapper(s)
		if !ok {
			return "", false
		}
		s = next
	}
}

// trimLeadingSteerWrapper removes one leading transient preference block that
// withTurnPreferences may have placed ahead of the steer prefix. It reports
// false when content does not start with such a block.
func trimLeadingSteerWrapper(content string) (string, bool) {
	s := strings.TrimLeft(content, " \t\r\n")
	for _, tag := range []string{"response-language", "reasoning-language"} {
		if !strings.HasPrefix(s, "<"+tag+">") {
			continue
		}
		if rest, ok := trimLeadingTransientBlock(s, tag); ok {
			return rest, true
		}
	}
	return content, false
}

// steerEntry is one mid-turn guidance admission.
type steerEntry struct {
	itemID string
	load   func() (string, error)
	// text is a fallback when load is nil (legacy Steer(string) path).
	text string
}

// Steer queues a message for mid-turn injection. It reports whether an active
// turn accepted the text; on false nothing was queued and the caller must
// deliver it another way (typically as a new turn). Without the active check,
// a steer landing in the window between the turn's exit flush and the
// controller observing running=false would sit in the queue unconsumed and
// unpersisted — invisible to both the model and history.
func (a *Agent) Steer(text string) bool {
	return a.SteerItem("", func() (string, error) { return text, nil })
}

// SteerItem queues durable-inbox guidance identified by itemID. load is called
// only when the entry is consumed so the agent does not retain every body.
func (a *Agent) SteerItem(itemID string, load func() (string, error)) bool {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	if !a.steerRunActive {
		return false
	}
	a.steerQueue = append(a.steerQueue, steerEntry{itemID: itemID, load: load})
	a.steerConsumed = false
	return true
}

// SteerConsumed returns true when the steer queue became empty after the last consume.
func (a *Agent) SteerConsumed() bool {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	return a.steerConsumed
}

// SetSink replaces the agent's event sink. Controllers use this to wrap the
// sink after construction (e.g. durable inbox observation) without rebuilding
// the agent.
func (a *Agent) SetSink(sink event.Sink) {
	if a == nil {
		return
	}
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	a.svc.sink = sink
}

func (a *Agent) consumeSteer() (text, itemID string, ok bool) {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	if len(a.steerQueue) == 0 {
		return "", "", false
	}
	e := a.steerQueue[0]
	a.steerQueue = a.steerQueue[1:]
	a.steerConsumed = len(a.steerQueue) == 0
	if e.load != nil {
		t, err := e.load()
		if err != nil {
			return "", e.itemID, false
		}
		return t, e.itemID, true
	}
	return e.text, e.itemID, true
}

// closeSteerIntakeIfIdle atomically closes the normal-completion race between
// the final queue check and Run returning. A steer accepted before this check
// keeps the loop alive; one arriving after it is rejected so the host can keep
// the user's draft and retry it as a regular follow-up.
func (a *Agent) closeSteerIntakeIfIdle() bool {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	if len(a.steerQueue) > 0 {
		return false
	}
	a.steerRunActive = false
	return true
}

// flushSteerQueue ends the turn's steer intake. Guidance that arrived too late
// to be consumed is persisted for transcript visibility but marked local-only:
// replaying it to the model on the next unrelated user turn can execute a stale
// historical task (#7045). An explicit warning keeps the transcript honest
// without presenting the text as successfully applied guidance (#6238).
func (a *Agent) flushSteerQueue() {
	a.steerMu.Lock()
	pending := a.steerQueue
	a.steerQueue = nil
	if len(pending) > 0 {
		a.steerConsumed = true
	}
	a.steerRunActive = false
	a.steerMu.Unlock()
	for _, e := range pending {
		text := e.text
		if e.load != nil {
			if t, err := e.load(); err == nil {
				text = t
			}
		}
		a.RecordUnappliedSteer(text, e.itemID)
	}
}

// UnappliedSteerNotice returns the durable warning shown for guidance that was
// accepted during an abnormal turn exit but never reached a provider request.
func UnappliedSteerNotice(text string) string {
	return "Guidance was not applied because the turn ended before it could be processed. Send it again if it is still needed:\n" + text
}

// RecordUnappliedSteer stores guidance that could not affect its intended
// in-flight turn. The orphan-tool sentinel makes older readers drop the record
// during wire normalization, while current readers use LocalOnly to exclude it
// before every provider request. itemID correlates the notice with the durable
// session inbox entry when one exists.
func (a *Agent) RecordUnappliedSteer(text string, itemID ...string) {
	if a == nil || a.sess.conversation == nil {
		return
	}
	id := ""
	if len(itemID) > 0 {
		id = itemID[0]
	}
	a.sess.conversation.Add(provider.Message{
		Role:       provider.RoleTool,
		Content:    a.withTurnPreferences(midTurnSteerMessage(text)),
		ToolCallID: provider.LocalOnlyToolID,
		Name:       provider.LocalOnlyToolName,
		LocalOnly:  true,
	})
	a.svc.sink.Emit(event.Event{
		Kind:   event.Notice,
		Level:  event.LevelWarn,
		Code:   event.NoticeCodeUnappliedSteer,
		Text:   UnappliedSteerNotice(text),
		ItemID: id,
	})
}

func (a *Agent) steerQueueLen() int {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	return len(a.steerQueue)
}
