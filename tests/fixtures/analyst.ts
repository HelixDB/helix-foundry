// Fixture-model replies for analyst prompts, used by the browser acceptance
// server and unit tests through referenceReply. Owned by the analyst area.
// Return undefined for prompts this area does not handle.
export function analystReply(system: string, context: any): unknown {
  const dataset = context.datasets?.find(
    (d: any) => d.name === "Evidence report Accounts",
  );
  if (
    system.includes("Answer the question with one read-only SQL query") &&
    dataset
  ) {
    return {
      title: "Saved row count",
      sql: `SELECT count(*) AS records FROM "${String(dataset.table).replaceAll('"', '""')}"`,
      inputs: [dataset.id],
      assumptions: ["All saved records are counted"],
    };
  }
  if (
    system.includes("Summarise this query result") &&
    dataset &&
    context.result
  )
    return {
      summary: "The saved query result is shown below.",
      highlights: [],
    };
  return undefined;
}
