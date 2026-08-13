import { addBreadcrumb } from "./breadcrumbs";
import { maybeShare } from "./queryCoalesce";
import { modeHasAutoApproveTools, normalizeToolApprovalMode } from "./types";
import type { Mode, ToolApprovalMode, UpdateProgress, WireEvent } from "./types";
import { bridgeBreadcrumb, elapsedMs } from "./bridge_events";
import { MockProviderPresetTemplate, mockProviderTemplate, mockPreset } from "./bridge_mock_helpers";
import { makeMockApp } from "./bridge_mock";
import { AppBindings, WailsRuntime } from "./bridge_types";
declare global {
    interface Window {
        runtime?: WailsRuntime;
        go?: {
            main?: {
                App?: AppBindings;
            };
        };
    }
}
export const GLOBAL_PROJECT_ORDER_KEY = "__global__";
export function stripLegacyGoalBudgetFlags(arg: string): string {
    const parts = arg.trim().split(/\s+/).filter(Boolean);
    while (parts.length > 0) {
        const flag = parts[0].toLowerCase();
        if (flag !== "--research" && flag !== "--auto-research" && flag !== "--deep" && flag !== "--simple" && flag !== "--no-research")
            break;
        parts.shift();
    }
    return parts.join(" ");
}
// Resolve the Wails binding at CALL time, not module-load time: in dev the Wails
// runtime can inject window.go AFTER this module first evaluates, so snapshotting
// once would pin the browser mock for the whole session (and show fake data — the
// dev mock's model list leaking into the real app was exactly this bug).
export function realApp(): AppBindings | undefined {
    return typeof window !== "undefined" ? window.go?.main?.App : undefined;
}
let mockSingleton: AppBindings | null = null;
function getMock(): AppBindings {
    if (!mockSingleton)
        mockSingleton = makeMockApp();
    return mockSingleton;
}
export const app: AppBindings = new Proxy({} as AppBindings, {
    get(_t, prop) {
        const target = realApp() ?? getMock();
        const v = (target as unknown as Record<string, unknown>)[String(prop)];
        if (typeof v !== "function")
            return v;
        return (...args: unknown[]) => {
            const method = String(prop), crumb = bridgeBreadcrumb(method);
            const startedAt = crumb ? (typeof performance !== "undefined" ? performance.now() : Date.now()) : 0;
            if (crumb)
                addBreadcrumb("bridge", crumb);
            try {
                const result = maybeShare(method, args, () => (v as (...a: unknown[]) => unknown).apply(target, args));
                if (result && typeof (result as Promise<unknown>).then === "function") {
                    return (result as Promise<unknown>).then((value) => {
                        if (crumb)
                            addBreadcrumb("bridge", `${crumb} done ms=${elapsedMs(startedAt)}`);
                        return value;
                    }, (err) => {
                        if (crumb)
                            addBreadcrumb("bridge.error", `${method} ms=${elapsedMs(startedAt)}`);
                        throw err;
                    });
                }
                if (crumb)
                    addBreadcrumb("bridge", `${crumb} done ms=${elapsedMs(startedAt)}`);
                return result;
            }
            catch (err) {
                if (crumb)
                    addBreadcrumb("bridge.error", `${method} ms=${elapsedMs(startedAt)}`);
                throw err;
            }
        };
    },
});
// openExternal opens a URL in the system browser (so links in rendered markdown
// don't navigate the webview away from the app). Falls back to window.open in the
// browser dev mock.
export function openExternal(url: string): void {
    if (typeof window !== "undefined" && window.runtime?.BrowserOpenURL) {
        window.runtime.BrowserOpenURL(url);
    }
    else if (typeof window !== "undefined") {
        window.open(url, "_blank", "noopener");
    }
}
// --- browser dev mock --------------------------------------------------------
const listeners = new Set<(e: WireEvent) => void>();
export let mockScopedTabId: string | undefined;
export let mockPendingTopicActivation: {
    requestId: string;
    tabId: string;
} | undefined;
export let mockTopicActivationCounter = 0;
// 跨文件（bridge_mock）写访问入口：ESM import 绑定只读，赋值必须经函数
export function bumpMockTopicActivationCounter(): number {
    return ++mockTopicActivationCounter;
}
export function setMockPendingTopicActivation(value: { requestId: string; tabId: string } | undefined): void {
    mockPendingTopicActivation = value;
}
export function mockSubscribe(cb: (e: WireEvent) => void): () => void {
    listeners.add(cb);
    return () => {
        listeners.delete(cb);
    };
}
export function emit(e: WireEvent) {
    const event = mockScopedTabId && !e.tabId ? { ...e, tabId: mockScopedTabId } : e;
    listeners.forEach((l) => l(event));
}
export function mockToolApprovalModeAfterModeChange(current: string | undefined, nextMode: Mode): ToolApprovalMode {
    if (modeHasAutoApproveTools(nextMode))
        return "yolo";
    const currentMode = normalizeToolApprovalMode(current);
    return currentMode === "yolo" ? "ask" : currentMode;
}
export async function withMockTabScope<T>(tabId: string, fn: () => Promise<T>): Promise<T> {
    const previous = mockScopedTabId;
    mockScopedTabId = tabId || previous;
    try {
        return await fn();
    }
    finally {
        mockScopedTabId = previous;
    }
}
// Updater progress has its own listener set so the browser dev mock can stream a
// fake download/install flow through onUpdaterProgress.
export const updaterListeners = new Set<(p: UpdateProgress) => void>();
export function emitUpdater(p: UpdateProgress) {
    updaterListeners.forEach((l) => l(p));
}
// Test seam for the browser-dev updater state machine. Production Wails builds
// receive the same payloads through runtime.EventsOn("updater:progress").
export function __emitMockUpdater(p: UpdateProgress): void {
    emitUpdater(p);
}
export function delay(ms: number): Promise<void> {
    return new Promise((r) => setTimeout(r, ms));
}
const mockKimiAPIModels = ["kimi-k3", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6", "kimi-k2.5"];
const mockLongCatModels = ["LongCat-2.0"];
const mockTokenRhythmModels = ["deepseek-v4-flash", "deepseek-v4-pro", "glm-5", "glm-5.1", "minimax-m2.7", "kimi-k2.5", "kimi-k2.6", "minimax-m2.5", "mimo-v2.5-pro", "qwen3.7-max", "kimi-k2.7-code", "glm-5.2", "qwen3.8-max", "deepseek-v4-flash-0731"];
const mockTokenRhythmModelOverrides = mockTokenRhythmModels.flatMap((model) => {
    if (model.startsWith("glm-"))
        return [{ model, reasoningProtocol: "glm", supportedEfforts: ["enabled", "disabled"], defaultEffort: "enabled" }];
    if (model.startsWith("deepseek-"))
        return [{ model, reasoningProtocol: "deepseek", supportedEfforts: model === "deepseek-v4-pro" ? ["disabled", "high", "max"] : ["disabled", "low", "high", "max"], defaultEffort: "high" }];
    return [];
});
const mockMiMoV25Models = ["mimo-v2.5-pro", "mimo-v2.5"];
const mockMiniMaxModels = ["MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed"];
const mockGLMAPIModels = ["glm-5.2", "glm-5.1", "glm-5", "glm-5-turbo", "glm-5v-turbo", "glm-4.7", "glm-4.7-flash", "glm-4.7-flashx", "glm-4.6", "glm-4.5", "glm-4.5-air", "glm-4.5-flash"];
const mockGLMCodingModels = ["glm-5.2", "glm-5.1", "glm-5", "glm-4.7"];
const mockGLMAnthropicModels = ["glm-5.2[1m]", "glm-5.2", "glm-5.1", "glm-5", "glm-4.7", "glm-4.5-air"];
const mockQwenAPIModels = ["qwen3.7-plus", "qwen3.7-max", "qwen3.6-plus", "qwen3.5-plus", "qwen3-max-2026-01-23", "qwen3-coder-next", "qwen3-coder-plus", "MiniMax-M2.5", "glm-5", "glm-4.7", "kimi-k2.5"];
const mockQwenPlanModels = ["qwen3.7-plus", "qwen3.6-plus", "kimi-k2.5", "glm-5", "MiniMax-M2.5", "qwen3.5-plus", "qwen3-max-2026-01-23", "qwen3-coder-next", "qwen3-coder-plus", "glm-4.7"];
const mockQwenPlanVisionModels = ["qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "kimi-k2.5"];
const mockStepFunModels = ["step-3.7-flash", "step-3.5-flash", "step-3.5-flash-2603"];
const mockOpenCodeGoModels = ["glm-5.2", "glm-5.1", "kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "deepseek-v4-pro", "deepseek-v4-flash", "mimo-v2.5-pro", "mimo-v2.5"];
const mockNovitaModels = ["zai-org/glm-5.2", "moonshotai/kimi-k2.7-code", "minimax/minimax-m3", "deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "qwen/qwen3.7-max", "qwen/qwen3.6-plus", "zai-org/glm-5v-turbo"];
const mockGMIModels = ["zai-org/GLM-5.2-FP8", "deepseek-ai/DeepSeek-V4-Pro", "deepseek-ai/DeepSeek-V4-Flash", "moonshotai/Kimi-K2.7-Code", "anthropic/claude-sonnet-4.6", "openai/gpt-5.5"];
const mockVercelModels = ["anthropic/claude-sonnet-4.6", "anthropic/claude-opus-4.8", "openai/gpt-5.4", "openai/gpt-5.4-pro", "moonshotai/kimi-k2.7-code", "zai/glm-5.2", "deepseek/deepseek-v4-pro"];
const mockOllamaCloudModels = ["glm-5.2", "kimi-k2.7-code", "deepseek-v4-pro", "deepseek-v4-flash", "minimax-m3", "nemotron-3-nano:30b", "qwen3-coder-next"];
export const mockProviderPresetTemplates: MockProviderPresetTemplate[] = [
    mockPreset("deepseek-responses", "DeepSeek Official Responses API", "Official stateless DeepSeek Responses API for Flash with server-side web search.", "DEEPSEEK_API_KEY", mockProviderTemplate({ name: "deepseek-responses", kind: "responses", baseUrl: "https://api.deepseek.com", models: ["deepseek-v4-flash"], default: "deepseek-v4-flash", apiKeyEnv: "DEEPSEEK_API_KEY", balanceUrl: "https://api.deepseek.com/user/balance", webSearch: true, serverWebSearchCapability: true, contextWindow: 1000000, supportedEfforts: ["low", "high", "max"], defaultEffort: "high" })),
    mockPreset("longcat-openai", "LongCat OpenAI", "LongCat Platform OpenAI-compatible endpoint for LongCat-2.0.", "LONGCAT_API_KEY", mockProviderTemplate({ name: "longcat-openai", kind: "openai", baseUrl: "https://api.longcat.chat/openai/v1", modelsUrl: "https://api.longcat.chat/openai/v1/models", models: mockLongCatModels, default: "LongCat-2.0", apiKeyEnv: "LONGCAT_API_KEY", contextWindow: 131072, thinking: "enabled", supportedEfforts: ["enabled", "disabled"], defaultEffort: "enabled" })),
    mockPreset("longcat-anthropic", "LongCat Anthropic", "LongCat Platform Anthropic-compatible Messages endpoint for LongCat-2.0.", "LONGCAT_API_KEY", mockProviderTemplate({ name: "longcat-anthropic", kind: "anthropic", baseUrl: "https://api.longcat.chat/anthropic", modelsUrl: "https://api.longcat.chat/anthropic/v1/models", models: mockLongCatModels, default: "LongCat-2.0", apiKeyEnv: "LONGCAT_API_KEY", authHeader: true, contextWindow: 131072, thinking: "enabled", supportedEfforts: ["enabled", "disabled"], defaultEffort: "enabled" })),
    mockPreset("token-rhythm", "Token Rhythm", "Token Rhythm (基元律动) multi-model OpenAI-compatible gateway.", "TOKEN_RHYTHM_API_KEY", mockProviderTemplate({ name: "token-rhythm", kind: "openai", baseUrl: "https://tokenrhythm.studio/v1", modelsUrl: "https://tokenrhythm.studio/v1/models", models: mockTokenRhythmModels, visionModels: ["kimi-k2.5", "kimi-k2.6", "kimi-k2.7-code"], default: "deepseek-v4-flash", apiKeyEnv: "TOKEN_RHYTHM_API_KEY", contextWindow: 1000000, modelOverrides: mockTokenRhythmModelOverrides })),
    mockPreset("kimi-cn", "Kimi CN API", "Moonshot Kimi China OpenAI-compatible API.", "KIMI_API_KEY", mockProviderTemplate({ name: "kimi-cn", kind: "openai", baseUrl: "https://api.moonshot.cn/v1", models: mockKimiAPIModels, visionModels: mockKimiAPIModels, default: "kimi-k2.7-code", apiKeyEnv: "KIMI_API_KEY", balanceUrl: "https://api.moonshot.cn/v1/users/me/balance", contextWindow: 262144, reasoningProtocol: "none", modelOverrides: [{ model: "kimi-k3", reasoningProtocol: "openai", supportedEfforts: ["low", "high", "max"], defaultEffort: "max", contextWindow: 1048576 }] })),
    mockPreset("kimi-global", "Kimi Global API", "Moonshot Kimi international OpenAI-compatible API.", "MOONSHOT_API_KEY", mockProviderTemplate({ name: "kimi-global", kind: "openai", baseUrl: "https://api.moonshot.ai/v1", models: mockKimiAPIModels, visionModels: mockKimiAPIModels, default: "kimi-k2.7-code", apiKeyEnv: "MOONSHOT_API_KEY", balanceUrl: "https://api.moonshot.ai/v1/users/me/balance", contextWindow: 262144, reasoningProtocol: "none", modelOverrides: [{ model: "kimi-k3", reasoningProtocol: "openai", supportedEfforts: ["low", "high", "max"], defaultEffort: "max", contextWindow: 1048576 }] })),
    mockPreset("kimi-coding-plan", "Kimi Coding Plan", "Kimi Coding Plan via its dedicated Anthropic-compatible endpoint.", "KIMI_CODING_API_KEY", mockProviderTemplate({ name: "kimi-coding-plan", kind: "anthropic", baseUrl: "https://api.kimi.com/coding/", models: ["kimi-for-coding"], visionModels: ["kimi-for-coding"], default: "kimi-for-coding", apiKeyEnv: "KIMI_CODING_API_KEY", headers: { "User-Agent": "claude-code/0.1.0" }, thinking: "adaptive", contextWindow: 262144 })),
    mockPreset("mimo-api", "MiMo API", "Xiaomi MiMo direct API with text and vision-capable models.", "MIMO_API_KEY", mockProviderTemplate({ name: "mimo-api", kind: "openai", baseUrl: "https://api.xiaomimimo.com/v1", models: mockMiMoV25Models, visionModels: ["mimo-v2.5"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_API_KEY", contextWindow: 1048576 })),
    mockPreset("mimo-anthropic", "MiMo Anthropic", "Xiaomi MiMo direct Anthropic-compatible endpoint.", "MIMO_API_KEY", mockProviderTemplate({ name: "mimo-anthropic", kind: "anthropic", baseUrl: "https://api.xiaomimimo.com/anthropic", models: mockMiMoV25Models, visionModels: ["mimo-v2.5"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_API_KEY", thinking: "adaptive", contextWindow: 1048576 })),
    mockPreset("mimo-token-plan-cn", "MiMo Token Plan CN", "Xiaomi MiMo token-plan China endpoint.", "MIMO_TOKEN_PLAN_API_KEY", mockProviderTemplate({ name: "mimo-token-plan-cn", kind: "openai", baseUrl: "https://token-plan-cn.xiaomimimo.com/v1", models: mockMiMoV25Models, visionModels: ["mimo-v2.5"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_TOKEN_PLAN_API_KEY", contextWindow: 1048576 })),
    mockPreset("mimo-token-plan-cn-anthropic", "MiMo Token Plan CN Anthropic", "Xiaomi MiMo token-plan China Anthropic-compatible endpoint.", "MIMO_TOKEN_PLAN_API_KEY", mockProviderTemplate({ name: "mimo-token-plan-cn-anthropic", kind: "anthropic", baseUrl: "https://token-plan-cn.xiaomimimo.com/anthropic", models: mockMiMoV25Models, visionModels: ["mimo-v2.5"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_TOKEN_PLAN_API_KEY", thinking: "adaptive", contextWindow: 1048576 })),
    mockPreset("mimo-token-plan-sgp", "MiMo Token Plan SGP", "Xiaomi MiMo token-plan Singapore endpoint.", "MIMO_TOKEN_PLAN_API_KEY", mockProviderTemplate({ name: "mimo-token-plan-sgp", kind: "openai", baseUrl: "https://token-plan-sgp.xiaomimimo.com/v1", models: mockMiMoV25Models, visionModels: ["mimo-v2.5"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_TOKEN_PLAN_API_KEY", contextWindow: 1048576 })),
    mockPreset("mimo-token-plan-sgp-anthropic", "MiMo Token Plan SGP Anthropic", "Xiaomi MiMo token-plan Singapore Anthropic-compatible endpoint.", "MIMO_TOKEN_PLAN_API_KEY", mockProviderTemplate({ name: "mimo-token-plan-sgp-anthropic", kind: "anthropic", baseUrl: "https://token-plan-sgp.xiaomimimo.com/anthropic", models: mockMiMoV25Models, visionModels: ["mimo-v2.5"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_TOKEN_PLAN_API_KEY", thinking: "adaptive", contextWindow: 1048576 })),
    mockPreset("mimo-token-plan-ams", "MiMo Token Plan AMS", "Xiaomi MiMo token-plan Amsterdam endpoint.", "MIMO_TOKEN_PLAN_API_KEY", mockProviderTemplate({ name: "mimo-token-plan-ams", kind: "openai", baseUrl: "https://token-plan-ams.xiaomimimo.com/v1", models: mockMiMoV25Models, visionModels: ["mimo-v2.5"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_TOKEN_PLAN_API_KEY", contextWindow: 1048576 })),
    mockPreset("mimo-token-plan-ams-anthropic", "MiMo Token Plan AMS Anthropic", "Xiaomi MiMo token-plan Amsterdam Anthropic-compatible endpoint.", "MIMO_TOKEN_PLAN_API_KEY", mockProviderTemplate({ name: "mimo-token-plan-ams-anthropic", kind: "anthropic", baseUrl: "https://token-plan-ams.xiaomimimo.com/anthropic", models: mockMiMoV25Models, visionModels: ["mimo-v2.5"], default: "mimo-v2.5-pro", apiKeyEnv: "MIMO_TOKEN_PLAN_API_KEY", thinking: "adaptive", contextWindow: 1048576 })),
    mockPreset("minimax-cn-api", "MiniMax CN API", "MiniMax China OpenAI-compatible M-series API endpoint.", "MINIMAX_API_KEY", mockProviderTemplate({ name: "minimax-cn-api", kind: "openai", baseUrl: "https://api.minimaxi.com/v1", models: mockMiniMaxModels, visionModels: ["MiniMax-M3"], default: "MiniMax-M3", apiKeyEnv: "MINIMAX_API_KEY", extraBody: { reasoning_split: true }, contextWindow: 1048576, thinking: "adaptive", supportedEfforts: ["disabled", "adaptive"], defaultEffort: "adaptive" })),
    mockPreset("minimax-global-api", "MiniMax Global API", "MiniMax international OpenAI-compatible M-series API endpoint.", "MINIMAX_API_KEY", mockProviderTemplate({ name: "minimax-global-api", kind: "openai", baseUrl: "https://api.minimax.io/v1", models: mockMiniMaxModels, visionModels: ["MiniMax-M3"], default: "MiniMax-M3", apiKeyEnv: "MINIMAX_API_KEY", extraBody: { reasoning_split: true }, contextWindow: 1048576, thinking: "adaptive", supportedEfforts: ["disabled", "adaptive"], defaultEffort: "adaptive" })),
    mockPreset("minimax-cn-anthropic", "MiniMax CN Anthropic", "MiniMax China Anthropic-compatible M-series endpoint.", "MINIMAX_PLAN_API_KEY", mockProviderTemplate({ name: "minimax-cn-anthropic", kind: "anthropic", baseUrl: "https://api.minimaxi.com/anthropic", models: mockMiniMaxModels, visionModels: ["MiniMax-M3"], default: "MiniMax-M3", apiKeyEnv: "MINIMAX_PLAN_API_KEY", authHeader: true, contextWindow: 1048576, thinking: "adaptive", supportedEfforts: ["disabled", "adaptive"], defaultEffort: "adaptive" })),
    mockPreset("minimax-global-anthropic", "MiniMax Global Anthropic", "MiniMax international Anthropic-compatible endpoint with Bearer auth.", "MINIMAX_API_KEY", mockProviderTemplate({ name: "minimax-global-anthropic", kind: "anthropic", baseUrl: "https://api.minimax.io/anthropic", models: mockMiniMaxModels, visionModels: ["MiniMax-M3"], default: "MiniMax-M3", apiKeyEnv: "MINIMAX_API_KEY", authHeader: true, contextWindow: 1048576, thinking: "adaptive", supportedEfforts: ["disabled", "adaptive"], defaultEffort: "adaptive" })),
    mockPreset("glm-cn", "GLM CN API", "Zhipu GLM China OpenAI-compatible API with thinking controls.", "GLM_API_KEY", mockProviderTemplate({ name: "glm-cn", kind: "openai", baseUrl: "https://open.bigmodel.cn/api/paas/v4", models: mockGLMAPIModels, visionModels: ["glm-5v-turbo"], default: "glm-5.2", apiKeyEnv: "GLM_API_KEY", contextWindow: 1000000, thinking: "enabled", supportedEfforts: ["enabled", "disabled"], defaultEffort: "enabled" })),
    mockPreset("zai-global", "Z.AI Global API", "Z.AI international OpenAI-compatible GLM API.", "ZAI_API_KEY", mockProviderTemplate({ name: "zai-global", kind: "openai", baseUrl: "https://api.z.ai/api/paas/v4", models: mockGLMAPIModels, visionModels: ["glm-5v-turbo"], default: "glm-5.2", apiKeyEnv: "ZAI_API_KEY", contextWindow: 1000000, thinking: "enabled", supportedEfforts: ["enabled", "disabled"], defaultEffort: "enabled" })),
    mockPreset("glm-coding-plan-cn", "GLM Coding Plan CN", "Zhipu GLM China coding-plan endpoint.", "GLM_PLAN_API_KEY", mockProviderTemplate({ name: "glm-coding-plan-cn", kind: "openai", baseUrl: "https://open.bigmodel.cn/api/coding/paas/v4", models: mockGLMCodingModels, default: "glm-5.2", apiKeyEnv: "GLM_PLAN_API_KEY", contextWindow: 1000000, thinking: "enabled", supportedEfforts: ["enabled", "disabled"], defaultEffort: "enabled" })),
    mockPreset("glm-coding-plan-cn-anthropic", "GLM Coding Plan CN Anthropic", "Zhipu GLM China coding-plan Anthropic-compatible endpoint.", "GLM_PLAN_API_KEY", mockProviderTemplate({ name: "glm-coding-plan-cn-anthropic", kind: "anthropic", baseUrl: "https://open.bigmodel.cn/api/anthropic", models: mockGLMAnthropicModels, default: "glm-5.2", apiKeyEnv: "GLM_PLAN_API_KEY", authHeader: true, thinking: "adaptive", contextWindow: 1000000 })),
    mockPreset("zai-coding-plan-global", "Z.AI Coding Plan Global", "Z.AI international coding-plan endpoint.", "ZAI_CODING_API_KEY", mockProviderTemplate({ name: "zai-coding-plan-global", kind: "openai", baseUrl: "https://api.z.ai/api/coding/paas/v4", models: mockGLMCodingModels, default: "glm-5.2", apiKeyEnv: "ZAI_CODING_API_KEY", contextWindow: 1000000, thinking: "enabled", supportedEfforts: ["enabled", "disabled"], defaultEffort: "enabled" })),
    mockPreset("zai-coding-plan-global-anthropic", "Z.AI Coding Plan Global Anthropic", "Z.AI international coding-plan Anthropic-compatible endpoint.", "ZAI_CODING_API_KEY", mockProviderTemplate({ name: "zai-coding-plan-global-anthropic", kind: "anthropic", baseUrl: "https://api.z.ai/api/anthropic", models: mockGLMAnthropicModels, default: "glm-5.2", apiKeyEnv: "ZAI_CODING_API_KEY", authHeader: true, thinking: "adaptive", contextWindow: 1000000 })),
    mockPreset("opencode-go", "OpenCode Go", "OpenCode Go relay with per-model capability overrides.", "OPENCODE_GO_API_KEY", mockProviderTemplate({ name: "opencode-go", kind: "openai", baseUrl: "https://opencode.ai/zen/go/v1", models: mockOpenCodeGoModels, visionModels: ["kimi-k3"], default: "glm-5.2", apiKeyEnv: "OPENCODE_GO_API_KEY", contextWindow: 128000, modelOverrides: [{ model: "kimi-k3", reasoningProtocol: "openai", supportedEfforts: ["high", "max"], defaultEffort: "max", contextWindow: 1048576 }] })),
    mockPreset("opencode-go-anthropic", "OpenCode Go Anthropic", "OpenCode Go subscription Anthropic-compatible route for Qwen and MiniMax.", "OPENCODE_GO_API_KEY", mockProviderTemplate({ name: "opencode-go-anthropic", kind: "anthropic", baseUrl: "https://opencode.ai/zen/go", models: ["qwen3.7-plus", "qwen3.7-max", "qwen3.6-plus", "minimax-m3", "minimax-m2.7", "minimax-m2.5"], visionModels: ["qwen3.7-plus", "qwen3.6-plus"], default: "qwen3.7-plus", apiKeyEnv: "OPENCODE_GO_API_KEY", thinking: "adaptive", contextWindow: 262144 })),
    mockPreset("opencode-go-deepseek-anthropic", "OpenCode Go DeepSeek Anthropic", "OpenCode Go Anthropic Messages route for DeepSeek Flash with server-side web search.", "OPENCODE_GO_API_KEY", mockProviderTemplate({ name: "opencode-go-deepseek-anthropic", kind: "anthropic", baseUrl: "https://opencode.ai/zen/go", models: ["deepseek-v4-flash"], default: "deepseek-v4-flash", apiKeyEnv: "OPENCODE_GO_API_KEY", thinking: "adaptive", webSearch: true, serverWebSearchCapability: true, contextWindow: 262144, supportedEfforts: ["disabled", "high", "max"], defaultEffort: "high" })),
    mockPreset("opencode-go-deepseek-responses", "OpenCode Go DeepSeek Responses", "OpenCode Go stateless Responses API route for DeepSeek Flash with server-side web search.", "OPENCODE_GO_API_KEY", mockProviderTemplate({ name: "opencode-go-deepseek-responses", kind: "responses", baseUrl: "https://opencode.ai/zen/go/v1", models: ["deepseek-v4-flash"], default: "deepseek-v4-flash", apiKeyEnv: "OPENCODE_GO_API_KEY", webSearch: true, serverWebSearchCapability: true, contextWindow: 128000, supportedEfforts: ["disabled", "high", "max"], defaultEffort: "high" })),
    mockPreset("opencode-zen-anthropic", "OpenCode Zen Anthropic", "OpenCode Zen Anthropic-compatible route for Claude and Qwen models.", "OPENCODE_API_KEY", mockProviderTemplate({ name: "opencode-zen-anthropic", kind: "anthropic", baseUrl: "https://opencode.ai/zen", models: ["claude-sonnet-4-6", "claude-opus-4-8", "claude-haiku-4-5", "qwen3.6-plus", "qwen3.5-plus", "qwen3.6-plus-free"], visionModels: ["claude-sonnet-4-6", "claude-opus-4-8", "claude-haiku-4-5"], default: "claude-sonnet-4-6", apiKeyEnv: "OPENCODE_API_KEY", contextWindow: 262144 })),
    mockPreset("qwen-cn", "Qwen CN API", "Alibaba DashScope China standard OpenAI-compatible endpoint.", "QWEN_API_KEY", mockProviderTemplate({ name: "qwen-cn", kind: "openai", baseUrl: "https://dashscope.aliyuncs.com/compatible-mode/v1", models: mockQwenAPIModels, visionModels: ["qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "kimi-k2.5"], default: "qwen3.7-plus", apiKeyEnv: "QWEN_API_KEY" })),
    mockPreset("qwen-global", "Qwen Global API", "Alibaba DashScope international standard OpenAI-compatible endpoint.", "QWEN_API_KEY", mockProviderTemplate({ name: "qwen-global", kind: "openai", baseUrl: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", models: mockQwenAPIModels, visionModels: ["qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "kimi-k2.5"], default: "qwen3.7-plus", apiKeyEnv: "QWEN_API_KEY" })),
    mockPreset("qwen-coding-plan-cn", "Qwen Coding Plan CN", "Alibaba Cloud Qwen Coding Plan China endpoint.", "QWEN_CODING_API_KEY", mockProviderTemplate({ name: "qwen-coding-plan-cn", kind: "openai", baseUrl: "https://coding.dashscope.aliyuncs.com/v1", models: mockQwenPlanModels, visionModels: mockQwenPlanVisionModels, default: "qwen3.7-plus", apiKeyEnv: "QWEN_CODING_API_KEY" })),
    mockPreset("qwen-coding-plan-cn-anthropic", "Qwen Coding Plan CN Anthropic", "Alibaba Cloud Qwen Coding Plan China Anthropic-compatible endpoint.", "QWEN_CODING_API_KEY", mockProviderTemplate({ name: "qwen-coding-plan-cn-anthropic", kind: "anthropic", baseUrl: "https://coding.dashscope.aliyuncs.com/apps/anthropic", models: mockQwenPlanModels, visionModels: mockQwenPlanVisionModels, default: "qwen3.7-plus", apiKeyEnv: "QWEN_CODING_API_KEY", thinking: "adaptive" })),
    mockPreset("qwen-coding-plan-global", "Qwen Coding Plan Global", "Alibaba Cloud Qwen Coding Plan international endpoint.", "QWEN_CODING_API_KEY", mockProviderTemplate({ name: "qwen-coding-plan-global", kind: "openai", baseUrl: "https://coding-intl.dashscope.aliyuncs.com/v1", models: mockQwenPlanModels, visionModels: mockQwenPlanVisionModels, default: "qwen3.7-plus", apiKeyEnv: "QWEN_CODING_API_KEY" })),
    mockPreset("qwen-coding-plan-global-anthropic", "Qwen Coding Plan Global Anthropic", "Alibaba Cloud Qwen Coding Plan international Anthropic-compatible endpoint.", "QWEN_CODING_API_KEY", mockProviderTemplate({ name: "qwen-coding-plan-global-anthropic", kind: "anthropic", baseUrl: "https://coding-intl.dashscope.aliyuncs.com/apps/anthropic", models: mockQwenPlanModels, visionModels: mockQwenPlanVisionModels, default: "qwen3.7-plus", apiKeyEnv: "QWEN_CODING_API_KEY", thinking: "adaptive" })),
    mockPreset("stepfun", "StepFun", "StepFun coding-plan OpenAI-compatible endpoint.", "STEPFUN_API_KEY", mockProviderTemplate({ name: "stepfun", kind: "openai", baseUrl: "https://api.stepfun.com/step_plan/v1", models: mockStepFunModels, default: "step-3.7-flash", apiKeyEnv: "STEPFUN_API_KEY", supportedEfforts: ["low", "medium", "high"], defaultEffort: "medium" })),
    mockPreset("stepfun-anthropic", "StepFun Anthropic", "StepFun coding-plan Anthropic-compatible endpoint.", "STEPFUN_API_KEY", mockProviderTemplate({ name: "stepfun-anthropic", kind: "anthropic", baseUrl: "https://api.stepfun.com/step_plan", models: mockStepFunModels, default: "step-3.7-flash", apiKeyEnv: "STEPFUN_API_KEY", thinking: "adaptive", supportedEfforts: ["low", "medium", "high"], defaultEffort: "medium" })),
    mockPreset("novita", "NovitaAI", "NovitaAI OpenAI-compatible multi-model gateway.", "NOVITA_API_KEY", mockProviderTemplate({ name: "novita", kind: "openai", baseUrl: "https://api.novita.ai/openai/v1", models: mockNovitaModels, default: "zai-org/glm-5.2", apiKeyEnv: "NOVITA_API_KEY" })),
    mockPreset("gmi", "GMI Cloud", "GMI Cloud direct multi-model OpenAI-compatible gateway.", "GMI_API_KEY", mockProviderTemplate({ name: "gmi", kind: "openai", baseUrl: "https://api.gmi-serving.com/v1", models: mockGMIModels, default: "zai-org/GLM-5.2-FP8", apiKeyEnv: "GMI_API_KEY", headers: { "User-Agent": "Reasonix" } })),
    mockPreset("vercel-ai-gateway", "Vercel AI Gateway", "Vercel AI Gateway via Anthropic-compatible Messages API.", "AI_GATEWAY_API_KEY", mockProviderTemplate({ name: "vercel-ai-gateway", kind: "anthropic", baseUrl: "https://ai-gateway.vercel.sh", models: mockVercelModels, visionModels: ["anthropic/claude-sonnet-4.6", "anthropic/claude-opus-4.8", "openai/gpt-5.4", "openai/gpt-5.4-pro", "moonshotai/kimi-k2.7-code"], default: "anthropic/claude-sonnet-4.6", apiKeyEnv: "AI_GATEWAY_API_KEY", authHeader: true, contextWindow: 1000000 })),
    mockPreset("huggingface", "HuggingFace Router", "HuggingFace Inference Router OpenAI-compatible endpoint.", "HF_TOKEN", mockProviderTemplate({ name: "huggingface", kind: "openai", baseUrl: "https://router.huggingface.co/v1", models: ["zai-org/GLM-5.2", "deepseek-ai/DeepSeek-V3.2", "Qwen/Qwen3.5-72B-Instruct"], default: "zai-org/GLM-5.2", apiKeyEnv: "HF_TOKEN" })),
    mockPreset("nvidia", "NVIDIA NIM", "NVIDIA NIM OpenAI-compatible accelerated inference endpoint.", "NVIDIA_API_KEY", mockProviderTemplate({ name: "nvidia", kind: "openai", baseUrl: "https://integrate.api.nvidia.com/v1", models: ["nvidia/nemotron-3-nano-30b-a3b", "nvidia/nemotron-3-super-120b-a12b", "nvidia/nemotron-3-ultra-550b-a55b", "deepseek-ai/deepseek-v4-pro", "qwen/qwen3.5-397b-a17b"], default: "nvidia/nemotron-3-nano-30b-a3b", apiKeyEnv: "NVIDIA_API_KEY" })),
    mockPreset("kilocode", "KiloCode", "Kilo Code gateway OpenAI-compatible endpoint.", "KILOCODE_API_KEY", mockProviderTemplate({ name: "kilocode", kind: "openai", baseUrl: "https://api.kilo.ai/api/gateway", models: ["kilo/auto"], default: "kilo/auto", apiKeyEnv: "KILOCODE_API_KEY" })),
    mockPreset("ollama-cloud", "Ollama Cloud", "Hosted Ollama Cloud OpenAI-compatible endpoint with max reasoning effort.", "OLLAMA_API_KEY", mockProviderTemplate({ name: "ollama-cloud", kind: "openai", baseUrl: "https://ollama.com/v1", models: mockOllamaCloudModels, default: "glm-5.2", apiKeyEnv: "OLLAMA_API_KEY" })),
];
export const mockPreviewImageDataURL = "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='160' height='120' viewBox='0 0 160 120'%3E%3Cdefs%3E%3ClinearGradient id='g' x1='0' y1='0' x2='1' y2='1'%3E%3Cstop offset='0' stop-color='%23f97316'/%3E%3Cstop offset='1' stop-color='%232563eb'/%3E%3C/linearGradient%3E%3C/defs%3E%3Crect width='160' height='120' rx='14' fill='url(%23g)'/%3E%3Ccircle cx='44' cy='38' r='16' fill='%23fff7ed' opacity='.9'/%3E%3Cpath d='M18 96 62 58l24 22 18-16 38 32z' fill='%23ffffff' opacity='.9'/%3E%3C/svg%3E";
export { onEvent as onEvent } from "./bridge_events";
export type { TerminalOutputEvent as TerminalOutputEvent } from "./bridge_events";
export type { TerminalExitEvent as TerminalExitEvent } from "./bridge_events";
export { onTerminalOutput as onTerminalOutput } from "./bridge_events";
export { onTerminalExit as onTerminalExit } from "./bridge_events";
export { __emitMockTerminalOutput as __emitMockTerminalOutput } from "./bridge_events";
export { __emitMockTerminalExit as __emitMockTerminalExit } from "./bridge_events";
export { onUpdaterProgress as onUpdaterProgress } from "./bridge_events";
export { isWailsNonFileDragError as isWailsNonFileDragError } from "./bridge_events";
export { isWailsNonFileDragErrorEvent as isWailsNonFileDragErrorEvent } from "./bridge_events";
export { isTransientWailsIPCError as isTransientWailsIPCError } from "./bridge_events";
export { installWailsNonFileDragErrorSuppression as installWailsNonFileDragErrorSuppression } from "./bridge_events";
export { onFilesDropped as onFilesDropped } from "./bridge_events";
export { onRuntimeRebuilt as onRuntimeRebuilt } from "./bridge_events";
export { onReady as onReady } from "./bridge_events";
export { onProjectTreeChanged as onProjectTreeChanged } from "./bridge_events";
export { onTopicActivation as onTopicActivation } from "./bridge_events";
export { onTabMeta as onTabMeta } from "./bridge_events";
export { __emitMockTopicActivation as __emitMockTopicActivation } from "./bridge_events";
export { __emitMockTabMeta as __emitMockTabMeta } from "./bridge_events";
export { onSessionRecovered as onSessionRecovered } from "./bridge_events";
export { onSessionRecoveryFailed as onSessionRecoveryFailed } from "./bridge_events";
export { onRemoteStatus as onRemoteStatus } from "./bridge_events";
export { onRemoteForwards as onRemoteForwards } from "./bridge_events";
export { onRemoteServer as onRemoteServer } from "./bridge_events";
export { __emitMockRemote as __emitMockRemote } from "./bridge_events";
export { EVENT_CHANNEL as EVENT_CHANNEL } from "./bridge_events";
export { errorMessage as errorMessage } from "./bridge_events";
export { bridgeBreadcrumb as bridgeBreadcrumb } from "./bridge_events";
export { elapsedMs as elapsedMs } from "./bridge_events";
export { baseName as baseName } from "./bridge_mock_helpers";
export { browserPlatformOverride as browserPlatformOverride } from "./bridge_mock_helpers";
export { browserPreviewBashSandboxMode as browserPreviewBashSandboxMode } from "./bridge_mock_helpers";
export { browserPreviewEffectiveShell as browserPreviewEffectiveShell } from "./bridge_mock_helpers";
export { mockScenario as mockScenario } from "./bridge_mock_helpers";
export type { MockProviderPresetTemplate as MockProviderPresetTemplate } from "./bridge_mock_helpers";
export { mockProviderTemplate as mockProviderTemplate } from "./bridge_mock_helpers";
export { mockPreset as mockPreset } from "./bridge_mock_helpers";
export { mockProviderPresetViews as mockProviderPresetViews } from "./bridge_mock_helpers";
export { cloneMockProviderTemplate as cloneMockProviderTemplate } from "./bridge_mock_helpers";
export { mockExternalOpenerIconDataURL as mockExternalOpenerIconDataURL } from "./bridge_mock_helpers";
export { makeMockApp as makeMockApp } from "./bridge_mock";
export type { AppBindings as AppBindings } from "./bridge_types";
export type { _CheckGenToApp as _CheckGenToApp } from "./bridge_types";
export type { WailsRuntime as WailsRuntime } from "./bridge_types";

