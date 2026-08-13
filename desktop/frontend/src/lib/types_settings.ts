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
export interface BotAllowlistView {
    enabled: boolean;
    allowAll: boolean;
    qqUsers: string[];
    feishuUsers: string[];
    weixinUsers: string[];
    qqApprovers: string[];
    feishuApprovers: string[];
    weixinApprovers: string[];
    qqAdmins: string[];
    feishuAdmins: string[];
    weixinAdmins: string[];
    qqGroups: string[];
    feishuGroups: string[];
    weixinGroups: string[];
}
export interface BotAccessView {
    enabled: boolean;
    allowAll: boolean;
    pairingEnabled: boolean;
    users: string[];
    groups: string[];
    approvers: string[];
    admins: string[];
}
export interface BotSelfUserIDsView {
    qq: string[];
    feishu: string[];
    weixin: string[];
}
export interface BotPairingView {
    enabled: boolean;
    requestTtlMinutes: number;
    maxPendingPerPlatform: number;
}
export interface BotControlView {
    enabled: boolean;
    addr: string;
    tokenEnv: string;
}
export interface BotRouteView {
    connectionId: string;
    platform: string;
    chatType: string;
    chatId: string;
    userId: string;
    threadId: string;
    model: string;
    toolApprovalMode: ToolApprovalMode | "" | string;
    workspaceRoot: string;
}
export interface QQBotView {
    enabled: boolean;
    appId: string;
    appSecretEnv: string;
    secretSet: boolean;
    sandbox: boolean;
    model: string;
    toolApprovalMode: ToolApprovalMode | "" | string;
    workspaceRoot: string;
    access: BotAccessView;
}
export interface FeishuBotView {
    enabled: boolean;
    domain: string;
    appId: string;
    appSecretEnv: string;
    secretSet: boolean;
    verificationToken: string;
    mode: string;
    webhookPort: number;
    requireMention: boolean;
}
export interface WeixinBotView {
    enabled: boolean;
    accountId: string;
    tokenEnv: string;
    tokenSet: boolean;
    apiBase: string;
}
export interface BotConnectionCredentialView {
    appId: string;
    appSecretEnv: string;
    accountId: string;
    tokenEnv: string;
    secretSet: boolean;
}
export interface BotConnectionSessionMappingView {
    remoteId: string;
    sessionId: string;
    sessionSource: string;
    chatType: string;
    userId: string;
    threadId: string;
    scope: "global" | "project" | string;
    workspaceRoot: string;
    updatedAt: string;
}
export interface BotConnectionView {
    id: string;
    provider: "qq" | "feishu" | "weixin" | string;
    domain: "qq" | "feishu" | "lark" | "weixin" | string;
    label: string;
    enabled: boolean;
    status: "disconnected" | "pending" | "connected" | "error" | string;
    model: string;
    toolApprovalMode: ToolApprovalMode | "" | string;
    workspaceRoot: string;
    access: BotAccessView;
    credential: BotConnectionCredentialView;
    sessionMappings: BotConnectionSessionMappingView[];
    lastError: string;
    createdAt: string;
    updatedAt: string;
}
export interface BotSettingsView {
    enabled: boolean;
    model: string;
    toolApprovalMode: ToolApprovalMode | "" | string;
    maxSteps: number;
    debounceMs: number;
    queueMode: string;
    queueCap: number;
    queueDrop: string;
    ignoreSelfMessages: boolean;
    selfUserIds: BotSelfUserIDsView;
    control: BotControlView;
    pairing: BotPairingView;
    routes: BotRouteView[];
    allowlist: BotAllowlistView;
    qq: QQBotView;
    feishu: FeishuBotView;
    weixin: WeixinBotView;
    connections: BotConnectionView[];
}
export interface BotRuntimeStatusView {
    running: boolean;
    status: string;
    message: string;
    connections: number;
    startedAt: string;
}
export interface BotInstallStartResult {
    ok: boolean;
    provider: string;
    domain: string;
    installId: string;
    url: string;
    deviceCode: string;
    userCode: string;
    interval: number;
    expireIn: number;
    message: string;
}
export interface BotInstallPollResult {
    done: boolean;
    connection: BotConnectionView;
    status: string;
    message: string;
    error: string;
}
export interface HookConfigView {
    event: string;
    match?: string;
    command: string;
    description?: string;
    timeout?: number;
    cwd?: string;
}
export interface HooksSettingsView {
    scope: string;
    path: string;
    projectRoot: string;
    trusted: boolean;
    hooks: HookConfigView[];
    events: string[];
}
export interface BotConnectionDiagnostic {
    id: string;
    label: string;
    status: string;
    message: string;
    messageId: string;
    phase: string;
    code: string;
    reportKind: string;
    reportDetail: string;
    occurredAt: string;
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
    bot: BotSettingsView;
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
    checkUpdates: boolean; // check for new versions on startup
    updateChannel: string; // compatibility field; always "stable"
    telemetry: boolean; // anonymous launch ping + scrubbed next-launch native crash diagnostics
    metrics: boolean; // aggregate quality/lifecycle metrics (anonymous signal/bucket counts)
    configPath: string;
    shadowedByPath?: string; // workspace reasonix.toml that outranks configPath, when one exists
    providerKinds: string[]; // provider implementations the kernel registered (for the kind picker)
    autoApproveTools: boolean;
    bypass: boolean; // legacy JSON key for live YOLO/full-access tool auto-approval
    conversationWidth?: string; // "standard" | "full"; absent from older Wails payloads
}
export interface DesktopStartupSettingsView {
    bot: BotSettingsView;
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
    checkUpdates: boolean; // check for new versions on startup
    updateChannel: string; // compatibility field; always "stable"
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
// Auto-updater payloads (desktop/updater.go). UpdateInfo drives the update banner;
// UpdateProgress streams on the "updater:progress" event during download/install.
export interface UpdateInfo {
    available: boolean;
    current: string;
    latest: string;
    notes: string;
    channel: string;
    canSelfUpdate: boolean; // macOS true only for signed/notarized builds
    manualOnly?: boolean;
    manualReason?: string;
    installMode?: "portable" | "deb" | "manual" | string;
    requiresElevation?: boolean;
    downloaded: boolean;
    downloadUrl: string; // human-facing releases page (macOS path / fallback link)
    assetSize: number; // running platform's artifact size, for the progress bar
    err?: string; // set when the check itself failed (both endpoints down)
}
export interface UpdateDownloadResult {
    requestId: string;
    version: string;
    channel: string;
    path: string;
    size: number;
    sha256: string;
}
export interface UpdateProgress {
    requestId: string;
    version: string;
    channel: "stable" | "preview" | string;
    phase: "downloading" | "verifying" | "downloaded" | "authorizing" | "recovering" | "installing" | "relaunching" | "done" | "error";
    received: number;
    total: number;
    err?: string;
}
// Task Monitor panel types (internal/taskmonitor).
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

