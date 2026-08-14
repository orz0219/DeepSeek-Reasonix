import { useCallback, useEffect, useState } from "react";
import { RefreshCw } from "lucide-react";
import { asArray } from "../lib/array";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import type { HookConfigView, HooksSettingsView, SettingsView } from "../lib/types";
import { Tooltip } from "./Tooltip";
import { SectionProps, SettingsSection, SettingsField } from "./SettingsPanel";
export function KeyField({ apiKeyEnv, busy, keySet = false, onSet, }: {
    apiKeyEnv: string;
    busy: boolean;
    keySet?: boolean;
    onSet: (apiKeyEnv: string, value: string) => Promise<void>;
}) {
    const t = useT();
    const [val, setVal] = useState("");
    if (!apiKeyEnv)
        return null;
    return (<div className="set-key">
      <input className="mem-input" type="password" placeholder={t(keySet ? "settings.updateKey" : "settings.setKey", { env: apiKeyEnv })} value={val} onChange={(e) => setVal(e.target.value)}/>
      <button className="btn btn--small" disabled={busy || !val.trim()} onClick={() => {
            void onSet(apiKeyEnv, val.trim());
            setVal("");
        }}>
        {t(keySet ? "settings.updateKeyAction" : "settings.saveKey")}
      </button>
    </div>);
}
export function PermissionsSection({ s, busy, apply }: SectionProps) {
    const t = useT();
    return (<>
    <SettingsSection title={t("settings.permissions")} description={t("settings.permissionsModeHint")}>
      <SettingsField label={t("settings.writerMode")}>
        <select className="mem-select set-grow" value={s.permissions.mode} disabled={busy} onChange={(e) => void apply(() => app.SetPermissionMode(e.target.value))}>
          <option value="ask">{t("settings.modeAsk")}</option>
          <option value="allow">{t("settings.modeAllow")}</option>
          <option value="deny">{t("settings.modeDeny")}</option>
        </select>
      </SettingsField>
    </SettingsSection>
    <SettingsSection title={t("settings.permissionRules")} description={t("settings.ruleForm")}>
      <div className="set-rules-grid">
        {(["deny", "ask", "allow"] as const).map((list) => (<RuleList key={list} list={list} rules={s.permissions[list]} busy={busy} onAdd={async (rule) => { await apply(() => app.AddPermissionRule(list, rule)); }} onRemove={async (rule) => { await apply(() => app.RemovePermissionRule(list, rule)); }}/>))}
      </div>
    </SettingsSection>
    </>);
}
function RuleList({ list, rules, busy, onAdd, onRemove, }: {
    list: string;
    rules: string[];
    busy: boolean;
    onAdd: (rule: string) => Promise<void>;
    onRemove: (rule: string) => Promise<void>;
}) {
    const t = useT();
    const [draft, setDraft] = useState("");
    const add = () => {
        const r = draft.trim();
        if (r) {
            void onAdd(r);
            setDraft("");
        }
    };
    return (<div className="set-rules">
      <div className="set-rules__head">
        <div className="set-rules__label">{ruleListLabel(list, t)}</div>
        {ruleListHint(list, t) && <div className="set-rules__hint">{ruleListHint(list, t)}</div>}
      </div>
      <div className="set-rules__chips">
        {rules.length === 0 && <span className="mem-empty">{t("common.none")}</span>}
        {rules.map((r) => (<span className="set-rule" key={r}>
            <span className="set-rule__text" title={r}>{r}</span>
            <Tooltip label={t("common.delete")}>
              <button className="set-rule__x" disabled={busy} onClick={() => void onRemove(r)}>
                ✕
              </button>
            </Tooltip>
          </span>))}
      </div>
      <div className="set-rules__add">
        <input className="mem-input" placeholder={t("settings.addRule", { list })} value={draft} onChange={(e) => setDraft(e.target.value)} onKeyDown={(e) => {
            if (e.key === "Enter")
                add();
        }}/>
        <button className="btn btn--small" disabled={busy || !draft.trim()} onClick={add}>
          {t("common.add")}
        </button>
      </div>
    </div>);
}
function ruleListLabel(list: string, t: ReturnType<typeof useT>): string {
    switch (list) {
        case "deny":
            return t("settings.ruleDeny");
        case "ask":
            return t("settings.ruleAsk");
        case "allow":
            return t("settings.ruleAllow");
        case "allow_write":
            return t("settings.ruleAllowWrite");
        default:
            return list;
    }
}
function ruleListHint(list: string, t: ReturnType<typeof useT>): string {
    switch (list) {
        case "deny":
            return t("settings.ruleDenyHint");
        case "ask":
            return t("settings.ruleAskHint");
        case "allow":
            return t("settings.ruleAllowHint");
        default:
            return "";
    }
}
type HookScope = "global" | "project";
export function HooksSection({ onChanged }: {
    onChanged: (settings?: SettingsView | null) => void;
}) {
    const t = useT();
    const [scope, setScope] = useState<HookScope>("global");
    const [view, setView] = useState<HooksSettingsView | null>(null);
    const [jsonText, setJsonText] = useState("");
    const [jsonMessage, setJsonMessage] = useState<string | null>(null);
    const [jsonError, setJsonError] = useState<string | null>(null);
    const [pathMessage, setPathMessage] = useState<string | null>(null);
    const [busy, setBusy] = useState(false);
    const [err, setErr] = useState<string | null>(null);
    const load = useCallback(async (nextScope: HookScope) => {
        setBusy(true);
        setErr(null);
        try {
            const next = normalizeHooksSettingsView(await app.HooksSettings(nextScope), nextScope);
            setView(next);
            setJsonText(formatHooksJSON(next.hooks, next.events));
            setJsonMessage(null);
            setJsonError(null);
            setPathMessage(null);
        }
        catch (e) {
            setErr(String((e as Error)?.message ?? e));
            setView(null);
            setJsonText("");
            setJsonMessage(null);
            setJsonError(null);
            setPathMessage(null);
        }
        finally {
            setBusy(false);
        }
    }, []);
    useEffect(() => {
        void load(scope);
    }, [load, scope]);
    const parseHooksEditorJSON = (raw = jsonText): {
        hooks: HookConfigView[];
        text: string;
    } | null => {
        try {
            const hooks = parseHooksJSON(raw, view?.events ?? [], t);
            const text = formatHooksJSON(hooks, view?.events ?? []);
            setJsonText(text);
            setJsonError(null);
            return { hooks, text };
        }
        catch (e) {
            setJsonError(t("settings.hooksJsonInvalid", { error: String((e as Error)?.message ?? e) }));
            setJsonMessage(null);
            return null;
        }
    };
    const copyHooksJSON = async () => {
        const parsed = parseHooksEditorJSON();
        if (!parsed)
            return;
        try {
            await navigator.clipboard?.writeText(parsed.text);
            setJsonMessage(t("settings.hooksJsonCopied"));
        }
        catch {
            setJsonMessage(t("settings.hooksJsonClipboardUnavailable"));
        }
    };
    const formatHooksEditorJSON = (raw = jsonText) => {
        const parsed = parseHooksEditorJSON(raw);
        if (parsed)
            setJsonMessage(t("settings.hooksJsonFormatted"));
    };
    const pasteHooksJSON = async () => {
        try {
            const raw = await navigator.clipboard?.readText();
            if (!raw)
                throw new Error(t("settings.hooksJsonClipboardEmpty"));
            setJsonText(raw);
            formatHooksEditorJSON(raw);
        }
        catch (e) {
            setJsonError(t("settings.hooksJsonPasteFailed", { error: String((e as Error)?.message ?? e) }));
            setJsonMessage(null);
        }
    };
    const copyHooksPath = async () => {
        const path = view?.path?.trim();
        if (!path) {
            setPathMessage(t("settings.hooksPathUnavailable"));
            return;
        }
        try {
            await navigator.clipboard?.writeText(path);
            setPathMessage(t("settings.hooksPathCopied"));
        }
        catch {
            setPathMessage(t("settings.hooksJsonClipboardUnavailable"));
        }
    };
    const save = async () => {
        setBusy(true);
        setErr(null);
        try {
            const parsed = parseHooksEditorJSON();
            if (!parsed)
                return;
            await app.SaveHooksSettingsForRoot(scope, view?.projectRoot?.trim() ?? "", parsed.hooks);
            await load(scope);
            onChanged();
        }
        catch (e) {
            setErr(String((e as Error)?.message ?? e));
        }
        finally {
            setBusy(false);
        }
    };
    return (<>
      {err && <div className="banner banner--error">{err}</div>}
      <SettingsSection title={t("settings.hooksScopeSection")} description={t("settings.hooksScopeHint")}>
        <SettingsField label={t("settings.hooksScopeField")}>
          <select name="hooks-scope" className="mem-select set-grow" value={scope} disabled={busy} onChange={(e) => setScope(e.target.value === "project" ? "project" : "global")}>
            <option value="global">{t("settings.hooksGlobal")}</option>
            <option value="project">{t("settings.hooksProject")}</option>
          </select>
        </SettingsField>
        <SettingsField label={t("settings.hooksPath")} hint={scope === "project" ? t("settings.hooksPathProjectHint") : t("settings.hooksPathGlobalHint")}>
          <div className="hooks-path-stack">
            <div className={`hooks-path-display${view?.path ? "" : " hooks-path-display--empty"}`}>
              <code className="hooks-path-display__value" title={view?.path || t("settings.hooksPathUnavailable")}>
                {view?.path || t("settings.hooksPathUnavailable")}
              </code>
              <button className="btn btn--small" disabled={busy || !view?.path} onClick={() => void copyHooksPath()}>{t("settings.hooksPathCopy")}</button>
            </div>
            {pathMessage && <div className="hooks-path-display__message">{pathMessage}</div>}
          </div>
        </SettingsField>
      </SettingsSection>

      <SettingsSection title={t("settings.hooks")} description={scope === "project" ? t("settings.hooksProjectHint") : t("settings.hooksGlobalHint")} actions={(<button className="btn btn--small btn--primary" disabled={busy} onClick={() => void save()}>{t("common.save")}</button>)}>
        {view && (<div className="hooks-json-panel">
            <div className="hooks-json-panel__head">
              <div>
                <div className="set-rules__label">{t("settings.hooksJsonTitle")}</div>
                <div className="set-rules__hint">{t("settings.hooksJsonHint")}</div>
              </div>
              <div className="hooks-json-panel__actions">
                <button className="btn btn--small" disabled={busy} onClick={() => void copyHooksJSON()}>{t("settings.hooksJsonCopy")}</button>
                <button className="btn btn--small" disabled={busy} onClick={() => void pasteHooksJSON()}>{t("settings.hooksJsonPaste")}</button>
                <button className="btn btn--small" disabled={busy || !jsonText.trim()} onClick={() => formatHooksEditorJSON()}>{t("settings.hooksJsonApply")}</button>
              </div>
            </div>
            <textarea name="hooks-json" className="mem-textarea hooks-json-panel__textarea" value={jsonText} disabled={busy} spellCheck={false} onChange={(e) => {
                setJsonText(e.target.value);
                setJsonMessage(null);
                setJsonError(null);
            }}/>
            {jsonError && <div className="hooks-json-panel__message hooks-json-panel__message--error">{jsonError}</div>}
            {jsonMessage && <div className="hooks-json-panel__message">{jsonMessage}</div>}
          </div>)}
        {!view && <div className="empty">{t("settings.loading")}</div>}
      </SettingsSection>
    </>);
}
function normalizeHooksSettingsView(view: HooksSettingsView, scope: HookScope): HooksSettingsView {
    const events = asArray(view?.events).filter(Boolean);
    return {
        scope: view?.scope === "project" ? "project" : scope,
        path: view?.path ?? "",
        projectRoot: view?.projectRoot ?? "",
        trusted: !!view?.trusted,
        events,
        hooks: asArray(view?.hooks).map(normalizeHookConfig).filter((h) => h.event),
    };
}
function formatHooksJSON(hooks: HookConfigView[], eventOrder: string[]): string {
    const grouped: Record<string, Array<Record<string, string | number>>> = {};
    const events = new Set(eventOrder);
    for (const hook of hooks.map(normalizeHookConfig).filter((h) => h.event)) {
        events.add(hook.event);
        const entry: Record<string, string | number> = { command: hook.command };
        if (hook.match)
            entry.match = hook.match;
        if (hook.description)
            entry.description = hook.description;
        if ((hook.timeout ?? 0) > 0)
            entry.timeout = hook.timeout ?? 0;
        if (hook.cwd)
            entry.cwd = hook.cwd;
        (grouped[hook.event] ||= []).push(entry);
    }
    const ordered: typeof grouped = {};
    for (const event of [...eventOrder, ...Array.from(events).sort()]) {
        if (grouped[event]?.length && !ordered[event])
            ordered[event] = grouped[event];
    }
    return JSON.stringify({ hooks: ordered }, null, 2);
}
function parseHooksJSON(raw: string, validEvents: string[], t: ReturnType<typeof useT>): HookConfigView[] {
    const trimmed = raw.trim();
    if (!trimmed)
        return [];
    let parsed: unknown;
    try {
        parsed = JSON.parse(trimmed);
    }
    catch (e) {
        throw new Error(String((e as Error)?.message ?? e));
    }
    if (Array.isArray(parsed)) {
        return parsed.map((item) => normalizeHookConfig(parseHookArrayItem(item, validEvents, t))).filter((h) => h.event);
    }
    if (!parsed || typeof parsed !== "object") {
        throw new Error(t("settings.hooksJsonExpectedObjectArray"));
    }
    const obj = parsed as Record<string, unknown>;
    const hooksValue = obj.hooks && typeof obj.hooks === "object" && !Array.isArray(obj.hooks) ? obj.hooks : obj;
    return flattenHooksMap(hooksValue as Record<string, unknown>, validEvents, t);
}
function parseHookArrayItem(item: unknown, validEvents: string[], t: ReturnType<typeof useT>): HookConfigView {
    if (!item || typeof item !== "object" || Array.isArray(item))
        throw new Error(t("settings.hooksJsonItemObject"));
    const obj = item as Record<string, unknown>;
    const event = stringField(obj, "event") || "PreToolUse";
    if (validEvents.length > 0 && !validEvents.includes(event))
        throw new Error(t("settings.hooksJsonUnknownEvent", { event }));
    return {
        event,
        match: stringField(obj, "match"),
        command: stringField(obj, "command"),
        description: stringField(obj, "description"),
        timeout: numberField(obj, "timeout"),
        cwd: stringField(obj, "cwd"),
    };
}
function flattenHooksMap(hooks: Record<string, unknown>, validEvents: string[], t: ReturnType<typeof useT>): HookConfigView[] {
    const valid = new Set(validEvents);
    const out: HookConfigView[] = [];
    for (const [event, value] of Object.entries(hooks)) {
        if (valid.size > 0 && !valid.has(event))
            throw new Error(t("settings.hooksJsonUnknownEvent", { event }));
        const items = Array.isArray(value) ? value : [value];
        for (const item of items) {
            if (!item || typeof item !== "object" || Array.isArray(item))
                throw new Error(t("settings.hooksJsonEventItemObject", { event }));
            const obj = item as Record<string, unknown>;
            out.push(normalizeHookConfig({
                event,
                match: stringField(obj, "match"),
                command: stringField(obj, "command"),
                description: stringField(obj, "description"),
                timeout: numberField(obj, "timeout"),
                cwd: stringField(obj, "cwd"),
            }));
        }
    }
    return out.filter((h) => h.event);
}
function stringField(obj: Record<string, unknown>, key: string): string {
    const value = obj[key];
    return typeof value === "string" ? value : "";
}
function numberField(obj: Record<string, unknown>, key: string): number {
    const value = obj[key];
    return typeof value === "number" && Number.isFinite(value) ? Math.floor(value) : 0;
}
function normalizeHookConfig(h: HookConfigView): HookConfigView {
    return {
        event: h.event || "PreToolUse",
        match: h.match ?? "",
        command: h.command ?? "",
        description: h.description ?? "",
        timeout: h.timeout && h.timeout > 0 ? Math.floor(h.timeout) : 0,
        cwd: h.cwd ?? "",
    };
}
function effectiveShellLabel(value: string, t: ReturnType<typeof useT>): string {
    switch (value) {
        case "git-bash": return t("settings.effectiveShellGitBash");
        case "pwsh": return t("settings.effectiveShellPwsh");
        case "powershell": return t("settings.effectiveShellPowershell");
        case "bash": return t("settings.effectiveShellBash");
        case "auto": return t("common.auto");
        default: return value.trim() || t("common.none");
    }
}
export function SandboxSection({ s, busy, apply, windows }: SectionProps & {
    windows: boolean;
}) {
    const t = useT();
    const sb = s.sandbox;
    const [root, setRoot] = useState(sb.workspaceRoot);
    const effectiveWriteRoots = asArray(sb.effectiveWriteRoots).filter((path) => String(path).trim());
    const effectiveShell = effectiveShellLabel(String(sb.effectiveShell || sb.shell || ""), t);
    const set = (next: Partial<typeof sb>) => apply(() => app.SetSandbox(next.bash ?? sb.bash, next.network ?? sb.network, next.workspaceRoot ?? sb.workspaceRoot, next.allowWrite ?? sb.allowWrite, next.shell ?? sb.shell));
    const reload = () => apply(() => app.ReloadSettings());
    return (<SettingsSection title={t("settings.sandboxTitle")} description={t("settings.sandboxBoundaryHint")} actions={<Tooltip label={t("settings.reloadSessionConfigHint")}>
          <button className="btn btn--small" disabled={busy} title={t("settings.reloadSessionConfigHint")} onClick={() => void reload()}>
            <RefreshCw size={14} aria-hidden="true"/>
            <span>{t("settings.reloadSessionConfig")}</span>
          </button>
        </Tooltip>}>
      <SettingsField label={t("settings.shellInterpreter")}>
        <select className="mem-select set-grow" value={sb.shell || "auto"} disabled={busy} onChange={(e) => void set({ shell: e.target.value })}>
          <option value="auto">{windows ? t("settings.shellAutoWindows") : t("settings.shellAuto")}</option>
          <option value="bash">{t("settings.shellBash")}</option>
          <option value="powershell">{t("settings.shellPowershell")}</option>
          <option value="pwsh">{t("settings.shellPwsh")}</option>
        </select>
      </SettingsField>
      <SettingsField label={t("settings.effectiveShell")}>
        <div className="settings-readonly-field">{effectiveShell}</div>
      </SettingsField>
      <SettingsField label={t("settings.bashSandbox")} hint={windows ? t("settings.bashUnavailableWindows") : undefined}>
        {/* Windows has no OS-level Bash backend and config.BashModeForGOOS fixes
            the effective value to off. Keep the control visibly immutable and
            omit enforce so the UI cannot imply a dormant capability. */}
        <select className="mem-select set-grow" value={windows ? "off" : sb.bash} disabled={busy || windows} onChange={(e) => void set({ bash: e.target.value })}>
          {!windows && <option value="enforce">{t("settings.bashEnforce")}</option>}
          <option value="off">{t("settings.bashOff")}</option>
        </select>
      </SettingsField>
      <SettingsField label={t("settings.allowNetwork")}>
        <label className="set-check set-check--inline">
          <input type="checkbox" checked={sb.network} disabled={busy} onChange={(e) => void set({ network: e.target.checked })}/>
          {t("settings.allowNetwork")}
        </label>
      </SettingsField>
      <SettingsField label={t("settings.workspaceRoot")}>
        <input className="mem-input set-grow" placeholder={t("settings.workspaceDefault")} value={root} disabled={busy} onChange={(e) => setRoot(e.target.value)} onBlur={() => root !== sb.workspaceRoot && void set({ workspaceRoot: root })}/>
      </SettingsField>
      <SettingsField label={t("settings.effectiveWriteRoots")} hint={t("settings.effectiveWriteRootsHint")} stacked>
        <div className="set-rules set-rules--readonly">
          <div className="set-rules__chips">
            {effectiveWriteRoots.length === 0 && <span className="mem-empty">{t("settings.noEffectiveWriteRoots")}</span>}
            {effectiveWriteRoots.map((path, index) => (<span className="set-rule set-rule--path" key={`${path}-${index}`}>
                {path}
              </span>))}
          </div>
        </div>
      </SettingsField>
      <RuleList list="allow_write" rules={sb.allowWrite} busy={busy} onAdd={async (d) => { await set({ allowWrite: [...sb.allowWrite, d] }); }} onRemove={async (d) => { await set({ allowWrite: sb.allowWrite.filter((x) => x !== d) }); }}/>
    </SettingsSection>);
}
// AboutSection shows the build version and the local configuration paths. The
// auto-updater and its privacy/update preferences were removed.
export function AboutSection({ configPath, shadowedByPath }: {
    configPath: string;
    shadowedByPath?: string;
}) {
    const t = useT();
    const [version, setVersion] = useState("");
    useEffect(() => {
        app.Version().then(setVersion).catch(() => { });
    }, []);
    return (<SettingsSection>
      <SettingsField className="settings-field--wide-copy" label={t("about.version")}>
        <div className="updates-control__version">{version || "…"}</div>
      </SettingsField>
      {configPath && (<Tooltip label={configPath} fill block className="mem-hint settings-config-path">
          {t("settings.config", { path: configPath })}
        </Tooltip>)}
      {shadowedByPath && (<Tooltip label={shadowedByPath} fill block className="mem-hint settings-config-path settings-config-path--shadowed">
          {t("settings.configShadowed", { path: shadowedByPath })}
        </Tooltip>)}
    </SettingsSection>);
}

