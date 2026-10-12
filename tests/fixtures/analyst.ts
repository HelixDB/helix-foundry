// Local model boundary only: the actual acceptance worker executes this SQL.
export function analystReply(system: string, context: any): unknown {
  if (!context.goal?.startsWith("Signed breakdown ")) return undefined;
  const dataset = context.datasets?.find((d: any) => d.name === context.goal);
  if (system.includes("Answer the question") && dataset)
    return {
      title: context.goal,
      sql: `SELECT category, sum(profit) profit FROM "${dataset.table}" GROUP BY category ORDER BY category`,
      inputs: [dataset.id],
      assumptions: [],
    };
  if (system.includes("Summarise this query result"))
    return {
      summary:
        "The table and chart show the executed profit totals by category.",
      highlights: [],
    };
  return undefined;
}
