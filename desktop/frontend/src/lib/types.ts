export type { ContextMaintenanceInfo, ContextMaintenanceReceipt, WireContextMaintenance } from "./contextMaintenanceTypes";
export type { ProjectTopicKey, ProjectTopicPage, ProjectTopicPageRequest, ProjectTreeChangedV2, ProjectTreeSnapshot, SessionCatalogBindings, SessionCatalogStatus, SessionReference } from "./sessionCatalogTypes";
export type { SessionMeta } from "./sessionMetaTypes";
export type { HistoryIndexStatus, HistorySearchContextLine, HistorySearchContextRequest, HistorySearchHit, HistorySearchPage, HistorySearchRequest, HistorySessionPage, HistorySessionPageRequest } from "./historyCatalogTypes";
export type { TaskActionRequest, TaskCatalogItem, TaskCatalogStatus, TaskEventPage, TaskEventPageRequest, TaskPage, TaskPageRequest } from "./taskCatalogTypes";
// DailyTokenUsage is one day's token total, per-model split and turn count in
// the daily trend series.
export interface DailyTokenUsage {
    day: string; // "2006-01-02"
    total: number;
    byModel: Record<string, number>; // model ref -> tokens
    byProvider: Record<string, number>; // provider name -> tokens
    requests: number; // API calls that day
    turns: number;
    cacheHit: number; // cached input tokens that day
    cacheMiss: number; // uncached input tokens that day
}
// ModelTokenUsage is one model's aggregate within the range.
export interface ModelTokenUsage {
    model: string; // canonical "provider/model"
    provider: string;
    tokens: number;
    percent: number; // 0..100
}
// ProviderTokenUsage is one provider's aggregate within the range.
export interface ProviderTokenUsage {
    provider: string;
    tokens: number;
    percent: number;
}
// UsageStatsRange is the full aggregate the settings panel renders.
export interface UsageStatsRange {
    from: string;
    to: string;
    tokens: number;
    requests: number; // API calls
    turns: number; // completed turns
    cacheHit: number;
    cacheMiss: number;
    activeDays: number;
    topModel: string;
    topProvider: string;
    daily: DailyTokenUsage[];
    models: ModelTokenUsage[];
    providers: ProviderTokenUsage[];
}
// JobView is one running background job (desktop/app.go Jobs) for the status bar.
export interface JobView {
    id: string;
    kind: string; // "bash" | "task"
    label: string;
    status: string; // "running"
    startedAt: number; // unix milliseconds
}
export interface ActiveWorkView {
    running: boolean;
    pendingPrompt: boolean;
    cancellable: boolean;
    jobs: JobView[];
}
export type { EventKind as EventKind } from "./types_events";
export type { StreamAttemptAction as StreamAttemptAction } from "./types_events";
export type { WireStreamAttempt as WireStreamAttempt } from "./types_events";
export type { WireCompaction as WireCompaction } from "./types_events";
export type { WireProfile as WireProfile } from "./types_events";
export type { WireShellExecution as WireShellExecution } from "./types_events";
export type { WireTool as WireTool } from "./types_events";
export type { WireCacheDiagnostics as WireCacheDiagnostics } from "./types_events";
export type { WireUsage as WireUsage } from "./types_events";
export type { Money as Money } from "./types_events";
export type { CostQuote as CostQuote } from "./types_events";
export type { WireRecoveryApproval as WireRecoveryApproval } from "./types_events";
export type { WireApproval as WireApproval } from "./types_events";
export type { WireGuardian as WireGuardian } from "./types_events";
export type { WireDecisionReceipt as WireDecisionReceipt } from "./types_events";
export type { WireAskOption as WireAskOption } from "./types_events";
export type { WireAskQuestion as WireAskQuestion } from "./types_events";
export type { WireAsk as WireAsk } from "./types_events";
export type { WireExtensionStatus as WireExtensionStatus } from "./types_events";
export type { WireExtensionKeyValue as WireExtensionKeyValue } from "./types_events";
export type { WireExtensionActionRef as WireExtensionActionRef } from "./types_events";
export type { WireExtensionCard as WireExtensionCard } from "./types_events";
export type { WireExtensionFormField as WireExtensionFormField } from "./types_events";
export type { WireExtensionForm as WireExtensionForm } from "./types_events";
export type { WireExtensionNotification as WireExtensionNotification } from "./types_events";
export type { WireExtensionSurface as WireExtensionSurface } from "./types_events";
export type { ExtensionActionView as ExtensionActionView } from "./types_events";
export type { QuestionAnswer as QuestionAnswer } from "./types_events";
export type { MemoryCitation as MemoryCitation } from "./types_events";
export type { WireEvent as WireEvent } from "./types_events";
export type { WireCompletionSummary as WireCompletionSummary } from "./types_events";
export type { WorkspaceWatchState as WorkspaceWatchState } from "./types_workspace";
export type { WorkspaceChangeOp as WorkspaceChangeOp } from "./types_workspace";
export type { WorkspaceRevisions as WorkspaceRevisions } from "./types_workspace";
export type { WorkspacePathChange as WorkspacePathChange } from "./types_workspace";
export type { WireWorkspaceChanged as WireWorkspaceChanged } from "./types_workspace";
export type { SessionRuntimePhase as SessionRuntimePhase } from "./types_workspace";
export type { SessionRuntimeIssue as SessionRuntimeIssue } from "./types_workspace";
export type { SessionRuntimeView as SessionRuntimeView } from "./types_workspace";
export type { WireFinalReadiness as WireFinalReadiness } from "./types_workspace";
export type { TabMeta as TabMeta } from "./types_workspace";
export type { TerminalSessionView as TerminalSessionView } from "./types_workspace";
export type { TerminalShellView as TerminalShellView } from "./types_workspace";
export type { TerminalWorkspaceView as TerminalWorkspaceView } from "./types_workspace";
export type { ProjectNode as ProjectNode } from "./types_workspace";
export type { RecoveryLineageMember as RecoveryLineageMember } from "./types_workspace";
export type { RecoveryLineageView as RecoveryLineageView } from "./types_workspace";
export type { RecoveryCleanupRequest as RecoveryCleanupRequest } from "./types_workspace";
export type { RecoveryPreferenceRequest as RecoveryPreferenceRequest } from "./types_workspace";
export type { RecoveryCleanupItem as RecoveryCleanupItem } from "./types_workspace";
export type { RecoveryCleanupResult as RecoveryCleanupResult } from "./types_workspace";
export type { DeliveryWorktreeAvailability as DeliveryWorktreeAvailability } from "./types_workspace";
export type { DeliveryWorktreeOpenResult as DeliveryWorktreeOpenResult } from "./types_workspace";
export type { ProjectTopicStatus as ProjectTopicStatus } from "./types_workspace";
export type { TopicMeta as TopicMeta } from "./types_workspace";
export type { SessionRecoveryEvent as SessionRecoveryEvent } from "./types_workspace";
export type { SessionRecoveryFailedEvent as SessionRecoveryFailedEvent } from "./types_workspace";
export type { ContextPanelInfo as ContextPanelInfo } from "./types_workspace";
export type { UsageSourceStats as UsageSourceStats } from "./types_history";
export type { ReadFileRecord as ReadFileRecord } from "./types_history";
export type { ChangedFileInfo as ChangedFileInfo } from "./types_history";
export type { HistoryMessage as HistoryMessage } from "./types_history";
export type { HistoryToolCall as HistoryToolCall } from "./types_history";
export type { HistoryPage as HistoryPage } from "./types_history";
export type { HistorySliceRequest as HistorySliceRequest } from "./types_history";
export type { HistoryContentRef as HistoryContentRef } from "./types_history";
export type { HistoryEntry as HistoryEntry } from "./types_history";
export type { SessionClearResult as SessionClearResult } from "./types_history";
export type { HistorySlice as HistorySlice } from "./types_history";
export type { HistoryContentChunk as HistoryContentChunk } from "./types_history";
export type { TopicActivationRequest as TopicActivationRequest } from "./types_history";
export type { TopicActivationTicket as TopicActivationTicket } from "./types_history";
export type { TopicActivationPhase as TopicActivationPhase } from "./types_history";
export type { TopicActivationEvent as TopicActivationEvent } from "./types_history";
export type { TabMetaRefreshEvent as TabMetaRefreshEvent } from "./types_history";
export type { PromptHistoryEntry as PromptHistoryEntry } from "./types_history";
export type { PromptHistoryResult as PromptHistoryResult } from "./types_history";
export type { CheckpointMeta as CheckpointMeta } from "./types_history";
export type { RewindPlanView as RewindPlanView } from "./types_history";
export type { RewindResultView as RewindResultView } from "./types_history";
export type { WorkspaceView as WorkspaceView } from "./types_history";
export type { ContextInfo as ContextInfo } from "./types_history";
export type { Meta as Meta } from "./types_history";
export type { CollaborationMode as CollaborationMode } from "./types_mode";
export type { ToolApprovalMode as ToolApprovalMode } from "./types_mode";
export type { TokenMode as TokenMode } from "./types_mode";
export type { AgentPreset as AgentPreset } from "./types_mode";
export type { GoalStatus as GoalStatus } from "./types_mode";
export type { GoalRuntime as GoalRuntime } from "./types_mode";
export { normalizeCollaborationMode as normalizeCollaborationMode } from "./types_mode";
export { normalizeToolApprovalMode as normalizeToolApprovalMode } from "./types_mode";
export { normalizeTokenMode as normalizeTokenMode } from "./types_mode";
export type { Mode as Mode } from "./types_mode";
export { normalizeMode as normalizeMode } from "./types_mode";
export { modeHasPlan as modeHasPlan } from "./types_mode";
export { modeHasAutoApproveTools as modeHasAutoApproveTools } from "./types_mode";
export { modeFromAxes as modeFromAxes } from "./types_mode";
export { modeWithPlan as modeWithPlan } from "./types_mode";
export { modeWithAutoApproveTools as modeWithAutoApproveTools } from "./types_mode";
export type { CommandInfo as CommandInfo } from "./types_mode";
export type { DirEntry as DirEntry } from "./types_mode";
export type { DroppedItem as DroppedItem } from "./types_mode";
export type { FilePreview as FilePreview } from "./types_mode";
export type { WorkspaceChangeView as WorkspaceChangeView } from "./types_mode";
export type { WorkspaceChangesView as WorkspaceChangesView } from "./types_mode";
export type { WorkspaceChangeDetailView as WorkspaceChangeDetailView } from "./types_mode";
export type { GitCommitView as GitCommitView } from "./types_mode";
export type { GitCommitDetailView as GitCommitDetailView } from "./types_mode";
export type { ComposerInsertRequest as ComposerInsertRequest } from "./types_mode";
export type { ServerView as ServerView } from "./types_mcp";
export type { MCPToolView as MCPToolView } from "./types_mcp";
export type { SkillView as SkillView } from "./types_mcp";
export type { SkillRootSkillView as SkillRootSkillView } from "./types_mcp";
export type { SkillRootView as SkillRootView } from "./types_mcp";
export type { CapabilitiesView as CapabilitiesView } from "./types_mcp";
export type { SkillsSettingsView as SkillsSettingsView } from "./types_mcp";
export type { SubagentProfileInput as SubagentProfileInput } from "./types_mcp";
export type { PluginView as PluginView } from "./types_mcp";
export type { PluginCompatibilityIssue as PluginCompatibilityIssue } from "./types_mcp";
export type { PluginAgentView as PluginAgentView } from "./types_mcp";
export type { PluginSkillView as PluginSkillView } from "./types_mcp";
export type { PluginCommandView as PluginCommandView } from "./types_mcp";
export type { PluginHookView as PluginHookView } from "./types_mcp";
export type { PluginMCPServerView as PluginMCPServerView } from "./types_mcp";
export type { PluginInstallOptions as PluginInstallOptions } from "./types_mcp";
export type { MCPServerInput as MCPServerInput } from "./types_mcp";
export type { MCPInstallResult as MCPInstallResult } from "./types_mcp";
export type { MCPMarketplaceEntry as MCPMarketplaceEntry } from "./types_mcp";
export type { MCPMarketplaceView as MCPMarketplaceView } from "./types_mcp";
export type { ModelInfo as ModelInfo } from "./types_mcp";
export type { EffortInfo as EffortInfo } from "./types_mcp";
export type { SlashArgItem as SlashArgItem } from "./types_mcp";
export type { SlashArgsResult as SlashArgsResult } from "./types_mcp";
export type { MemoryDoc as MemoryDoc } from "./types_remote";
export type { InstructionDiagnostic as InstructionDiagnostic } from "./types_remote";
export type { MemoryFact as MemoryFact } from "./types_remote";
export type { MemoryConflict as MemoryConflict } from "./types_remote";
export type { MemoryRecallHit as MemoryRecallHit } from "./types_remote";
export type { MemoryRecallTrace as MemoryRecallTrace } from "./types_remote";
export type { MemoryArchive as MemoryArchive } from "./types_remote";
export type { MemoryScope as MemoryScope } from "./types_remote";
export type { MemorySuggestion as MemorySuggestion } from "./types_remote";
export type { SkillSuggestion as SkillSuggestion } from "./types_remote";
export type { MemorySuggestionsView as MemorySuggestionsView } from "./types_remote";
export type { MemoryView as MemoryView } from "./types_remote";
export type { SettingsTab as SettingsTab } from "./types_remote";
export type { RemoteConnState as RemoteConnState } from "./types_remote";
export type { RemoteServerState as RemoteServerState } from "./types_remote";
export type { RemoteHostView as RemoteHostView } from "./types_remote";
export type { RemoteHostInput as RemoteHostInput } from "./types_remote";
export type { RemoteFingerprintView as RemoteFingerprintView } from "./types_remote";
export type { RemoteSecretPromptView as RemoteSecretPromptView } from "./types_remote";
export type { RemoteKnownHostLocation as RemoteKnownHostLocation } from "./types_remote";
export type { RemoteConnectionErrorDetails as RemoteConnectionErrorDetails } from "./types_remote";
export type { RemoteConnectionStatus as RemoteConnectionStatus } from "./types_remote";
export type { RemoteDirEntry as RemoteDirEntry } from "./types_remote";
export type { RemoteFilePreview as RemoteFilePreview } from "./types_remote";
export type { RemoteWriteResult as RemoteWriteResult } from "./types_remote";
export type { RemoteForwardInput as RemoteForwardInput } from "./types_remote";
export type { RemoteForwardView as RemoteForwardView } from "./types_remote";
export type { RemoteServerView as RemoteServerView } from "./types_remote";
export type { RemoteLegacyWorkbenchData as RemoteLegacyWorkbenchData } from "./types_remote";
export type { RemoteForwardsEvent as RemoteForwardsEvent } from "./types_remote";
export type { RuntimeDoctorReport as RuntimeDoctorReport } from "./types_remote";
export type { CapabilityDiagnosticsReport as CapabilityDiagnosticsReport } from "./types_remote";
export type { CapabilityAssetReport as CapabilityAssetReport } from "./types_remote";
export type { CapabilityIssue as CapabilityIssue } from "./types_remote";
export type { ProviderView as ProviderView } from "./types_remote";
export type { ProviderModelCatalogUpdate as ProviderModelCatalogUpdate } from "./types_remote";
export type { ProviderPresetView as ProviderPresetView } from "./types_remote";
export type { ProviderModelOverrideView as ProviderModelOverrideView } from "./types_remote";
export type { BalanceInfo as BalanceInfo } from "./types_remote";
export type { UsageStatsRequest as UsageStatsRequest } from "./types_remote";
export type { JobCancelBatchView as JobCancelBatchView } from "./types_settings";
export type { BackgroundRuntimeView as BackgroundRuntimeView } from "./types_settings";
export type { WorkspaceConflictView as WorkspaceConflictView } from "./types_settings";
export type { PermissionsView as PermissionsView } from "./types_settings";
export type { SandboxView as SandboxView } from "./types_settings";
export type { NetworkProxyView as NetworkProxyView } from "./types_settings";
export type { NetworkView as NetworkView } from "./types_settings";
export type { AgentView as AgentView } from "./types_settings";
export type { HookConfigView as HookConfigView } from "./types_settings";
export type { HooksSettingsView as HooksSettingsView } from "./types_settings";
export type { SettingsView as SettingsView } from "./types_settings";
export type { DesktopStartupSettingsView as DesktopStartupSettingsView } from "./types_settings";
export type { ExternalOpenerKind as ExternalOpenerKind } from "./types_settings";
export type { ExternalOpenerView as ExternalOpenerView } from "./types_settings";
export type { ExternalOpenersView as ExternalOpenersView } from "./types_settings";
export type { TaskState as TaskState } from "./types_settings";
export type { RuntimeState as RuntimeState } from "./types_settings";
export type { TaskSnapshot as TaskSnapshot } from "./types_settings";
export type { ControlResult as ControlResult } from "./types_settings";
export type { TaskEvent as TaskEvent } from "./types_settings";

