# Architecture Mapping: Harness → Reasonix

## Overview

This document maps DeepSeek Harness (TypeScript/Cordis) architecture concepts to DeepSeek-Reasonix (Go) equivalents, identifying differences, target designs, and migration strategies.

**Priority Scale**:
- **P0**: Hot path / core architecture
- **P1**: Runtime architecture
- **P2**: Lifecycle / decoupling
- **P3**: Engineering quality
- **P4**: Non-core differences

---

## 1. Dependency Injection Framework

### Harness: Cordis Context

```typescript
// Harness uses Cordis DI framework
const ctx = new Context()
ctx.plugin(llmPlugin, config)
ctx.plugin(toolsPlugin, config)

// Services accessed via context
ctx.llm.stream(options)
ctx.tools.register(tool)
ctx.fs.editText(target, request)
```

### Reasonix: Constructor Injection

```go
// Reasonix uses manual constructor injection
agent := &Agent{
    svc: agentServices{
        provider: provider,
        tools:    registry,
        sandbox:  sandboxPolicy,
    },
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| DI pattern | Framework (Cordis) | Manual injection |
| Service lifetime | Context-scoped | Process-scoped |
| Service lookup | `ctx.serviceName` | `a.svc.serviceName` |
| Plugin system | Built-in | Manual registration |

### Target Design

**Keep manual injection** - Go's type system and interfaces provide sufficient DI without framework overhead. The current pattern is idiomatic Go.

### Migration Strategy

**No change needed** - Current approach is appropriate for Go.

**Priority**: P4

---

## 2. Tool Registry

### Harness: ToolRegistry (Service)

```typescript
class ToolRegistry extends Service {
    private tools = new Map<string, RegisteredTool>()
    
    register(tool: DefineToolOptions): void {
        this.tools.set(tool.name, createRegisteredTool(tool))
    }
    
    get(name: string): RegisteredTool | undefined {
        return this.tools.get(name)  // O(1)
    }
    
    schemas(): ToolSchema[] {
        // Cached, invalidated on register
    }
}
```

### Reasonix: tool.Registry

```go
type Registry struct {
    mu      sync.RWMutex
    tools   map[string]Tool
    order   []string
    schemas []provider.ToolSchema  // Cached
}

func (r *Registry) Register(t Tool) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.tools[t.Name()] = t
    r.invalidateSchemas()
}

func (r *Registry) Get(name string) (Tool, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    t, ok := r.tools[name]
    return t, ok
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Lookup | O(1) map | O(1) map ✓ |
| Schema cache | Yes ✓ | Yes ✓ |
| Lifecycle hooks | Yes (pre/guard/around/post) | No |
| Execution pipeline | Built-in | Manual |
| Registration | `ctx.tools.register()` | `registry.Register()` |

### Target Design

**Enhance existing Registry** with:
1. Execution pipeline (pre/post hooks)
2. Lifecycle events (registered/unregistered)
3. Metrics collection

### Migration Strategy

1. Add `ExecutionPipeline` to Registry
2. Add hook registration API
3. Wrap existing Execute calls
4. Add metrics integration

**Priority**: P1

---

## 3. Tool Execution

### Harness: Pipeline Pattern

```typescript
// Tool execution goes through pipeline
ctx.tools.execute(name, args, context)
    ↓
pre hooks
    ↓
guard checks
    ↓
around interceptors
    ↓
tool.execute(args, exec)
    ↓
post hooks
    ↓
result processing
```

### Reasonix: Direct Execution

```go
// Agent directly calls tool.Execute()
result, err := plan.tool.Execute(ctx, plan.execArgs)
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Execution path | Pipeline | Direct call |
| Pre-hooks | Yes | No |
| Post-hooks | Yes | No |
| Guard checks | Built-in | Manual (permission.Gate) |
| Metrics | Built-in | Manual |

### Target Design

**Introduce ToolRuntime**:

```go
type ToolRuntime struct {
    registry   *Registry
    pipeline   *ExecutionPipeline
    permission permission.Gate
    sandbox    sandbox.Policy
    metrics    *Metrics
}

func (r *ToolRuntime) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
    tool, ok := r.registry.Get(name)
    if !ok {
        return "", ErrUnknownTool
    }
    
    // Pre-hooks
    if err := r.pipeline.PreExecute(ctx, name, args); err != nil {
        return "", err
    }
    
    // Permission check
    if err := r.permission.Check(ctx, name, args); err != nil {
        return "", err
    }
    
    // Execute
    result, err := tool.Execute(ctx, args)
    
    // Post-hooks
    r.pipeline.PostExecute(ctx, name, args, result, err)
    
    return result, err
}
```

### Migration Strategy

1. Create `internal/tool/runtime.go`
2. Move execution logic from `agent/execute_one.go`
3. Agent calls `toolRuntime.Execute()` instead of `tool.Execute()`
4. Add pipeline hooks for extensibility

**Priority**: P0

---

## 4. Filesystem Abstraction

### Harness: FileSystem Service

```typescript
abstract class FileSystem extends Service {
    abstract resolve(path: string, opts?): Promise<FsTarget>
    abstract readText(target: FsTarget): Promise<string>
    abstract writeText(target: FsTarget, content: string): Promise<FsWriteOutcome>
    abstract editText(target: FsTarget, request: FsEditRequest): Promise<FsEditOutcome>
}
```

### Reasonix: Direct File I/O

```go
// Builtin tools directly use os.ReadFile/os.WriteFile
func (e editFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    content, err := os.ReadFile(path)
    // ...
    err = os.WriteFile(path, newContent, 0644)
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Abstraction | FileSystem service | Direct os calls |
| Target identity | Stable FsTarget | File path string |
| Observation | Version tracking | None |
| Sandbox | Integrated | Separate sandbox.Policy |
| Overlay | Supported (FileOverlay) | Supported (overlay) |

### Target Design

**Keep direct I/O** with enhancements:

```go
// Add observation tracking
type FileObservation struct {
    Path    string
    Version int64
    LastRead time.Time
}

// Add to tool context
type ToolContext struct {
    Observations map[string]FileObservation
    Overlay      FileOverlay
    Sandbox      sandbox.Policy
}
```

### Migration Strategy

1. Add `FileObservation` tracking to tool context
2. Update read tools to record observations
3. Update write tools to check observations (optional)
4. Keep direct I/O (no abstraction layer needed)

**Priority**: P2

---

## 5. Session Management

### Harness: Event-Sourced Session

```typescript
class SessionStore extends Service {
    create(options): Session
    append(session, event): void  // Event sourcing
    flush(session): Promise<void> // Persistence
}

// Session is event log
interface Session {
    events: SessionEvent[]
    // Derived messages computed from events
}
```

### Reasonix: Message-Based Session

```go
type Session struct {
    Messages []provider.Message
    version  uint64
}

func (s *Session) Add(m provider.Message) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.Messages = append(s.Messages, m)
    s.version++
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Storage model | Event sourcing | Message array |
| Append | Event log | Message array |
| Derived state | Computed from events | Direct access |
| Persistence | Plugin concern | Embedded (Session.Save) |
| Replay | Event replay | Message reload |

### Target Design

**Keep message-based model** with separation:

```go
// Session owns in-memory state
type Session struct {
    Messages []provider.Message
    version  uint64
}

// SessionStore owns persistence
type SessionStore interface {
    Save(ctx context.Context, session *Session) error
    Load(ctx context.Context, id string) (*Session, error)
}

// Agent uses SessionStore
type Agent struct {
    session     *Session
    sessionStore SessionStore
}
```

### Migration Strategy

1. Extract `SessionStore` interface
2. Move persistence from `Session` to `sessioncatalog`
3. Agent owns `Session` + `SessionStore`
4. Remove embedded save/load from Session

**Priority**: P1

---

## 6. Event System

### Harness: Cordis Events

```typescript
// Three dispatch modes
ctx.emit('event', payload)           // Fire-and-forget
ctx.waterfall('event', payload, next) // Sequential intercept
ctx.parallel('event', payload)        // Parallel await

// Scope-filtered dispatch
ctx.emit('agent/status', { status })  // Only relevant listeners
```

### Reasonix: Typed Sink

```go
type Sink interface {
    Emit(e Event)
}

type Event struct {
    Kind  Kind
    Text  string
    Tool  *ToolEvent
    Usage *Usage
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Dispatch modes | emit/waterfall/parallel | emit only |
| Scope filtering | Yes | No |
| Interceptors | Yes (waterfall) | No |
| Event types | Typed by string | Typed by enum |
| Registration | `ctx.on('event', handler)` | Sink interface |

### Target Design

**Enhance event system** with interceptor support:

```go
type EventBus struct {
    listeners map[Kind][]Listener
    interceptors map[Kind][]Interceptor
}

type Interceptor func(ctx context.Context, event Event) (Event, error)

func (eb *EventBus) EmitWithIntercept(ctx context.Context, event Event) error {
    // Run interceptors
    for _, interceptor := range eb.interceptors[event.Kind] {
        modified, err := interceptor(ctx, event)
        if err != nil {
            return err
        }
        event = modified
    }
    
    // Emit to listeners
    for _, listener := range eb.listeners[event.Kind] {
        listener.Handle(ctx, event)
    }
    return nil
}
```

### Migration Strategy

1. Add `EventBus` with interceptor support
2. Add waterfall pattern for tool.before/tool.after
3. Keep Sink interface for frontend rendering
4. Add scope filtering for subagent events

**Priority**: P2

---

## 7. Context / System Prompt

### Harness: SystemPrompt Service

```typescript
class SystemPrompt extends Service {
    section(section: PromptSection): void  // Ordered sections
    context(context: PromptContext): void   // Dynamic contexts
    assemble(ctx): PromptAssembly           // Waterfall assembly
}
```

### Reasonix: Embedded Context Building

```go
// Context built in agent.go
func (a *Agent) buildSystemPrompt() string {
    parts := []string{
        a.identity,
        a.persona,
        a.toolGuidance,
        a.environment,
    }
    return strings.Join(parts, "\n\n")
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Registration | Service API | Hardcoded assembly |
| Ordering | Explicit order field | Implicit concatenation |
| Dynamic | Per-assembly evaluation | Static |
| Waterfall | Plugin interception | None |
| Caching | Prefix stability | Manual |

### Target Design

**Extract SystemPrompt builder**:

```go
type SystemPromptBuilder struct {
    sections []Section
    contexts []DynamicContext
}

type Section struct {
    Name  string
    Order int
    Text  string  // or func(ctx) string
}

func (b *SystemPromptBuilder) AddSection(s Section) { ... }
func (b *SystemPromptBuilder) AddContext(c DynamicContext) { ... }
func (b *SystemPromptBuilder) Build(ctx context.Context) string { ... }
```

### Migration Strategy

1. Create `internal/prompt/builder.go`
2. Register sections at startup
3. Agent calls `builder.Build()` instead of manual assembly
4. Add ordering and caching

**Priority**: P2

---

## 8. LLM / Provider

### Harness: LlmRuntime Service

```typescript
class LlmRuntime extends Service {
    registerAdapter(adapter: LlmAdapter): void
    stream(options: GenerateOptions): AsyncIterable<StreamChunk>
}

// Waterfall intercept
ctx.on('llm/stream', async (options, next) => {
    // Modify request
    return next()
})
```

### Reasonix: Provider Interface

```go
type Provider interface {
    Stream(ctx context.Context, messages []Message, tools []ToolSchema) (*Stream, error)
}

// Direct usage
stream, err := a.svc.provider.Stream(ctx, messages, tools)
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Abstraction | LlmRuntime service | Provider interface |
| Interceptors | llm/stream waterfall | None |
| Retry | Built-in (llm-retry) | Manual (provider/retry.go) |
| Model discovery | Yes | Yes |
| Adapter registry | Yes | Init-based registration |

### Target Design

**Add ModelRuntime wrapper**:

```go
type ModelRuntime struct {
    provider    provider.Provider
    retryPolicy RetryPolicy
    interceptors []StreamInterceptor
}

type StreamInterceptor func(ctx context.Context, messages []provider.Message, next StreamFunc) (*provider.Stream, error)

func (r *ModelRuntime) Stream(ctx context.Context, messages []provider.Message, tools []provider.ToolSchema) (*provider.Stream, error) {
    // Apply interceptors
    // Apply retry logic
    return r.provider.Stream(ctx, messages, tools)
}
```

### Migration Strategy

1. Create `internal/model/runtime.go`
2. Wrap existing provider with interceptors
3. Add retry policy integration
4. Agent uses ModelRuntime instead of direct Provider

**Priority**: P1

---

## 9. Plugin System

### Harness: Cordis Plugins

```typescript
// Plugins are Cordis packages
export function apply(ctx: Context, config: Config): void {
    ctx.tools.register(myTool)
    ctx.systemPrompt.section(mySection)
    ctx.on('event', myHandler)
}
```

### Reasonix: MCP Client

```go
// Plugins are MCP servers
type Client struct {
    spec      Spec
    transport Transport
    tools     map[string]Tool
}

// Registration
func (c *Client) RegisterTools(registry *tool.Registry) {
    for name, t := range c.tools {
        registry.Register(t)
    }
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Plugin type | Cordis packages | MCP servers |
| Registration | ctx.plugin() | Manual registration |
| Lifecycle | Managed by Cordis | Manual start/stop |
| Tool discovery | Static (imports) | Dynamic (tools/list) |

### Target Design

**Keep MCP-based plugin system** with lifecycle management:

```go
type PluginManager struct {
    clients  map[string]*Client
    registry *tool.Registry
}

func (pm *PluginManager) Start(ctx context.Context, spec Spec) error {
    client := NewClient(spec)
    if err := client.Connect(ctx); err != nil {
        return err
    }
    client.RegisterTools(pm.registry)
    pm.clients[spec.Name] = client
    return nil
}

func (pm *PluginManager) Stop(name string) error {
    client := pm.clients[name]
    client.UnregisterTools(pm.registry)
    return client.Close()
}
```

### Migration Strategy

1. Add `PluginManager` for lifecycle management
2. Add tool unregistration
3. Add health checking
4. Keep MCP protocol (no change)

**Priority**: P2

---

## 10. Compaction

### Harness: Compaction Service

```typescript
// Separate compaction package
class CompactionService extends Service {
    compact(session: Session, trigger: string): Promise<CompactionResult>
}

// Multiple strategies
packages/compaction/compaction-basic/      // LLM summarization
packages/compaction/compaction-tool-result-pruner/  // Tool output pruning
```

### Reasonix: Embedded Compaction

```go
// Compaction in agent package
func (a *Agent) compactToProjection(ctx context.Context, ...) (CompactionOutcome, error) {
    // Summarization logic
    // Checkpoint installation
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Location | Separate package | Embedded in agent |
| Strategies | Multiple plugins | Single implementation |
| Extensibility | Plugin-based | Hardcoded |

### Target Design

**Extract compaction strategies**:

```go
// internal/compaction/
type Strategy interface {
    Name() string
    Compact(ctx context.Context, messages []provider.Message, target int) (*CompactionResult, error)
}

type Manager struct {
    strategies []Strategy
    agent      *Agent
}

func (m *Manager) Compact(ctx context.Context, trigger string) error {
    // Select strategy based on trigger
    // Execute compaction
    // Install checkpoint
}
```

### Migration Strategy

1. Create `internal/compaction/` package
2. Extract summarization logic
3. Add tool result pruning strategy
4. Agent uses CompactionManager

**Priority**: P2

---

## 11. Permission System

### Harness: Approval Service

```typescript
// Permission via approval waterfall
ctx.waterfall('fs/edit-intent', target, exec, () => undefined)

// Sandbox escalation
sandbox.resolvePolicy('edit', args, exec)
```

### Reasonix: Permission Gate

```go
type Gate interface {
    Check(ctx context.Context, name string, args json.RawMessage) (allow bool, reason string, err error)
}

// Agent checks before execution
if blocked, early := a.applyRecoveryAndPermission(ctx, plan); early {
    return blocked, true
}
```

### Difference

| Aspect | Harness | Reasonix |
|--------|---------|----------|
| Pattern | Waterfall intercept | Gate check |
| Integration | Built into tool pipeline | Manual check |
| Escalation | sandbox_permissions args | Separate approval flow |

### Target Design

**Keep Gate interface** with integration into ToolRuntime:

```go
type ToolRuntime struct {
    permission permission.Gate
    // ...
}

func (r *ToolRuntime) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
    // Permission check integrated
    if allow, reason, err := r.permission.Check(ctx, name, args); !allow {
        return "", fmt.Errorf("permission denied: %s", reason)
    }
    // ...
}
```

### Migration Strategy

1. Integrate Gate into ToolRuntime
2. Keep existing permission logic
3. Add escalation support for sandbox

**Priority**: P1

---

## 12. Summary: Migration Priorities

### P0: Hot Path (Immediate)

1. **ToolRuntime extraction** - Agent → ToolRuntime → Tool
2. **Registry enhancement** - Add execution pipeline

### P1: Runtime Architecture

3. **SessionRuntime extraction** - Agent → SessionRuntime → SessionStore
4. **ModelRuntime extraction** - Agent → ModelRuntime → Provider
5. **Permission integration** - Gate into ToolRuntime

### P2: Lifecycle / Decoupling

6. **Event system enhancement** - Add interceptors
7. **SystemPrompt extraction** - Builder pattern
8. **Compaction extraction** - Strategy pattern
9. **PluginManager** - Lifecycle management
10. **FileObservation** - Version tracking

### P3: Engineering Quality

11. **Metrics integration** - Tool execution metrics
12. **Logging standardization** - Structured logging
13. **Error classification** - Typed errors

### P4: Non-core

14. **DI framework** - Keep manual injection
15. **UI concerns** - Separate from agent

---

## 13. Architecture Diagrams

### Current Reasonix Architecture

```
CLI / Desktop / ACP
        │
        ▼
    Controller
        │
        ▼
      Agent (100+ fields)
        │
        ├── provider.Provider
        ├── tool.Registry
        ├── Session
        ├── permission.Gate
        └── sandbox.Policy
```

### Target Reasonix Architecture

```
CLI / Desktop / ACP
        │
        ▼
    Controller
        │
        ▼
    Agent Runtime
        │
        ├── ToolRuntime
        │     ├── ToolRegistry
        │     ├── ExecutionPipeline
        │     └── PermissionGate
        │
        ├── ContextRuntime
        │     ├── SystemPromptBuilder
        │     └── CompactionManager
        │
        ├── ModelRuntime
        │     ├── Provider
        │     ├── RetryPolicy
        │     └── Interceptors
        │
        ├── SessionRuntime
        │     ├── Session (in-memory)
        │     └── SessionStore (persistence)
        │
        └── EventRuntime
              ├── EventBus
              └── Interceptors
```
