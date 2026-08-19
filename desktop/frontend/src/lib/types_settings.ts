import { JobView, ActiveWorkView } from "./types";
import { ToolApprovalMode } from "./types_mode";
import { ProviderView, ProviderPresetView } from "./types_remote";
export interface JobCancelBatchView {
    cancelled: string[];
    notRunning: string[];
}
export interface BackgroundRuntimeView {
    tabId: string;
    title: string;
    detached: boolean;
    running: boolean;
    pendingPrompt: boolean;
    jobs: JobView[];
}
export interface WorkspaceConflictView {
    state: "none" | "local" | "external";
    ownerTabId?: string;
    ownerTitle?: string;
    ownerWork: ActiveWorkView;
    canReveal: boolean;
    canCreateWorktree: boolean;
}
export interface PermissionsView {
    mode: string; // "ask" | "allow" | "deny"
    allow: string[];
    ask: string[];
    deny: string[];
}
export interface SandboxView {
    bash: string; // "enforce" | "off"
    network: boolean;
    workspaceRoot: string;
    allowWrite: string[];
    effectiveWorkspaceRoot: string;
    effectiveWriteRoots: string[];
    shell: string; // "auto" | "bash" | "powershell" | "pwsh"
    effectiveShell?: string; // "bash" | "git-bash" | "powershell" | "pwsh"
}
export interface NetworkProxyView {
    type: string;
    server: string;
    port: number;
    username: string;
    password: string;
}
export interface NetworkView {
    proxyMode: string; // "auto" | "custom" | "off" (backend may still return legacy "env")
    proxyUrl: string;
    noProxy: string;
    proxy: NetworkProxyView;
}
export interface AgentView {
    temperature: number;
    maxSteps: number;
    plannerMaxSteps: number;
    maxSubagentDepth: number;
    maxSubagentConcurrency: number;
    maxParallelWriters: number;
    systemPrompt: string;
    reasoningLanguage: string; // "auto" | "zh" | "en"
    compactRatio?: number; // Advanced global default; older backends omit it.
    effectiveCompactRatio?: number; // Active local session after project overrides.
    compactRatioOverridden?: boolean;
}
export interface SettingsView {
    defaultModel: string;
    plannerModel: string;
    subagentModel: string;
    subagentEffort: string;
    autoPlan: string;
    providers: ProviderView[];
    officialProviders: ProviderView[];
    providerPresets: ProviderPresetView[];
    permissions: PermissionsView;
    sandbox: SandboxView;
    network: NetworkView;
    agent: AgentView;
    desktopLanguage: string; // "" | "en" | "zh"; empty = auto
    desktopCurrency?: string; // "" | "CNY" | "USD"; absent/empty = follow language
    desktopLayoutStyle: string; // "classic" | "workbench" | "creation"
    desktopTheme: string; // "auto" | "dark" | "light"
    desktopThemeStyle: string;
    desktopTerminalTheme: string; // "auto" follows app | "dark" | "light"
    closeBehavior: string; // "background" | "quit"
    displayMode: string;
    reasoningDisplayMode: string;
    reasoningDisplayModeExplicit?: boolean;
    statusBarStyle: string; // "icon" | "text"
    statusBarItems: string[]; // ordered visible status bar item ids
    defaultToolApprovalMode: ToolApprovalMode | string; // default for newly-created sessions
    configPath: string;
    shadowedByPath?: string; // workspace reasonix.toml that outranks configPath, when one exists
    providerKinds: string[]; // provider implementations the kernel registered (for the kind picker)
    autoApproveTools: boolean;
    bypass: boolean; // legacy JSON key for live YOLO/full-access tool auto-approval
    conversationWidth?: string; // "standard" | "full"; absent from older Wails payloads
}
export interface DesktopStartupSettingsView {
    desktopLanguage: string; // "" | "en" | "zh"; empty = auto
    desktopLayoutStyle: string; // "classic" | "workbench"
    desktopTheme: string; // "auto" | "dark" | "light"
    desktopThemeStyle: string;
    desktopTerminalTheme: string; // "auto" follows app | "dark" | "light"
    displayMode: string;
    reasoningDisplayMode: string;
    reasoningDisplayModeExplicit?: boolean;
    statusBarStyle: string; // "icon" | "text"
    statusBarItems: string[]; // ordered visible status bar item ids
    conversationWidth?: string; // "standard" | "full"; absent from older Wails payloads
    configWarnings?: string[];
    configWarningsRevision?: number; // load recovery notices and async delivery barrier
    configPath?: string;
}
export type ExternalOpenerKind = "file-manager" | "editor" | "terminal";
export interface ExternalOpenerView {
    id: string;
    name: string;
    kind: ExternalOpenerKind;
    iconDataUrl?: string;
}
export interface ExternalOpenersView {
    openers: ExternalOpenerView[];
    preferred: string;
    workspaceOpenable?: boolean;
}
export type TaskState = "queued" | "running" | "waiting" | "succeeded" | "failed" | "cancelled" | "stale" | string; // forward-compat
export type RuntimeState = "unknown" | "alive" | "exited" | string;
export interface TaskSnapshot {
    schema_version: number;
    task_id: string;
    job_id?: string; // jobs.Manager-local runtime identifier
    session_id: string;
    state: TaskState;
    runtime_state?: RuntimeState; // absent in snapshots written before this field existed
    version: number;
    created_at: string; // ISO 8601
    updated_at: string; // ISO 8601
    error_code?: string;
    error_summary?: string;
}
export interface ControlResult {
    schema_version: number;
    command: string;
    task_id: string;
    session_id?: string;
    state?: TaskState;
    runtime_state?: RuntimeState;
    version?: number;
    accepted: boolean;
    idempotent: boolean;
    error?: {
        code: string;
        message: string;
    };
}
export interface TaskEvent {
    sequence: number;
    timestamp: string; // ISO 8601
    event_type: string;
    task_id: string;
    session_id: string;
    state: TaskState;
    runtime_state?: RuntimeState;
    error_code?: string;
    error_summary?: string;
}

