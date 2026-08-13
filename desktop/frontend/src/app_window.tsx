import { useCallback, useEffect, useRef, useState } from "react";
import { Copy as RestoreIcon, Minus, Square, X } from "lucide-react";
import { app } from "./lib/bridge";
export const CHAT_MIN_WIDTH = 400;
export const CHAT_COMFORT_MIN_WIDTH = 560;
export const WORKSPACE_RESIZER_WIDTH = 8;
export type DesktopPlatform = "darwin" | "windows" | "linux";
const MACOS_WORKBENCH_TITLEBAR_HEIGHT = 46;
export function isMacOSWorkbenchSidebarTitlebar(target: HTMLElement | null, clientY: number, platform: DesktopPlatform): boolean {
    if (platform !== "darwin")
        return false;
    const sidebar = target?.closest(".sidebar--workbench");
    if (!(sidebar instanceof HTMLElement))
        return false;
    const offsetY = clientY - sidebar.getBoundingClientRect().top;
    return offsetY >= 0 && offsetY < MACOS_WORKBENCH_TITLEBAR_HEIGHT;
}
export function useWindowsMaximised(enabled: boolean): readonly [
    boolean,
    () => void
] {
    const [maximised, setMaximised] = useState(false);
    const syncGenerationRef = useRef(0);
    const syncMaximised = useCallback(() => {
        if (!enabled)
            return;
        const generation = ++syncGenerationRef.current;
        void app.IsMainWindowMaximised()
            .then((value) => {
            if (generation === syncGenerationRef.current)
                setMaximised(value);
        })
            .catch(() => {
            if (generation === syncGenerationRef.current)
                setMaximised(false);
        });
    }, [enabled]);
    useEffect(() => {
        if (!enabled) {
            syncGenerationRef.current += 1;
            setMaximised(false);
            return;
        }
        syncMaximised();
        window.addEventListener("resize", syncMaximised);
        window.addEventListener("focus", syncMaximised);
        return () => {
            syncGenerationRef.current += 1;
            window.removeEventListener("resize", syncMaximised);
            window.removeEventListener("focus", syncMaximised);
        };
    }, [enabled, syncMaximised]);
    return [maximised, syncMaximised] as const;
}
export function WindowsWindowControls({ maximised, syncMaximised, }: {
    maximised: boolean;
    syncMaximised: () => void;
}) {
    const toggleMaximise = useCallback(() => {
        void app.ToggleMaximiseMainWindow()
            .then(() => window.setTimeout(syncMaximised, 80))
            .catch(() => undefined);
    }, [syncMaximised]);
    return (<div className="windows-window-controls" aria-label="Window controls">
      <button className="windows-window-control windows-window-control--minimize" type="button" aria-label="Minimize window" title="Minimize" onClick={() => void app.MinimiseMainWindow()}>
        <Minus size={13} strokeWidth={1.9}/>
      </button>
      <button className="windows-window-control windows-window-control--maximize" type="button" aria-label="Maximize or restore window" aria-pressed={maximised} title={maximised ? "Restore" : "Maximize"} onClick={toggleMaximise}>
        {maximised ? <RestoreIcon size={12} strokeWidth={1.75}/> : <Square size={11} strokeWidth={1.8}/>}
      </button>
      <button className="windows-window-control windows-window-control--close" type="button" aria-label="Close window" title="Close" onClick={() => void app.CloseMainWindow()}>
        <X size={13} strokeWidth={1.9}/>
      </button>
    </div>);
}
export function normalizeDesktopPlatform(value: string): DesktopPlatform {
    if (value === "darwin" || value === "windows")
        return value;
    return "linux";
}
export function browserPlatformOverride(): DesktopPlatform | null {
    if (typeof window === "undefined" || window.runtime)
        return null;
    const value = new URLSearchParams(window.location.search).get("platform");
    if (value === "darwin" || value === "windows" || value === "linux")
        return value;
    return null;
}

