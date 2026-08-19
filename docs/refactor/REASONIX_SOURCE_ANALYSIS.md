# Reasonix Source Analysis

## Overview

DeepSeek-Reasonix is a Go-based coding agent with a rich terminal UI (TUI), desktop app, and ACP (Agent Communication Protocol) support. It implements a monolithic agent with embedded tool execution, session management, and provider integration.

**Repository**: `/Users/wangxingchao/Documents/DeepSeek-Reasonix`  
**Branch**: `stable` (commit 58ade5c4)  
**Language**: Go  
**UI Framework**: Wails (desktop), BubbleTea (TUI)

---

## 1. Core Architecture

### 1.1 Package Structure

```
internal/
├── agent/          # Core agent runtime (130+ files)
├── tool/           # Tool abstraction + builtin tools
├── provider/       # LLM provider abstraction
├── control/        # Controller (orchestrates agent, UI, persistence)
├── event/          # Event system (typed events, sinks)
├── plugin/         # MCP plugin client
├── sessioncatalog/ # Session persistence (SQLite)
├── memory/         # Memory system (consolidation, recall)
├── permission/     # Permission policy
├── sandbox/        # Sandbox policy
├── hook/           # Shell hooks
├── config/         # Configuration
└── ...
```

### 1.2 Entry Points

```
cmd/
├── reasonix/           # Main CLI entry
├── reasonix-launcher/  # Launcher wrapper
└── e2ebench/          # Benchmarking tool

desktop/
├── cmd/               # Desktop app entry (Wails)
└── frontend/          # React frontend
```

---

## 2. Agent Runtime

### 2.1 Agent Structure

**File**: `internal/agent/agent.go`

```go
type Agent struct {
    agentConfig
    svc        agentServices    // Collaborators (provider, tools, etc.)
    sess       sessionRuntime   // Session state
    turn       turnRuntime      // Per-turn state
    task       taskRuntime      // Per-task state
    pending    pendingState     // Queued state
    planMode   atomic.Bool      // Plan mode flag
    // ... 50+ fields
}
```

**Key Observation**: Agent is a **monolithic struct** with 100+ fields. All state is embedded, not injected.

### 2.2 Agent Services

**File**: `internal/agent/services.go`

```go
type agentServices struct {
    provider  provider.Provider
    tools     *tool.Registry
    sandbox   sandbox.Policy
    permission permission.Gate
    hooks     hook.Hooks
    jobs      *jobs.Manager
    memory    *memory.Store
    // ...
}
```

### 2.3 Agent Loop

**File**: `internal/agent/run_loop.go`

```go
func (a *Agent) Run(ctx context.Context, input string) error {
    // 1. Begin turn (evidence, delivery classification)
    rawInput, state := a.beginRunTurn(ctx, input)
    
    // 2. Main loop
    for {
        // Prepare context (compaction if needed)
        prepared, err := a.contextManager().Prepare(ctx, policy)
        
        // Stream from provider
        turn, err := a.stream(ctx, prepared.Messages)
        
        // Execute tool calls
        if len(turn.calls) > 0 {
            outcomes := a.executeBatch(ctx, turn.calls)
        }
        
        // Check completion
        if turn.text != "" || turn.err != nil {
            break
        }
    }
    
    // 3. End turn (persistence, events)
    return a.endRunTurn(ctx, state)
}
```

---

## 3. Tool Runtime

### 3.1 Tool Interface

**File**: `internal/tool/tool.go`

```go
type Tool interface {
    Name() string
    Description() string
    Schema() json.RawMessage
    Execute(ctx context.Context, args json.RawMessage) (string, error)
    ReadOnly() bool
}

// Optional interfaces
type ContextualTool interface {
    ProviderVisible(context.Context) bool
}

type Previewer interface {
    Preview(ctx context.Context, args json.RawMessage) (diff.Change, error)
}

type ImageTool interface {
    ExecuteWithImages(ctx context.Context, args json.RawMessage) (text string, images []string, err error)
}
```

### 3.2 Tool Registry

**File**: `internal/tool/tool.go`

```go
type Registry struct {
    mu       sync.RWMutex
    tools    map[string]Tool
    order    []string
    schemas  []provider.ToolSchema  // Cached schemas
}

func (r *Registry) Register(t Tool) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.tools[t.Name()] = t
    r.order = append(r.order, t.Name())
    r.invalidateSchemas()
}

func (r *Registry) Get(name string) (Tool, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    t, ok := r.tools[name]
    return t, ok
}

func (r *Registry) ResolveCall(name string) (Tool, string, []string) {
    // Exact match → prefix match → ambiguity check
}
```

**Key Observation**: Registry uses `map[string]Tool` for O(1) lookup. Schemas are cached and invalidated on registration.

### 3.3 Builtin Tools

**Directory**: `internal/tool/builtin/`

```
bash.go          # Shell execution
readfile.go      # File reading
writefile.go     # File writing
editfile.go      # String replacement editing
grep.go          # Content search
glob.go          # File pattern matching
multiedit.go     # Multi-file editing
ls.go            # Directory listing
webfetch.go      # HTTP fetching
todo.go          # Task management
updategoal.go    # Goal management
...
```

**Registration**: Builtin tools self-register via `init()`:

```go
func init() { tool.RegisterBuiltin(editFile{}) }
```

### 3.4 Tool Execution Flow

**File**: `internal/agent/execute_one.go`

```go
func (a *Agent) executeOne(ctx context.Context, turn *turnRuntime, call provider.ToolCall) toolOutcome {
    plan := &toolCallPlan{call: call}
    
    // 1. Parse tool call (resolve name, check ambiguity)
    if blocked, early := a.parseToolCall(ctx, plan); early {
        return blocked
    }
    
    // 2. Intercept (extension hooks)
    if blocked, early := a.interceptToolBefore(ctx, plan); early {
        return blocked
    }
    
    // 3. Resolve policy (plan mode, permissions, delivery gates)
    if blocked, early := a.resolveToolPolicy(ctx, turn, plan); early {
        return blocked
    }
    
    // 4. Prepare execution (sandbox, context)
    if blocked, early := a.prepareToolExecution(ctx, plan); early {
        return blocked
    }
    
    // 5. Execute and finish
    return a.finishToolExecution(ctx, plan)
}
```

**Hot Path**:
1. Parse tool call → registry lookup
2. Extension intercept (tool.before hooks)
3. Policy resolution (plan mode, permissions)
4. Execution preparation (sandbox setup)
5. Tool.Execute()
6. Result processing

---

## 4. Edit File Tool

### 4.1 Implementation

**File**: `internal/tool/builtin/editfile.go`

```go
func (e editFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    var p struct {
        Path      string `json:"path"`
        OldString string `json:"old_string"`
        NewString string `json:"new_string"`
    }
    json.Unmarshal(args, &p)
    
    // Resolve path
    p.Path = resolveIn(e.workDir, p.Path)
    
    // Check permissions (confine to workspace)
    if err := confineWrite(ctx, e.roots, e.guard, e.managed, p.Path); err != nil {
        return "", err
    }
    
    // Read file
    src, err := readEditSource(ctx, e.overlay, p.Path)
    
    // Apply edit (string replacement)
    applied := applyOldStringEdit(src.content, p.OldString, p.NewString, false)
    
    // Write file
    if err := src.write(ctx, e.overlay, p.Path, applied.updated); err != nil {
        return "", err
    }
    
    return summary, nil
}
```

### 4.2 Edit Hot Path

```
Tool Call (edit_file)
    ↓
json.Unmarshal(args)           // Parse JSON
    ↓
resolveIn(workDir, path)       // Path resolution
    ↓
confineWrite(ctx, roots, ...)  // Permission check
    ↓
readEditSource(ctx, overlay, path)  // Read file (filesystem I/O)
    ↓
applyOldStringEdit(content, old, new)  // String replacement (CPU)
    ↓
src.write(ctx, overlay, path, content)  // Write file (filesystem I/O)
    ↓
return summary                 // Return result
```

---

## 5. Session Management

### 5.1 Session Structure

**File**: `internal/agent/session.go`

```go
type Session struct {
    mu             sync.RWMutex
    Messages       []provider.Message
    version        uint64
    rewriteVersion int
    persisted      sessionPersistState
    writeAuth      *SessionWriteAuthority
    // ...
}
```

**Key Observation**: Session is **in-memory** with mutex protection. Persistence is handled by sessioncatalog.

### 5.2 Session Catalog

**File**: `internal/sessioncatalog/catalog.go`

```go
type Catalog struct {
    db       *sql.DB           // SQLite database
    opts     Options
    writeCh  chan string        // Async write queue
    // ...
}
```

**Key Design**:
- **SQLite backend**: Durable session storage
- **Async writes**: Channel-based write queue
- **Repair/reconcile**: Background workers for consistency

---

## 6. Provider Layer

### 6.1 Provider Interface

**File**: `internal/provider/provider.go`

```go
type Provider interface {
    Name() string
    Stream(ctx context.Context, messages []Message, tools []ToolSchema) (*Stream, error)
    // ...
}
```

### 6.2 Provider Implementations

- `internal/provider/openai/`: OpenAI-compatible (DeepSeek, etc.)
- `internal/provider/anthropic/`: Anthropic Claude
- `internal/provider/responses/`: OpenAI Responses API

### 6.3 Streaming

```go
type Stream struct {
    Chunks   chan StreamChunk
    Usage    *Usage
    // ...
}

type StreamChunk struct {
    Text           string
    Reasoning      string
    ToolCalls      []ToolCall
    ResponsesItems []json.RawMessage
    // ...
}
```

---

## 7. Event System

### 7.1 Event Types

**File**: `internal/event/event.go`

```go
type Kind int

const (
    TurnStarted Kind = iota
    Reasoning
    Text
    Message
    ToolDispatch
    ToolResult
    Usage
    Notice
    Phase
    ApprovalRequest
    AskRequest
    TurnDone
    CompactionStarted
    CompactionDone
    ToolProgress
    // ...
)
```

### 7.2 Event Sink

```go
type Sink interface {
    Emit(e Event)
}

type Event struct {
    Kind    Kind
    Text    string
    Tool    *ToolEvent
    Usage   *Usage
    // ...
}
```

**Key Design**:
- **Typed events**: Enum-based event kinds
- **Sink interface**: Frontend implements rendering
- **Coalescing**: Batch events for performance
- **Fan-out**: Multiple sinks per agent

---

## 8. Plugin System (MCP)

### 8.1 MCP Client

**File**: `internal/plugin/plugin.go`

```go
type Client struct {
    spec     Spec
    transport Transport  // stdio, http, sse
    tools    map[string]Tool
    // ...
}
```

### 8.2 Plugin Lifecycle

1. **Discovery**: Read config (JSON/YAML)
2. **Launch**: Start subprocess (stdio) or connect (http)
3. **Initialize**: MCP handshake
4. **Tools/list**: Discover available tools
5. **Register**: Add tools to registry
6. **Execute**: Forward tool calls via JSON-RPC

---

## 9. Context Management

### 9.1 Context Manager

**File**: `internal/agent/context_manager.go`

```go
type ContextManager struct {
    agent *Agent
}

func (m ContextManager) Prepare(ctx context.Context, policy ContextPreparePolicy) (PreparedContext, error) {
    visible := a.modelVisibleMessages()
    est := a.estimatedVisibleRequestTokens(visible)
    
    // Check if compaction needed
    if est < fold && !forceFold {
        return prepared, nil
    }
    
    // Run compaction
    return m.foldContext(ctx, prepared, policy, ...)
}
```

### 9.2 Compaction

- **Trigger**: Context approaching window limit
- **Strategy**: Summarize old turns, keep recent
- **Checkpoint**: Install summary as conversation prefix
- **Stuck detection**: Avoid infinite compaction loops

---

## 10. Controller Layer

### 10.1 Controller Structure

**File**: `internal/control/controller.go`

```go
type Controller struct {
    agent    *agent.Agent
    session  *agent.Session
    catalog  *sessioncatalog.Catalog
    // ...
}
```

**Responsibility**:
- **Lifecycle**: Create/resume/destroy sessions
- **Turn orchestration**: Route user input to agent
- **Approval**: Handle permission requests
- **Persistence**: Save/load sessions
- **UI bridge**: Connect frontend to agent

---

## 11. Hot Path Analysis

### 11.1 Tool Execution Hot Path

```
Model Response
    ↓
json.Unmarshal(tool_call)        // Parse tool call
    ↓
registry.Get(name)               // O(1) map lookup
    ↓
permission.Check(ctx, name, args) // Permission gate
    ↓
tool.Execute(ctx, args)          // Tool logic
    ↓
json.Marshal(result)             // Serialize result
    ↓
session.Add(toolMessage)         // Append to session
    ↓
event.Sink.Emit(toolResult)      // Emit event
```

### 11.2 Edit File Hot Path

```
Model Response (edit_file)
    ↓
json.Unmarshal(args)             // Parse args
    ↓
resolveIn(workDir, path)         // Path resolution
    ↓
confineWrite(ctx, roots, ...)    // Permission check
    ↓
readEditSource(ctx, overlay, path)  // Read file (I/O)
    ↓
applyOldStringEdit(content, old, new)  // String replacement
    ↓
src.write(ctx, overlay, path, content)  // Write file (I/O)
    ↓
session.Add(toolMessage)         // Persist result
    ↓
event.Sink.Emit(toolResult)      // Emit event
```

---

## 12. Architecture Strengths

1. **Rich tool ecosystem**: 30+ builtin tools
2. **Plugin support**: MCP client for external tools
3. **Multi-frontend**: CLI, Desktop, ACP
4. **Session persistence**: SQLite-based durable storage
5. **Memory system**: Consolidation and recall
6. **Plan mode**: Read-only planning phase
7. **Permission system**: Fine-grained access control

---

## 13. Architecture Weaknesses

### 13.1 Monolithic Agent

The Agent struct has 100+ fields, mixing:
- Runtime state (turn, task, session)
- Configuration
- Services (provider, tools, etc.)
- UI concerns (renderer, asker)

**Impact**: Hard to test, hard to extend, tight coupling.

### 13.2 Direct Tool Access

Agent directly calls tool.Execute():
```go
// In execute_one.go
result, err := plan.tool.Execute(ctx, plan.execArgs)
```

**Impact**: No abstraction layer for tool lifecycle, metrics, caching.

### 13.3 Embedded Persistence

Agent directly manages session persistence:
```go
// In session.go
func (s *Session) Save() error { ... }
```

**Impact**: Agent knows about storage details.

### 13.4 Event System Simplicity

Events are typed enums with simple Sink interface:
```go
type Sink interface {
    Emit(e Event)
}
```

**Impact**: No waterfall pattern, no scoped dispatch, no interceptor chain.

---

## 14. Dependencies

### 14.1 Core Dependencies

- `database/sql`: SQLite via mattn/go-sqlite3
- `encoding/json`: JSON serialization
- `sync`: Mutex, WaitGroup
- `context`: Cancellation

### 14.2 UI Dependencies

- `github.com/charmbracelet/bubbletea`: TUI framework
- `github.com/wailsapp/wails`: Desktop framework
- `github.com/charmbracelet/lipgloss`: TUI styling

### 14.3 Provider Dependencies

- OpenAI Go SDK
- Anthropic Go SDK

---

## 15. Refactor Targets

### Priority 1: Tool Runtime Extraction

**Current**: Agent directly calls tool.Execute()
**Target**: Agent → ToolRuntime → Tool

### Priority 2: Registry Optimization

**Current**: Registry has O(1) lookup ✓
**Target**: Add schema caching ✓, add lifecycle hooks

### Priority 3: Context Runtime Extraction

**Current**: Agent manages context compaction
**Target**: Agent → ContextRuntime → Compaction

### Priority 4: Session Runtime Extraction

**Current**: Agent manages session persistence
**Target**: Agent → SessionRuntime → SessionStore

### Priority 5: Event System Enhancement

**Current**: Simple Sink interface
**Target**: Add waterfall, scoped dispatch, interceptors

---

## 16. File Statistics

- **Total Go files**: ~704
- **Agent package files**: ~130
- **Tool package files**: ~45
- **Provider package files**: ~35
- **Control package files**: ~80
