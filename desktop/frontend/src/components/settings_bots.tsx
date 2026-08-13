import { Suspense, useEffect, useRef, useState } from "react";
import { Bot as BotIcon, CheckCircle2, ChevronDown, KeyRound, Loader2, MessageCircle, QrCode, RefreshCw } from "lucide-react";
import { app } from "../lib/bridge";
import { useT, type DictKey } from "../lib/i18n";
import type { BotAccessView, BotAllowlistView, BotConnectionDiagnostic, BotConnectionView, BotRouteView, BotSettingsView } from "../lib/types";
import { ToggleSegment } from "./ToggleSegment";
import { allRefs, toRef, BOT_TOOL_APPROVAL_MODES, BOT_QUEUE_MODES, BOT_QUEUE_DROPS, BOT_ROUTE_CHAT_TYPES, normalizeBotQueueMode, normalizeBotQueueDrop, normalizeBotRoute, emptyBotRoute, defaultBotAccess, normalizeBotAccess, normalizeBotSettings, normalizeBotToolApprovalMode } from "./settings_normalize";
import { BotListInput, BotQQDetailCard, BotConnectionDetailCard } from "./bots_detail_cards";
import { BotInstallTarget, BotOfficialInstallTarget, BotAllowlistTextKey, BotSelfUserTextKey, BotInstallState, BOT_INSTALL_TARGETS, BOT_INSTALL_DEFAULT_TIMEOUT_SECONDS, BOT_INSTALL_MIN_POLL_SECONDS, DEFAULT_QQ_SECRET_ENV, QQ_CONNECTION_ID, BOT_PLATFORM_KEYS, BotPlatformKey, BOT_ALLOWLIST_ROLES, BotAccessListField, botAllowlistKey, botConnectionPlatform, botPlatformLabel, diagnosticReportDetail, botTargetLabel, botTargetHint, qqBotAdded, botAccessReady, botInstallTargetMatchesConnection, botInstallTargetForConnection, formatInstallUserCode, formatInstallTimeLeft, botConnectionLabel, firstConnectionRemote, botConnectionSecretEnv, botDraftWithDerivedGatewayState } from "./settings_bot_helpers";
import { ModelPicker } from "./settings_models";
import { botAllowlistTextValues, botSelfUserTextValues, parseBotListInput } from "./settings_provider_helpers";
import { SectionProps, SettingsInitialFocus, SettingsField, settingsModelMeta, QRCodeSVG } from "./SettingsPanel";
type BotConnectionListItem = {
    kind: "qq";
} | {
    kind: "connection";
    connection: BotConnectionView;
};
type BotsSectionProps = SectionProps & {
    initialFocus?: SettingsInitialFocus;
};
export function BotsSection({ s, busy, apply, initialFocus }: BotsSectionProps) {
    const t = useT();
    const savedBot = normalizeBotSettings(s.bot);
    const [draft, setDraft] = useState<BotSettingsView>(savedBot);
    const [allowlistText, setAllowlistText] = useState<Record<BotAllowlistTextKey, string>>(() => botAllowlistTextValues(savedBot.allowlist));
    const [selfUserText, setSelfUserText] = useState<Record<BotSelfUserTextKey, string>>(() => botSelfUserTextValues(savedBot.selfUserIds));
    const [showAllPlatforms, setShowAllPlatforms] = useState(false);
    const [installTarget, setInstallTarget] = useState<BotInstallTarget>("qq");
    const [install, setInstall] = useState<BotInstallState>({ target: "qq", result: null, status: "idle", timeLeft: 0, message: "" });
    const [diagnostics, setDiagnostics] = useState<Record<string, BotConnectionDiagnostic | string>>({});
    const [testTargets, setTestTargets] = useState<Record<string, string>>({});
    const [connectionSecrets, setConnectionSecrets] = useState<Record<string, string>>({});
    const [accessText, setAccessText] = useState<Record<string, string>>({});
    const [qqSecretValue, setQQSecretValue] = useState("");
    const [expandedConnectionId, setExpandedConnectionId] = useState("");
    const [advancedMode, setAdvancedMode] = useState(false);
    const installRef = useRef(install);
    const installPollTimerRef = useRef<number | null>(null);
    const installCountdownTimerRef = useRef<number | null>(null);
    const installRequestInFlightRef = useRef(false);
    const installAttemptRef = useRef(0);
    const stepConnectRef = useRef<HTMLElement | null>(null);
    const initialFocusHandledRef = useRef("");
    const refs = allRefs(s);
    useEffect(() => {
        const nextBot = normalizeBotSettings(s.bot);
        setDraft(nextBot);
        setAllowlistText(botAllowlistTextValues(nextBot.allowlist));
        setSelfUserText(botSelfUserTextValues(nextBot.selfUserIds));
        setConnectionSecrets({});
        setAccessText({});
        setQQSecretValue("");
        setTestTargets({});
    }, [s.bot]);
    const focusAccessStep = () => {
        if (!expandedConnectionId && connectionItems.length > 0) {
            const first = connectionItems[0];
            if (first.kind === "qq") {
                setInstallTarget("qq");
                setExpandedConnectionId(QQ_CONNECTION_ID);
            }
            else {
                const nextTarget = botInstallTargetForConnection(first.connection);
                setInstallTarget(nextTarget);
                setExpandedConnectionId(first.connection.id);
            }
        }
        window.setTimeout(() => stepConnectRef.current?.scrollIntoView({ block: "start", behavior: "smooth" }), 60);
    };
    useEffect(() => {
        if (initialFocus?.target !== "bot-allowlist")
            return;
        const focusKey = `${initialFocus.target}:${initialFocus.connectionId ?? ""}`;
        if (initialFocusHandledRef.current === focusKey)
            return;
        initialFocusHandledRef.current = focusKey;
        focusAccessStep();
    }, [initialFocus]);
    useEffect(() => {
        installRef.current = install;
    }, [install]);
    useEffect(() => {
        installAttemptRef.current += 1;
        installRequestInFlightRef.current = false;
        clearInstallTimers();
        setInstall({ target: installTarget, result: null, status: "idle", timeLeft: 0, message: "" });
    }, [installTarget]);
    useEffect(() => () => {
        installAttemptRef.current += 1;
        clearInstallTimers();
    }, []);
    const setConnections = (mapper: (connections: BotConnectionView[]) => BotConnectionView[]) => setDraft((prev) => ({ ...prev, connections: mapper(prev.connections) }));
    const persistBotDraft = async (nextDraft: BotSettingsView) => {
        const nextBot = botDraftWithDerivedGatewayState(nextDraft);
        setDraft(nextBot);
        await apply(async () => {
            await app.SetBotSettings(nextBot);
        });
    };
    const persistConnections = (mapper: (connections: BotConnectionView[]) => BotConnectionView[]) => persistBotDraft({ ...draft, connections: mapper(draft.connections) });
    const updateConnection = (id: string, patch: Partial<BotConnectionView>) => setConnections((items) => items.map((item) => item.id === id ? { ...item, ...patch } : item));
    const persistConnection = (id: string, patch: Partial<BotConnectionView>) => persistConnections((items) => items.map((item) => item.id === id ? { ...item, ...patch } : item));
    const persistConnectionToolApprovalMode = (id: string, mode: string) => {
        const normalizedMode = normalizeBotToolApprovalMode(mode, true);
        setConnections((items) => items.map((item) => item.id === id ? { ...item, toolApprovalMode: normalizedMode } : item));
        void apply(() => app.SetBotConnectionToolApprovalMode(id, normalizedMode));
    };
    const updateConnectionCredential = (id: string, patch: Partial<BotConnectionView["credential"]>) => setConnections((items) => items.map((item) => item.id === id ? { ...item, credential: { ...item.credential, ...patch } } : item));
    const persistConnectionCredential = (id: string, patch: Partial<BotConnectionView["credential"]>) => persistConnections((items) => items.map((item) => item.id === id ? { ...item, credential: { ...item.credential, ...patch } } : item));
    const updateAllowlist = (patch: Partial<BotAllowlistView>) => setDraft((prev) => ({ ...prev, allowlist: { ...prev.allowlist, ...patch } }));
    const persistAllowlist = (patch: Partial<BotAllowlistView>) => persistBotDraft({ ...draft, allowlist: { ...draft.allowlist, ...patch } });
    const persistAllowlistText = (key: BotAllowlistTextKey, value: string) => {
        const entries = parseBotListInput(value);
        setAllowlistText((prev) => ({ ...prev, [key]: entries.join("\n") }));
        void persistAllowlist({ [key]: entries } as Partial<BotAllowlistView>);
    };
    const updateBotSettings = (patch: Partial<BotSettingsView>) => setDraft((prev) => ({ ...prev, ...patch }));
    const persistBotSettings = (patch: Partial<BotSettingsView>) => persistBotDraft({ ...draft, ...patch });
    const updateSelfUserText = (key: BotSelfUserTextKey, value: string) => setSelfUserText((prev) => ({ ...prev, [key]: value }));
    const persistSelfUserText = (key: BotSelfUserTextKey, value: string) => {
        const entries = parseBotListInput(value);
        const nextSelfUserIds = { ...draft.selfUserIds, [key]: entries };
        setSelfUserText((prev) => ({ ...prev, [key]: entries.join("\n") }));
        void persistBotSettings({ selfUserIds: nextSelfUserIds });
    };
    const updateRoute = (index: number, patch: Partial<BotRouteView>) => setDraft((prev) => ({
        ...prev,
        routes: prev.routes.map((route, routeIndex) => routeIndex === index ? normalizeBotRoute({ ...route, ...patch }) : route),
    }));
    const persistRoute = (index: number, patch: Partial<BotRouteView>) => persistBotDraft({
        ...draft,
        routes: draft.routes.map((route, routeIndex) => routeIndex === index ? normalizeBotRoute({ ...route, ...patch }) : route),
    });
    const addRoute = () => setDraft((prev) => ({ ...prev, routes: [...prev.routes, emptyBotRoute()] }));
    const removeRoute = (index: number) => void persistBotDraft({ ...draft, routes: draft.routes.filter((_, routeIndex) => routeIndex !== index) });
    const updateQQ = (patch: Partial<BotSettingsView["qq"]>) => setDraft((prev) => ({ ...prev, qq: { ...prev.qq, ...patch } }));
    const persistQQ = (patch: Partial<BotSettingsView["qq"]>) => persistBotDraft({ ...draft, qq: { ...draft.qq, ...patch } });
    const updateQQAccess = (patch: Partial<BotAccessView>) => updateQQ({ access: normalizeBotAccess({ ...draft.qq.access, ...patch }) });
    const persistQQAccess = (patch: Partial<BotAccessView>) => persistQQ({ access: normalizeBotAccess({ ...draft.qq.access, ...patch }) });
    const updateConnectionAccess = (id: string, patch: Partial<BotAccessView>) => setConnections((items) => items.map((item) => item.id === id ? { ...item, access: normalizeBotAccess({ ...item.access, ...patch }) } : item));
    const persistConnectionAccess = (connection: BotConnectionView, patch: Partial<BotAccessView>) => persistConnection(connection.id, { access: normalizeBotAccess({ ...connection.access, ...patch }) });
    const accessTextKey = (id: string, field: BotAccessListField) => `${id}:${field}`;
    const accessListText = (id: string, access: BotAccessView, field: BotAccessListField) => accessText[accessTextKey(id, field)] ?? access[field].join("\n");
    const setAccessListText = (id: string, field: BotAccessListField, value: string) => setAccessText((prev) => ({ ...prev, [accessTextKey(id, field)]: value }));
    const persistAccessListText = (id: string, access: BotAccessView, field: BotAccessListField, value: string, persistAccess: (patch: Partial<BotAccessView>) => void) => {
        const entries = parseBotListInput(value);
        setAccessText((prev) => ({ ...prev, [accessTextKey(id, field)]: entries.join("\n") }));
        persistAccess({ ...access, [field]: entries } as Partial<BotAccessView>);
    };
    const removeConnection = async (connection: BotConnectionView) => {
        const nextDraft = botDraftWithDerivedGatewayState({
            ...draft,
            connections: draft.connections.filter((item) => item.id !== connection.id),
        });
        await apply(async () => {
            await app.SetBotSettings(nextDraft);
        });
    };
    const installQrURL = install.result?.url ?? "";
    const installQrIsImage = installQrURL.startsWith("data:image/");
    const isQQInstallTarget = installTarget === "qq";
    const selectedInstallLabel = botTargetLabel(installTarget, t);
    const installUserCode = install.result?.userCode && installTarget !== "weixin" ? formatInstallUserCode(install.result.userCode) : "";
    const qqSecretEnv = draft.qq.appSecretEnv.trim() || DEFAULT_QQ_SECRET_ENV;
    const qqConfigured = Boolean(draft.qq.enabled && draft.qq.appId.trim() && qqSecretEnv && draft.qq.secretSet);
    const qqCanEnableAccess = botAccessReady(draft.qq.access);
    const qqCanSaveAndEnable = Boolean(draft.qq.appId.trim() && qqSecretEnv && (draft.qq.secretSet || qqSecretValue.trim()) && qqCanEnableAccess);
    const qqAdded = qqBotAdded(draft.qq);
    const nativeRuntimeAvailable = typeof window !== "undefined" && Boolean(window.runtime);
    const browserPreviewBotConfigured = !nativeRuntimeAvailable && (qqAdded || draft.connections.length > 0);
    const qqOnline = qqConfigured && nativeRuntimeAvailable;
    const connectionItems: BotConnectionListItem[] = [
        ...(qqAdded ? [{ kind: "qq" as const }] : []),
        ...draft.connections.map((connection) => ({ kind: "connection" as const, connection })),
    ];
    const selectedInstallConnection = isQQInstallTarget ? undefined : draft.connections.find((connection) => botInstallTargetMatchesConnection(installTarget, connection));
    const selectedChannelConfigured = isQQInstallTarget ? qqAdded : Boolean(selectedInstallConnection);
    const routeConnectionOptions = [
        ...(qqAdded ? [{ id: "qq", label: "QQ" }] : []),
        ...draft.connections.map((connection) => ({
            id: connection.id || [connection.provider, connection.domain].filter(Boolean).join("-"),
            label: connection.label || botConnectionLabel(connection, t),
        })).filter((item) => item.id),
    ];
    const saveBot = () => app.SetBotSettings(botDraftWithDerivedGatewayState(draft));
    function clearInstallTimers() {
        if (installPollTimerRef.current !== null) {
            window.clearTimeout(installPollTimerRef.current);
            installPollTimerRef.current = null;
        }
        if (installCountdownTimerRef.current !== null) {
            window.clearInterval(installCountdownTimerRef.current);
            installCountdownTimerRef.current = null;
        }
    }
    function beginInstallCountdown(attempt: number) {
        if (installCountdownTimerRef.current !== null) {
            window.clearInterval(installCountdownTimerRef.current);
        }
        installCountdownTimerRef.current = window.setInterval(() => {
            setInstall((prev) => {
                if (installAttemptRef.current !== attempt || prev.status !== "showing")
                    return prev;
                return { ...prev, timeLeft: Math.max(0, prev.timeLeft - 1) };
            });
        }, 1000);
    }
    function scheduleInstallPoll(attempt: number, interval: number) {
        if (installPollTimerRef.current !== null) {
            window.clearTimeout(installPollTimerRef.current);
        }
        installPollTimerRef.current = window.setTimeout(() => void pollInstall(attempt), Math.max(interval || BOT_INSTALL_MIN_POLL_SECONDS, BOT_INSTALL_MIN_POLL_SECONDS) * 1000);
    }
    const startInstall = async (target: BotOfficialInstallTarget) => {
        if (installRequestInFlightRef.current)
            return;
        const existing = draft.connections.find((connection) => botInstallTargetMatchesConnection(target, connection));
        if (existing) {
            installAttemptRef.current += 1;
            clearInstallTimers();
            setInstall({ target, result: null, status: "connected", timeLeft: 0, message: t("settings.botInstallAlreadyConnected", { provider: botTargetLabel(target, t) }) });
            return;
        }
        clearInstallTimers();
        const attempt = installAttemptRef.current + 1;
        installAttemptRef.current = attempt;
        installRequestInFlightRef.current = true;
        setInstall({ target, result: null, status: "starting", timeLeft: 0, message: t("settings.botInstallStarting") });
        const provider = target === "weixin" ? "weixin" : "feishu";
        const domain = target === "lark" ? "lark" : target === "weixin" ? "weixin" : "feishu";
        try {
            const result = await app.StartBotConnectionInstall(provider, domain);
            if (installAttemptRef.current !== attempt)
                return;
            if (!result.ok) {
                setInstall({ target, result, status: "error", timeLeft: 0, message: result.message || t("settings.botInstallFailed") });
                return;
            }
            const timeLeft = result.expireIn > 0 ? result.expireIn : BOT_INSTALL_DEFAULT_TIMEOUT_SECONDS;
            setInstall({ target, result, status: "showing", timeLeft, message: result.message || t("settings.botInstallScanHint") });
            beginInstallCountdown(attempt);
            scheduleInstallPoll(attempt, result.interval);
        }
        catch (err) {
            if (installAttemptRef.current === attempt) {
                setInstall({ target, result: null, status: "error", timeLeft: 0, message: err instanceof Error ? err.message : t("settings.botInstallFailed") });
            }
        }
        finally {
            if (installAttemptRef.current === attempt) {
                installRequestInFlightRef.current = false;
            }
        }
    };
    const pollInstall = async (attempt = installAttemptRef.current) => {
        const current = installRef.current;
        if (installAttemptRef.current !== attempt || current.status !== "showing" || !current.result?.installId || !current.target)
            return;
        const poll = await app.PollBotConnectionInstall(current.result.installId);
        if (installAttemptRef.current !== attempt)
            return;
        if (poll.done) {
            clearInstallTimers();
            setDraft((prev) => ({
                ...prev,
                enabled: true,
                connections: [...prev.connections.filter((c) => c.id !== poll.connection.id), poll.connection],
            }));
            setInstall((prev) => ({ ...prev, status: "connected", timeLeft: 0, message: poll.message || t("settings.botInstallConnected") }));
            return;
        }
        if (poll.error) {
            clearInstallTimers();
            setInstall((prev) => ({ ...prev, status: "error", timeLeft: 0, message: poll.error }));
            return;
        }
        setInstall((prev) => ({ ...prev, message: poll.message || t("settings.botInstallWaiting") }));
        scheduleInstallPoll(attempt, current.result.interval);
    };
    useEffect(() => {
        if (install.status !== "showing" || install.timeLeft > 0)
            return;
        installAttemptRef.current += 1;
        clearInstallTimers();
        setInstall((prev) => prev.status === "showing" ? { ...prev, status: "error", message: t("settings.botInstallExpired") } : prev);
    }, [install.status, install.timeLeft]);
    const diagnoseConnection = async (id: string) => {
        const diag = await app.DiagnoseBotConnection(id);
        setDiagnostics((prev) => ({ ...prev, [id]: diag }));
        return diag;
    };
    const testConnection = async (connection: BotConnectionView) => {
        const target = (testTargets[connection.id] ?? firstConnectionRemote(connection)).trim();
        const diag = await app.TestBotConnection(connection.id, target);
        setDiagnostics((prev) => ({ ...prev, [connection.id]: diag }));
        if (diag.messageId && target) {
            const updatedAt = new Date().toISOString();
            await persistConnections((items) => items.map((item) => {
                if (item.id !== connection.id)
                    return item;
                const scope = connection.workspaceRoot ? "project" : "global";
                const matchesTestMapping = (mapping: BotConnectionView["sessionMappings"][number]) => mapping.remoteId === target &&
                    !mapping.chatType.trim() &&
                    !mapping.userId.trim() &&
                    !mapping.threadId.trim();
                const sessionMappings = [
                    ...item.sessionMappings.filter((mapping) => !matchesTestMapping(mapping)),
                    { remoteId: target, sessionId: "", sessionSource: "", chatType: "", userId: "", threadId: "", scope, workspaceRoot: scope === "project" ? connection.workspaceRoot : "", updatedAt },
                ];
                return { ...item, sessionMappings, updatedAt };
            }));
        }
    };
    const ensureReportableDiagnostic = async (connection: BotConnectionView) => {
        return diagnoseConnection(connection.id);
    };
    const copyConnectionDiagnostic = async (connection: BotConnectionView) => {
        const diag = await ensureReportableDiagnostic(connection);
        if (!diag.reportDetail)
            return;
        try {
            await navigator.clipboard.writeText(diag.reportDetail);
            setDiagnostics((prev) => ({ ...prev, [connection.id]: { ...diag, message: t("settings.botDiagnosticCopied") } }));
        }
        catch (err) {
            setDiagnostics((prev) => ({
                ...prev,
                [connection.id]: { ...diag, status: "error", message: err instanceof Error ? err.message : t("settings.botDiagnosticCopyFailed") },
            }));
        }
    };
    const reportConnectionDiagnostic = async (connection: BotConnectionView) => {
        const diag = await ensureReportableDiagnostic(connection);
        if (!diag.reportDetail)
            return;
        try {
            await app.ReportCrash(diag.reportKind || "bot", diag.reportDetail);
            setDiagnostics((prev) => ({ ...prev, [connection.id]: { ...diag, status: "ok", message: t("settings.botDiagnosticReportSent") } }));
        }
        catch (err) {
            setDiagnostics((prev) => ({
                ...prev,
                [connection.id]: { ...diag, status: "error", message: err instanceof Error ? err.message : t("settings.botDiagnosticReportFailed") },
            }));
        }
    };
    const saveConnectionSecret = async (connection: BotConnectionView) => {
        const env = botConnectionSecretEnv(connection).trim();
        const value = (connectionSecrets[connection.id] ?? "").trim();
        if (!env || !value)
            return;
        await apply(async () => {
            await saveBot();
            await app.SetBotSecret(env, value);
        });
        setConnectionSecrets((prev) => ({ ...prev, [connection.id]: "" }));
    };
    const clearConnectionSecret = async (connection: BotConnectionView) => {
        const env = botConnectionSecretEnv(connection).trim();
        if (!env)
            return;
        await apply(async () => {
            await saveBot();
            await app.ClearBotSecret(env);
        });
    };
    const clearQQSecret = async () => {
        const env = draft.qq.appSecretEnv.trim() || DEFAULT_QQ_SECRET_ENV;
        if (!env)
            return;
        await apply(async () => {
            await saveBot();
            await app.ClearBotSecret(env);
        });
        setQQSecretValue("");
    };
    const focusQQAccessSettings = () => {
        setDiagnostics((prev) => ({ ...prev, [QQ_CONNECTION_ID]: t("settings.botQQAccessRequired") }));
        setExpandedConnectionId(QQ_CONNECTION_ID);
        window.setTimeout(() => stepConnectRef.current?.scrollIntoView({ block: "start", behavior: "smooth" }), 60);
    };
    const saveQQAndEnable = async () => {
        if (!qqCanEnableAccess) {
            focusQQAccessSettings();
            return;
        }
        const env = draft.qq.appSecretEnv.trim() || DEFAULT_QQ_SECRET_ENV;
        const secret = qqSecretValue.trim();
        const nextDraft = botDraftWithDerivedGatewayState({
            ...draft,
            qq: {
                ...draft.qq,
                enabled: true,
                appId: draft.qq.appId.trim(),
                appSecretEnv: env,
                secretSet: draft.qq.secretSet || Boolean(secret),
            },
        });
        await apply(async () => {
            await app.SetBotSettings(nextDraft);
            if (secret)
                await app.SetBotSecret(env, secret);
        });
        setDraft(nextDraft);
        setQQSecretValue("");
    };
    const removeQQBot = async () => {
        const env = draft.qq.appSecretEnv.trim() || DEFAULT_QQ_SECRET_ENV;
        const nextDraft = botDraftWithDerivedGatewayState({
            ...draft,
            qq: { enabled: false, appId: "", appSecretEnv: DEFAULT_QQ_SECRET_ENV, secretSet: false, sandbox: false, model: "", toolApprovalMode: "ask", workspaceRoot: "", access: defaultBotAccess() },
        });
        await apply(async () => {
            await app.SetBotSettings(nextDraft);
            if (draft.qq.secretSet)
                await app.ClearBotSecret(env);
        });
        setDraft(nextDraft);
        setQQSecretValue("");
        setExpandedConnectionId("");
    };
    const selectedQQ = isQQInstallTarget && qqAdded;
    const selectedConnection = isQQInstallTarget ? null : selectedInstallConnection ?? null;
    const selectedDiagnostic = selectedConnection ? diagnostics[selectedConnection.id] : undefined;
    const selectedDiagnosticDetail = diagnosticReportDetail(selectedDiagnostic);
    const selectedConnectionRemote = selectedConnection ? firstConnectionRemote(selectedConnection) : "";
    const selectedConnectionToolApprovalMode = selectedConnection ? normalizeBotToolApprovalMode(selectedConnection.toolApprovalMode) : "ask";
    const simpleAccessMode = draft.allowlist.allowAll ? "everyone" : "trusted";
    const connectedPlatforms = new Set<BotPlatformKey>();
    if (qqAdded)
        connectedPlatforms.add("qq");
    for (const connection of draft.connections)
        connectedPlatforms.add(botConnectionPlatform(connection));
    const platformHasAllowlistText = (platform: BotPlatformKey) => BOT_ALLOWLIST_ROLES.some((role) => allowlistText[botAllowlistKey(platform, role)].trim());
    const visibleAccessPlatforms = BOT_PLATFORM_KEYS.filter((platform) => showAllPlatforms || connectedPlatforms.size === 0 || connectedPlatforms.has(platform) || platformHasAllowlistText(platform));
    const platformFilterAvailable = connectedPlatforms.size > 0 &&
        BOT_PLATFORM_KEYS.some((platform) => !connectedPlatforms.has(platform) && !platformHasAllowlistText(platform));
    const botChannelConnectionForTarget = (target: BotInstallTarget) => target === "qq" ? null : draft.connections.find((connection) => botInstallTargetMatchesConnection(target, connection));
    const botChannelIsConfigured = (target: BotInstallTarget) => target === "qq" ? qqAdded : Boolean(botChannelConnectionForTarget(target));
    const openBotChannel = (target: BotInstallTarget) => {
        setInstallTarget(target);
        const connection = botChannelConnectionForTarget(target);
        setExpandedConnectionId(target === "qq" && qqAdded ? QQ_CONNECTION_ID : connection?.id || "");
    };
    const setSimpleAccessMode = (mode: "trusted" | "everyone") => {
        const patch = mode === "everyone"
            ? { enabled: false, allowAll: true }
            : { enabled: true, allowAll: false };
        updateAllowlist(patch);
        void persistAllowlist(patch);
    };
    const qqDetailCard = (<BotQQDetailCard draft={draft} busy={busy} refs={refs} s={s} qqSecretValue={qqSecretValue} setQQSecretValue={setQQSecretValue} persistQQ={persistQQ} updateQQ={updateQQ} removeQQBot={removeQQBot} qqOnline={qqOnline} qqConfigured={qqConfigured} qqCanEnableAccess={qqCanEnableAccess} qqCanSaveAndEnable={qqCanSaveAndEnable} saveQQAndEnable={saveQQAndEnable} clearQQSecret={clearQQSecret} focusQQAccessSettings={focusQQAccessSettings} updateQQAccess={updateQQAccess} persistQQAccess={persistQQAccess} accessListText={accessListText} setAccessListText={setAccessListText} persistAccessListText={persistAccessListText}/>);
    const connectionDetailCard = selectedConnection ? (<BotConnectionDetailCard selectedConnection={selectedConnection} selectedConnectionRemote={selectedConnectionRemote} selectedConnectionToolApprovalMode={selectedConnectionToolApprovalMode} selectedDiagnostic={selectedDiagnostic} selectedDiagnosticDetail={selectedDiagnosticDetail} busy={busy} connectionSecrets={connectionSecrets} setConnectionSecrets={setConnectionSecrets} diagnoseConnection={diagnoseConnection} testConnection={testConnection} copyConnectionDiagnostic={copyConnectionDiagnostic} reportConnectionDiagnostic={reportConnectionDiagnostic} persistConnection={persistConnection} updateConnection={updateConnection} persistConnectionAccess={persistConnectionAccess} updateConnectionAccess={updateConnectionAccess} persistConnectionToolApprovalMode={persistConnectionToolApprovalMode} saveConnectionSecret={saveConnectionSecret} updateConnectionCredential={updateConnectionCredential} persistConnectionCredential={persistConnectionCredential} clearConnectionSecret={clearConnectionSecret} removeConnection={removeConnection} refs={refs} s={s} accessListText={accessListText} setAccessListText={setAccessListText} persistAccessListText={persistAccessListText}/>) : null;
    const installPanelContent = (<>
      {isQQInstallTarget ? (<div className="bot-connect-panel bot-connect-panel--manual bot-connect-panel--qq">
          <div className="bot-connect-panel__body">
            <div className="bot-qq-simple__head">
              <div>
                <strong>{selectedInstallLabel}</strong>
                <p>{t("settings.botInstallManualQQ")}</p>
              </div>
              <span className={`bot-qq-simple__status${qqConfigured ? " bot-qq-simple__status--ready" : ""}`}>
                {qqConfigured ? <CheckCircle2 aria-hidden="true"/> : <KeyRound aria-hidden="true"/>}
                {draft.qq.secretSet ? t("settings.botSecretSet") : t("settings.botSecretMissing")}
              </span>
            </div>
            <div className="bot-manual-form bot-manual-form--qq">
              <div className="bot-card-field">
                <span>{t("settings.botAppId")}</span>
                <div>
                  <input className="mem-input" aria-label={t("settings.botAppId")} value={draft.qq.appId} disabled={busy} spellCheck={false} onChange={(event) => updateQQ({ appId: event.target.value })} onBlur={(event) => void persistQQ({ appId: event.currentTarget.value })}/>
                </div>
              </div>
              <div className="bot-card-field">
                <span>{t("settings.botAppSecret")}</span>
                <div>
                  <input className="mem-input" type="password" value={qqSecretValue} disabled={busy} placeholder={draft.qq.secretSet ? t("settings.botSecretSavedOptional") : t("settings.botSecretPaste")} spellCheck={false} aria-label={t("settings.botSecretValue")} onChange={(event) => setQQSecretValue(event.target.value)}/>
                </div>
              </div>
              <div className="bot-qq-simple__actions">
                <button type="button" className="btn btn--primary btn--small" disabled={busy || !qqCanSaveAndEnable} onClick={() => void saveQQAndEnable()}>
                  {t("settings.botSaveAndEnable")}
                </button>
              </div>
              {!qqCanEnableAccess ? <div className="bot-connect-panel__hint bot-connect-panel__hint--warning">{t("settings.botQQAccessRequired")}</div> : null}
            </div>
          </div>
        </div>) : (<div className="bot-connect-panel bot-connect-panel--phone">
          <div className="bot-connect-panel__qr">
            {selectedInstallConnection ? (<div className="bot-connect-panel__state bot-connect-panel__state--success">
                <CheckCircle2 aria-hidden="true"/>
              </div>) : install.status === "showing" && installQrURL ? (installQrIsImage ? (<img src={installQrURL} alt={t("settings.botInstallQrAlt")}/>) : (<Suspense fallback={<div className="bot-connect-panel__state"><QrCode aria-hidden="true"/></div>}>
                  <QRCodeSVG className="bot-connect-panel__qr-code" value={installQrURL} size={196} marginSize={1}/>
                </Suspense>)) : install.status === "starting" ? (<div className="bot-connect-panel__state">
                <Loader2 className="bot-spin" aria-hidden="true"/>
                <span>{t("settings.botInstallStarting")}</span>
              </div>) : install.status === "error" ? (<div className="bot-connect-panel__state bot-connect-panel__state--error">
                <RefreshCw aria-hidden="true"/>
              </div>) : (<div className="bot-connect-panel__state">
                <QrCode aria-hidden="true"/>
              </div>)}
          </div>
          <div className="bot-connect-panel__body">
            <strong>{selectedInstallLabel}</strong>
            <p>
              {selectedInstallConnection
                ? t("settings.botInstallAlreadyConnected", { provider: selectedInstallLabel })
                : install.message || botTargetHint(installTarget, t)}
            </p>
            {install.status === "showing" && install.timeLeft > 0 ? (<span className="bot-connect-panel__timer">{t("settings.botInstallTimeLeft", { time: formatInstallTimeLeft(install.timeLeft) })}</span>) : null}
            {installUserCode ? <code>{installUserCode}</code> : null}
            <div className="bot-connect-panel__actions">
              {!selectedInstallConnection && install.status !== "showing" && install.status !== "starting" ? (<button type="button" className="btn btn--primary btn--small" disabled={busy} onClick={() => void startInstall(installTarget)}>
                  {install.status === "error" ? <RefreshCw aria-hidden="true"/> : <QrCode aria-hidden="true"/>}
                  {install.status === "error" ? t("settings.botInstallRetry") : t("settings.botInstallGenerate")}
                </button>) : null}
              {install.status === "showing" ? (<button type="button" className="btn btn--secondary btn--small" disabled={busy} onClick={() => void pollInstall()}>
                  {t("settings.botInstallCheck")}
                </button>) : null}
              {selectedInstallConnection ? (<button type="button" className="btn btn--secondary btn--small" disabled={busy} onClick={() => void diagnoseConnection(selectedInstallConnection.id)}>
                  {t("settings.botDiagnose")}
                </button>) : null}
            </div>
          </div>
        </div>)}
    </>);
    const botManager = (<section ref={stepConnectRef} id="bot-step-connect" className="bot-channel-manager-card">
      <div className="bot-channel-manager-card__head">
        <div>
          <strong>{t("settings.botManageBots")}</strong>
          <span>{t("settings.botManageBotsHint")}</span>
        </div>
      </div>
      {browserPreviewBotConfigured ? (<div className="bot-connection-warning">{t("settings.botBrowserPreviewWarning")}</div>) : null}
      <div className="bot-channel-manager">
        <div className="bot-channel-tabs" role="tablist" aria-label={t("settings.botChannelTabsLabel")}>
          {BOT_INSTALL_TARGETS.map((target) => {
            const configured = botChannelIsConfigured(target);
            const connected = target === "qq" ? qqOnline : botChannelConnectionForTarget(target)?.status === "connected";
            return (<button key={target} type="button" role="tab" aria-selected={installTarget === target} className={`bot-channel-tab${installTarget === target ? " bot-channel-tab--active" : ""}`} disabled={busy || install.status === "starting"} onClick={() => openBotChannel(target)}>
                <span className="bot-channel-tab__icon" aria-hidden="true">
                  {target === "qq" || target === "weixin" ? <MessageCircle size={24}/> : <BotIcon size={24}/>}
                </span>
                <span className="bot-channel-tab__text">
                  <strong>{botTargetLabel(target, t)}</strong>
                  <small>{botTargetHint(target, t)}</small>
                </span>
                <span className={`bot-channel-tab__dot${connected ? " bot-channel-tab__dot--online" : configured ? " bot-channel-tab__dot--configured" : ""}`}/>
              </button>);
        })}
        </div>
        <div className="bot-channel-manager__detail" role="tabpanel" aria-label={selectedInstallLabel}>
          {!selectedChannelConfigured ? (<article className="bot-channel-setup-card">
              <div className="bot-channel-setup-card__head">
                <div>
                  <strong>{t("settings.botChannelSetupTitle", { provider: selectedInstallLabel })}</strong>
                  <span>{t("settings.botChannelSetupHint")}</span>
                </div>
                <span className="badge badge--neutral">{t("settings.botChannelNeedsSetup")}</span>
              </div>
              {installPanelContent}
            </article>) : selectedQQ ? (qqDetailCard) : selectedConnection ? (connectionDetailCard) : (<div className="bot-manager__empty">{t("settings.botSelectBotHint")}</div>)}
        </div>
      </div>
    </section>);
    return (<div className="bot-phone-connect">
      {botManager}

	      <details id="bot-advanced-settings" className="bot-simple-advanced" open={advancedMode} onToggle={(event) => {
            const nextOpen = event.currentTarget.open;
            setAdvancedMode((current) => current === nextOpen ? current : nextOpen);
        }}>
        <summary className="bot-simple-advanced__summary">
          <span>
            <strong>{t("settings.botShowAdvancedSettings")}</strong>
            <small>{t("settings.botAdvancedSettingsHint")}</small>
          </span>
          <span className="bot-simple-advanced__toggle">
            {advancedMode ? t("common.collapse") : t("common.expand")}
            <ChevronDown aria-hidden="true" size={16}/>
          </span>
	        </summary>
	        <div className="bot-simple-advanced__body">
	          <details className="bot-access-panel bot-global-access-panel">
	            <summary className="bot-access-panel__summary">
	              <span>
	                <strong>{t("settings.botGlobalAllowlist")}</strong>
	                <small>{t("settings.botGlobalAllowlistHint")}</small>
	              </span>
	              <ChevronDown className="bot-access-panel__chevron" size={16} aria-hidden="true"/>
	            </summary>
	            <div className="bot-access-panel__body">
	              <div className="bot-choice-grid bot-choice-grid--access">
	                <button type="button" className={`bot-choice-card${simpleAccessMode === "trusted" ? " bot-choice-card--active" : ""}`} disabled={busy} onClick={() => setSimpleAccessMode("trusted")}>
	                  <strong>{t("settings.botAccessTrusted")}</strong>
	                  <span>{t("settings.botAccessTrustedHint")}</span>
	                </button>
	                <button type="button" className={`bot-choice-card${simpleAccessMode === "everyone" ? " bot-choice-card--active" : ""}`} disabled={busy} onClick={() => setSimpleAccessMode("everyone")}>
	                  <strong>{t("settings.botAccessEveryone")}</strong>
	                  <span>{t("settings.botAccessEveryoneHint")}</span>
	                </button>
	              </div>
	              <div className="bot-pairing-row">
	                <div>
	                  <strong>{t("settings.botAccessPairing")}</strong>
	                  <span>{t("settings.botAccessPairingHint")}</span>
	                </div>
	                <ToggleSegment value={draft.pairing.enabled} disabled={busy} onChange={(enabled) => void persistBotSettings({ pairing: { ...draft.pairing, enabled } })}/>
	              </div>
	              {draft.allowlist.allowAll ? (<div className="bot-access-panel__warning">{t("settings.botAllowAllWarn")}</div>) : (<>
	                  <div className="bot-access-platforms">
	                    {visibleAccessPlatforms.map((platform) => (<div className="bot-access-platform" key={platform}>
	                        <div className="bot-access-platform__name">{botPlatformLabel(platform, t)}</div>
	                        <BotListInput label={t("settings.botListUsers")} value={allowlistText[botAllowlistKey(platform, "Users")]} disabled={busy} placeholder={t("settings.botListPlaceholder")} onChange={(value) => setAllowlistText((prev) => ({ ...prev, [botAllowlistKey(platform, "Users")]: value }))} onBlur={(value) => persistAllowlistText(botAllowlistKey(platform, "Users"), value)}/>
	                        <BotListInput label={t("settings.botListGroups")} value={allowlistText[botAllowlistKey(platform, "Groups")]} disabled={busy} placeholder={t("settings.botListPlaceholder")} onChange={(value) => setAllowlistText((prev) => ({ ...prev, [botAllowlistKey(platform, "Groups")]: value }))} onBlur={(value) => persistAllowlistText(botAllowlistKey(platform, "Groups"), value)}/>
	                      </div>))}
	                  </div>
	                  {platformFilterAvailable ? (<button type="button" className="bot-access-platforms__toggle" onClick={() => setShowAllPlatforms((value) => !value)}>
	                      {showAllPlatforms ? t("settings.botAccessShowConnectedOnly") : t("settings.botAccessShowAllPlatforms")}
	                    </button>) : null}
	                </>)}
	              <details className="bot-access-panel bot-simple-roles">
	                <summary className="bot-access-panel__summary">
	                  <span>
	                    <strong>{t("settings.botRoleAccess")}</strong>
	                    <small>{t("settings.botRoleAccessHint")}</small>
	                  </span>
	                  <ChevronDown className="bot-access-panel__chevron" size={16} aria-hidden="true"/>
	                </summary>
	                <div className="bot-access-panel__body">
	                  <div className="bot-access-platforms">
	                    {visibleAccessPlatforms.map((platform) => (<div className="bot-access-platform" key={platform}>
	                        <div className="bot-access-platform__name">{botPlatformLabel(platform, t)}</div>
	                        <BotListInput label={t("settings.botListApprovers")} value={allowlistText[botAllowlistKey(platform, "Approvers")]} disabled={busy || draft.allowlist.allowAll} placeholder={t("settings.botListPlaceholder")} onChange={(value) => setAllowlistText((prev) => ({ ...prev, [botAllowlistKey(platform, "Approvers")]: value }))} onBlur={(value) => persistAllowlistText(botAllowlistKey(platform, "Approvers"), value)}/>
	                        <BotListInput label={t("settings.botListAdmins")} value={allowlistText[botAllowlistKey(platform, "Admins")]} disabled={busy || draft.allowlist.allowAll} placeholder={t("settings.botListPlaceholder")} onChange={(value) => setAllowlistText((prev) => ({ ...prev, [botAllowlistKey(platform, "Admins")]: value }))} onBlur={(value) => persistAllowlistText(botAllowlistKey(platform, "Admins"), value)}/>
	                      </div>))}
	                  </div>
	                </div>
	              </details>
	            </div>
	          </details>
	          <details className="bot-access-panel bot-gateway-panel">
            <summary className="bot-access-panel__summary">
              <span>
                <strong>{t("settings.botGatewayDefaults")}</strong>
                <small>{t("settings.botGatewayDefaultsHint")}</small>
              </span>
              <ChevronDown className="bot-access-panel__chevron" size={16} aria-hidden="true"/>
            </summary>
            <div className="bot-access-panel__body">
              <SettingsField label={t("settings.botRuntime")} hint={t("settings.botRuntimeHint")}>
                <div className="bot-inline-grid bot-inline-grid--runtime">
                  <label>
                    <span>{t("settings.botMaxSteps")}</span>
                    <input className="mem-input" type="number" min={0} value={draft.maxSteps} disabled={busy} onChange={(event) => updateBotSettings({ maxSteps: Number(event.target.value) || 0 })} onBlur={(event) => void persistBotSettings({ maxSteps: Number(event.currentTarget.value) || 0 })}/>
                  </label>
                  <label>
                    <span>{t("settings.botDebounceMs")}</span>
                    <input className="mem-input" type="number" min={0} value={draft.debounceMs} disabled={busy} onChange={(event) => updateBotSettings({ debounceMs: Number(event.target.value) || 0 })} onBlur={(event) => void persistBotSettings({ debounceMs: Number(event.currentTarget.value) || 0 })}/>
                  </label>
                  <label>
                    <span>{t("settings.botQueueCap")}</span>
                    <input className="mem-input" type="number" min={0} value={draft.queueCap} disabled={busy} onChange={(event) => updateBotSettings({ queueCap: Number(event.target.value) || 0 })} onBlur={(event) => void persistBotSettings({ queueCap: Number(event.currentTarget.value) || 0 })}/>
                  </label>
                </div>
              </SettingsField>
              <SettingsField label={t("settings.botQueueModeSimple")} hint={t("settings.botQueueModeSimpleHint")}>
                <select className="mem-select" value={normalizeBotQueueMode(draft.queueMode)} disabled={busy} onChange={(event) => void persistBotSettings({ queueMode: event.target.value })}>
                  {BOT_QUEUE_MODES.map((mode) => (<option key={mode} value={mode}>{t(`settings.botQueueMode.${mode}` as DictKey)}</option>))}
                </select>
              </SettingsField>
              <SettingsField label={t("settings.botQueueDropLabel")} hint={t("settings.botQueueDropHint")}>
                <select className="mem-select" value={normalizeBotQueueDrop(draft.queueDrop)} disabled={busy} onChange={(event) => void persistBotSettings({ queueDrop: event.target.value })}>
                  {BOT_QUEUE_DROPS.map((mode) => (<option key={mode} value={mode}>{t(`settings.botQueueDrop.${mode}` as DictKey)}</option>))}
                </select>
              </SettingsField>
              <SettingsField label={t("settings.botIgnoreSelfMessages")} hint={t("settings.botIgnoreSelfMessagesHint")}>
                <ToggleSegment value={draft.ignoreSelfMessages} disabled={busy} onChange={(ignoreSelfMessages) => void persistBotSettings({ ignoreSelfMessages })}/>
              </SettingsField>
              <SettingsField label={t("settings.botSelfUserIds")} hint={t("settings.botSelfUserIdsHint")}>
                <div className="bot-list-grid">
                  <BotListInput label={t("settings.botQQUsers")} value={selfUserText.qq} disabled={busy} placeholder={t("settings.botListPlaceholder")} onChange={(value) => updateSelfUserText("qq", value)} onBlur={(value) => persistSelfUserText("qq", value)}/>
                  <BotListInput label={t("settings.botFeishuLarkUsers")} value={selfUserText.feishu} disabled={busy} placeholder={t("settings.botListPlaceholder")} onChange={(value) => updateSelfUserText("feishu", value)} onBlur={(value) => persistSelfUserText("feishu", value)}/>
                  <BotListInput label={t("settings.botWeixinUsers")} value={selfUserText.weixin} disabled={busy} placeholder={t("settings.botListPlaceholder")} onChange={(value) => updateSelfUserText("weixin", value)} onBlur={(value) => persistSelfUserText("weixin", value)}/>
                </div>
              </SettingsField>
              <SettingsField label={t("settings.botPairing")} hint={t("settings.botPairingDetailHint")}>
                <div className="bot-inline-grid bot-inline-grid--runtime">
                  <label>
                    <span>{t("settings.botPairingTTL")}</span>
                    <input className="mem-input" type="number" min={0} value={draft.pairing.requestTtlMinutes} disabled={busy} onChange={(event) => updateBotSettings({ pairing: { ...draft.pairing, requestTtlMinutes: Number(event.target.value) || 0 } })} onBlur={(event) => void persistBotSettings({ pairing: { ...draft.pairing, requestTtlMinutes: Number(event.currentTarget.value) || 0 } })}/>
                  </label>
                  <label>
                    <span>{t("settings.botPairingMaxPending")}</span>
                    <input className="mem-input" type="number" min={0} value={draft.pairing.maxPendingPerPlatform} disabled={busy} onChange={(event) => updateBotSettings({ pairing: { ...draft.pairing, maxPendingPerPlatform: Number(event.target.value) || 0 } })} onBlur={(event) => void persistBotSettings({ pairing: { ...draft.pairing, maxPendingPerPlatform: Number(event.currentTarget.value) || 0 } })}/>
                  </label>
                </div>
              </SettingsField>
            </div>
          </details>

          <details className="bot-access-panel bot-routes-panel">
            <summary className="bot-access-panel__summary">
              <span>
                <strong>{t("settings.botRoutes")}</strong>
                <small>{t("settings.botRoutesHint")}</small>
              </span>
              <ChevronDown className="bot-access-panel__chevron" size={16} aria-hidden="true"/>
            </summary>
            <div className="bot-access-panel__body">
              {draft.routes.length === 0 ? (<div className="bot-route-empty">{t("settings.botRoutesEmpty")}</div>) : (<div className="bot-route-list">
                  {draft.routes.map((route, index) => (<div className="bot-route-row" key={index}>
                      <div className="bot-route-row__head">
                        <strong>{t("settings.botRouteTitle", { n: index + 1 })}</strong>
                        <button type="button" className="btn btn--secondary btn--small" disabled={busy} onClick={() => removeRoute(index)}>
                          {t("common.delete")}
                        </button>
                      </div>
                      <div className="bot-route-grid">
                        <label>
                          <span>{t("settings.botRouteConnection")}</span>
                          <select className="mem-select" value={route.connectionId} disabled={busy} onChange={(event) => {
                    updateRoute(index, { connectionId: event.target.value });
                    void persistRoute(index, { connectionId: event.target.value });
                }}>
                            <option value="">{t("settings.botRouteAny")}</option>
                            {routeConnectionOptions.map((option) => (<option key={option.id} value={option.id}>{option.label} · {option.id}</option>))}
                          </select>
                        </label>
                        <label>
                          <span>{t("settings.botRoutePlatform")}</span>
                          <select className="mem-select" value={route.platform} disabled={busy} onChange={(event) => {
                    updateRoute(index, { platform: event.target.value });
                    void persistRoute(index, { platform: event.target.value });
                }}>
                            <option value="">{t("settings.botRouteAny")}</option>
                            <option value="qq">QQ</option>
                            <option value="feishu">{t("settings.botFeishu")}</option>
                            <option value="weixin">{t("settings.botWeixin")}</option>
                          </select>
                        </label>
                        <label>
                          <span>{t("settings.botRouteChatType")}</span>
                          <select className="mem-select" value={route.chatType} disabled={busy} onChange={(event) => {
                    updateRoute(index, { chatType: event.target.value });
                    void persistRoute(index, { chatType: event.target.value });
                }}>
                            {BOT_ROUTE_CHAT_TYPES.map((chatType) => (<option key={chatType || "any"} value={chatType}>{t(`settings.botRouteChatType.${chatType || "any"}` as DictKey)}</option>))}
                          </select>
                        </label>
                        <label>
                          <span>{t("settings.botRouteChatId")}</span>
                          <input className="mem-input" value={route.chatId} disabled={busy} spellCheck={false} onChange={(event) => updateRoute(index, { chatId: event.target.value })} onBlur={(event) => void persistRoute(index, { chatId: event.currentTarget.value })}/>
                        </label>
                        <label>
                          <span>{t("settings.botRouteUserId")}</span>
                          <input className="mem-input" value={route.userId} disabled={busy} spellCheck={false} onChange={(event) => updateRoute(index, { userId: event.target.value })} onBlur={(event) => void persistRoute(index, { userId: event.currentTarget.value })}/>
                        </label>
                        <label>
                          <span>{t("settings.botRouteThreadId")}</span>
                          <input className="mem-input" value={route.threadId} disabled={busy} spellCheck={false} onChange={(event) => updateRoute(index, { threadId: event.target.value })} onBlur={(event) => void persistRoute(index, { threadId: event.currentTarget.value })}/>
                        </label>
                      </div>
                      <div className="bot-route-grid bot-route-grid--outputs">
                        <label>
                          <span>{t("settings.botWorkspaceRoot")}</span>
                          <input className="mem-input" value={route.workspaceRoot} disabled={busy} placeholder={t("settings.botWorkspaceRootPlaceholder")} spellCheck={false} onChange={(event) => updateRoute(index, { workspaceRoot: event.target.value })} onBlur={(event) => void persistRoute(index, { workspaceRoot: event.currentTarget.value })}/>
                        </label>
                        <label>
                          <span>{t("settings.botChannelModel")}</span>
                          <ModelPicker s={s} refs={refs} value={toRef(route.model, s)} disabled={busy} emptyOptionLabel={t("settings.botChannelModelAuto")} emptyOptionHint={settingsModelMeta(s, t)} onPick={(model) => void persistRoute(index, { model })}/>
                        </label>
                        <label>
                          <span>{t("settings.botToolApprovalMode")}</span>
                          <select className="mem-select" value={route.toolApprovalMode} disabled={busy} onChange={(event) => {
                    updateRoute(index, { toolApprovalMode: event.target.value });
                    void persistRoute(index, { toolApprovalMode: event.target.value });
                }}>
                            {BOT_TOOL_APPROVAL_MODES.map((mode) => (<option key={mode || "inherit"} value={mode}>{t(`settings.botToolApprovalMode.${mode || "inherit"}` as DictKey)}</option>))}
                          </select>
                        </label>
                      </div>
                    </div>))}
                </div>)}
              <button type="button" className="btn btn--secondary btn--small bot-route-add" disabled={busy} onClick={addRoute}>
                {t("settings.botAddRoute")}
              </button>
            </div>
          </details>
        </div>
      </details>
    </div>);
}
