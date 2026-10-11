import type {
  Resource,
  ThreadEvidenceReport,
} from "../../../packages/shared/src/index.js";

const text = (value: unknown) => (typeof value === "string" ? value : "");
const strings = (value: unknown): string[] =>
  Array.isArray(value)
    ? value.filter((x): x is string => typeof x === "string")
    : [];

export function threadReport(
  scope: string,
  id: string,
  runs: Resource[],
): ThreadEvidenceReport {
  return {
    format: "helix-foundry.thread-evidence",
    version: 1,
    workspaceId: scope,
    threadId: id,
    title: text(runs[0]?.data.goal) || runs[0]?.name || "Analyst report",
    exportedAt: new Date().toISOString(),
    notice:
      "Saved results reflect the cited snapshots. Included rows are the saved answer preview; total and truncated describe its coverage. Narrative and assumptions remain model-authored.",
    turns: runs.map((run) => {
      const a = run.data.status === "succeeded" ? run.data.answer : undefined;
      const rows = Array.isArray(a?.rows) ? a.rows : [];
      const total =
        typeof a?.total === "number" &&
        Number.isSafeInteger(a.total) &&
        a.total >= rows.length
          ? a.total
          : null;
      return {
        id: run.id,
        question: text(run.data.goal) || run.name,
        status: text(run.data.status),
        createdAt: run.createdAt,
        updatedAt: run.updatedAt,
        ...(a && typeof a === "object"
          ? {
              answer: {
                title: text(a.title),
                sql: text(a.sql),
                summary: text(a.summary),
                highlights: strings(a.highlights),
                assumptions: strings(a.assumptions),
                inputs: strings(a.inputs),
                citations: (Array.isArray(a.citations) ? a.citations : [])
                  .filter((c: any) => c && typeof c === "object")
                  .map((c: any) => ({
                    datasetId: text(c.datasetId),
                    name: text(c.name),
                    version: text(c.version),
                  })),
                rows,
                total,
                includedRows: rows.length,
                truncated: total === null ? null : total > rows.length,
              },
            }
          : {}),
      };
    }),
  };
}

function block(value: string, language = "text") {
  let longest = 0;
  for (const match of value.matchAll(/`+/g))
    longest = Math.max(longest, match[0].length);
  const fence = "`".repeat(Math.max(3, longest + 1));
  return `${fence}${language}\n${value}\n${fence}`;
}

export function reportMarkdown(report: ThreadEvidenceReport) {
  const sections = [
    "# Analyst evidence report",
    report.notice,
    block(
      JSON.stringify(
        {
          workspaceId: report.workspaceId,
          threadId: report.threadId,
          title: report.title,
          exportedAt: report.exportedAt,
        },
        null,
        2,
      ),
      "json",
    ),
  ];
  for (const [index, turn] of report.turns.entries()) {
    sections.push(
      `## Question ${index + 1}`,
      block(turn.question),
      block(
        JSON.stringify(
          {
            id: turn.id,
            status: turn.status,
            createdAt: turn.createdAt,
            updatedAt: turn.updatedAt,
          },
          null,
          2,
        ),
        "json",
      ),
    );
    if (turn.answer) {
      const { sql, rows, ...evidence } = turn.answer;
      sections.push(
        "### Answer and snapshot evidence",
        block(JSON.stringify(evidence, null, 2), "json"),
        "### Executed SQL",
        block(sql, "sql"),
        "### Saved result rows",
        block(JSON.stringify(rows, null, 2), "json"),
      );
    }
  }
  return sections.join("\n\n") + "\n";
}
