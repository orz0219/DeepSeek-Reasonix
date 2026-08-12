package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ReadContentRef pages one whole content ref back from the host in
// ContentRefChunkBytes chunks, verifies the reassembled byte count and
// SHA-256 against the host's own report, and fails on any inconsistency. An
// expired or unknown ref returns a *ProtocolError with Reason
// ErrContentRefExpired.
func ReadContentRef(ctx context.Context, ref string) ([]byte, error) {
	s := serverFrom(ctx)
	if s == nil {
		return nil, ErrNoConnection
	}
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("extension: content ref is required")
	}
	var out []byte
	var offset int64
	for {
		raw, err := s.callHost(ctx, MethodHostContentRead, ContentReadParams{ContentRef: ref, Offset: offset})
		if err != nil {
			return nil, err
		}
		var result ContentReadResult
		if err := strictDecode(raw, &result); err != nil {
			return nil, &ProtocolError{Reason: ErrProtocolError, Message: "invalid host/content/read result"}
		}
		if result.ContentRef != ref || result.Offset != offset {
			return nil, &ProtocolError{Reason: ErrProtocolError, Message: "host/content/read answered a different ref or offset"}
		}
		if result.Encoding != ContentUTF8 {
			return nil, &ProtocolError{Reason: ErrProtocolError, Message: "host/content/read answered with an unknown encoding"}
		}
		if result.TotalBytes > ContentRefObjectBytes {
			return nil, &ProtocolError{Reason: ErrFrameTooLarge, Message: fmt.Sprintf(
				"content ref is %d bytes, above the %d byte object cap", result.TotalBytes, ContentRefObjectBytes)}
		}
		chunk, err := base64.StdEncoding.DecodeString(result.DataBase64)
		if err != nil {
			return nil, &ProtocolError{Reason: ErrProtocolError, Message: "host/content/read returned invalid base64"}
		}
		if len(chunk) > ContentRefChunkBytes {
			return nil, &ProtocolError{Reason: ErrProtocolError, Message: "host/content/read returned an oversized chunk"}
		}
		out = append(out, chunk...)
		if result.NextOffset == nil {
			if int64(len(out)) != result.TotalBytes {
				return nil, &ProtocolError{Reason: ErrProtocolError, Message: fmt.Sprintf(
					"content ref reassembled to %d bytes, host reported %d", len(out), result.TotalBytes)}
			}
			sum := sha256.Sum256(out)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), result.SHA256) {
				return nil, &ProtocolError{Reason: ErrProtocolError, Message: "content ref SHA-256 mismatch"}
			}
			return out, nil
		}
		if *result.NextOffset <= offset {
			return nil, &ProtocolError{Reason: ErrProtocolError, Message: "host/content/read made no progress"}
		}
		offset = *result.NextOffset
	}
}

// ResolveExternalized rehydrates one owner document's externalizable field.
// raw is the field's inline value and externalized the owner's envelope, at
// the schema-registered JSON pointer ("/payload" for intercept and event
// params, "/replacement" for intercept results). With an empty envelope the
// inline value passes through; otherwise the envelope must hold exactly the
// pointer's descriptor, and the ref is paged back and verified against the
// descriptor's byte count and SHA-256 before it is returned. An inline value
// alongside an envelope, a wrong pointer, or unverifiable content is a
// protocol error — never decode bytes the peer did not prove.
//
// Intercept and event payloads are resolved automatically before the
// interceptor/observer runs; this helper remains for manual use.
func ResolveExternalized(ctx context.Context, raw json.RawMessage, externalized []ExternalizedField, pointer string) (json.RawMessage, error) {
	if serverFrom(ctx) == nil {
		return nil, ErrNoConnection
	}
	return resolveExternalized(ctx, raw, externalized, pointer)
}

func (s *server) rehydrate(ctx context.Context, raw json.RawMessage, externalized []ExternalizedField, pointer string) (json.RawMessage, error) {
	return resolveExternalized(ctx, raw, externalized, pointer)
}

func resolveExternalized(ctx context.Context, raw json.RawMessage, externalized []ExternalizedField, pointer string) (json.RawMessage, error) {
	if len(externalized) == 0 {
		return raw, nil
	}
	if inline := bytes.TrimSpace(raw); len(inline) > 0 && !bytes.Equal(inline, []byte("null")) {
		return nil, &ProtocolError{Reason: ErrProtocolError, Message: "document carries both an inline value and an externalized envelope"}
	}
	if len(externalized) != 1 || externalized[0].JSONPointer != pointer {
		return nil, &ProtocolError{Reason: ErrProtocolError, Message: fmt.Sprintf(
			"externalized envelope must hold exactly the %s descriptor", pointer)}
	}
	descriptor := externalized[0]
	if descriptor.TotalBytes > ContentRefObjectBytes {
		return nil, &ProtocolError{Reason: ErrFrameTooLarge, Message: fmt.Sprintf(
			"externalized value is %d bytes, above the %d byte object cap", descriptor.TotalBytes, ContentRefObjectBytes)}
	}
	data, err := ReadContentRef(ctx, descriptor.ContentRef)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != descriptor.TotalBytes {
		return nil, &ProtocolError{Reason: ErrProtocolError, Message: fmt.Sprintf(
			"externalized value reassembled to %d bytes, want %d", len(data), descriptor.TotalBytes)}
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), descriptor.SHA256) {
		return nil, &ProtocolError{Reason: ErrProtocolError, Message: "externalized value SHA-256 mismatch"}
	}
	return data, nil
}

// callHost issues one Extension → Host request behind the handshake barrier
// and maps a structured wire error back to a *ProtocolError.
func (s *server) callHost(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	raw, err := s.conn.call(ctx, method, params)
	if err != nil {
		return nil, mapCallError(err)
	}
	return raw, nil
}

// mapCallError converts a peer's JSON-RPC error into a *ProtocolError when it
// carries a frozen reason.
func mapCallError(err error) error {
	var respErr *ResponseError
	if errors.As(err, &respErr) {
		var data ProtocolErrorData
		if len(respErr.Data) > 0 && json.Unmarshal(respErr.Data, &data) == nil && data.Validate() == nil {
			return &ProtocolError{Reason: data.Reason, Message: respErr.Message}
		}
	}
	return err
}

// strictDecode decodes one params/result document rejecting unknown fields
// and trailing JSON, mirroring the host's strict decoder envelope rules.
func strictDecode(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

// jsonKeyPresent reports whether raw is an object containing key, for
// required-but-nullable fields such as the externalizable payload.
func jsonKeyPresent(raw json.RawMessage, key string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	_, ok := object[key]
	return ok
}
