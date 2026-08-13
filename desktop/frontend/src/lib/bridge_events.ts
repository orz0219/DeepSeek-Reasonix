import type { RemoteConnectionStatus, RemoteServerView, RemoteForwardsEvent, TabMetaRefreshEvent, TopicActivationEvent, SessionRecoveryEvent, UpdateProgress, WireEvent } from "./types";
import { realApp, mockSubscribe, updaterListeners } from "./bridge";
// Must match desktop/app.go's eventChannel constant.
export const EVENT_CHANNEL = "agent:event";
const RECENT_NATIVE_FILE_DRAG_MS = 2000;
const WAILS_NON_FILE_DRAG_MESSAGE = "additional File object is not a file on the disk";
const UNCAUGHT_ERROR_PREFIX_RE = /^Uncaught(?:\s+\(in promise\))?(?:\s+\w*Error)?:\s*/i;
const WAILS_IPC_CONNECTING_RE = /Failed to execute 'send' on 'WebSocket': Still in CONNECTING state/i;
const WAILS_IPC_NULL_SEND_RE = /Cannot read properties of null \(reading 'send'\)/i;
// onEvent subscribes to the agent's typed event stream; returns an unsubscribe.
export function onEvent(cb: (e: WireEvent) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn(EVENT_CHANNEL, (payload) => cb(payload as WireEvent));
    }
    return mockSubscribe(cb);
}
export interface TerminalOutputEvent {
    id: string;
    data: string;
}
export interface TerminalExitEvent {
    id: string;
    exitCode: number;
    removed?: boolean;
}
function terminalEventPayload<T>(payload: unknown): T | null {
    if (!payload || typeof payload !== "object")
        return null;
    return payload as T;
}
export function onTerminalOutput(cb: (event: TerminalOutputEvent) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("terminal:output", (payload) => {
            const event = terminalEventPayload<TerminalOutputEvent>(payload);
            if (event?.id && typeof event.data === "string")
                cb(event);
        });
    }
    mockTerminalOutputListeners.add(cb);
    return () => mockTerminalOutputListeners.delete(cb);
}
export function onTerminalExit(cb: (event: TerminalExitEvent) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("terminal:exit", (payload) => {
            const event = terminalEventPayload<TerminalExitEvent>(payload);
            if (event?.id && typeof event.exitCode === "number")
                cb(event);
        });
    }
    mockTerminalExitListeners.add(cb);
    return () => mockTerminalExitListeners.delete(cb);
}
const mockTerminalOutputListeners = new Set<(event: TerminalOutputEvent) => void>();
const mockTerminalExitListeners = new Set<(event: TerminalExitEvent) => void>();
export function __emitMockTerminalOutput(event: TerminalOutputEvent): void {
    mockTerminalOutputListeners.forEach((listener) => listener(event));
}
export function __emitMockTerminalExit(event: TerminalExitEvent): void {
    mockTerminalExitListeners.forEach((listener) => listener(event));
}
// onUpdaterProgress subscribes to the auto-updater's progress events (a separate
// channel from the agent stream); returns an unsubscribe. Must match the event
// name emitted in desktop/updater_app.go.
export function onUpdaterProgress(cb: (p: UpdateProgress) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("updater:progress", (p) => cb(p as UpdateProgress));
    }
    updaterListeners.add(cb);
    return () => {
        updaterListeners.delete(cb);
    };
}
export function errorMessage(err: unknown): string {
    if (err && typeof err === "object" && "message" in err) {
        const msg = (err as {
            message?: unknown;
        }).message;
        if (typeof msg === "string")
            return msg;
    }
    return String(err);
}
export function isWailsNonFileDragError(err: unknown, recentNativeFileDrag = false): boolean {
    const msg = errorMessage(err).trim().replace(UNCAUGHT_ERROR_PREFIX_RE, "");
    if (msg.includes(WAILS_NON_FILE_DRAG_MESSAGE))
        return true;
    return recentNativeFileDrag && msg.toLowerCase() === "invalid argument";
}
export function isWailsNonFileDragErrorEvent(event: Pick<ErrorEvent, "error" | "message">, recentNativeFileDrag = false): boolean {
    if (isWailsNonFileDragError(event.error ?? event.message, recentNativeFileDrag))
        return true;
    return event.error != null && isWailsNonFileDragError(event.message, recentNativeFileDrag);
}
export function isTransientWailsIPCError(err: unknown): boolean {
    const msg = errorMessage(err).trim().replace(UNCAUGHT_ERROR_PREFIX_RE, "");
    return WAILS_IPC_CONNECTING_RE.test(msg) || WAILS_IPC_NULL_SEND_RE.test(msg);
}
function dataTransferLooksLikeFileDrag(dt: DataTransfer | null): boolean {
    if (!dt)
        return false;
    if (dt.files?.length > 0)
        return true;
    return Array.from(dt.types ?? []).includes("Files");
}
let wailsDragSuppressionRefs = 0;
let wailsDragSuppressionUninstall: (() => void) | null = null;
let lastNativeFileDragAt = 0;
export function installWailsNonFileDragErrorSuppression(): () => void {
    if (typeof window === "undefined")
        return () => { };
    wailsDragSuppressionRefs += 1;
    if (!wailsDragSuppressionUninstall) {
        const markNativeFileDrag = (e: DragEvent) => {
            if (dataTransferLooksLikeFileDrag(e.dataTransfer))
                lastNativeFileDragAt = Date.now();
        };
        const hasRecentNativeFileDrag = () => Date.now() - lastNativeFileDragAt <= RECENT_NATIVE_FILE_DRAG_MS;
        const suppressNonFileDragError = (e: ErrorEvent) => {
            if (isWailsNonFileDragErrorEvent(e, hasRecentNativeFileDrag()) || isTransientWailsIPCError(e.error ?? e.message)) {
                e.preventDefault();
            }
        };
        const suppressNonFileDragRejection = (e: PromiseRejectionEvent) => {
            if (isWailsNonFileDragError(e.reason, hasRecentNativeFileDrag()) || isTransientWailsIPCError(e.reason)) {
                e.preventDefault();
            }
        };
        window.addEventListener("dragenter", markNativeFileDrag, true);
        window.addEventListener("dragover", markNativeFileDrag, true);
        window.addEventListener("drop", markNativeFileDrag, true);
        window.addEventListener("error", suppressNonFileDragError);
        window.addEventListener("unhandledrejection", suppressNonFileDragRejection);
        wailsDragSuppressionUninstall = () => {
            window.removeEventListener("dragenter", markNativeFileDrag, true);
            window.removeEventListener("dragover", markNativeFileDrag, true);
            window.removeEventListener("drop", markNativeFileDrag, true);
            window.removeEventListener("error", suppressNonFileDragError);
            window.removeEventListener("unhandledrejection", suppressNonFileDragRejection);
            lastNativeFileDragAt = 0;
        };
    }
    let disposed = false;
    return () => {
        if (disposed)
            return;
        disposed = true;
        wailsDragSuppressionRefs = Math.max(0, wailsDragSuppressionRefs - 1);
        if (wailsDragSuppressionRefs === 0 && wailsDragSuppressionUninstall) {
            wailsDragSuppressionUninstall();
            wailsDragSuppressionUninstall = null;
        }
    };
}
// onFilesDropped subscribes to native OS file drops landing on the composer (the
// --wails-drop-target element); the callback gets the dropped files' absolute
// paths. No-op in the browser dev mock, where the runtime is absent.
export function onFilesDropped(cb: (paths: string[]) => void): () => void {
    const rt = typeof window !== "undefined" ? window.runtime : undefined;
    if (!rt?.OnFileDrop)
        return () => { };
    // Wails' internal ResolveFilePaths throws when a non-file object (e.g. the
    // window icon) is dragged onto the webview. The error is uncaught and crashes
    // the app. Intercept it here so only real file drops reach the callback.
    const uninstallDragSuppression = installWailsNonFileDragErrorSuppression();
    rt.OnFileDrop((_x, _y, paths) => {
        if (Array.isArray(paths) && paths.length > 0)
            cb(paths);
    }, true);
    return () => {
        rt.OnFileDropOff?.();
        uninstallDragSuppression();
    };
}
// onReady subscribes to the agent:ready event fired when boot.Build completes.
// The frontend re-fetches Meta/Context/History when this lands.
// onRuntimeRebuilt fires when a tab's controller is replaced in place
// (model/effort/token-mode switch, clear-while-running). The rebuilt
// controller restarts prompt ids, so per-tab id-keyed state must reset.
export function onRuntimeRebuilt(cb: (tabId?: string, runtimeEpoch?: string) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("runtime:rebuilt", (tabId?: unknown, runtimeEpoch?: unknown) => cb(typeof tabId === "string" ? tabId : undefined, typeof runtimeEpoch === "string" ? runtimeEpoch : undefined));
    }
    return () => { };
}
export function onReady(cb: (tabId?: string) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("agent:ready", (tabId?: unknown) => cb(typeof tabId === "string" ? tabId : undefined));
    }
    // In dev mock, fire immediately since there's no real boot sequence.
    cb();
    return () => { };
}
export function onProjectTreeChanged(cb: () => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("project-tree:changed", () => cb());
    }
    return () => { };
}
// onTopicActivation subscribes to the "topic:activation" channel carrying the
// lifecycle of ticketed StartTopicActivation requests (starting/ready/failed/
// cancelled). Returns an unsubscribe.
export function onTopicActivation(cb: (event: TopicActivationEvent) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("topic:activation", (payload?: unknown) => {
            if (payload && typeof payload === "object")
                cb(payload as TopicActivationEvent);
        });
    }
    mockTopicActivationListeners.add(cb);
    return () => mockTopicActivationListeners.delete(cb);
}
// onTabMeta subscribes to the "tab:meta" channel: a full refreshed Meta pushed
// after the backend recomputes the expensive MetaForTab fields (git branch,
// image-input capability) in the background.
export function onTabMeta(cb: (event: TabMetaRefreshEvent) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("tab:meta", (payload?: unknown) => {
            if (payload && typeof payload === "object")
                cb(payload as TabMetaRefreshEvent);
        });
    }
    mockTabMetaListeners.add(cb);
    return () => mockTabMetaListeners.delete(cb);
}
const mockTopicActivationListeners = new Set<(event: TopicActivationEvent) => void>();
const mockTabMetaListeners = new Set<(event: TabMetaRefreshEvent) => void>();
export function __emitMockTopicActivation(event: TopicActivationEvent): void {
    mockTopicActivationListeners.forEach((listener) => listener(event));
}
export function onSessionRecovered(cb: (payload: SessionRecoveryEvent) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("session:recovered", (payload?: unknown) => cb((payload ?? {}) as SessionRecoveryEvent));
    }
    return () => { };
}
export function onRemoteStatus(cb: (s: RemoteConnectionStatus) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("remote:status", (payload?: unknown) => cb((payload ?? {}) as RemoteConnectionStatus));
    }
    return registerMockRemoteListener("status", cb as (v: unknown) => void);
}
export function onRemoteForwards(cb: (e: RemoteForwardsEvent) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("remote:forwards", (payload?: unknown) => cb((payload ?? {}) as RemoteForwardsEvent));
    }
    return registerMockRemoteListener("forwards", cb as (v: unknown) => void);
}
export function onRemoteServer(cb: (s: RemoteServerView) => void): () => void {
    if (realApp() && typeof window !== "undefined" && window.runtime) {
        return window.runtime.EventsOn("remote:server", (payload?: unknown) => cb((payload ?? {}) as RemoteServerView));
    }
    return registerMockRemoteListener("server", cb as (v: unknown) => void);
}
// Mock event fan-out so browser-dev and tsx tests can drive remote:* events
// without a Wails runtime.
type MockRemoteChannel = "status" | "forwards" | "server";
const mockRemoteListeners: Record<MockRemoteChannel, Set<(v: unknown) => void>> = {
    status: new Set(),
    forwards: new Set(),
    server: new Set(),
};
function registerMockRemoteListener(ch: MockRemoteChannel, cb: (v: unknown) => void): () => void {
    mockRemoteListeners[ch].add(cb);
    return () => mockRemoteListeners[ch].delete(cb);
}
export function __emitMockRemote(ch: MockRemoteChannel, payload: unknown): void {
    for (const cb of mockRemoteListeners[ch])
        cb(payload);
}
// app proxies each call to the live binding (or the dev mock only when truly
// outside the shell), so a late-injected window.go is picked up transparently.
export function bridgeBreadcrumb(method: string): string {
    if (method === "ReportCrash" || method === "RecordUIPerf")
        return "";
    if (/^(Submit|SubmitDisplay|RunShell|Steer|Cancel|Approve|AnswerQuestion|ReplayPendingPrompts)/.test(method))
        return `turn ${method}`;
    if (/^(SetModel|SetEffort|SetTokenMode|SetDefaultModel|SetPlannerModel|SetSubagentModel|SetSubagentEffort|SetMaxSubagentDepth|SetMaxSubagentConcurrency|SetMaxParallelWriters)/.test(method))
        return `model ${method}`;
    if (/^(SetDesktop|SetCloseBehavior|SetDisplayMode|SetStatusBar|SetReasoningDisplayMode|SetExpandThinking|SetAutoPlan|SetDefaultToolApprovalMode|SetCompactRatio|SetReasoningLanguage)/.test(method))
        return `settings ${method}`;
    if (/^(SaveProvider|SetProviderWebSearch|SaveProviderModelCatalogs|AddOfficialProviderAccess|UpgradeDeepSeekProviderAccess|AddProviderPresetAccess|ResetProviderPresetAccess|RemoveProviderAccess|RemoveProviderAccesses|DeleteProvider|SaveProviderKey|SetProviderKey|ClearProviderKey|FetchProviderModels|FetchAllProviderModels|ConnectKey)/.test(method))
        return `provider ${method}`;
    if (/^(CheckUpdate|ApplyUpdateRequest|OpenDownloadPage|OpenUserConfigPath|ReloadUserConfig)/.test(method))
        return `update ${method}`;
    if (/^(AddMCPServer|InstallMCPServer|UpdateMCPServer|RemoveMCPServer|AuthorizeAndConnectMCPServer|AuthenticateMCPServer|ReconnectMCPServer|ClearMCPServerAuthentication|SetMCPServer)/.test(method))
        return `mcp ${method}`;
    if (/^(AddSkillPath|RemoveSkillPath|SetSkillPathEnabled|RefreshSkills|SetSkillEnabled|SetSkillImplicitInvocation|AcceptSkillSuggestion|AvailableSubagentTools|CreateSubagentProfile|UpdateSubagentProfile|DeleteSubagentProfile|SetSubagentProfileModel|SetSubagentProfileEffort|TrySubagentProfile|CancelTrySubagentProfile)/.test(method))
        return `skill ${method}`;
    if (/^(MinimiseMainWindow|ToggleMaximiseMainWindow|IsMainWindowMaximised|CloseMainWindow)$/.test(method))
        return `window ${method}`;
    if (/^(OpenProjectTab|OpenGlobalTab|OpenTopicSession|EnsureBlankTab|ActivateTopic|StartTopicActivation|EnsureBlankSurface|SetActiveTab|CloseTab|ReorderTabs|CreateTopic|RenameTopic|DeleteTopic|TrashTopic|RenameProject|RemoveWorkspace|SwitchWorkspace|PickWorkspace|DeliveryWorktreeAvailability|CreateDeliveryWorktree)/.test(method))
        return `nav ${method}`;
    return "";
}
export function elapsedMs(startedAt: number): number {
    const now = typeof performance !== "undefined" ? performance.now() : Date.now();
    return Math.max(0, Math.round(now - startedAt));
}

