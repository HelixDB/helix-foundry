// The datasets a question is answered from. Every dataset's schema and sample
// rows go to the model with the question, which on a workspace of hundreds of
// wide tables is well over a hundred thousand tokens for each call. Within
// the budget, every dataset still goes. Past it, the datasets the question is
// already about come first (the dataset or record it was asked from, the
// tables the previous answer read), then those whose name, columns or
// description share words with the question and the chat's earlier
// questions, until the budget is used. A word counts for more the fewer
// datasets use it, so "users" picks out the user tables while a suffix most
// tables share barely counts; datasets scoring under a third of the best
// match are left out. With no match at all, the datasets behind the
// published object types go first.
export const ANSWER_CONTEXT_CHARS = 120_000;

type Described = {
  id: string;
  name: string;
  description?: string;
  columns: string[];
};

export function focusDatasets<T extends Described>(
  datasets: T[],
  question: { text: string; pinned?: string[]; preferred?: string[] },
  budget = ANSWER_CONTEXT_CHARS,
): T[] {
  const size = (d: T) => JSON.stringify(d).length;
  if (datasets.reduce((n, d) => n + size(d), 0) <= budget) return datasets;
  const terms = new Set(words(question.text)),
    pinned = new Set(question.pinned),
    preferred = new Set(question.preferred);
  // Each dataset's words by where they appear: name, columns, description.
  const fields = datasets.map((d) => ({
    name: new Set(words(d.name)),
    columns: new Set(d.columns.flatMap((c) => words(c.split(" ")[0]))),
    description: new Set(words(d.description || "")),
  }));
  const used = new Map<string, number>();
  for (const f of fields)
    for (const w of new Set([...f.name, ...f.columns, ...f.description]))
      if (terms.has(w)) used.set(w, (used.get(w) || 0) + 1);
  const weight = (w: string) => Math.log(1 + datasets.length / used.get(w)!);
  const scores = fields.map((f, i) =>
    pinned.has(datasets[i].id)
      ? Infinity
      : [...used.keys()].reduce(
          (n, w) =>
            n +
            weight(w) *
              (f.name.has(w)
                ? 4
                : f.columns.has(w)
                  ? 2
                  : f.description.has(w)
                    ? 1
                    : 0),
          0,
        ),
  );
  const best = Math.max(0, ...scores.filter((n) => n !== Infinity));
  const ranked = datasets
    .map((d, i) => ({
      i,
      n: size(d),
      // With no match, the published types' datasets.
      s: best
        ? scores[i] >= best / 3
          ? scores[i]
          : 0
        : scores[i] || (preferred.has(d.id) ? 1 : 0),
    }))
    .filter((x) => x.s > 0)
    .sort((a, b) => b.s - a.s || a.n - b.n);
  const chosen = new Set<number>();
  let total = 0;
  for (const x of ranked)
    if (x.s === Infinity || total + x.n <= budget) {
      chosen.add(x.i);
      total += x.n;
    }
  // Nothing matched: the datasets that fit, in their usual order.
  if (!chosen.size)
    datasets.forEach((d, i) => {
      if (total + size(d) <= budget) {
        chosen.add(i);
        total += size(d);
      }
    });
  return datasets.filter((_, i) => chosen.has(i));
}

const stopWords = new Set(
  "the and for with what which who whom how many much our are was were have has had from that this those these each per most least top show list give all any into over than their there them they its not but can".split(
    " ",
  ),
);

// Lowercase words of three letters or more, singular, without stop words:
// "Which institution fcts have the most users fcts?" -> institution, fct,
// user, fct.
function words(text: string) {
  return text
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter((w) => w.length > 2 && !stopWords.has(w))
    .map((w) =>
      w.length > 4 && w.endsWith("ies")
        ? w.slice(0, -3) + "y"
        : w.length > 3 && w.endsWith("s") && !w.endsWith("ss")
          ? w.slice(0, -1)
          : w,
    );
}
