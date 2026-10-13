// Fixture-model replies for analyst prompts, used by the browser acceptance
// server and unit tests through referenceReply. Owned by the analyst area.
// Return undefined for prompts this area does not handle.
//
// Questions are answered; one about orders counts them per customer from the
// Orders dataset, and its summary names the leading customer from the result
// rows, as a model would.
export function analystReply(system: string, context: any): unknown {
  if (system.includes("Choose answer for questions"))
    return { intent: "answer" };
  const signed = signedReply(system, context);
  if (signed !== undefined) return signed;
  const orders = context.datasets?.find((d: any) => /^orders$/i.test(d.name));
  if (system.includes("Answer the question with one read-only SQL") && orders)
    return {
      title: "Customers by order count",
      sql: `SELECT customer, count(*) AS order_count FROM "${orders.table}" GROUP BY customer ORDER BY order_count DESC, customer`,
      inputs: [orders.id],
      assumptions: ["Every order counts, whatever its status."],
    };
  if (system.includes("Summarise this query result")) {
    const [top, next] = context.result?.rows || [];
    if (!top?.customer) return undefined;
    return {
      summary:
        `Customer ${top.customer} has the most orders. They placed ${top.order_count} orders` +
        (next
          ? `, ${top.order_count - next.order_count} more than customer ${next.customer}.`
          : "."),
      highlights: [],
    };
  }
  return undefined;
}

// Local model boundary only: the actual acceptance worker executes this SQL.
function signedReply(system: string, context: any): unknown {
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
