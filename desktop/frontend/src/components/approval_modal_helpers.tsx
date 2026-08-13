import { type Translator } from "../lib/i18n";
import type { ToolApprovalMode } from "../lib/types";
export function requiresFreshHumanApproval(tool: string): boolean {
    return tool === "remember" || tool === "forget" || tool === "exit_plan_mode" || tool === "sandbox_escape" || tool === "config_write";
}
export const APPROVAL_MODE_RANK: Record<ToolApprovalMode, number> = { ask: 0, auto: 1, yolo: 2 };
export function approvalToolLabel(tool: string, t: Translator): string {
    switch (tool) {
        case "bash":
            return t("approval.toolLabelBash");
        case "edit_file":
            return t("approval.toolLabelEditFile");
        case "write_file":
            return t("approval.toolLabelWriteFile");
        case "multi_edit":
            return t("approval.toolLabelMultiEdit");
        case "move_file":
            return t("approval.toolLabelMoveFile");
        case "web_fetch":
            return t("approval.toolLabelWebFetch");
        case "run_skill":
            return t("approval.toolLabelRunSkill");
        case "remember":
            return t("approval.toolLabelRemember");
        case "forget":
            return t("approval.toolLabelForget");
        case "sandbox_escape":
            return t("approval.toolLabelSandboxEscape");
        case "config_write":
            return t("approval.toolLabelConfigWrite");
        case "plan_mode_read_only_command":
            return t("approval.toolLabelPlanModeReadOnly");
        case "exit_plan_mode":
            return t("approval.toolLabelExitPlan");
        default:
            return tool;
    }
}
const sandboxEscapeEnglishSubjectFallback = "run shell command unconfined once";
const sandboxEscapeEnglishSubjectPrefix = "run unconfined once: ";
const configWriteEnglishSubjectPrefix = "write Reasonix config: ";
const planModeBashEnglishSubject = /^Trust (.+) as a read-only command prefix while planning\r?\nCommand: ([\s\S]+)$/;
export function localizeApprovalSubject(tool: string, subject: string, t: Translator): string {
    const trimmed = subject.trim();
    if (tool === "sandbox_escape") {
        if (!trimmed || trimmed === sandboxEscapeEnglishSubjectFallback)
            return t("approval.sandboxEscapeSubjectFallback");
        const localizedPrefix = t("approval.sandboxEscapeSubjectPrefix");
        if (trimmed.startsWith(sandboxEscapeEnglishSubjectPrefix)) {
            return trimmed.slice(sandboxEscapeEnglishSubjectPrefix.length).trim() || t("approval.sandboxEscapeSubjectFallback");
        }
        if (localizedPrefix !== sandboxEscapeEnglishSubjectPrefix && trimmed.startsWith(localizedPrefix)) {
            return trimmed.slice(localizedPrefix.length).trim() || t("approval.sandboxEscapeSubjectFallback");
        }
        return trimmed;
    }
    if (tool === "config_write") {
        if (trimmed.startsWith(configWriteEnglishSubjectPrefix)) {
            return `${t("approval.configWriteSubjectPrefix")}${trimmed.slice(configWriteEnglishSubjectPrefix.length)}`;
        }
        return trimmed;
    }
    if (tool === "remember") {
        return trimmed
            .replace(/^Save\/update memory/, t("approval.memorySaveUpdate"))
            .replace(/\bbody: /g, `${t("approval.memoryBodyLabel")}: `);
    }
    if (tool === "forget" && trimmed.startsWith("Archive memory ")) {
        return `${t("approval.memoryArchivePrefix")}${trimmed.slice("Archive memory ".length)}`;
    }
    const bashTrust = trimmed.match(planModeBashEnglishSubject);
    if (bashTrust) {
        return t("approval.planModeBashTrustSubject", { prefix: bashTrust[1] ?? "", command: bashTrust[2] ?? "" });
    }
    return trimmed;
}
export function localizeApprovalReason(tool: string, reason: string | undefined, t: Translator): string {
    let trimmed = reason?.trim() ?? "";
    let matchedRule = "";
    const matchedRulePrefix = "Matched permission rule: ";
    if (trimmed.startsWith(matchedRulePrefix)) {
        const [ruleLine, ...remainingLines] = trimmed.split(/\r?\n/);
        matchedRule = t("approval.matchedPermissionRule", { rule: ruleLine.slice(matchedRulePrefix.length).trim() });
        trimmed = remainingLines.join("\n").trim();
    }
    let localized = trimmed;
    if (tool === "bash" && trimmed.includes("nested or indirect shell execution")) {
        localized = t("approval.dynamicBashReason");
    }
    if (tool === "config_write") {
        localized = !trimmed || trimmed.includes("Reasonix-managed configuration file") ? t("approval.configWriteReason") : trimmed;
    }
    if (tool === "sandbox_escape") {
        if (trimmed.includes("could not wrap this command") || trimmed.includes("does not provide an OS-level Bash sandbox")) {
            localized = t("approval.sandboxEscapeWrapReason");
        }
        else if (trimmed.includes("failed while starting this command") ||
            trimmed.includes("could not start this command") ||
            trimmed.includes("Run this command unconfined once?")) {
            localized = t("approval.sandboxEscapeRuntimeReason");
        }
        else {
            localized ||= t("approval.sandboxEscapeRuntimeReason");
        }
    }
    return [matchedRule, localized].filter(Boolean).join(" ");
}
export function localizePlanModeApprovalReason(tool: string, reason: string, t: Translator): string {
    if (tool === "plan_mode_read_only_command" && reason.includes("built-in read-only set")) {
        return t("approval.planModeBashTrustReason");
    }
    return reason;
}
export type DecisionAction = {
    key: string;
    label: string;
    desc: string;
    tone?: "default" | "danger";
    primary?: boolean;
    // Plan revision and plan guidance open inline editors instead of submitting.
    // Other recovery actions use direct-click submit (no select-then-confirm).
    kind: "submit" | "toggle-revision" | "toggle-guidance" | "direct";
    run?: () => void;
};
export const RECOVERY_FEEDBACK_MAX = 1000;
export function recoveryReasonText(changeKind: string | undefined, fallback: string | undefined, t: Translator): string {
    switch ((changeKind ?? "").toLowerCase()) {
        case "risk":
            return t("approval.recoveryReasonRisk");
        case "scope":
            return t("approval.recoveryReasonScope");
        case "strategy":
            return t("approval.recoveryReasonStrategy");
        case "uncertain":
        case "same_strategy":
            return t("approval.recoveryReasonUncertain");
        default:
            return fallback?.trim() || t("approval.recoveryReasonUncertain");
    }
}
type PlanLine = {
    key: string;
    text: string;
};
type PlanDelta = {
    removed: string[];
    added: string[];
};
function planLines(raw: string | undefined): PlanLine[] {
    return (raw ?? "")
        .split(/\r?\n/)
        .map((line) => line.replace(/\s+\[[^\]\r\n]+\]\s*$/, "").trimEnd())
        .filter((line) => line.trim() !== "")
        .map((line) => {
        const match = line.match(/^(\s*)(?:\d+\.\s*)?(.*)$/);
        const nested = (match?.[1].length ?? 0) > 0;
        const body = (match?.[2] ?? line).replace(/\s+/g, " ").trim();
        return { key: `${nested ? 1 : 0}:${body}`, text: `${nested ? "  " : ""}${body}` };
    });
}
// LCS keeps unchanged steps out of the card and turns additions, removals, and
// reordering into a compact plan-level delta. Status suffixes are ignored.
export function planDelta(beforeRaw: string | undefined, afterRaw: string | undefined): PlanDelta | null {
    const before = planLines(beforeRaw);
    const after = planLines(afterRaw);
    if (before.length === 0 || after.length === 0)
        return null;
    const dp = Array.from({ length: before.length + 1 }, () => Array<number>(after.length + 1).fill(0));
    for (let i = before.length - 1; i >= 0; i -= 1) {
        for (let j = after.length - 1; j >= 0; j -= 1) {
            dp[i][j] = before[i].key === after[j].key
                ? dp[i + 1][j + 1] + 1
                : Math.max(dp[i + 1][j], dp[i][j + 1]);
        }
    }
    const removed: string[] = [];
    const added: string[] = [];
    let i = 0;
    let j = 0;
    while (i < before.length && j < after.length) {
        if (before[i].key === after[j].key) {
            i += 1;
            j += 1;
        }
        else if (dp[i + 1][j] >= dp[i][j + 1]) {
            removed.push(before[i].text);
            i += 1;
        }
        else {
            added.push(after[j].text);
            j += 1;
        }
    }
    while (i < before.length)
        removed.push(before[i++].text);
    while (j < after.length)
        added.push(after[j++].text);
    return removed.length > 0 || added.length > 0 ? { removed, added } : null;
}

