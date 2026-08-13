// Wails and the browser mock share this React-to-Go contract.
// @ts-ignore `wails generate module` creates this locally; fresh checkouts keep
// typecheck green by falling back to a disabled drift check below.
import type * as GeneratedApp from "../../wailsjs/go/main/App";
import type { InvocationRequest } from "./invocationDisplay";
import { type HistoryCatalogBindings } from "./historyCatalogBridge";
import { type TaskCatalogBindings } from "./taskCatalogBridge";
import type { RemoteHostView, RemoteHostInput, RemoteConnectionStatus, RemoteDirEntry, RemoteFilePreview, RemoteWriteResult, RemoteForwardInput, RemoteForwardView, RemoteServerView, RemoteLegacyWorkbenchData, BalanceInfo, UsageStatsRange, UsageStatsRequest, BotConnectionDiagnostic, BotInstallPollResult, BotInstallStartResult, BotRuntimeStatusView, BotSettingsView, CapabilitiesView, CapabilityDiagnosticsReport, RuntimeDoctorReport, CheckpointMeta, CommandInfo, ControlResult, ContextInfo, ContextPanelInfo, DirEntry, DesktopStartupSettingsView, DeliveryWorktreeAvailability, DeliveryWorktreeOpenResult, DroppedItem, EffortInfo, ExtensionActionView, FilePreview, ExternalOpenersView, HistoryMessage, HistoryPage, HistoryContentChunk, HistoryContentRef, HistorySlice, HistorySliceRequest, TopicActivationRequest, TopicActivationTicket, HookConfigView, HooksSettingsView, JobView, ActiveWorkView, BackgroundRuntimeView, JobCancelBatchView, WorkspaceConflictView, MCPMarketplaceEntry, MCPServerInput, MCPInstallResult, MCPMarketplaceView, MCPToolView, MemoryFact, MemorySuggestion, MemorySuggestionsView, MemoryView, Meta, ModelInfo, NetworkView, PluginInstallOptions, PluginView, ProjectNode, RecoveryLineageView, RecoveryCleanupRequest, RecoveryCleanupResult, SessionCatalogBindings, PromptHistoryResult, ProviderModelCatalogUpdate, ProviderView, QuestionAnswer, ServerView, SessionMeta, SettingsView, SkillsSettingsView, SkillSuggestion, TaskEvent, TaskSnapshot, SlashArgsResult, SubagentProfileInput, TabMeta, TerminalSessionView, TerminalWorkspaceView, TopicMeta, UpdateInfo, WorkspaceChangeDetailView, WorkspaceChangesView, WorkspaceRevisions, GitCommitView, GitCommitDetailView, WorkspaceView, SessionClearResult } from "./types";
// AppBindings is derived from the Wails-generated Go → TS method signatures, so
// the compiler catches drift between the Go binding surface and the frontend mock.
// Run `wails generate module` after adding/renaming a bound method on App, then
// `pnpm typecheck` to verify the mock still satisfies the contract.
//
// Types for the new native-feel bindings — kept inline since they are
// bridge-specific and only used in AppBindings / the dev mock.
interface NativeConfirmRequest {
    title: string;
    message: string;
    detail: string;
    confirmLabel: string;
    cancelLabel: string;
    destructive: boolean;
}
interface DesktopWindowState {
    width: number;
    height: number;
    x: number;
    y: number;
    maximised: boolean;
}
// AppBindings is the hand-written contract between the React app and the Go
// kernel. It uses local types (types.ts) so components don't import generated
// model classes. _CheckGeneratedBindings catches drift: when a Go method is
// added or renamed, the generated types shift, and a key present in GeneratedApp
// but missing from AppBindings causes a type error here. Fix: add the new method
// to AppBindings, then run `pnpm typecheck` to verify.
export interface AppBindings extends SessionCatalogBindings, HistoryCatalogBindings, TaskCatalogBindings {
    Platform(): Promise<string>;
    MinimiseMainWindow(): Promise<void>;
    ToggleMaximiseMainWindow(): Promise<void>;
    IsMainWindowMaximised(): Promise<boolean>;
    CloseMainWindow(): Promise<void>;
    // ── Heartbeat ──
    HeartbeatListTasks(): Promise<unknown>;
    HeartbeatReloadTasks(): Promise<unknown>;
    HeartbeatSaveTasks(tasks: unknown): Promise<void>;
    HeartbeatReloadConfig(): Promise<unknown>;
    HeartbeatSaveConfig(update: unknown): Promise<unknown>;
    HeartbeatTriggerNow(id: string): Promise<void>;
    HeartbeatGenerateID(): Promise<string>;
    Submit(input: string): Promise<void>;
    SubmitToTab(tabID: string, input: string): Promise<void>;
    SubmitToTabWithID(tabID: string, input: string, submissionID: string): Promise<void>;
    SubmitDisplay(display: string, input: string): Promise<void>;
    SubmitDisplayToTab(tabID: string, display: string, input: string): Promise<void>;
    SubmitDisplayToTabWithID(tabID: string, display: string, input: string, submissionID: string): Promise<void>;
    SubmitDeliveryRecoveryToTab(tabID: string, display: string, input: string): Promise<void>;
    SubmitDeliveryRecoveryToTabWithID(tabID: string, display: string, input: string, submissionID: string): Promise<void>;
    SubmitDeliveryWaiverToTabWithID(tabID: string, display: string, input: string, submissionID: string): Promise<void>;
    SubmitInvocationsToTab(tabID: string, display: string, input: string, invocations: InvocationRequest[]): Promise<void>;
    SubmitInvocationsToTabWithID(tabID: string, display: string, input: string, invocations: InvocationRequest[], submissionID: string): Promise<void>;
    SubmitInitialGoalToTab(tabID: string, goal: string, display: string, input: string, invocations: InvocationRequest[], collaborationMode: string, toolApprovalMode: string): Promise<string[]>;
    SubmitInitialGoalToTabWithID(tabID: string, goal: string, display: string, input: string, invocations: InvocationRequest[], collaborationMode: string, toolApprovalMode: string, submissionID: string): Promise<string[]>;
    SubmitEditedDisplayToTab(tabID: string, display: string, input: string, original: string): Promise<void>;
    SubmitEditedDisplayToTabWithID(tabID: string, display: string, input: string, original: string, submissionID: string): Promise<void>;
    RunShell(command: string): Promise<void>;
    RunShellForTab(tabID: string, command: string): Promise<void>;
    Steer(text: string): Promise<void>;
    SteerForTab(tabID: string, text: string): Promise<void>;
    InboxSnapshot(tabID: string): Promise<{
        revision: number;
        paused: boolean;
        recovered: boolean;
        recoveredCount?: number;
        items: Array<{
            id: string;
            intent: string;
            state: string;
            preview: string;
            byteSize: number;
            source?: string;
            position: number;
            blockReason?: string;
        }>;
        itemsCount: number;
        bytes: number;
        maxItems: number;
        maxBytes: number;
    }>;
    EnqueueInboxFollowup(tabID: string, display: string, submit: string, idempotency: string): Promise<{
        itemId: string;
        disposition: string;
        position: number;
        paused: boolean;
        idempotent?: boolean;
        error?: string;
    }>;
    EnqueueInboxFollowupWithInvocations(tabID: string, display: string, submit: string, invocations: InvocationRequest[], idempotency: string): Promise<{
        itemId: string;
        disposition: string;
        position: number;
        paused: boolean;
        idempotent?: boolean;
        error?: string;
    }>;
    EnqueueInboxSteer(tabID: string, display: string, submit: string, idempotency: string): Promise<{
        itemId: string;
        disposition: string;
        position: number;
        paused: boolean;
        idempotent?: boolean;
        error?: string;
    }>;
    SteerInboxItem(tabID: string, itemID: string): Promise<{
        itemId: string;
        disposition: string;
        position: number;
        paused: boolean;
        idempotent?: boolean;
        error?: string;
    }>;
    ReadInboxItem(tabID: string, id: string): Promise<{
        id: string;
        displayText: string;
        rawText: string;
        submitText: string;
    }>;
    UpdateInboxItem(tabID: string, id: string, display: string, submit: string): Promise<void>;
    DeleteInboxItem(tabID: string, id: string): Promise<void>;
    MoveInboxItem(tabID: string, id: string, toIndex: number): Promise<void>;
    SetInboxPaused(tabID: string, paused: boolean): Promise<void>;
    RetryInboxItem(tabID: string, id: string): Promise<void>;
    RefreshInboxItem(tabID: string, id: string): Promise<void>;
    InboxHasItems(tabID: string): Promise<boolean>;
    Cancel(): Promise<void>;
    CancelTab(tabID: string): Promise<void>;
    CancelTabWithInboxItems(tabID: string, itemIDs: string[]): Promise<void>;
    Approve(id: string, allow: boolean, session: boolean, persist: boolean): Promise<void>;
    ApproveTab(tabID: string, id: string, allow: boolean, session: boolean, persist: boolean): Promise<void>;
    ResolvePlanDecision(id: string, action: "start_execution" | "revise_plan" | "exit_plan"): Promise<void>;
    ResolvePlanDecisionTab(tabID: string, id: string, action: "start_execution" | "revise_plan" | "exit_plan"): Promise<void>;
    ResolveRecovery(id: string, action: string, feedback: string): Promise<void>;
    ResolveRecoveryTab(tabID: string, id: string, action: string, feedback: string): Promise<void>;
    // Legacy no-ops: Auto Guard is always built into Auto.
    SetRecoveryCheckpointEnabled(enabled: boolean): Promise<void>;
    SetRecoveryCheckpointEnabledTab(tabID: string, enabled: boolean): Promise<void>;
    RecoveryCheckpointEnabled(): Promise<boolean>;
    RecoveryCheckpointEnabledTab(tabID: string): Promise<boolean>;
    AnswerQuestion(id: string, answers: QuestionAnswer[]): Promise<void>;
    AnswerQuestionForTab(tabID: string, id: string, answers: QuestionAnswer[]): Promise<void>;
    ReplayPendingPrompts(): Promise<void>;
    SetPlanMode(on: boolean): Promise<void>;
    SetMode(mode: string): Promise<void>;
    // Resolves with the pending approval prompt ids the switch auto-allowed
    // (drained); prompts not listed are still pending backend-side (#6432).
    SetModeForTab(tabID: string, mode: string): Promise<string[] | void>;
    SetAutoApproveTools(on: boolean): Promise<void>;
    SetCollaborationMode(mode: string): Promise<void>;
    SetCollaborationModeForTab(tabID: string, mode: string): Promise<void>;
    SetToolApprovalMode(mode: string): Promise<void>;
    // Same drained-prompt-id contract as SetModeForTab.
    SetToolApprovalModeForTab(tabID: string, mode: string): Promise<string[] | void>;
    // Atomically applies the controller-facing composer profile and reports any
    // approval prompts drained by the resulting tool-approval posture.
    SetComposerProfileForTab(tabID: string, collaborationMode: string, toolApprovalMode: string, goal: string): Promise<string[] | void>;
    SetGoal(goal: string): Promise<void>;
    SetGoalForTab(tabID: string, goal: string): Promise<void>;
    ResumeGoalForTab(tabID: string): Promise<boolean>;
    PauseGoalForTab(tabID: string): Promise<boolean>;
    ClearGoal(): Promise<void>;
    ClearGoalForTab(tabID: string): Promise<void>;
    Compact(): Promise<void>;
    CompactForTab(tabID: string): Promise<void>;
    NewSession(): Promise<void>;
    NewSessionForTab(tabID: string): Promise<void>;
    ClearSession(): Promise<SessionClearResult>;
    ClearSessionForTab(tabID: string): Promise<SessionClearResult>;
    History(): Promise<HistoryMessage[]>;
    HistoryForTab(tabID: string): Promise<HistoryMessage[]>;
    HistoryPage(beforeTurn: number, limit: number): Promise<HistoryPage>;
    HistoryPageForTab(tabID: string, beforeTurn: number, limit: number): Promise<HistoryPage>;
    // Windowed history paging (supersedes HistoryPageForTab for tab history).
    HistorySliceForTab(tabID: string, req: HistorySliceRequest): Promise<HistorySlice>;
    HistoryContentForTab(tabID: string, ref: HistoryContentRef, chunkIndex: number): Promise<HistoryContentChunk>;
    HistoryCheckpointTurnsForTab(tabID: string): Promise<number[]>;
    Checkpoints(): Promise<CheckpointMeta[]>;
    CheckpointsForTab(tabID: string): Promise<CheckpointMeta[]>;
    Rewind(turn: number, scope: string): Promise<void>;
    RewindForTab(tabID: string, turn: number, scope: string): Promise<void>;
    PreviewRewindForTab(tabID: string, turn: number, scope: string): Promise<import("./types").RewindPlanView>;
    CommitRewindForTab(tabID: string, planID: string, turn: number, scope: string): Promise<import("./types").RewindResultView>;
    UndoRewindForTab(tabID: string, transactionID: string): Promise<import("./types").RewindResultView>;
    PreviewWorkspaceFileRevertForTab(tabID: string, path: string): Promise<import("./types").RewindPlanView>;
    CommitWorkspaceFileRevertForTab(tabID: string, planID: string, resolution: string): Promise<import("./types").RewindResultView>;
    Fork(turn: number): Promise<TabMeta>;
    ForkForTab(tabID: string, turn: number): Promise<TabMeta>;
    SummarizeFrom(turn: number): Promise<void>;
    SummarizeFromForTab(tabID: string, turn: number): Promise<void>;
    SummarizeUpTo(turn: number): Promise<void>;
    SummarizeUpToForTab(tabID: string, turn: number): Promise<void>;
    ListSessions(): Promise<SessionMeta[]>;
    ListSessionsForTab(tabID: string): Promise<SessionMeta[]>;
    ListTrashedSessions(): Promise<SessionMeta[]>;
    ResumeSession(path: string): Promise<HistoryMessage[]>;
    ResumeSessionForTab(tabID: string, path: string): Promise<HistoryMessage[]>;
    ResumeSessionPage(path: string, limit: number): Promise<HistoryPage>;
    ResumeSessionPageForTab(tabID: string, path: string, limit: number): Promise<HistoryPage>;
    OpenChannelSessionForTab(tabID: string, path: string): Promise<HistoryMessage[]>;
    OpenChannelSessionPageForTab(tabID: string, path: string, limit: number): Promise<HistoryPage>;
    PreviewSession(path: string): Promise<HistoryMessage[]>;
    DeleteSession(path: string): Promise<void>;
    DeleteRecoveryCopy(path: string): Promise<void>;
    GetRecoveryLineage(key: {
        scope: string;
        workspaceRoot?: string;
        topicId: string;
    }): Promise<RecoveryLineageView>;
    ChooseRecoveryBranch(request: import("./types").RecoveryPreferenceRequest): Promise<void>;
    CleanRecoveryLineage(request: RecoveryCleanupRequest): Promise<RecoveryCleanupResult>;
    RestoreSession(path: string): Promise<void>;
    PurgeTrashedSession(path: string): Promise<void>;
    PurgeRecoveryCopy(path: string): Promise<void>;
    RenameSession(path: string, title: string): Promise<void>;
    ScanPromptHistory(nonce: string): Promise<PromptHistoryResult>;
    ListWorkspaces(): Promise<WorkspaceView[]>;
    PickWorkspace(): Promise<string>;
    SwitchWorkspace(path: string): Promise<string>;
    RemoveWorkspace(path: string): Promise<void>;
    ContextUsage(): Promise<ContextInfo>;
    ContextUsageForTab(tabID: string): Promise<ContextInfo>;
    Balance(): Promise<BalanceInfo>;
    BalanceForTab(tabID: string): Promise<BalanceInfo>;
    UsageStats(req: UsageStatsRequest): Promise<UsageStatsRange>;
    Jobs(): Promise<JobView[]>;
    ListTasks(): Promise<TaskSnapshot[]>;
    CurrentTaskSessionID(): Promise<string>;
    ListTasksForSession(sessionID: string): Promise<TaskSnapshot[]>;
    GetTask(taskID: string): Promise<TaskSnapshot | null>;
    ListTaskEvents(taskID: string, afterSequence: number): Promise<TaskEvent[]>;
    StopTask(taskID: string, expectedVersion: number, reason: string, idemKey: string): Promise<ControlResult>;
    CancelTask(taskID: string, expectedVersion: number, reason: string, idemKey: string): Promise<ControlResult>;
    RequeueTask(taskID: string, expectedVersion: number, idemKey: string): Promise<ControlResult>;
    OpenTaskSession(taskID: string): Promise<ControlResult>;
    ListTasksForTab(tabID: string): Promise<TaskSnapshot[]>;
    ListTaskEventsForTab(tabID: string, taskID: string, afterSequence: number): Promise<TaskEvent[]>;
    StopTaskForTab(tabID: string, taskID: string, expectedVersion: number, reason: string, idemKey: string): Promise<ControlResult>;
    CancelTaskForTab(tabID: string, taskID: string, expectedVersion: number, reason: string, idemKey: string): Promise<ControlResult>;
    RequeueTaskForTab(tabID: string, taskID: string, expectedVersion: number, idemKey: string): Promise<ControlResult>;
    OpenTaskSessionForTab(tabID: string, taskID: string): Promise<ControlResult>;
    JobsForTab(tabID: string): Promise<JobView[]>;
    CancelJob(jobID: string): Promise<boolean>;
    CancelJobForTab(tabID: string, jobID: string): Promise<boolean>;
    CancelJobsForTab(tabID: string, jobIDs: string[]): Promise<JobCancelBatchView>;
    ActiveWorkForTab(tabID: string): Promise<ActiveWorkView>;
    BackgroundRuntimes(): Promise<BackgroundRuntimeView[]>;
    RevealBackgroundRuntime(tabID: string): Promise<TabMeta>;
    WorkspaceConflictForTab(tabID: string): Promise<WorkspaceConflictView>;
    RevealWorkspaceWriterForTab(tabID: string): Promise<TabMeta>;
    CloseTabWithPolicy(tabID: string, policy: "keep_running" | "stop_and_close"): Promise<void>;
    ToolResultForTab(tabID: string, toolID: string): Promise<{
        args: string;
        output: string;
        execution?: import("./types").WireShellExecution;
    } | null>;
    Meta(): Promise<Meta>;
    MetaForTab(tabID: string): Promise<Meta>;
    Commands(): Promise<CommandInfo[]>;
    Capabilities(): Promise<CapabilitiesView>;
    MCPServers(): Promise<ServerView[]>;
    MCPMarketplace(query: string): Promise<MCPMarketplaceView>;
    MCPMarketplaceResolve(registryName: string): Promise<MCPMarketplaceEntry>;
    SkillsSettings(): Promise<SkillsSettingsView>;
    CapabilityDiagnostics(includeSessionRuntime: boolean): Promise<CapabilityDiagnosticsReport>;
    RuntimeDoctor(): Promise<RuntimeDoctorReport>;
    Plugins(): Promise<PluginView[]>;
    PlanPluginInstall(source: string, options: PluginInstallOptions): Promise<string>;
    InstallPlugin(source: string, options: PluginInstallOptions): Promise<string>;
    RemovePlugin(name: string): Promise<void>;
    SetPluginEnabled(name: string, enabled: boolean): Promise<void>;
    UpdatePlugin(name: string): Promise<string>;
    PluginDoctor(name: string): Promise<PluginView>;
    // Extension UI (stage 8b2): enumerate handshake-declared extension actions
    // for the command palette, invoke one, and deliver a form surface's values
    // (or {cancelled: true} on dismissal) back to the owning sidecar.
    ExtensionActions(tabID: string): Promise<ExtensionActionView[]>;
    InvokeExtensionAction(tabID: string, name: string, args: Record<string, string>): Promise<string>;
    SubmitExtensionForm(tabID: string, pluginID: string, surfaceID: string, values: Record<string, unknown>): Promise<void>;
    AddMCPServer(input: MCPServerInput): Promise<number>;
    InstallMCPServer(input: MCPServerInput): Promise<MCPInstallResult>;
    UpdateMCPServer(name: string, input: MCPServerInput): Promise<void>;
    RemoveMCPServer(name: string): Promise<void>;
    AuthorizeAndConnectMCPServer(name: string): Promise<void>;
    AuthenticateMCPServer(name: string): Promise<void>;
    ReconnectMCPServer(name: string): Promise<void>;
    ClearMCPServerAuthentication(name: string): Promise<void>;
    PickSkillFolder(): Promise<string>;
    PickPluginFolder(): Promise<string>;
    AddSkillPath(path: string): Promise<void>;
    RemoveSkillPath(path: string): Promise<void>;
    SetSkillPathEnabled(path: string, enabled: boolean): Promise<void>;
    RefreshSkills(): Promise<void>;
    ReloadCommands(): Promise<void>;
    SetSkillEnabled(name: string, enabled: boolean): Promise<void>;
    SetSkillImplicitInvocation(enabled: boolean): Promise<void>;
    AvailableSubagentTools(): Promise<MCPToolView[]>;
    CreateSubagentProfile(input: SubagentProfileInput): Promise<string>;
    UpdateSubagentProfile(name: string, scope: string, input: SubagentProfileInput): Promise<void>;
    DeleteSubagentProfile(name: string, scope: string): Promise<void>;
    SetSubagentProfileModel(name: string, ref: string): Promise<void>;
    SetSubagentProfileEffort(name: string, level: string): Promise<void>;
    TrySubagentProfile(input: SubagentProfileInput, task: string): Promise<string>;
    CancelTrySubagentProfile(): Promise<void>;
    SetMCPServerEnabled(name: string, enabled: boolean): Promise<void>;
    SetMCPServerTier(name: string, tier: string): Promise<void>;
    SlashArgs(input: string): Promise<SlashArgsResult>;
    ListDir(rel: string): Promise<DirEntry[]>;
    ListDirForTab(tabID: string, rel: string): Promise<DirEntry[]>;
    SearchFileRefs(query: string): Promise<DirEntry[]>;
    SearchFileRefsForTab(tabID: string, query: string): Promise<DirEntry[]>;
    ReadFile(rel: string): Promise<FilePreview>;
    ReadFileForTab(tabID: string, rel: string): Promise<FilePreview>;
    WorkspaceRevisionForTab(tabID: string): Promise<{
        revisions: WorkspaceRevisions;
        watchState: "active" | "degraded" | "unavailable";
    }>;
    WorkspaceChanges(tabID: string): Promise<WorkspaceChangesView>;
    WorkspaceChangeDetail(tabID: string, path: string): Promise<WorkspaceChangeDetailView>;
    GitBranches(): Promise<string[]>;
    GitCheckout(branch: string): Promise<void>;
    WorkspaceGitHistory(tabID: string, path: string): Promise<GitCommitView[]>;
    WorkspaceGitCommitDetail(tabID: string, hash: string, path: string): Promise<GitCommitDetailView>;
    OpenWorkspacePath(rel: string): Promise<void>;
    OpenWorkspacePathForTab(tabID: string, rel: string): Promise<void>;
    ResolveWorkspacePathForTab(tabID: string, rel: string): Promise<string>;
    ExternalOpeners(): Promise<ExternalOpenersView>;
    ExternalOpenersForTab(tabID: string): Promise<ExternalOpenersView>;
    SetPreferredExternalOpener(id: string): Promise<void>;
    OpenWorkspaceInExternalOpener(id: string): Promise<void>;
    OpenWorkspaceInExternalOpenerForTab(tabID: string, id: string): Promise<void>;
    OpenLocalPathInExternalOpener(path: string, id: string): Promise<void>;
    SaveLocalPathAs(path: string): Promise<string>;
    RevealWorkspacePath(rel: string): Promise<void>;
    RevealWorkspacePathForTab(tabID: string, rel: string): Promise<void>;
    RevealPath(path: string): Promise<void>;
    OpenLocalPath(path: string): Promise<void>;
    SavePastedImage(dataUrl: string): Promise<string>;
    SaveClipboardImage(): Promise<string>;
    SavePastedFile(name: string, dataUrl: string): Promise<string>;
    PickExportFile(defaultFilename: string, mimeType: string): Promise<string>;
    SaveExportFile(path: string, payload: string, base64Encoded: boolean): Promise<void>;
    SaveExportImageFiles(path: string, payloads: string[]): Promise<void>;
    AttachDropped(path: string): Promise<DroppedItem>;
    AttachmentDataURL(path: string): Promise<string>;
    Models(): Promise<ModelInfo[]>;
    SetModel(name: string): Promise<void>;
    ModelsForTab(tabID: string): Promise<ModelInfo[]>;
    SetModelForTab(tabID: string, name: string): Promise<void>;
    Effort(): Promise<EffortInfo>;
    SetEffort(level: string): Promise<void>;
    EffortForTab(tabID: string): Promise<EffortInfo>;
    SetEffortForTab(tabID: string, level: string): Promise<void>;
    SetTokenMode(mode: string): Promise<void>;
    SetTokenModeForTab(tabID: string, mode: string): Promise<void>;
    SetAgentPreset(preset: string): Promise<void>;
    SetAgentPresetForTab(tabID: string, preset: string): Promise<void>;
    // ReloadRuntime rebuilds the tab's agent runtime in place (tools, skills,
    // commands, hooks, providers, MCP servers) via boot.Rebuild, keeping the
    // session. Busy tabs queue one reload for when they go idle.
    ReloadRuntime(tabID: string): Promise<void>;
    Memory(): Promise<MemoryView>;
    MemorySuggestions(): Promise<MemorySuggestionsView>;
    AcceptMemorySuggestion(suggestion: MemorySuggestion): Promise<string>;
    AcceptSkillSuggestion(suggestion: SkillSuggestion): Promise<string>;
    MemoryForTab(tabID: string): Promise<MemoryView>;
    MemoryRevisions(ref: string): Promise<MemoryFact[]>;
    MemoryRevisionsForTab(tabID: string, ref: string): Promise<MemoryFact[]>;
    RestoreMemoryRevision(ref: string, revision: number): Promise<MemoryFact>;
    RestoreMemoryRevisionForTab(tabID: string, ref: string, revision: number): Promise<MemoryFact>;
    MemorySuggestionsForTab(tabID: string): Promise<MemorySuggestionsView>;
    AcceptMemorySuggestionForTab(tabID: string, suggestion: MemorySuggestion): Promise<string>;
    AcceptSkillSuggestionForTab(tabID: string, suggestion: SkillSuggestion): Promise<string>;
    Remember(scope: string, note: string): Promise<string>;
    RememberForTab(tabID: string, scope: string, note: string): Promise<string>;
    Forget(name: string): Promise<void>;
    ForgetForTab(tabID: string, name: string): Promise<void>;
    RestoreArchivedMemory(archivePath: string): Promise<MemoryFact>;
    RestoreArchivedMemoryForTab(tabID: string, archivePath: string): Promise<MemoryFact>;
    SaveDoc(path: string, body: string): Promise<string>;
    SaveDocForTab(tabID: string, path: string, body: string): Promise<string>;
    DesktopStartupSettings(): Promise<DesktopStartupSettingsView>;
    Settings(): Promise<SettingsView>;
    HooksSettings(scope: string): Promise<HooksSettingsView>;
    SaveHooksSettings(scope: string, hooks: HookConfigView[]): Promise<void>;
    SaveHooksSettingsForRoot(scope: string, projectRoot: string, hooks: HookConfigView[]): Promise<void>;
    TrustProjectHooks(): Promise<void>;
    TrustProjectHooksForRoot(projectRoot: string): Promise<void>;
    SetDefaultModel(ref: string): Promise<void>;
    SetPlannerModel(ref: string): Promise<void>;
    SetSubagentModel(ref: string): Promise<void>;
    SetSubagentEffort(level: string): Promise<void>;
    SetMaxSubagentDepth(depth: number): Promise<void>;
    SetMaxSubagentConcurrency(n: number): Promise<void>;
    SetMaxParallelWriters(n: number): Promise<void>;
    SetAutoPlan(mode: string): Promise<void>;
    SetDefaultToolApprovalMode(mode: string): Promise<void>;
    SetDefaultAutoRecoveryCheckpoint(enabled: boolean): Promise<void>;
    SaveProvider(p: ProviderView): Promise<void>;
    SetProviderWebSearch(names: string[], enabled: boolean): Promise<void>;
    SaveProviderModelCatalogs(updates: ProviderModelCatalogUpdate[]): Promise<string[]>;
    SaveProviderWithKey(p: ProviderView, key: string): Promise<string>;
    AddOfficialProviderAccess(kind: string, key: string): Promise<string>;
    UpgradeDeepSeekProviderAccess(name: string): Promise<string>;
    AddProviderPresetAccess(id: string, key: string): Promise<string>;
    ResetProviderPresetAccess(id: string): Promise<void>;
    FetchProviderModels(p: ProviderView): Promise<string[]>;
    FetchAllProviderModels(providers: ProviderView[]): Promise<Record<string, string[]>>;
    DeleteProvider(name: string): Promise<void>;
    RemoveProviderAccess(name: string): Promise<void>;
    RemoveProviderAccesses(names: string[]): Promise<void>;
    SaveProviderKey(apiKeyEnv: string, value: string): Promise<string>;
    SetProviderKey(apiKeyEnv: string, value: string): Promise<string>;
    ClearProviderKey(apiKeyEnv: string): Promise<void>;
    SetPermissionMode(mode: string): Promise<void>;
    AddPermissionRule(list: string, rule: string): Promise<void>;
    RemovePermissionRule(list: string, rule: string): Promise<void>;
    ReloadSettings(): Promise<void>;
    SetSandbox(bash: string, network: boolean, workspaceRoot: string, allowWrite: string[], shell: string): Promise<void>;
    SetNetwork(n: NetworkView): Promise<void>;
    SetBotSettings(b: BotSettingsView): Promise<void>;
    SetBotConnectionToolApprovalMode(connID: string, mode: string): Promise<void>;
    SetBotSecret(envName: string, value: string): Promise<void>;
    ClearBotSecret(envName: string): Promise<void>;
    StartBotConnectionInstall(provider: string, domain: string): Promise<BotInstallStartResult>;
    PollBotConnectionInstall(installID: string): Promise<BotInstallPollResult>;
    BotRuntimeStatus(): Promise<BotRuntimeStatusView>;
    DiagnoseBotConnection(id: string): Promise<BotConnectionDiagnostic>;
    TestBotConnection(id: string, target?: string): Promise<BotConnectionDiagnostic>;
    SetCloseBehavior(mode: string): Promise<void>;
    SetDisplayMode(mode: string): Promise<void>;
    SetStatusBarStyle(style: string): Promise<void>;
    SetStatusBarItems(items: string[]): Promise<void>;
    SetReasoningDisplayMode(mode: "hidden" | "summary" | "auto"): Promise<void>;
    SetDesktopLanguage(lang: string): Promise<void>;
    SetDesktopCurrency(currency: string): Promise<void>;
    SetDesktopAppearance(theme: string, style: string): Promise<void>;
    SetDesktopTerminalTheme(theme: string): Promise<void>;
    ListThemePacks(): Promise<import("./themePack").ThemePackView[]>;
    GetActiveThemePack(): Promise<import("./themePack").ThemeActiveView>;
    GetThemeExperience(): Promise<import("./themeExperience").ThemeExperienceView>;
    ActivateThemePack(id: string): Promise<void>;
    ActivateBaseStyle(style: string): Promise<void>;
    DisableThemePack(): Promise<void>;
    RestoreGraphiteAppearance(): Promise<void>;
    ResetThemePack(): Promise<void>;
    SaveThemePack(input: import("./themePack").ThemeSaveInput): Promise<import("./themePack").ThemePackView>;
    DeleteThemePack(id: string): Promise<void>;
    CopyThemePack(sourceID: string, newID: string, newName: string): Promise<import("./themePack").ThemePackView>;
    ImportThemePack(sourcePath: string, replace: boolean): Promise<import("./themePack").ThemeImportResult>;
    ExportThemePack(id: string, destPath: string): Promise<string>;
    PickThemeBackground(): Promise<string>;
    SetDesktopLayoutStyle(style: string): Promise<void>;
    SetDesktopZoomFactor(factor: number): Promise<void>;
    GetDesktopZoomFactor(): Promise<number>;
    RestartApplication(): Promise<void>;
    SetDesktopCheckUpdates(enabled: boolean): Promise<void>;
    SetDesktopUpdateChannel(channel: string): Promise<void>;
    SetDesktopTelemetry(enabled: boolean): Promise<void>;
    SetDesktopMetrics(enabled: boolean): Promise<void>;
    SetExpandThinking(on: boolean): Promise<void>;
    SetDesktopConversationWidth(width: string): Promise<void>;
    MigrateDesktopPreferences(language: string, theme: string, style: string): Promise<void>;
    SetAgentParams(temperature: number, maxSteps: number, plannerMaxSteps: number, systemPrompt: string): Promise<void>;
    SetCompactRatio(ratio: number): Promise<void>;
    SetReasoningLanguage(lang: string): Promise<void>;
    SetTrayLocale(locale: "en" | "zh" | "zh-TW"): Promise<void>;
    // SetBypass is the legacy Wails name for YOLO/full-access tool auto-approval
    // (ask questions and plan approvals still wait; deny rules still apply).
    // Runtime-only.
    SetBypass(on: boolean): Promise<void>;
    Version(): Promise<string>;
    CheckUpdate(channel: string): Promise<UpdateInfo | null>;
    /** v1.20+ single-action update: download, verify, install, relaunch. */
    ApplyUpdateRequest(channel: string, expectedVersion: string, requestId: string): Promise<void>;
    /** Discard a stuck previous update transaction so the next install can proceed. */
    AbandonPendingUpdate?(): Promise<void>;
    OpenDownloadPage(): Promise<void>;
    OpenUserConfigPath?(): Promise<void>;
    ReloadUserConfig?(): Promise<{
        configWarnings?: string[];
        configWarningsRevision?: number;
        configPath?: string;
    } | null>;
    StorageSettings(): Promise<{
        defaultWorkspace: string;
        statePath: string;
        cachePath: string;
        extensionsPath: string;
    }>;
    NeedsOnboarding(): Promise<boolean>;
    ConnectKey(apiKey: string): Promise<string>;
    // Crash overlay "Send report" (desktop/crash_app.go): scrubs user paths, attaches
    // version/os/arch, POSTs to the collection endpoint. Only ever sent on user click.
    ReportCrash(kind: string, detail: string): Promise<void>;
    RecordUIPerf(signals: Record<string, string>): Promise<void>;
    ListTabs(): Promise<TabMeta[]>;
    OpenProjectTab(workspaceRoot: string, topicID: string): Promise<TabMeta>;
    DeliveryWorktreeAvailability(workspaceRoot: string): Promise<DeliveryWorktreeAvailability>;
    CreateDeliveryWorktree(workspaceRoot: string): Promise<DeliveryWorktreeOpenResult>;
    OpenGlobalTab(topicID: string): Promise<TabMeta>;
    OpenTopicSession(scope: string, workspaceRoot: string, topicID: string, sessionPath: string): Promise<TabMeta>;
    EnsureBlankTab(scope: string, workspaceRoot: string): Promise<TabMeta>;
    ActivateTopic(scope: string, workspaceRoot: string, topicID: string, sessionPath: string): Promise<TabMeta>;
    // Two-phase ticketed topic activation (supersedes ActivateTopic for topic
    // navigation): returns a ticket after the surface switch; completion lands
    // on the "topic:activation" channel.
    StartTopicActivation(req: TopicActivationRequest): Promise<TopicActivationTicket>;
    EnsureBlankSurface(scope: string, workspaceRoot: string): Promise<TabMeta>;
    SetActiveTab(tabID: string): Promise<void>;
    ReorderTabs(tabIDs: string[]): Promise<void>;
    CloseTab(tabID: string): Promise<void>;
    TerminalWorkspaceForTab(tabID: string): Promise<TerminalWorkspaceView>;
    TerminalOutputForTab(tabID: string, sessionID: string): Promise<string>;
    CreateTerminalForTab(tabID: string, relativePath: string, shellID: string): Promise<TerminalSessionView>;
    WriteTerminalForTab(tabID: string, sessionID: string, data: string): Promise<void>;
    ResizeTerminalForTab(tabID: string, sessionID: string, cols: number, rows: number): Promise<void>;
    CloseTerminalForTab(tabID: string, sessionID: string): Promise<void>;
    RenameTerminalForTab(tabID: string, sessionID: string, title: string): Promise<void>;
    ListProjectTree(): Promise<ProjectNode[]>;
    RenameProject(workspaceRoot: string, title: string): Promise<void>;
    SetProjectColor(workspaceRoot: string, color: string): Promise<void>;
    SetProjectPinned(workspaceRoot: string, pinned: boolean): Promise<void>;
    ReorderProjects(workspaceRoots: string[]): Promise<void>;
    CreateTopic(scope: string, workspaceRoot: string, title: string): Promise<TopicMeta>;
    RenameTopic(topicID: string, title: string): Promise<void>;
    DeleteTopic(topicID: string): Promise<void>;
    TrashTopic(topicID: string): Promise<void>;
    SetTopicPinned(topicID: string, pinned: boolean): Promise<void>;
    ContextPanel(tabID: string): Promise<ContextPanelInfo>;
    // New native-feel bindings (added with the desktop native-feel plan).
    ConfirmAction(req: NativeConfirmRequest): Promise<boolean>;
    SaveWindowState(state: DesktopWindowState): Promise<void>;
    // ── Remote (SSH) ──
    RemoteHosts(): Promise<RemoteHostView[]>;
    AddRemoteHost(input: RemoteHostInput): Promise<RemoteHostView>;
    UpdateRemoteHost(id: string, input: RemoteHostInput): Promise<RemoteHostView>;
    RemoveRemoteHost(id: string): Promise<void>;
    ScanSSHConfig(): Promise<RemoteHostInput[]>;
    ConnectRemoteHost(id: string): Promise<void>;
    DisconnectRemoteHost(id: string): Promise<void>;
    RemoteConnectionStatuses(): Promise<RemoteConnectionStatus[]>;
    ConfirmRemoteHostKey(hostId: string, accept: boolean): Promise<void>;
    ConfirmRemoteSecret(hostId: string, promptId: string, secret: string, accept: boolean): Promise<void>;
    ListRemoteDir(hostId: string, path: string): Promise<RemoteDirEntry[]>;
    ReadRemoteFile(hostId: string, path: string): Promise<RemoteFilePreview>;
    WriteRemoteFile(hostId: string, path: string, body: string, expectMtimeUnix: number): Promise<RemoteWriteResult>;
    MkdirRemote(hostId: string, path: string): Promise<void>;
    RenameRemotePath(hostId: string, oldPath: string, newPath: string): Promise<void>;
    DeleteRemotePath(hostId: string, path: string, recursive: boolean): Promise<void>;
    RemoteForwards(hostId: string): Promise<RemoteForwardView[]>;
    AddRemoteForward(hostId: string, input: RemoteForwardInput): Promise<RemoteForwardView>;
    RemoveRemoteForward(hostId: string, forwardId: string): Promise<void>;
    OpenRemoteWorkspace(hostId: string, workspace: string): Promise<void>;
    StopRemoteServer(hostId: string): Promise<void>;
    RemoteServerStatus(hostId: string): Promise<RemoteServerView>;
    RemoteServerLogs(hostId: string, tailLines: number): Promise<string>;
    RemoteLastWorkspace(hostId: string): Promise<string>;
    ScanRemoteLegacyWorkbenchData(): Promise<RemoteLegacyWorkbenchData>;
    CleanRemoteLegacyWorkbenchData(target: "mirrors" | "trust"): Promise<void>;
}
// Compile-time drift check. Exclude<A, B> extracts keys in A that are missing
// from B. If that set is non-empty, AssertNever<non-never> fails with
// "Type 'X' does not satisfy the constraint 'never'".
// _CheckGenToApp errors mean a generated Go method has no TS counterpart.
// These compare method *names* only; full signature checking isn't possible here
// because local types (types.ts) use plain interfaces while generated types
// (models.ts) use classes with a convertValues prototype method. The structural
// mismatch would produce false positives. Method-arity and parameter-order drift
// are caught at the call sites by tsc when components invoke app.<method>(...).
type AssertNever<T extends never> = T;
type GeneratedAppKeys = keyof typeof GeneratedApp;
type GeneratedAppMissing = string extends GeneratedAppKeys ? true : number extends GeneratedAppKeys ? true : symbol extends GeneratedAppKeys ? true : false;
export type _CheckGenToApp = AssertNever<GeneratedAppMissing extends true ? never : Exclude<GeneratedAppKeys, keyof AppBindings>>;
export interface WailsRuntime {
    EventsOn(name: string, cb: (...data: unknown[]) => void): () => void;
    BrowserOpenURL(url: string): void;
    WindowSetSystemDefaultTheme?(): void;
    WindowSetLightTheme?(): void;
    WindowSetDarkTheme?(): void;
    WindowSetBackgroundColour?(r: number, g: number, b: number, a: number): void;
    WindowGetSize?(): Promise<{
        w: number;
        h: number;
    }>;
    WindowGetPosition?(): Promise<{
        x: number;
        y: number;
    }>;
    WindowIsMaximised?(): Promise<boolean>;
    ClipboardSetText?(text: string): Promise<boolean>;
    // Native OS file drop (desktop only); useDropTarget gates delivery to elements
    // carrying the --wails-drop-target CSS property. Absent in the browser dev mock.
    OnFileDrop?(cb: (x: number, y: number, paths: string[]) => void, useDropTarget: boolean): void;
    OnFileDropOff?(): void;
}

