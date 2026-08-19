# Harness Source Analysis

## Overview

DeepSeek Harness (DSH) is a TypeScript-based agent runtime built on the Cordis dependency injection framework. It implements a plugin-based architecture with scoped contexts, event-driven communication, and a layered execution model.

**Repository**: `/Users/wangxingchao/Documents/deepseek-harness`  
**Branch**: `master` (commit 99f6f02fec)  
**Language**: TypeScript (ES modules)  
**DI Framework**: Cordis (`@deepseek-ai/cordis`)

---

## 1. Core Architecture Layers

### 1.1 Runtime Composition (Cordis)

Harness uses Cordis as its dependency injection and lifecycle management framework. Key patterns:

- **Context**: Scoped DI container that owns service instances
- **Service**: Singleton within a context (e.g., `ctx.llm`, `ctx.tools`, `ctx.fs`)
- **Fiber**: Lifecycle state machine for contexts (LOADING → ACTIVE → UNLOADING → DISPOSED)
- **Events**: Typed event bus with emit/waterfall/parallel dispatch modes

### 1.2 Package Structure

```
packages/
├── core/           # Core runtime (agent, session, tools, scope)
├── llm/            # LLM adapters (DeepSeek, Pi AI, retry)
├── fs/             # Filesystem abstraction + tools
├── compaction/     # Context compaction strategies
├── context/        # Dynamic context providers
├── hooks/          # Hook protocol (Claude Code, Codex compatibility)
├── plugin/         # Plugin runtime (future)
├── host/           # Web server, directory picker
├── client/         # UI components (React)
└── ...
```

---

## 2. Agent Runtime

### 2.1 Entry Points

**File**: `packages/core/agent/src/index.ts`

```typescript
// Agent Registry
class AgentRegistry extends Service {
  // Factory delegation
  create(options: CreateAgentOptions): Promise<AgentHandle>
  resume(options: ResumeAgentOptions): Promise<AgentHandle>
  
  // Live agent lookup
  get(id: SessionId): Agent | undefined
  has(id: SessionId): boolean
}
```

**Responsibility**: Agent lifecycle management, factory delegation, process-local initiator scope.

### 2.2 Agent Loop

**File**: `packages/core/agent-loop/src/agent.ts`

```typescript
class ReactLoopAgent implements Agent {
  // Phase management
  private phase: Phase  // idle | maintenance | running
  
  // Inbox for user messages
  readonly inbox: Inbox
  
  // Scoped context
  readonly scope: Scope
  readonly ctx: Context
  
  // Event dispatch (fused, zero-alloc hot path)
  private readonly dispatch: AgentEventDispatch
  
  // Main loop
  private async kick(): Promise<void>
  private async runTurn(): Promise<void>
  private async runStep(): Promise<StepEndReason>
}
```

**Key Design**:
- **Phase-based state machine**: idle → running → idle (with maintenance as special case)
- **Inbox pattern**: User messages queued, claimed per turn/step
- **Scoped context**: Each agent gets its own Cordis context with tool/session bindings
- **Fused dispatcher**: Single object for all event emissions, built once in constructor

### 2.3 Agent Loop Hot Path

```
User Input
    ↓
inbox.splice() → wakeDriver()
    ↓
kick() → runTurn()
    ↓
assembleContextFor(session) → system prompt + messages
    ↓
llm.stream(options) → provider call
    ↓
BlockAssembler → accumulate chunks
    ↓
executeToolCalls() → parallel tool execution
    ↓
session.append(events) → persist
    ↓
next step or turn complete
```

---

## 3. Tool Runtime

### 3.1 Tool Registry

**File**: `packages/core/tools/src/index.ts`

```typescript
class ToolRegistry extends Service {
  // Registration
  register(tool: DefineToolOptions): void
  
  // Lookup (O(1) by name)
  get(name: string): RegisteredTool | undefined
  
  // Schema generation (cached)
  schemas(): ToolSchema[]
  
  // Execution pipeline
  execute(name: string, args: JsonValue, context: ExecutionContext): Promise<ToolResult>
}
```

**Key Design**:
- **Long-lived registry**: Tools registered once at startup
- **O(1) lookup**: Map-based storage
- **Cached schemas**: Generated once, reused for all requests
- **Execution pipeline**: pre → guard → around → execute → post → result

### 3.2 Tool Definition

```typescript
interface DefineToolOptions {
  name: string
  description: string
  parameters: ParameterSchemaSpec
  output?: OutputSchema
  execute: (args, exec) => Promise<ToolResult>
  presentCall?: (args) => DiffCallView
  presentResult?: (args, result) => DiffResultView
}
```

### 3.3 Tool Execution Pipeline

```
Model Response (tool_call)
    ↓
registry.get(name) → O(1) lookup
    ↓
validateArgs(args, schema) → JSON Schema validation
    ↓
pre hooks → guard checks → around interceptors
    ↓
execute(args, exec) → tool logic
    ↓
post hooks → result processing
    ↓
ToolResult → back to agent loop
```

---

## 4. Filesystem Tools

### 4.1 FS Service Abstraction

**File**: `packages/fs/fs/src/index.ts`

```typescript
abstract class FileSystem extends Service {
  // Target resolution
  abstract resolve(path: string, opts?): Promise<FsTarget>
  
  // Read operations
  abstract readText(target: FsTarget, opts?): Promise<string>
  abstract readDir(target: FsTarget): Promise<FsDirEntry[]>
  
  // Write operations (atomic)
  abstract writeText(target: FsTarget, content: string, intent?): Promise<FsWriteOutcome>
  abstract editText(target: FsTarget, request: FsEditRequest, intent?): Promise<FsEditOutcome>
  
  // Observation policy
  abstract stat(target: FsTarget): Promise<FsInfo | undefined>
}
```

**Key Design**:
- **Abstract backend**: Local filesystem, sandboxed, remote implementations
- **Stable target identity**: Same file → same FsTarget (regardless of path aliases)
- **Atomic mutations**: Write/edit operations are atomic
- **Observation policy**: Version tracking for stale detection

### 4.2 Edit Tool Implementation

**File**: `packages/fs/tool-fs/src/edit.ts`

```typescript
export function applyEditTool(ctx: Context, sandbox: FsSandboxController): void {
  ctx.tools.register(defineTool({
    name: 'edit',
    parameters: { file_path, old_string, new_string, replace_all },
    async execute(args, exec) {
      const input = parseEditArgs(args)
      const sandboxPolicy = await sandbox.resolvePolicy('edit', args, exec)
      const target = await ctx.fs.resolve(input.filePath, ...)
      const intent = await ctx.waterfall('fs/edit-intent', target, exec, () => undefined)
      const outcome = await ctx.fs.editText(target, request, intent, exec.signal, sandboxPolicy)
      ctx.emit('fs/observed', target, { kind: 'present', version: outcome.version }, exec)
      return { path: target.displayPath, before: outcome.before, after: outcome.after }
    }
  }))
}
```

**Hot Path**:
1. Parse args (sync)
2. Resolve sandbox policy (async)
3. Resolve target (async, may do I/O)
4. Get edit intent via waterfall (async, policy check)
5. Execute editText (async, atomic file I/O)
6. Emit observation event (sync)

---

## 5. Session Management

### 5.1 Session Service

**File**: `packages/core/session/src/index.ts`

```typescript
class SessionStore extends Service {
  // Session lifecycle
  create(options: CreateSessionOptions): Session
  get(id: SessionId): Session | undefined
  
  // Event sourcing
  append(session: Session, event: SessionEvent): void
  flush(session: Session): Promise<void>
}
```

**Key Design**:
- **Event-sourced**: Append-only session log
- **In-memory store**: Sessions live in memory during execution
- **Persistence is plugin concern**: Subscribe to `session/event`, drain on `session/flush`
- **Derived LLM messages**: Message history computed from event log

### 5.2 Session Events

```typescript
interface SessionEvent {
  type: SessionEventType
  data: JsonValue
  timestamp: number
}

// Event types:
// - turn/start, turn/end
// - message/user, message/assistant, message/tool
// - tool/start, tool/end
// - checkpoint, compact
```

---

## 6. LLM / Provider Layer

### 6.1 LLM Runtime

**File**: `packages/llm/llm/src/index.ts`

```typescript
class LlmRuntime extends Service {
  // Adapter registry
  registerAdapter(adapter: LlmAdapter): void
  
  // Streaming call
  stream(options: GenerateOptions): AsyncIterable<StreamChunk>
  
  // Model discovery
  discoverModels(): Promise<LlmDiscoveredModel[]>
}
```

**Key Design**:
- **Adapter pattern**: Provider-specific adapters (DeepSeek, Pi AI)
- **Waterfall intercept**: `llm/stream` event allows plugins to intercept/modify requests
- **Retry policy**: Built-in retry with exponential backoff
- **Block assembler**: Accumulates streaming chunks into complete messages

### 6.2 DeepSeek Adapter

**File**: `packages/llm/llm-deepseek/src/adapter.ts`

- OpenAI-compatible API
- Streaming SSE parsing
- Reasoning content extraction
- Token usage tracking

---

## 7. Context / System Prompt

### 7.1 System Prompt Service

**File**: `packages/core/system-prompt/src/index.ts`

```typescript
class SystemPrompt extends Service {
  // Section registration (ordered)
  section(section: PromptSection): void
  
  // Dynamic context
  context(context: PromptContext): void
  
  // Tool schemas
  tools(schemas: ToolSchema[]): void
  
  // Assembly (waterfall)
  assemble(context: AssembleContext): PromptAssembly
}
```

**Key Design**:
- **Ordered sections**: Sections concatenated by order (-100 to 199+)
- **Dynamic contexts**: Evaluated per assembly
- **Waterfall assembly**: Plugins can intercept/modify the assembled prompt
- **Variable interpolation**: `{{variable}}` substitution

### 7.2 Context Stability (Prefix Cache)

Harness organizes context for optimal prompt caching:

```
[Stable Prefix]
  - System identity (-100)
  - Persona (0)
  - Tool schemas (100-199)
  - Environment info

[Dynamic Suffix]
  - Conversation messages
  - Tool outputs
  - Current turn context
```

---

## 8. Event System

### 8.1 Event Dispatch

Cordis provides three dispatch modes:

1. **emit**: Fire-and-forget, parallel listeners, no return value
2. **waterfall**: Sequential, each listener can intercept/modify
3. **parallel**: All listeners run, caller awaits all

### 8.2 Agent Events

```typescript
interface AgentEvents {
  'agent/status': { status: AgentStatus }
  'agent/inbox/inserted': { message: UserMessage }
  'agent/turn/start': { turn: number }
  'agent/turn/end': { turn: number, reason: TurnEndReason }
  'agent/step/start': { step: number }
  'agent/step/end': { step: number }
  'agent/tool/call': { name: string, args: JsonValue }
  'agent/tool/result': { name: string, result: ToolResult }
  'agent/token': { text: string }
  'agent/reasoning': { text: string }
}
```

**Key Design**:
- **Fused dispatcher**: Single object, zero-alloc on hot path
- **Scope-filtered**: Events dispatched only to relevant listeners
- **Non-blocking**: Observer failures logged but don't block agent loop

---

## 9. Plugin System

### 9.1 Plugin Pattern

Plugins are Cordis packages that:

1. Export `apply(ctx, config)` function
2. Register services, tools, event listeners
3. Contribute system prompt sections
4. May have lifecycle hooks (start/stop)

### 9.2 Plugin Discovery

- **Static**: Package imports at startup
- **Dynamic**: Plugin inventory (host concern)
- **No hot-path discovery**: Plugins loaded once, not per tool call

---

## 10. Compaction

### 10.1 Compaction Service

**File**: `packages/compaction/compaction/src/index.ts`

- **Region detection**: Identify foldable conversation regions
- **Summarization**: LLM-based summarization of old turns
- **Checkpoint**: Install summary as conversation checkpoint
- **Tool result pruning**: Remove large tool outputs

### 10.2 Compaction Triggers

- **Pressure**: Context approaching window limit
- **Overflow**: Context exceeds window
- **Manual**: User-initiated compaction

---

## 11. Hot Path Summary

### 11.1 Tool Execution Hot Path

```
Model Response
    ↓
registry.get(name)              // O(1) map lookup
    ↓
validateArgs(args, schema)      // JSON Schema validation
    ↓
sandbox.resolvePolicy()         // Permission check
    ↓
ctx.fs.resolve(path)            // Target resolution
    ↓
ctx.waterfall('fs/edit-intent') // Policy waterfall
    ↓
ctx.fs.editText(target, ...)    // Atomic file I/O
    ↓
ctx.emit('fs/observed', ...)    // Observation event
    ↓
return ToolResult                // Back to agent
```

### 11.2 Key Performance Characteristics

- **Zero-alloc event dispatch**: Fused dispatcher built once
- **O(1) tool lookup**: Map-based registry
- **Cached schemas**: Generated once at registration
- **Atomic file operations**: Single I/O call per edit
- **Minimal serialization**: JSON only at boundaries

---

## 12. Dependencies

### 12.1 Core Dependencies

- `@deepseek-ai/cordis`: DI framework
- `@deepseek-ai/schemastery`: JSON Schema DSL
- `node:crypto`: UUID generation
- `node:async_hooks`: AsyncLocalStorage for initiator scope

### 12.2 External Dependencies

- Provider SDKs (OpenAI-compatible)
- Ripgrep (search tools)
- Node.js built-ins

---

## 13. Architecture Strengths

1. **Clean separation**: Tools, providers, session are independent services
2. **Plugin-based extensibility**: New capabilities via plugins, not core changes
3. **Event-driven**: Loose coupling via typed events
4. **Scoped contexts**: Each agent gets isolated environment
5. **Observation policy**: Stale detection without extra I/O
6. **Atomic mutations**: File operations are crash-safe

---

## 14. Architecture Patterns for Reasonix Migration

### Patterns to Adopt:

1. **Long-lived tool registry**: Register once, lookup O(1)
2. **Cached tool schemas**: Generate once at startup
3. **Abstract filesystem**: Separate tool logic from I/O
4. **Event-driven observation**: Emit events, don't poll
5. **Scoped execution context**: Per-tool-call context

### Patterns to Adapt (Go idioms):

1. **DI framework** → Go interfaces + constructor injection
2. **Event bus** → Go channels or sync callbacks
3. **Waterfall pattern** → Chain of responsibility
4. **AsyncLocalStorage** → Context values

### Patterns to Skip:

1. **Cordis lifecycle** → Go's simpler goroutine lifecycle
2. **Schemastery** → Go's static typing + JSON tags
3. **Complex fiber states** → Go's context cancellation
