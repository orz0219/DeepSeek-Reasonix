import { useEffect, useState } from "react";
import { RefreshCw } from "lucide-react";
import { asArray } from "../lib/array";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
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

