package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"time"

	"reasonix/internal/provider"
	"reasonix/internal/secrets"
	"reasonix/internal/tool"
)

type mcpTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	// Annotations carries MCP's optional tool hints. readOnlyHint controls reader
	// classification; destructiveHint remains destructive even when another hint
	// claims the tool is read-only. Approval policy is applied separately.
	Annotations *struct {
		ReadOnlyHint    bool `json:"readOnlyHint"`
		DestructiveHint bool `json:"destructiveHint"`
	} `json:"annotations"`
}

func (c *Client) listTools(ctx context.Context) ([]tool.Tool, error) {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	if c.toolsListed {
		return append([]tool.Tool(nil), c.toolAdapters...), nil
	}

	out, err := c.listToolsRawSettled(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateMCPToolNames(out); err != nil {
		return nil, fmt.Errorf("plugin %q: %w", c.name, err)
	}

	toolInfos := make([]ToolInfo, 0, len(out))
	tools := make([]tool.Tool, 0, len(out))
	normalizedSchemas := make(map[string]json.RawMessage, len(out))
	for _, t := range out {
		schema, err := normalizeAndValidateToolSchema(t.InputSchema)
		if err != nil {
			continue
		}
		normalizedSchemas[t.Name] = schema
	}
	for _, t := range out {
		readOnlyHint := t.Annotations != nil && t.Annotations.ReadOnlyHint
		destructiveHint := t.Annotations != nil && t.Annotations.DestructiveHint
		info := ToolInfo{Name: t.Name, Description: t.Description, ReadOnlyHint: readOnlyHint, DestructiveHint: destructiveHint}
		schema, ok := normalizedSchemas[t.Name]
		if !ok {
			if _, err := normalizeAndValidateToolSchema(t.InputSchema); err != nil {
				info.SchemaError = schemaValidationError(err)
			}
			toolInfos = append(toolInfos, info)
			continue
		}
		visibleName := t.Name
		if c.spec.StripRawPrefix != "" {
			visibleName = strings.TrimPrefix(visibleName, c.spec.StripRawPrefix)
		}
		readOnly := readOnlyHint
		toolInfos = append(toolInfos, info)
		tools = append(tools, &remoteTool{
			client:           c,
			name:             toolName(c.name, visibleName),
			rawName:          t.Name,
			visibleName:      visibleName,
			desc:             t.Description,
			schema:           schema,
			outputSchema:     t.OutputSchema,
			declaredReadOnly: readOnlyHint,
			readOnly:         readOnly,
			destructive:      destructiveHint,
		})
	}
	sort.SliceStable(toolInfos, func(i, j int) bool { return toolInfos[i].Name < toolInfos[j].Name })
	sortedTools := sortToolsByName(tools)
	c.tools = toolInfos
	c.toolAdapters = append([]tool.Tool(nil), sortedTools...)
	c.toolsListed = true
	return append([]tool.Tool(nil), sortedTools...), nil
}

func normalizeAndValidateToolSchema(raw json.RawMessage) (json.RawMessage, error) {
	schema := canonicalizeSchema(raw)
	if err := provider.ValidateToolSchema(schema); err != nil {
		return nil, err
	}
	return schema, nil
}

func schemaValidationError(err error) string {
	const maxRunes = 512
	msg := strings.TrimSpace(err.Error())
	runes := []rune(msg)
	if len(runes) > maxRunes {
		msg = string(runes[:maxRunes]) + "..."
	}
	return "invalid input schema: " + msg
}

func (c *Client) listToolsRaw(ctx context.Context) ([]mcpTool, error) {
	res, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("plugin %q: decode tools/list: %w", c.name, err)
	}
	return out.Tools, nil
}

// listToolsRawSettled gives dynamically registering servers a bounded startup
// window before their initial tool catalog is considered complete.
func (c *Client) listToolsRawSettled(ctx context.Context) ([]mcpTool, error) {
	out, err := c.listToolsRaw(ctx)
	if err != nil || !c.hasTools || len(out) > 0 {
		return out, err
	}
	for _, delay := range advertisedToolsEmptyListRetryDelays {
		if err := sleepContext(ctx, delay); err != nil {
			return nil, err
		}
		out, err = c.listToolsRaw(ctx)
		if err != nil || len(out) > 0 {
			return out, err
		}
	}
	return out, nil
}

func validateMCPToolNames(tools []mcpTool) error {
	seen := make(map[string]bool, len(tools))
	for _, candidate := range tools {
		name := strings.TrimSpace(candidate.Name)
		if name == "" {
			return fmt.Errorf("tools/list returned an empty tool name")
		}
		if seen[candidate.Name] {
			return fmt.Errorf("tools/list returned duplicate tool name %q", candidate.Name)
		}
		seen[candidate.Name] = true
	}
	return nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) cachedTools() ([]tool.Tool, bool) {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	if !c.toolsListed {
		return nil, false
	}
	return append([]tool.Tool(nil), c.toolAdapters...), true
}

// toolName builds Reasonix's canonical model-visible name
// "mcp__<server>__<tool>". The registry separately resolves unique portable
// and Claude plugin-qualified references without exposing duplicate schemas.
func toolName(server, raw string) string {
	return ToolPrefix(server) + normalizeName(raw)
}

// ToolPrefix is the model-visible namespace prefix for every tool from server.
func ToolPrefix(server string) string {
	return "mcp__" + normalizeName(server) + "__"
}

// MCPConnectPermissionName is the canonical permission identity for
// starting server on demand. It is intentionally outside the mcp__ tool
// namespace: permission rules match tool names exactly, so a connect must have
// its own non-colliding name instead of pretending a tool-prefix is a glob.
func MCPConnectPermissionName(server string) string {
	return "mcp_connect__" + normalizeName(server)
}

// ModelToolName is the canonical model-visible name for server's raw tool —
// including the collision-hash suffix normalizeName appends when the raw name
// needed sanitising. Every permission/audit surface that names an MCP
// tool must build the name through this function; a second normalization that
// skips the hash would let deny/ask rules written for the executed name miss.
func ModelToolName(server, raw string) string {
	return toolName(server, raw)
}

var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func normalizeName(s string) string {
	raw := s
	s = strings.Trim(invalidNameChars.ReplaceAllString(s, "_"), "_")
	if s == "" {
		s = "unnamed"
	}
	if s != raw {
		s += "_" + shortNameHash(raw)
	}
	return s
}

func shortNameHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())[:6]
}

func summarizeFailureError(err error) string {
	msg := strings.Join(strings.Fields(secrets.RedactCredentials(err.Error())), " ")
	const max = 500
	if len(msg) > max {
		msg = msg[:max] + "..."
	}
	return msg
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"` // omitted for notifications (id 0 unused)
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type remoteTool struct {
	client           *Client
	name             string // namespaced "mcp__<server>__<tool>"
	rawName          string // original name for tools/call
	visibleName      string // raw name after configured prefix stripping
	desc             string
	schema           json.RawMessage
	outputSchema     json.RawMessage
	declaredReadOnly bool // server hint, independent of server authorization
	readOnly         bool // effective reader classification for this live snapshot
	// destructive is the MCP destructiveHint. It takes precedence over a
	// conflicting readOnlyHint in Plan and strict read-only execution.
	destructive bool
}

func (t *remoteTool) Name() string        { return t.name }
func (t *remoteTool) Description() string { return t.desc }
func (t *remoteTool) MCPServerName() string {
	if t.client == nil {
		return ""
	}
	return t.client.name
}
func (t *remoteTool) MCPRawToolName() string     { return t.rawName }
func (t *remoteTool) MCPVisibleToolName() string { return t.visibleName }
func (t *remoteTool) MCPPackageName() string {
	if t.client == nil {
		return ""
	}
	return t.client.spec.Package
}

func (t *remoteTool) MCPServerAuthorized() bool {
	return t.client != nil && t.client.spec.ServerAuthorized()
}

// ReadOnly reflects MCP readOnlyHint plus backward-compatible Spec overrides.
// It defaults to false, so opaque tools remain write-capable unless the server
// or local configuration explicitly classifies them as read-only.
func (t *remoteTool) securitySnapshot() (declaredReadOnly, readOnly, destructive bool) {
	if t.client == nil {
		return t.declaredReadOnly, t.readOnly, t.destructive
	}
	t.client.toolsMu.Lock()
	defer t.client.toolsMu.Unlock()
	return t.declaredReadOnly, t.readOnly, t.destructive
}

func (t *remoteTool) ReadOnly() bool {
	_, readOnly, _ := t.securitySnapshot()
	return readOnly
}

func (t *remoteTool) MCPDestructiveHint() bool {
	_, _, destructive := t.securitySnapshot()
	return destructive
}

func (t *remoteTool) Schema() json.RawMessage {
	if len(t.schema) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return canonicalizeSchema(t.schema)
}

func (t *remoteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	text, _, err := t.ExecuteWithImages(ctx, args)
	return text, err
}

// ExecuteWithImages implements tool.ImageTool: MCP results may carry image
// content items, which callers with a structural image channel (the agent)
// forward to vision models instead of relying on the text placeholders alone.
func (t *remoteTool) ExecuteWithImages(ctx context.Context, args json.RawMessage) (string, []string, error) {
	var argMap map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argMap); err != nil {
			return "", nil, fmt.Errorf("invalid args: %w", err)
		}
	}
	_, readOnly, destructive := t.securitySnapshot()
	if tool.HasReaderExecutionIntent(ctx) {

		if !t.MCPServerAuthorized() || !readOnly || destructive {
			return "", nil, fmt.Errorf("MCP server %q changed the authorization or security metadata for tool %q; the call was blocked before dispatch — refresh the server from a parent session before retrying", t.client.name, t.rawName)
		}
	}
	if tool.HasNonDestructiveMCPExecutionIntent(ctx) {

		if !t.MCPServerAuthorized() || destructive {
			return "", nil, fmt.Errorf("MCP server %q changed the authorization or destructive classification for tool %q; the call was blocked before dispatch — retry so Reasonix can re-apply the current Planner MCP safety boundary", t.client.name, t.rawName)
		}
	}
	res, err := t.client.call(ctx, "tools/call", map[string]any{
		"name":      t.rawName,
		"arguments": argMap,
	})
	if err != nil {
		return "", nil, err
	}
	return parseToolResult(res)
}

var toolResultImageMimes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// parseToolResult flattens an MCP tools/call result into plain text plus the
// image content items as data URLs. Every image item leaves a short placeholder
// in the text at its position, so text-only consumers (and non-vision models)
// still learn an image was returned.
func parseToolResult(res json.RawMessage) (string, []string, error) {
	var out struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", nil, fmt.Errorf("decode tool result: %w", err)
	}
	var sb strings.Builder
	var images []string
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			sb.WriteString(c.Text)
		case "image":
			placeholder, url := toolResultImage(c.MimeType, c.Data, len(images))
			sb.WriteString(placeholder)
			if url != "" {
				images = append(images, url)
			}
		}
	}
	text := sb.String()
	if out.IsError {
		return text, images, fmt.Errorf("plugin tool reported error: %s", text)
	}
	return text, images, nil
}

// toolResultImage validates one MCP image content item and returns its text
// placeholder plus the data URL to forward ("" when the item is dropped).
func toolResultImage(mime, data string, kept int) (placeholder, url string) {
	if kept >= maxToolResultImages {
		return "[image omitted: per-result image limit reached]", ""
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "" {
		mime = "image/png"
	}
	if !toolResultImageMimes[mime] {
		return "[image omitted: unsupported type " + mime + "]", ""
	}

	data = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', ' ':
			return -1
		}
		return r
	}, data)
	if data == "" {
		return "[image omitted: no data]", ""
	}
	if len(data) > maxToolResultImageBytes {
		return fmt.Sprintf("[image omitted: %d bytes exceeds the %d-byte limit]", len(data), maxToolResultImageBytes), ""
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return "[image omitted: invalid base64]", ""
	}
	return "[image: " + mime + "]", "data:" + mime + ";base64," + data
}
