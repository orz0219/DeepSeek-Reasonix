// Memory panel payloads (desktop/app.go MemoryView).
export interface MemoryDoc {
    path: string;
    scope: string; // "user" | "ancestor" | "project" | "local"
    directory?: string;
    body: string;
    imports: Array<{
        path: string;
        sourcePath: string;
    }>;
    depth: number;
    order: number;
    precedence: number;
}
export interface InstructionDiagnostic {
    code: string;
    path: string;
    sourcePath?: string;
    line?: number;
    message: string;
}
export interface MemoryFact {
    id?: string;
    revision?: number;
    createdAt?: string;
    updatedAt?: string;
    name: string;
    title?: string;
    description: string;
    type: string; // "user" | "feedback" | "project" | "reference"
    scope: string; // "project" | "global"
    body: string;
    freshness: string; // "fresh" | "current" | "stale"
}
export interface MemoryConflict {
    key: string;
    projectId: string;
    projectName: string;
    globalId: string;
    globalName: string;
    resolution: "project_over_global";
}
export interface MemoryRecallHit {
    id: string;
    revision: number;
    name: string;
    title?: string;
    type: string;
    scope: string;
    score: number;
    freshness: string;
    reason: string;
    snippet: string;
}
export interface MemoryRecallTrace {
    query: string;
    hits: MemoryRecallHit[];
    omitted: number;
    charBudget: number;
    usedChars: number;
    suppressed?: string;
}
export interface MemoryArchive extends MemoryFact {
    path: string;
    archivedAt?: string;
}
export interface MemoryScope {
    scope: string; // "user" | "project" | "local"
    path: string;
}
export interface MemorySuggestion {
    id: string;
    name: string;
    title: string;
    description: string;
    type: string;
    scope: string; // "project" | "global"
    body: string;
    reason: string;
    evidence: string[];
}
export interface SkillSuggestion {
    id: string;
    name: string;
    description: string;
    scope: string;
    body: string;
    reason: string;
    evidence: string[];
}
export interface MemorySuggestionsView {
    memories: MemorySuggestion[];
    skills: SkillSuggestion[];
    generatedAt: string;
    available: boolean;
    source: string;
}
export interface MemoryView {
    docs: MemoryDoc[];
    facts: MemoryFact[];
    archives: MemoryArchive[];
    scopes: MemoryScope[];
    instructionDiagnostics: InstructionDiagnostic[];
    conflicts: MemoryConflict[];
    lastRecall: MemoryRecallTrace;
    storeDir: string;
    storeGlobalDir?: string;
    available: boolean;
}
// SettingsTab is the top-level navigation item in the Settings Centre modal.
export type SettingsTab = "general" | "models" | "providers" | "mcp" | "skills" | "subagents" | "plugins" | "memory" | "hooks" | "diagnostics" | "shortcuts" | "permissions" | "sandbox" | "network" | "appearance" | "storage" | "about";
/** Extension runtime doctor report from App.RuntimeDoctor. */
export interface RuntimeDoctorReport {
    text: string;
    publishedGeneration: number;
    allowResume: boolean;
    cleanRollback: boolean;
    hasIrreversible: boolean;
    noOpRebuilds: number;
    fullRebuilds: number;
    subgraphRebuilds: number;
    staleDrops: number;
    admissionRejected: number;
    runtimeOwnerFallbacks: number;
}
/** Capability diagnostics report from App.CapabilityDiagnostics (capdiag.Report). */
export interface CapabilityDiagnosticsReport {
    schema_version: number;
    root: string;
    live: boolean;
    summary: {
        errors: number;
        warnings: number;
        infos: number;
        instructions: number;
        skills: number;
        commands: number;
        hooks: number;
        plugins: number;
        mcp_servers: number;
    };
    instructions: {
        docs: Array<{
            path: string;
            scope: string;
            directory?: string;
            depth: number;
            order: number;
        }>;
    };
    skills: CapabilityAssetReport;
    commands: CapabilityAssetReport;
    hooks: {
        trusted_project: boolean;
        project_defines_hooks: boolean;
        sources: Array<{
            scope: string;
            path: string;
            status: string;
            hook_count: number;
            parse_error?: string;
        }>;
        entries: Array<{
            event: string;
            match?: string;
            command?: string;
            context_file?: string;
            description?: string;
            timeout_ms?: number;
            scope: string;
            source: string;
            blocking: boolean;
        }>;
    };
    plugins: {
        state_path?: string;
        packages: Array<{
            name: string;
            enabled: boolean;
            version?: string;
            root: string;
            manifest_kind?: string;
            skills: number;
            commands: number;
            hooks: number;
            mcp_servers: number;
            warnings?: string[];
            status: string;
        }>;
    };
    mcp: {
        servers: Array<{
            name: string;
            source?: string;
            package_owner?: string;
            transport: string;
            start_intent: string;
            command?: string;
            url_host?: string;
            env_keys?: string[];
            header_keys?: string[];
            runtime_status?: string;
            tool_count?: number;
            tools?: Array<{
                name: string;
                read_only_hint?: boolean;
            }>;
            error?: string;
        }>;
    };
    issues: CapabilityIssue[];
}
export interface CapabilityAssetReport {
    roots: Array<{
        path: string;
        scope?: string;
        status: string;
    }>;
    entries: Array<{
        name: string;
        description?: string;
        scope?: string;
        path: string;
        status: string;
        winner_path?: string;
        error?: string;
        run_as?: string;
    }>;
    winners: number;
    shadowed: number;
    disabled?: number;
    parse_errors?: number;
}
export interface CapabilityIssue {
    severity: "error" | "warning" | "info" | string;
    code: string;
    subsystem: string;
    name?: string;
    source?: string;
    message: string;
    remediation?: string;
    settings_tab?: string;
}
// Settings panel payloads (desktop/settings_app.go).
export interface ProviderView {
    name: string;
    builtIn: boolean;
    added: boolean;
    kind: string;
    baseUrl: string;
    chatUrl?: string; // legacy OpenAI chat endpoint override; preserved for old-config compatibility
    requestUrl?: string; // exact provider request URL written by the current settings UI
    models: string[];
    visionModels: string[]; // subset of models that accepts image input
    visionModelsConfigured: boolean; // true when an empty list is an explicit choice
    visionCapability?: "configurable" | "unsupported"; // backend authority; absent on older Wails payloads
    modelsUrl: string; // optional override for model discovery; empty derives from baseUrl
    default: string;
    apiKeyEnv: string;
    headers?: Record<string, string> | null; // optional extra request headers for compatible gateways
    extraBody?: Record<string, unknown> | null; // optional extra top-level request body fields for compatible gateways
    authHeader?: boolean; // Anthropic-compatible: send Authorization: Bearer instead of x-api-key
    keySet: boolean; // the env var currently resolves to a value
    requiresKey?: boolean; // false for explicit no-auth providers
    configured?: boolean; // selectable: key is set or no key is required
    keySource?: string;
    keySourcePath?: string;
    balanceUrl: string; // optional wallet-balance endpoint; "" disables the readout
    contextWindow: number;
    reasoningProtocol: string; // auto|deepseek|glm|kimi-k3|openai|none; empty = auto/model registry
    thinking: string; // provider-specific thinking override: ""|enabled|disabled|adaptive
    webSearch?: boolean; // expose a provider-executed web search tool when supported
    serverWebSearchCapability?: boolean; // backend-verified provider capability; absent on older Wails payloads
    supportedEfforts: string[]; // custom /effort levels; empty = use built-in Kind/BaseURL default
    defaultEffort: string; // /effort level when user picks "auto" or unset; "" = supportedEfforts[0]
    modelOverrides?: ProviderModelOverrideView[] | null;
    recommendedUpgradeAvailable?: boolean; // official legacy OpenAI entry can switch to recommended Anthropic access
    modelCatalogFingerprint?: string; // opaque compare-and-apply token for background model discovery
}
export interface ProviderModelCatalogUpdate {
    name: string;
    expectedFingerprint: string;
    models: string[];
    default: string;
    visionModels: string[];
}
export interface ProviderPresetView {
    id: string;
    label: string;
    description: string;
    keyEnv: string;
    providerNames: string[];
    models: string[];
    added: boolean;
    status?: "available" | "installed" | "installed_modified" | "name_conflict" | "similar_existing";
    statusProviderNames?: string[];
    keySet: boolean;
    requiresKey?: boolean;
    configured?: boolean;
    keySource?: string;
    keySourcePath?: string;
}
export interface ProviderModelOverrideView {
    model: string;
    reasoningProtocol: string;
    supportedEfforts: string[];
    defaultEffort: string;
    vision?: boolean | null;
    contextWindow?: number;
    maxOutputTokens?: number;
}
// BalanceInfo is the wallet-balance readout (desktop/app.go Balance). available
// is false when the provider declares no balanceUrl or a fetch failed; display is
// the formatted amount in an original wallet currency; no implicit FX conversion.
export interface BalanceInfo {
    available: boolean;
    display: string;
    detail?: string;
    complete?: boolean;
    rateDate?: string;
    approx?: boolean;
    currencies?: string[];
    primaryCurrency?: string;
    costDisplayCurrency?: string;
    multiCurrency?: boolean;
    err?: string;
}

