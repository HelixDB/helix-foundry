import type {
  Bundle,
  DatasetProfile,
  Resource,
  ChatFollowup,
} from "../../../packages/shared/src/index.js";

// Schema-backed investigations, not claims or speculative business measures.
// No rows, joins, graph scans or model calls are needed to propose these.
export function chatFollowups(
  bundle: Bundle,
  datasets: Resource[],
  runs: Resource[],
): ChatFollowup[] {
  const latest = runs.at(-1);
  if (latest?.data.status !== "succeeded" || !latest.data.answer) return [];
  const answer = latest.data.answer;
  const used = new Set<string>(answer.inputs || []);
  const current = new Map(datasets.map((d) => [d.id, d]));
  // Don't suggest a continuation of an answer from replaced snapshots.
  if (
    !used.size ||
    [...used].some((id) => !current.get(id)?.data.activeVersion)
  )
    return [];
  if (
    !Array.isArray(answer.citations) ||
    [...used].some(
      (id) =>
        !answer.citations.some(
          (c: any) =>
            c.datasetId === id &&
            c.version === current.get(id)?.data.activeVersion,
        ),
    )
  )
    return [];
  const previous = new Set(
    runs.map((r) =>
      String(r.data.goal || "")
        .trim()
        .toLowerCase(),
    ),
  );
  const goal = String(latest.data.goal || "").toLowerCase();
  const choices: { item: ChatFollowup; rank: number }[] = [];
  for (const type of bundle.ontology) {
    if (!used.has(type.datasetId)) continue;
    const d = current.get(type.datasetId)!;
    const profile = d.data.profile as DatasetProfile | undefined;
    if (!profile?.rows || type.name.length > 80) continue;
    for (const property of type.properties) {
      const c = profile.columns.find((c) => c.name === property);
      if (
        !c ||
        c.sensitive ||
        c.primaryKey ||
        property === type.primaryKey ||
        c.name.length > 60 ||
        c.nulls >= profile.rows
      )
        continue;
      // Don't repeat a dimension already grouped in a successful chat answer.
      if (
        runs.some(
          (r) =>
            r.data.status === "succeeded" &&
            r.data.answer?.inputs?.includes(type.datasetId) &&
            /\bgroup\s+by\b/i.test(String(r.data.answer.sql || "")) &&
            r.data.answer.rows?.some((row: any) =>
              Object.hasOwn(row, property),
            ),
        )
      )
        continue;
      const temporal = /^(DATE|TIMESTAMP)/i.test(c.type);
      const categorical =
        /^(VARCHAR|TEXT|STRING|BOOL)/i.test(c.type) &&
        c.distinct > 1 &&
        c.distinct <= 100 &&
        c.distinct < profile.rows;
      if (!temporal && !categorical) continue;
      const label = temporal
        ? "How does it vary daily?"
        : "How does " + property.replaceAll("_", " ") + " vary?";
      if (label.length > 55 || label.split(/\s+/).length > 5) continue;
      const question = temporal
        ? `With the same filters and scope, how do ${type.name} counts vary daily by ${property}?`
        : `With the same filters and scope, how do ${type.name} counts vary by ${property}?`;
      if (previous.has(question.toLowerCase())) continue;
      choices.push({
        item: {
          id: `${type.datasetId}:${property}`,
          label,
          question,
          ontology: { type: type.name, datasetId: type.datasetId, property },
          version: d.data.activeVersion,
        },
        rank:
          (goal.includes(property.toLowerCase()) ? 2 : 0) +
          (goal.includes(type.name.toLowerCase()) ? 1 : 0),
      });
    }
  }
  choices.sort((a, b) => b.rank - a.rank || a.item.id.localeCompare(b.item.id));
  return [...new Map(choices.map((c) => [c.item.id, c.item])).values()].slice(
    0,
    3,
  );
}
