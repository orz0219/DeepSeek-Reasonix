package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// HostUI is the sidecar's client for the host's structured UI surfaces. The
// zero value is ready to use; every method takes the context of an SDK
// callback (interceptor, observer, provider, UI, or shutdown) and fails with
// ErrNoConnection otherwise, and with ErrNotReady before the handshake
// barrier opens. Surfaces are structured-only by design: there is no way to
// send HTML, CSS, JavaScript, or URLs.
type HostUI struct{}

// uiAnswerKey is the field key the host uses for single-field prompts.
const uiAnswerKey = "value"

// PublishStatus publishes or replaces a one-line status surface.
func (HostUI) PublishStatus(ctx context.Context, sessionID string, generation uint64, surfaceID string, p UIStatusPayload) error {
	if strings.TrimSpace(p.Label) == "" {
		return errors.New("extension: status payload requires a label")
	}
	if !validUISeverity(p.Severity) {
		return fmt.Errorf("extension: invalid severity %q", p.Severity)
	}
	return publishSurface(ctx, sessionID, generation, surfaceID, UISurfaceStatus, p)
}

// PublishCard publishes or replaces a rich read-only card surface.
func (HostUI) PublishCard(ctx context.Context, sessionID string, generation uint64, surfaceID string, p UICardPayload) error {
	for i, field := range p.Fields {
		if strings.TrimSpace(field.Key) == "" {
			return fmt.Errorf("extension: card field %d requires a key", i)
		}
	}
	for i, action := range p.Actions {
		if strings.TrimSpace(action.ActionID) == "" || strings.TrimSpace(action.Label) == "" {
			return fmt.Errorf("extension: card action %d requires an actionId and label", i)
		}
	}
	return publishSurface(ctx, sessionID, generation, surfaceID, UISurfaceCard, p)
}

// PublishForm publishes or replaces an editable form surface; submissions
// return through the Options.UI.Submit callback.
func (HostUI) PublishForm(ctx context.Context, sessionID string, generation uint64, surfaceID string, p UIFormPayload) error {
	if err := validateFormPayload(p); err != nil {
		return err
	}
	return publishSurface(ctx, sessionID, generation, surfaceID, UISurfaceForm, p)
}

// PublishNotification publishes a transient toast-style message.
func (HostUI) PublishNotification(ctx context.Context, sessionID string, generation uint64, surfaceID string, p UINotificationPayload) error {
	if strings.TrimSpace(p.Title) == "" {
		return errors.New("extension: notification payload requires a title")
	}
	if !validUISeverity(p.Severity) {
		return fmt.Errorf("extension: invalid severity %q", p.Severity)
	}
	return publishSurface(ctx, sessionID, generation, surfaceID, UISurfaceNotification, p)
}

func publishSurface(ctx context.Context, sessionID string, generation uint64, surfaceID string, kind UISurfaceKind, payload any) error {
	s := serverFrom(ctx)
	if s == nil {
		return ErrNoConnection
	}
	if strings.TrimSpace(surfaceID) == "" || strings.TrimSpace(sessionID) == "" {
		return errors.New("extension: surfaceId and sessionId are required")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("extension: marshal %s payload: %w", kind, err)
	}
	resultRaw, err := s.callHost(ctx, MethodHostUIPublish, UIPublishParams{
		SurfaceID: surfaceID, SessionID: sessionID, Generation: generation, Kind: kind, Payload: raw,
	})
	if err != nil {
		return err
	}
	var result UIPublishResult
	if err := strictDecode(resultRaw, &result); err != nil {
		return &ProtocolError{Reason: ErrProtocolError, Message: "invalid host/ui/publish result"}
	}
	if !result.Accepted {
		return fmt.Errorf("extension: host rejected the %s surface %q", kind, surfaceID)
	}
	return nil
}

// InputPrompt configures RequestInput.
type InputPrompt struct {
	Title    string
	Message  string
	Label    string
	Default  string
	Required bool
}

// SelectPrompt configures RequestSelect.
type SelectPrompt struct {
	Title    string
	Message  string
	Label    string
	Options  []string
	Default  string
	Required bool
}

// MultiSelectPrompt configures RequestMultiSelect.
type MultiSelectPrompt struct {
	Title    string
	Message  string
	Label    string
	Options  []string
	Required bool
}

// RequestConfirm blocks on a yes/no prompt; the bool is the user's answer.
// A dismissed prompt returns ErrUICancelled.
func (h HostUI) RequestConfirm(ctx context.Context, sessionID string, generation uint64, surfaceID, message string) (bool, error) {
	form := UIFormPayload{
		Message: message,
		Fields:  []UIFormField{{Key: uiAnswerKey, Label: message, Kind: UIFieldConfirm}},
	}
	values, err := h.requestPrompt(ctx, sessionID, generation, surfaceID, UIRequestConfirm, form)
	if err != nil {
		return false, err
	}
	answer, _ := values[uiAnswerKey].(bool)
	return answer, nil
}

// RequestInput blocks on a free-text prompt and returns the entered text.
func (h HostUI) RequestInput(ctx context.Context, sessionID string, generation uint64, surfaceID string, p InputPrompt) (string, error) {
	field := UIFormField{Key: uiAnswerKey, Label: p.Label, Kind: UIFieldInput, Required: p.Required}
	if p.Default != "" {
		field.Default = p.Default
	}
	values, err := h.requestPrompt(ctx, sessionID, generation, surfaceID, UIRequestInput, UIFormPayload{
		Title: p.Title, Message: p.Message, Fields: []UIFormField{field},
	})
	if err != nil {
		return "", err
	}
	answer, _ := values[uiAnswerKey].(string)
	return answer, nil
}

// RequestSelect blocks on a single-choice prompt and returns the picked
// option.
func (h HostUI) RequestSelect(ctx context.Context, sessionID string, generation uint64, surfaceID string, p SelectPrompt) (string, error) {
	if len(p.Options) == 0 {
		return "", errors.New("extension: select prompt requires options")
	}
	field := UIFormField{Key: uiAnswerKey, Label: p.Label, Kind: UIFieldSelect, Options: p.Options, Required: p.Required}
	if p.Default != "" {
		field.Default = p.Default
	}
	values, err := h.requestPrompt(ctx, sessionID, generation, surfaceID, UIRequestSelect, UIFormPayload{
		Title: p.Title, Message: p.Message, Fields: []UIFormField{field},
	})
	if err != nil {
		return "", err
	}
	answer, _ := values[uiAnswerKey].(string)
	return answer, nil
}

// RequestMultiSelect blocks on a multi-choice prompt and returns the picked
// options.
func (h HostUI) RequestMultiSelect(ctx context.Context, sessionID string, generation uint64, surfaceID string, p MultiSelectPrompt) ([]string, error) {
	if len(p.Options) == 0 {
		return nil, errors.New("extension: multiselect prompt requires options")
	}
	field := UIFormField{Key: uiAnswerKey, Label: p.Label, Kind: UIFieldMultiselect, Options: p.Options, Required: p.Required}
	values, err := h.requestPrompt(ctx, sessionID, generation, surfaceID, UIRequestMultiselect, UIFormPayload{
		Title: p.Title, Message: p.Message, Fields: []UIFormField{field},
	})
	if err != nil {
		return nil, err
	}
	switch answer := values[uiAnswerKey].(type) {
	case []string:
		return answer, nil
	case []any:
		out := make([]string, 0, len(answer))
		for _, item := range answer {
			text, ok := item.(string)
			if !ok {
				return nil, &ProtocolError{Reason: ErrProtocolError, Message: "host/ui/request multiselect answer is not a string list"}
			}
			out = append(out, text)
		}
		return out, nil
	case nil:
		return []string{}, nil
	default:
		return nil, &ProtocolError{Reason: ErrProtocolError, Message: "host/ui/request multiselect answer is not a string list"}
	}
}

// RequestForm blocks on a fully custom form prompt and returns all values
// keyed by field key. It is the structured escape hatch behind the typed
// prompt helpers.
func (h HostUI) RequestForm(ctx context.Context, sessionID string, generation uint64, surfaceID string, form UIFormPayload) (map[string]any, error) {
	if err := validateFormPayload(form); err != nil {
		return nil, err
	}
	return h.requestPrompt(ctx, sessionID, generation, surfaceID, UIRequestInput, form)
}

func (h HostUI) requestPrompt(ctx context.Context, sessionID string, generation uint64, surfaceID string, kind UIRequestKind, form UIFormPayload) (map[string]any, error) {
	s := serverFrom(ctx)
	if s == nil {
		return nil, ErrNoConnection
	}
	if strings.TrimSpace(surfaceID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("extension: surfaceId and sessionId are required")
	}
	raw, err := json.Marshal(form)
	if err != nil {
		return nil, fmt.Errorf("extension: marshal %s payload: %w", kind, err)
	}
	resultRaw, err := s.callHost(ctx, MethodHostUIRequest, UIRequestParams{
		SurfaceID: surfaceID, SessionID: sessionID, Generation: generation, Kind: kind, Payload: raw,
	})
	if err != nil {
		return nil, err
	}
	var result UIRequestResult
	if err := strictDecode(resultRaw, &result); err != nil {
		return nil, &ProtocolError{Reason: ErrProtocolError, Message: "invalid host/ui/request result"}
	}
	if result.Cancelled {
		return nil, ErrUICancelled
	}
	return result.Values, nil
}

func validateFormPayload(p UIFormPayload) error {
	if p.Fields == nil {
		return errors.New("extension: form payload requires a fields array (possibly empty)")
	}
	for i, field := range p.Fields {
		if strings.TrimSpace(field.Key) == "" {
			return fmt.Errorf("extension: form field %d requires a key", i)
		}
		if !validUIFieldKind(field.Kind) {
			return fmt.Errorf("extension: form field %q has invalid kind %q", field.Key, field.Kind)
		}
	}
	return nil
}
