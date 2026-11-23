// "Accept all like this" (./walkthrough.tsx, G): every open card whose
// suggestion shares the shown card's batch key (./walk.ts batchOf), only
// where that is measured or repeats the person's own choice (./advice.ts).
// The person sees each card, all ticked, unticks any, then confirms; the
// choices go a few at a time with progress and Stop, the refusals are listed
// with the backend's reason, and the whole batch can be taken back at once.
import { useRef, useState } from "react";
import type { FoundryReviewChoice } from "../../../shared/wfe";
import { Button, DataGrid, Dialog, formatNumber, plural } from "../../kit";
import { useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import type { Advice } from "./advice";
import type { QueueCard } from "./queue";
import { cardLine } from "./review-card";
import { batchReason } from "./walk";

/** What became of one card of a batch. */
export type BatchResult = { key: string; failed?: string; stopped?: boolean };

type Stage =
  | { kind: "choose"; off: ReadonlySet<string> }
  | { kind: "sending"; done: number; total: number }
  // `meanwhile`: cards ticked that were decided or left the walk while the
  // list was read, so not sent.
  | { kind: "done"; results: readonly BatchResult[]; meanwhile: number }
  | { kind: "undone" };

/** What a batch's confirm says, by the choice its cards share. */
const verbOf = (c: FoundryReviewChoice | undefined) =>
  c?.release
    ? "Approve"
    : c?.resolve?.notInData
      ? "Mark not in data"
      : c?.resolve
        ? "Link"
        : "Accept";

export function BatchDialog({
  items,
  cards,
  advice,
  label,
  drawn,
  run,
  undo,
  onClose,
}: {
  items: readonly { key: string; choice: FoundryReviewChoice }[];
  cards: ReadonlyMap<string, QueueCard>;
  // The shown card's suggestion, which every item shares.
  advice: Advice;
  // The suggested choice's words, as the card shows them.
  label?: string;
  // How many cards like these are spot checks, which the batch leaves out.
  drawn: number;
  // Sends the ticked cards the walk still takes: a result for each sent.
  run: (
    keys: readonly string[],
    signal: AbortSignal,
    onProgress: (done: number) => void,
  ) => Promise<BatchResult[]>;
  undo: () => Promise<void>;
  onClose: () => void;
}) {
  const { id: ws } = useWorkspace();
  const docName = useDocumentNames(ws, "A document");
  const [stage, setStage] = useState<Stage>({ kind: "choose", off: new Set() });
  const [undoing, setUndoing] = useState(false);
  const stop = useRef<AbortController>(undefined);
  const verb = verbOf(items[0]?.choice);
  const rows = items.flatMap((it) => {
    const c = cards.get(it.key);
    return c && (c.kind === "name" || c.kind === "claim" || c.kind === "value")
      ? [
          {
            key: it.key,
            what: cardLine(c),
            document: docName(c.review.documentId),
          },
        ]
      : [];
  });
  const lineOf = (key: string) => rows.find((r) => r.key === key)?.what;
  const start = async (keys: readonly string[]) => {
    const controller = new AbortController();
    stop.current = controller;
    setStage({ kind: "sending", done: 0, total: keys.length });
    const results = await run(keys, controller.signal, (done) =>
      setStage({ kind: "sending", done, total: keys.length }),
    );
    setStage({
      kind: "done",
      results,
      meanwhile: keys.length - results.length,
    });
  };
  const sending = stage.kind === "sending";
  return (
    <Dialog
      title="Accept all like this"
      description={
        stage.kind === "choose"
          ? `${batchReason(advice, label, rows.length) ?? ""} Untick any you want to read yourself.`.trim()
          : undefined
      }
      size="lg"
      // While sending, closing stops what hasn't gone yet.
      onClose={sending ? () => stop.current?.abort() : onClose}
      footer={
        stage.kind === "choose" ? (
          <>
            <Button onClick={onClose}>Cancel</Button>
            <Button
              variant="primary"
              data-autofocus
              disabled={stage.off.size === rows.length}
              onClick={() =>
                void start(
                  rows.filter((r) => !stage.off.has(r.key)).map((r) => r.key),
                )
              }
            >
              {verb} {formatNumber(rows.length - stage.off.size)}
            </Button>
          </>
        ) : stage.kind === "sending" ? (
          <Button onClick={() => stop.current?.abort()}>Stop</Button>
        ) : (
          <>
            {stage.kind === "done" &&
              stage.results.some((r) => !r.failed && !r.stopped) && (
                <Button
                  pending={undoing}
                  onClick={async () => {
                    setUndoing(true);
                    await undo();
                    setUndoing(false);
                    setStage({ kind: "undone" });
                  }}
                >
                  Undo these{" "}
                  {formatNumber(
                    stage.results.filter((r) => !r.failed && !r.stopped).length,
                  )}
                </Button>
              )}
            <Button variant="primary" data-autofocus onClick={onClose}>
              Done
            </Button>
          </>
        )
      }
    >
      {stage.kind === "choose" && (
        <div className="on-stack">
          {drawn > 0 && (
            <p role="note" className="on-muted">
              {drawn === 1
                ? "1 more like these is a spot check: drawn at random to measure how often claims scored like this are right, so you decide it on its own at the end."
                : `${formatNumber(drawn)} more like these are spot checks: drawn at random to measure how often claims scored like this are right, so you decide each on its own at the end.`}
            </p>
          )}
          <DataGrid
            label="Cards to accept"
            rows={rows}
            getRowKey={(r) => r.key}
            maxHeight={360}
            columns={[
              {
                key: "tick",
                label: "Include",
                width: 84,
                sortable: false,
                // Ticks, not text: no type icon in its head.
                className: "rv-tick",
                render: (r) => (
                  <input
                    type="checkbox"
                    aria-label="Include this card"
                    checked={!stage.off.has(r.key)}
                    onChange={(e) => {
                      const off = new Set(stage.off);
                      if (e.target.checked) off.delete(r.key);
                      else off.add(r.key);
                      setStage({ kind: "choose", off });
                    }}
                  />
                ),
              },
              { key: "what", label: "Card", width: 360 },
              { key: "document", label: "Document", width: 220 },
            ]}
          />
        </div>
      )}
      {stage.kind === "sending" && (
        <div className="on-stack">
          <p role="status">
            Accepting {formatNumber(stage.done)} of {formatNumber(stage.total)}
          </p>
          <div className="rv-meter" aria-hidden>
            <span className="rv-meter-done" style={{ flexGrow: stage.done }} />
            <span
              className="rv-meter-left"
              style={{ flexGrow: stage.total - stage.done }}
            />
          </div>
        </div>
      )}
      {stage.kind === "done" && (
        <div className="on-stack">
          <p role="status">
            Saved{" "}
            {plural(
              stage.results.filter((r) => !r.failed && !r.stopped).length,
              "choice",
            )}
            {stage.results.some((r) => r.stopped) &&
              `; ${plural(stage.results.filter((r) => r.stopped).length, "card was", "cards were")} stopped before being sent and ${stage.results.filter((r) => r.stopped).length === 1 ? "is" : "are"} still to decide`}
            {stage.meanwhile > 0 &&
              `; ${plural(stage.meanwhile, "card was", "cards were")} decided meanwhile, or left the walk, and ${stage.meanwhile === 1 ? "isn’t" : "aren’t"} sent`}
            .
          </p>
          {stage.results.some((r) => r.failed) && (
            <>
              <p>These couldn’t be saved, and are back in the walk:</p>
              <ul className="rv-failed" aria-label="Not saved">
                {stage.results.flatMap((r) =>
                  r.failed
                    ? [
                        <li key={r.key}>
                          {lineOf(r.key) ?? "A card"}: {r.failed}
                        </li>,
                      ]
                    : [],
                )}
              </ul>
            </>
          )}
        </div>
      )}
      {stage.kind === "undone" && (
        <p role="status">The batch is taken back: its cards are open again.</p>
      )}
    </Dialog>
  );
}
