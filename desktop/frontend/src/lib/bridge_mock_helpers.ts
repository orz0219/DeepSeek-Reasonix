import type { ProviderPresetView, ProviderView } from "./types";
import { mockProviderPresetTemplates } from "./bridge";
export function baseName(path: string): string {
    return path.replace(/[/\\]+$/, "").split(/[/\\]/).filter(Boolean).pop() ?? path;
}
export function browserPlatformOverride(): "darwin" | "windows" | "linux" | "" {
    if (typeof window === "undefined" || window.runtime)
        return "";
    const value = new URLSearchParams(window.location.search).get("platform");
    return value === "darwin" || value === "windows" || value === "linux" ? value : "";
}
export function browserPreviewBashSandboxMode(): "enforce" | "off" {
    return browserPlatformOverride() === "windows" ? "off" : "enforce";
}
export function browserPreviewEffectiveShell(prefer = "auto"): "bash" | "git-bash" | "powershell" | "pwsh" {
    const normalized = prefer.trim().toLowerCase();
    if (normalized === "powershell" || normalized === "pwsh")
        return normalized;
    return browserPlatformOverride() === "windows" ? "git-bash" : "bash";
}
export function mockScenario(): "demo" | "fresh" | "running" | "guidance" | "recovery" | "sandbox_escape" | "notice" | "deepseek_upgrade" | "bench" {
    if (typeof window === "undefined")
        return "demo";
    const value = new URLSearchParams(window.location.search).get("mock")?.trim().toLowerCase();
    if (value === "fresh" || value === "empty" || value === "first-run")
        return "fresh";
    if (typeof import.meta.env !== "undefined" && import.meta.env.DEV && (value === "recovery" || value === "inbox-recovery"))
        return "recovery";
    if (value === "guidance" || value === "guide" || value === "steer")
        return "guidance";
    if (value === "running" || value === "busy" || value === "streaming")
        return "running";
    if (value === "sandbox_escape" || value === "sandbox-escape" || value === "sandboxescape")
        return "sandbox_escape";
    if (value === "notice" || value === "notices" || value === "notice-preview")
        return "notice";
    if (value === "deepseek_upgrade" || value === "deepseek-upgrade")
        return "deepseek_upgrade";
    if (value === "bench" || value === "benchmark" || value === "perf")
        return "bench";
    return "demo";
}
export type MockProviderPresetTemplate = {
    id: string;
    label: string;
    description: string;
    keyEnv: string;
    provider: ProviderView;
};
export function mockProviderTemplate(p: Pick<ProviderView, "name" | "kind" | "baseUrl" | "models" | "default" | "apiKeyEnv"> & Partial<ProviderView>): ProviderView {
    return {
        name: p.name,
        builtIn: false,
        added: true,
        kind: p.kind,
        baseUrl: p.baseUrl,
        modelsUrl: p.modelsUrl ?? "",
        models: p.models,
        visionModels: p.visionModels ?? [],
        visionModelsConfigured: Boolean(p.visionModelsConfigured ?? ((p.visionModels ?? []).length > 0)),
        visionCapability: p.visionCapability,
        default: p.default,
        apiKeyEnv: p.apiKeyEnv,
        headers: p.headers,
        extraBody: p.extraBody,
        authHeader: p.authHeader,
        keySet: Boolean(p.keySet),
        balanceUrl: p.balanceUrl ?? "",
        contextWindow: p.contextWindow ?? 0,
        reasoningProtocol: p.reasoningProtocol ?? "",
        thinking: p.thinking ?? "",
        webSearch: Boolean(p.webSearch),
        serverWebSearchCapability: Boolean(p.serverWebSearchCapability),
        supportedEfforts: p.supportedEfforts ?? [],
        defaultEffort: p.defaultEffort ?? "",
        modelOverrides: p.modelOverrides,
    };
}
export function mockPreset(id: string, label: string, description: string, keyEnv: string, provider: ProviderView): MockProviderPresetTemplate {
    return { id, label, description, keyEnv, provider };
}
export function mockProviderPresetViews(): ProviderPresetView[] {
    return [...mockProviderPresetTemplates].sort((a, b) => mockProviderPresetDisplayRank(a.id) - mockProviderPresetDisplayRank(b.id)).map((template) => ({
        id: template.id,
        label: template.label,
        description: template.description,
        keyEnv: template.keyEnv,
        providerNames: [template.provider.name],
        models: [...template.provider.models],
        added: false,
        status: "available",
        statusProviderNames: [],
        keySet: false,
        requiresKey: true,
        configured: false,
    }));
}
function mockProviderPresetDisplayRank(id: string): number {
    if (id === "deepseek-responses")
        return -2;
    if (id === "glm-cn" || id === "zai-global" || id.startsWith("glm-coding-plan-") || id.startsWith("zai-coding-plan-"))
        return 0;
    if (id.startsWith("longcat-"))
        return 1;
    if (id === "token-rhythm")
        return 1;
    if (id.startsWith("kimi-"))
        return 2;
    if (id.startsWith("minimax-"))
        return 3;
    return 4;
}
export function cloneMockProviderTemplate(id: string, key: string): ProviderView | undefined {
    const template = mockProviderPresetTemplates.find((candidate) => candidate.id === id);
    if (!template)
        return undefined;
    return {
        ...JSON.parse(JSON.stringify(template.provider)) as ProviderView,
        keySet: Boolean(key.trim()),
    };
}
export function mockExternalOpenerIconDataURL(color: string, label: string): string {
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="${color}"/><text x="32" y="40" text-anchor="middle" font-family="system-ui" font-size="25" font-weight="700" fill="white">${label}</text></svg>`;
    return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}

