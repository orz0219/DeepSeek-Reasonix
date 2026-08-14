import { type ReactNode } from "react";
import { useT } from "../lib/i18n";
import type { SettingsTab, SettingsView } from "../lib/types";
import { Tooltip } from "./Tooltip";
import { toRef, normalizeProxyMode, normalizeCloseBehavior, normalizeDesktopLayoutStyle, desktopLayoutStyleLabel, closeBehaviorLabel, permissionModeLabel, sandboxModeLabel } from "./settings_normalize";
import { proxyModeLabel, modelProviderLabel } from "./settings_models";
export function SettingsPageShell({ s: _s, tab, children }: {
    s: SettingsView | null;
    tab: SettingsTab;
    busy: boolean;
    apply: (fn: () => Promise<unknown>) => Promise<boolean>;
    children: ReactNode;
}) {
    const t = useT();
    const descKey = `settings.pageDesc.${tab}` as keyof typeof import("../locales/en").en;
    const desc = t(descKey as any);
    return (<div className={`settings-page settings-page--${settingsPageKind(tab)} settings-page--${tab}`}>
      {tab !== "appearance" ? (<div className="settings-page__header">
          <h2 className="settings-page__title">{settingsTabPageTitle(tab, t)}</h2>
          {typeof desc === "string" && desc !== `settings.pageDesc.${tab}` && <p className="settings-page__desc">{desc}</p>}
        </div>) : null}
      {children}
    </div>);
}
function settingsPageKind(tab: SettingsTab): "form" | "manager" {
    switch (tab) {
        case "models":
        case "mcp":
        case "remote":
        case "skills":
        case "subagents":
        case "plugins":
        case "memory":
        case "appearance":
            return "manager";
        default:
            return "form";
    }
}
export function SettingsSection({ title, description, actions, children, }: {
    title?: ReactNode;
    description?: ReactNode;
    actions?: ReactNode;
    children: ReactNode;
}) {
    const hasHead = Boolean(title || description || actions);
    return (<section className="settings-section">
      {hasHead && (<div className="settings-section__head">
          <div>
            {title && <div className="settings-section__title">{title}</div>}
            {description && (<div className="settings-section__desc">
                <SettingsHint hint={description}/>
              </div>)}
          </div>
          {actions && <div className="settings-section__actions">{actions}</div>}
        </div>)}
      <div className="settings-section__body">{children}</div>
    </section>);
}
export function SettingsField({ label, hint, icon, children, className, stacked = false, }: {
    label: ReactNode;
    hint?: ReactNode;
    icon?: ReactNode;
    children: ReactNode;
    className?: string;
    stacked?: boolean;
}) {
    return (<div className={`settings-field${stacked ? " settings-field--stacked" : ""}${className ? ` ${className}` : ""}`}>
      <div className={`settings-field__copy${icon ? " settings-field__copy--icon" : ""}`}>
        {icon && <span className="settings-field__icon" aria-hidden="true">{icon}</span>}
        <div className="settings-field__copy-body">
          <div className="settings-field__label">{label}</div>
          {hint && (<div className="settings-field__hint">
              <SettingsHint hint={hint}/>
            </div>)}
        </div>
      </div>
      <div className="settings-field__control">{children}</div>
    </div>);
}
function SettingsHint({ hint }: {
    hint: ReactNode;
}) {
    if (typeof hint === "string" || typeof hint === "number") {
        const label = String(hint);
        return (<Tooltip label={label} fill block className="settings-field__hint-tooltip">
        <span className="settings-field__hint-line">{label}</span>
      </Tooltip>);
    }
    return hint;
}
function settingsTabPageTitle(id: SettingsTab, t: ReturnType<typeof useT>): string {
    switch (id) {
        case "mcp": return t("settings.tab.mcp");
        case "skills": return t("settings.tab.skills");
        case "plugins": return t("settings.tab.plugins");
        case "memory": return t("settings.tab.memory");
        case "diagnostics": return t("settings.tab.diagnostics");
        case "shortcuts": return t("settings.tab.shortcuts");
        default: return settingsTabLabel(id, t);
    }
}
export function settingsTabLabel(id: SettingsTab, t: ReturnType<typeof useT>): string {
    switch (id) {
        case "general":
            return t("settings.tab.general");
        case "models":
            return t("settings.tab.models");
        case "providers":
            return t("settings.tab.providers");
        case "mcp":
            return t("settings.tab.mcp");
        case "remote":
            return t("settings.tab.remote");
        case "skills":
            return t("settings.tab.skills");
        case "subagents":
            return t("settings.tab.subagents");
        case "plugins":
            return t("settings.tab.plugins");
        case "memory":
            return t("settings.tab.memory");
        case "hooks":
            return t("settings.tab.hooks");
        case "diagnostics":
            return t("settings.tab.diagnostics");
        case "shortcuts":
            return t("settings.tab.shortcuts");
        case "network":
            return t("settings.tab.network");
        case "permissions":
            return t("settings.tab.permissions");
        case "sandbox":
            return t("settings.tab.sandbox");
        case "appearance": return t("settings.tab.appearance");
        case "storage": return t("settings.tab.storage");
        case "about":
            return t("settings.tab.about");
    }
}
export function settingsTabMeta(id: SettingsTab, s: SettingsView, t: ReturnType<typeof useT>): string {
    switch (id) {
        case "models":
            return settingsModelMeta(s, t);
        case "general":
            return `${desktopLayoutStyleLabel(normalizeDesktopLayoutStyle(s.desktopLayoutStyle), t)} · ${closeBehaviorLabel(normalizeCloseBehavior(s.closeBehavior), t)}`;
        case "providers":
            return t("settings.providerCount", { n: s.providers.length });
        case "mcp":
            return t("caps.connectorsTab");
        case "remote":
            return t("remote.tabHint");
        case "skills":
            return t("settings.tabSub.skills");
        case "subagents":
            return t("subagents.tabHint");
        case "plugins":
            return t("settings.tabSub.plugins");
        case "memory":
            return t("settings.tabSub.memory");
        case "hooks":
            return t("settings.tabSub.hooks");
        case "diagnostics":
            return t("settings.tabSub.diagnostics");
        case "shortcuts":
            return t("settings.tabSub.shortcuts");
        case "network":
            return proxyModeLabel(normalizeProxyMode(s.network.proxyMode), t);
        case "permissions":
            return permissionModeLabel(s.permissions.mode, t);
        case "sandbox":
            return sandboxModeLabel(s.sandbox.bash, t);
        case "appearance": return t("settings.appearanceMeta");
        case "storage": return t("settings.storageMeta");
        case "about":
            return t("settings.aboutMeta");
    }
}
export function settingsModelMeta(s: SettingsView, t: ReturnType<typeof useT>): string {
    const ref = toRef(s.defaultModel, s);
    if (!ref)
        return t("common.none");
    if (!ref.includes("/"))
        return ref;
    const [provider, ...modelParts] = ref.split("/");
    const model = modelParts.join("/") || ref;
    const providerView = s.providers.find((p) => p.name === provider);
    return `${modelProviderLabel(provider, providerView, t)} · ${model}`;
}

