import { replaceAttachmentRefsForDisplay } from "./attachmentDisplay";
import type { Item } from "./useController";

export type QuestionAnchor = { id: string; text: string; turn: number; checkpointTurn?: number };

export interface TurnGroup {
  userItem: Item;
  assistantPreview: string;
  toolCount: number;
  startIdx: number;
  endIdx: number;
}

export interface StepGroup {
  items: Item[];
  isFinal: boolean;
  isComplete: boolean;
}

export function questionAnchorId(id: string): string {
  return `question-anchor-${id}`;
}

export function compactQuestionText(text: string): string {
  const cleaned = replaceAttachmentRefsForDisplay(text).replace(/\s+/g, " ").trim();
  if (cleaned.length <= 80) return cleaned;
  return cleaned.slice(0, 80);
}

export function questionTurnsById(questions: QuestionAnchor[]): Map<string, number> {
  const hasCheckpointTurns = questions.some((question) => question.checkpointTurn != null);
  const turns = new Map<string, number>();
  for (const question of questions) {
    if (question.checkpointTurn != null) {
      turns.set(question.id, question.checkpointTurn);
    } else if (!hasCheckpointTurns) {
      turns.set(question.id, question.turn);
    }
  }
  return turns;
}

export function lastQuestionTurn(questions: readonly QuestionAnchor[], turns: ReadonlyMap<string, number>): number | undefined {
  for (let i = questions.length - 1; i >= 0; i -= 1) {
    const turn = turns.get(questions[i].id);
    if (turn != null) return turn;
  }
  return undefined;
}

export function scrollVersion(items: Item[]): string {
  return items
    .map((it) => {
      switch (it.kind) {
        case "assistant":
          return `${it.id}:a:${it.streaming ? 1 : 0}`;
        case "tool":
          return `${it.id}:t:${it.status}`;
        default:
          return `${it.id}:${it.kind}`;
      }
    })
    .join("|");
}

