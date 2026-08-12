// Run: tsx src/__tests__/workspace-manual-refresh.test.tsx

import { JSDOM } from "jsdom";
import React, { useRef } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { useWorkspaceRefreshInvalidation } from "../lib/workspaceRefreshInvalidation";
import {
  createWorkspaceRefreshScheduler,
  type WorkspaceRefreshScheduler,
  type WorkspaceRefreshTimer,
} from "../lib/workspaceRefreshScheduler";
import type { WorkspaceRefreshSnapshot } from "../lib/workspaceRefreshStore";
import type { WorkspaceRevisions } from "../lib/types";

let passed = 0;
let failed = 0;

function eq(actual: unknown, expected: unknown, label: string) {
  if (JSON.stringify(actual) === JSON.stringify(expected)) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}\n`);
    failed += 1;
  }
}

const dom = new JSDOM("<!doctype html><html><body><div id=\"root\"></div></body></html>", {
  pretendToBeVisual: true,
});
(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
globalThis.window = dom.window as unknown as Window & typeof globalThis;
globalThis.document = dom.window.document;
globalThis.Node = dom.window.Node;
globalThis.Element = dom.window.Element;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Event = dom.window.Event;

class FakeTimer implements WorkspaceRefreshTimer {
  callbacks: Array<() => void> = [];
  schedule(callback: () => void): unknown { this.callbacks.push(callback); return callback; }
  cancel(handle: unknown): void { this.callbacks = this.callbacks.filter((callback) => callback !== handle); }
  fire(): void { const callback = this.callbacks.shift(); if (callback) callback(); }
}

const revisions = (content: number, workingTree: number): WorkspaceRevisions => ({
  content, tree: content, workingTree, gitMeta: 0, session: content,
});

const snapshot = (sequence: number, revs: WorkspaceRevisions): WorkspaceRefreshSnapshot => ({
  revisions: revs,
  changes: [],
  allPaths: true,
  source: "reconcile",
  watchState: "active",
  sequence,
});

function Harness({
  autoRefreshContent,
  snap,
  viewMode,
  refreshSelected,
  loadChangeDetail,
  loadWorkspaceChanges,
  timer,
}: {
  autoRefreshContent: boolean;
  snap: WorkspaceRefreshSnapshot;
  viewMode: "files" | "changed";
  refreshSelected: () => void;
  loadChangeDetail: () => void;
  loadWorkspaceChanges: () => void;
  timer: FakeTimer;
}) {
  const openDirsRef = useRef<Set<string>>(new Set([""]));
  const workingTreeSchedulerRef = useRef<WorkspaceRefreshScheduler | null>(null);
  const gitMetaSchedulerRef = useRef<WorkspaceRefreshScheduler | null>(null);
  if (workingTreeSchedulerRef.current === null) {
    workingTreeSchedulerRef.current = createWorkspaceRefreshScheduler(300, timer);
    gitMetaSchedulerRef.current = createWorkspaceRefreshScheduler(300, timer);
  }
  useWorkspaceRefreshInvalidation({
    commitHistoryOpen: false,
    filter: "",
    gitMetaSchedulerRef,
    loadChangeDetail,
    loadDir: () => undefined,
    loadGitHistory: () => undefined,
    loadWorkspaceChanges,
    open: true,
    openDirsRef,
    refreshSelected,
    selectedPath: "src/a.go",
    setSearchResults: () => undefined,
    viewMode,
    workingTreeSchedulerRef,
    workspaceRefresh: snap,
    workspaceScopeKey: "root",
    autoRefreshContent,
  });
  return null;
}

function renderHarness(props: Omit<Parameters<typeof Harness>[0], "timer"> & { timer: FakeTimer }) {
  const { timer, ...rest } = props;
  const container = document.getElementById("root") as HTMLElement;
  container.replaceChildren();
  const root = createRoot(container);
  act(() => {
    root.render(<Harness {...rest} timer={timer} />);
  });
  return {
    rerender(next: Omit<Parameters<typeof Harness>[0], "timer">) {
      act(() => {
        root.render(<Harness {...next} timer={timer} />);
      });
    },
    fireTimer() {
      act(() => {
        timer.fire();
      });
    },
    unmount() {
      act(() => {
        root.unmount();
      });
    },
  };
}

async function manualPreviewNeverAutoReloadsContent() {
  const timer = new FakeTimer();
  const calls = { refreshSelected: 0, changeDetail: 0, workspaceChanges: 0 };
  const base = {
    autoRefreshContent: false,
    snap: snapshot(1, revisions(1, 1)),
    viewMode: "files" as const,
    refreshSelected: () => { calls.refreshSelected += 1; },
    loadChangeDetail: () => { calls.changeDetail += 1; },
    loadWorkspaceChanges: () => { calls.workspaceChanges += 1; },
    timer,
  };
  const harness = renderHarness(base);

  harness.rerender({ ...base, snap: snapshot(2, revisions(2, 1)) });
  eq(calls.refreshSelected, 0, "manual mode must not auto-reload the file body on a content revision");
  harness.unmount();
}

async function manualPreviewSkipsAutoChangeDetailButKeepsListRefresh() {
  const timer = new FakeTimer();
  const calls = { refreshSelected: 0, changeDetail: 0, workspaceChanges: 0 };
  const base = {
    autoRefreshContent: false,
    snap: snapshot(1, revisions(1, 1)),
    viewMode: "changed" as const,
    refreshSelected: () => { calls.refreshSelected += 1; },
    loadChangeDetail: () => { calls.changeDetail += 1; },
    loadWorkspaceChanges: () => { calls.workspaceChanges += 1; },
    timer,
  };
  const harness = renderHarness(base);

  harness.rerender({ ...base, snap: snapshot(2, revisions(2, 2)) });
  harness.fireTimer();
  await Promise.resolve();
  await Promise.resolve();
  eq(calls.changeDetail, 0, "manual mode must not auto-reload the change detail");
  eq(calls.workspaceChanges, 1, "the change list itself still refreshes automatically");
  harness.unmount();
}

async function autoRefreshContentKeepsPriorBehavior() {
  const timer = new FakeTimer();
  const calls = { refreshSelected: 0, changeDetail: 0, workspaceChanges: 0 };
  const base = {
    autoRefreshContent: true,
    snap: snapshot(1, revisions(1, 1)),
    viewMode: "changed" as const,
    refreshSelected: () => { calls.refreshSelected += 1; },
    loadChangeDetail: () => { calls.changeDetail += 1; },
    loadWorkspaceChanges: () => { calls.workspaceChanges += 1; },
    timer,
  };
  const harness = renderHarness(base);

  harness.rerender({ ...base, snap: snapshot(2, revisions(2, 2)) });
  eq(calls.refreshSelected, 1, "auto mode still reloads the file body on a content revision");
  harness.fireTimer();
  await Promise.resolve();
  await Promise.resolve();
  eq(calls.changeDetail, 1, "auto mode still reloads the change detail");
  eq(calls.workspaceChanges, 1, "auto mode still refreshes the change list");
  harness.unmount();
}

await manualPreviewNeverAutoReloadsContent();
await manualPreviewSkipsAutoChangeDetailButKeepsListRefresh();
await autoRefreshContentKeepsPriorBehavior();
console.log(`ok  workspace manual-refresh invalidation (${passed} passed, ${failed} failed)`);
if (failed > 0) process.exit(1);
