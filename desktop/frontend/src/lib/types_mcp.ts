// MCP & Skills drawer (desktop/app.go Capabilities) — the GUI counterpart to
// /mcp + /skill: connected/failed servers and discoverable skills.
export interface ServerView {
    name: string;
    transport: string;
    status: "connected" | "deferred" | "failed" | "initializing" | "disabled";
    /** @deprecated derived from enabled */
    startIntent?: "off" | "automatic" | string;
    runtimeState?: "idle" | "connecting" | "ready" | "issue" | string;
    /** Product availability: available_on_demand | starting | connected | auth_required | project_auth_changed | start_failed | disabled */
    availability?: string;
    enabled?: boolean;
    installed?: boolean;
    action?: "none" | "authenticate" | "authorize" | "retry" | string;
    source?: "project" | "user" | "plugin" | "builtin" | string;
    configSource?: string;
    builtIn?: boolean;
    configured?: boolean;
    /** @deprecated same as enabled */
    autoStart: boolean;
    /** @deprecated ignored by runtime */
    tier?: "background" | "eager" | string;
    command?: string;
    args?: string[];
    url?: string;
    envKeys?: string[];
    headerKeys?: string[];
    tools: number;
    toolCount?: number;
    prompts: number;
    resources: number;
    hasTools?: boolean;
    error?: string;
    toolList?: MCPToolView[];
    callTimeoutSeconds?: number;
    toolTimeoutSeconds?: Record<string, number>;
    requiresLaunchApproval?: boolean;
    authStatus?: "none" | "possible" | "required" | string;
    authUrl?: string;
    authConfigured?: boolean;
    managedByPlugin?: string;
}
export interface MCPToolView {
    name: string;
    description: string;
    readOnlyHint?: boolean;
    destructiveHint?: boolean;
    schemaError?: string;
}
export interface SkillView {
    name: string;
    description: string;
    scope: string;
    sourceDir?: string;
    runAs: string;
    enabled: boolean;
    plugin?: string;
    model?: string;
    effort?: string;
    allowedTools?: string[];
    readOnly?: boolean;
    color?: string;
    invocation?: string;
    invocationMode?: string;
    body?: string;
    configuredModel?: string;
    configuredEffort?: string;
}
export interface SkillRootSkillView {
    name: string;
    description: string;
    scope: string;
    runAs: string;
    plugin?: string;
    model?: string;
    effort?: string;
    allowedTools?: string[];
    color?: string;
    invocation?: string;
}
export interface SkillRootView {
    dir: string;
    scope: string;
    priority: number;
    status: string;
    enabled: boolean;
    configured: boolean;
    removable: boolean;
    skills: number;
    skillItems?: SkillRootSkillView[];
    warning?: string;
}
export interface CapabilitiesView {
    servers: ServerView[];
    skills: SkillView[];
    skillRoots: SkillRootView[];
    plugins: PluginView[];
    allowImplicitInvocation?: boolean;
}
export interface SkillsSettingsView {
    skills: SkillView[];
    skillRoots: SkillRootView[];
    allowImplicitInvocation?: boolean;
}
export interface SubagentProfileInput {
    name: string;
    description: string;
    systemPrompt: string;
    color?: string;
    model?: string;
    effort?: string;
    allowedTools?: string[];
    readOnly?: boolean;
    scope?: "project" | "global";
}
export interface PluginView {
    name: string;
    version?: string;
    description?: string;
    source?: string;
    root: string;
    manifestKind?: string;
    enabled: boolean;
    skills: number;
    commands?: number;
    hooks: number;
    mcpServers: number;
    agents?: number;
    compatibility?: "full" | "partial" | "none" | string;
    mappedCapabilities?: string[];
    skippedCapabilities?: PluginCompatibilityIssue[];
    skillDetails?: PluginSkillView[];
    agentDetails?: PluginAgentView[];
    commandDetails?: PluginCommandView[];
    hookDetails?: PluginHookView[];
    mcpServerDetails?: PluginMCPServerView[];
    warnings?: string[];
    error?: string;
}
export interface PluginCompatibilityIssue {
    capability: string;
    path?: string;
    reason: string;
}
export interface PluginAgentView {
    name: string;
    description?: string;
    path?: string;
    invocation?: string;
    model?: string;
    allowedTools?: string[];
}
export interface PluginSkillView {
    name: string;
    description?: string;
    path?: string;
    invocation?: string;
    runAs?: string;
}
export interface PluginCommandView {
    name: string;
    description?: string;
    argHint?: string;
    path?: string;
    invocation?: string;
    shadowed?: boolean;
    shadowedByPlugin?: string;
}
export interface PluginHookView {
    event: string;
    match?: string;
    command?: string;
    contextFile?: string;
    description?: string;
}
export interface PluginMCPServerView {
    name: string;
    displayName?: string;
    description?: string;
    transport?: string;
    command?: string;
    url?: string;
    autoStart?: boolean;
}
export interface PluginInstallOptions {
    dryRun?: boolean;
    link?: boolean;
    replace?: boolean;
    name?: string;
}
export interface MCPServerInput {
    name: string;
    transport: string; // stdio | http | sse
    command: string;
    args: string[];
    url: string;
    env?: Record<string, string> | null;
    headers?: Record<string, string> | null;
    autoStart?: boolean | null;
    callTimeoutSeconds?: number | null;
    toolTimeoutSeconds?: Record<string, number> | null;
}
export interface MCPInstallResult {
    name: string;
    state: "ready" | "action_required" | "issue";
    toolCount: number;
    action: "none" | "authenticate" | "authorize" | "retry";
    message: string;
}
export interface MCPMarketplaceEntry {
    name: string;
    suggestedName: string;
    title?: string;
    description?: string;
    version?: string;
    repositoryUrl?: string;
    installable: boolean;
    unavailableReason?: string;
    transport?: "stdio" | "http" | "sse" | string;
    command?: string;
    args: string[];
    url?: string;
}
export interface MCPMarketplaceView {
    servers: MCPMarketplaceEntry[];
    cached: boolean;
    warning?: string;
}
export interface ModelInfo {
    ref: string; // "provider/model" — pass to SetModel
    provider: string;
    model: string;
    current: boolean;
}
export interface EffortInfo {
    supported: boolean;
    current: string; // "auto" | "low" | "medium" | "high" | "xhigh" | "max"
    default: string;
    levels: string[];
}
// Slash sub-command / argument completion (desktop/app.go SlashArgs). Mirrors the
// CLI's arg hints so the composer can suggest e.g. /skill → list/show/new/paths.
export interface SlashArgItem {
    label: string;
    insert: string; // token to place at the current position
    hint: string;
    descend: boolean; // re-open the menu one level deeper after accepting
}
export interface SlashArgsResult {
    items: SlashArgItem[];
    from: number; // byte offset where the current token begins
}

