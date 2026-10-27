import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, ArrowRight, ShieldAlert } from "lucide-react";
import type { Severity } from "../../../shared/foundry-read";
import { ApiError } from "../../api";
import {
  Button,
  ButtonLink,
  DataGrid,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  Tag,
  plural,
  type Tone,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { Why, type RefView } from "../explore/derived";
import { identityOf, refOf, rulesOf } from "../explore/graph-read";
import { entityId } from "../explore/model";
import { namingKeys, readable, type Naming } from "../explore/naming";
import { readOptions, runRead, startingOf } from "../explore/reads";
import {
  RULE_BREAK_LIMITS,
  parseRuleBreakCounts,
  parseRuleBreakPage,
  ruleBreakCountRequests,
  ruleBreakPageRequest,
  type PageAt,
  type RuleBreak,
} from "../explore/rule-breaks";
import { DatabaseStarting } from "../explore/starting";
import { useTenantReads } from "../explore/tenant-reads";
import { useNaming } from "../explore/use-naming";
import { loadVersion, withLoad, type AgentConstraint } from "./model";
import { ontologyQueries } from "./queries";
import { ontologyName, useOntology } from "./scope";

// /ontology/:ontologyId/loads/:loadId/rule-breaks: what a load's version's
// constraints found in it. Counts per constraint and a page of one
// constraint's breaks come from the violation records the load keeps
// (../explore/rule-breaks.ts, read from the database); each break's "why"
// comes from the load's read with its rules (../explore/graph-read.ts).
// `?constraint=` picks the constraint, and `?key=` (an Ask citation's
// violation) starts its list at that record. What the load's checks left
// out isn't counted: the engine doesn't record it in the graph, so the page
// says what it leaves out instead.

const SEVERITY: Record<Severity, { label: string; tone: Tone }> = {
  block: { label: "Blocks", tone: "danger" },
  warn: { label: "Warns", tone: "warning" },
  info: { label: "Informs", tone: "info" },
};

export function LoadRuleBreaksPage({ loadId }: { loadId: string }) {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const { read } = useTenantReads();
  const [params, setParams] = useSearchParams();
  const load = useQuery(ontologyQueries.load(ws, o, loadId));
  const l = load.data?.load;
  const version = useQuery({
    ...ontologyQueries.version(ws, o, l?.versionId || ""),
    enabled: !!l?.versionId,
  });
  const { naming } = useNaming(l?.versionId || "");
  const rules = rulesOf(version.data?.version);
  const constraints = rules?.constraints || [];
  const canRead =
    !!ontology.tenantId &&
    !!l &&
    l.status !== "deleting" &&
    l.status !== "delete_failed";
  const batches = ruleBreakCountRequests(
    loadId,
    constraints.map((c) => c.name),
  );
  const counts = useQuery({
    queryKey: [
      ws,
      "ontology",
      o,
      "read",
      loadId,
      "rule-breaks",
      "counts",
      constraints.map((c) => c.name),
    ],
    queryFn: async ({ signal }) => {
      const bodies: unknown[] = [];
      for (const b of batches)
        bodies.push(await runRead(read, b.request, signal));
      return parseRuleBreakCounts(bodies, batches);
    },
    enabled: canRead && constraints.length > 0,
    ...readOptions,
  });
  const crumbs = [
    { label: "Ontologies", to: paths.ontologies() },
    { label: ontologyName(ontology), to: paths.ontology(o) },
    { label: "Loads", to: paths.loads(o) },
    { label: "Load", to: paths.load(o, loadId) },
  ];
  const header = (title = "Rule breaks") => (
    <PageHeader
      breadcrumbs={[...crumbs, { label: "Rule breaks" }]}
      eyebrow={
        l ? `Load of version ${loadVersion(l).replace(/^v/, "")}` : "Load"
      }
      title={title}
      actions={
        <ButtonLink icon={ArrowLeft} to={paths.load(o, loadId)}>
          Load
        </ButtonLink>
      }
    />
  );
  if (load.isPending || (l?.versionId && version.isPending))
    return (
      <Page>
        {header()}
        <SkeletonText lines={6} />
      </Page>
    );
  if (!l)
    return (
      <Page>
        {header("Load unavailable")}
        <EmptyState
          icon={ShieldAlert}
          title={
            load.error instanceof ApiError && load.error.status === 404
              ? "This load was deleted, or isn’t this ontology’s"
              : "Couldn’t load this load"
          }
          text={load.error?.message}
          action={<ButtonLink to={paths.loads(o)}>All loads</ButtonLink>}
        />
      </Page>
    );
  if (!ontology.tenantId)
    return (
      <Page>
        {header()}
        <EmptyState
          icon={ShieldAlert}
          title="You can’t read the ontology’s database"
          text="It lives in a project you can’t see. Ask a workspace admin for access to that project."
        />
      </Page>
    );
  if (!canRead)
    return (
      <Page>
        {header()}
        <EmptyState
          icon={ShieldAlert}
          title="This load is being deleted"
          text="Its rule breaks go with it. Pick another load on the Loads page."
          action={<ButtonLink to={paths.loads(o)}>All loads</ButtonLink>}
        />
      </Page>
    );
  if (!rules)
    return (
      <Page>
        {header()}
        <EmptyState
          icon={ShieldAlert}
          title="Its version has no rules"
          text="A version with rules or constraints is checked as it loads; this one has none to check."
        />
      </Page>
    );
  const count = (name: string) => counts.data?.get(name);
  const asked = params.get("constraint") || "";
  // A cited constraint the version doesn't list by name (a link's
  // cardinality or required) still has its records.
  const chosen: AgentConstraint | undefined =
    constraints.find((c) => c.name === asked) ||
    (asked
      ? { name: asked, severity: "warn", description: "", datalog: "" }
      : undefined) ||
    constraints.find((c) => (count(c.name) || 0) > 0) ||
    constraints[0];
  return (
    <Page>
      {header()}
      <div className="on-stack">
        {!constraints.length && !asked ? (
          <EmptyState
            size="sm"
            icon={ShieldAlert}
            title="No constraints"
            text="Its version derives facts with rules but checks nothing."
          />
        ) : (
          <>
            {startingOf(counts.error ?? counts.failureReason) ? (
              <DatabaseStarting
                inline
                queryKey={[ws, "ontology", o, "read", loadId, "rule-breaks"]}
              />
            ) : (
              counts.error && (
                <p role="alert" className="on-error-text">
                  Couldn’t count the rule breaks: {counts.error.message}
                </p>
              )
            )}
            <Panel
              title="Constraints"
              subtitle="Each constraint of the load’s version, with the breaks the load records. It records no rule linking an entity to itself (an entity’s page shows those as it reads them) and leaves out entities too costly to check as it was written, so a count of 0 isn’t proof."
            >
              <DataGrid<AgentConstraint>
                label="Constraints"
                rows={constraints}
                getRowKey={(c) => c.name}
                rowHeight={44}
                columns={[
                  {
                    key: "name",
                    label: "Constraint",
                    width: 240,
                    render: (c) => (
                      <Link
                        to={`?constraint=${encodeURIComponent(c.name)}`}
                        aria-current={c.name === chosen?.name || undefined}
                        onClick={(e) => {
                          e.preventDefault();
                          setParams({ constraint: c.name });
                        }}
                      >
                        {c.name}
                      </Link>
                    ),
                  },
                  {
                    key: "severity",
                    label: "Severity",
                    width: 110,
                    render: (c) => (
                      <Tag tone={SEVERITY[c.severity].tone}>
                        {SEVERITY[c.severity].label}
                      </Tag>
                    ),
                  },
                  {
                    key: "breaks",
                    label: "Breaks",
                    width: 100,
                    type: "integer",
                    sortValue: (c) => count(c.name) ?? -1,
                    render: (c) => {
                      const n = count(c.name);
                      return n === undefined || n === null ? (
                        <span className="on-muted">…</span>
                      ) : (
                        n.toLocaleString()
                      );
                    },
                  },
                  {
                    key: "description",
                    label: "Checks",
                    width: 420,
                    sortable: false,
                    render: (c) => (
                      <span className="on-cell-text" title={c.description}>
                        {c.description}
                      </span>
                    ),
                  },
                ]}
              />
            </Panel>
            {chosen && (
              <BreakList
                key={`${chosen.name}\u0000${params.get("key") || ""}`}
                loadId={loadId}
                current={!!l.current}
                constraint={chosen}
                cited={params.get("key") || undefined}
                total={count(chosen.name)}
                naming={naming}
              />
            )}
          </>
        )}
      </div>
    </Page>
  );
}

/**
 * One constraint's breaks, a page at a time in key order: from the first,
 * or from the `cited` record, which is marked.
 */
function BreakList({
  loadId,
  current,
  constraint,
  cited,
  total,
  naming,
}: {
  loadId: string;
  current: boolean;
  constraint: AgentConstraint;
  cited?: string;
  total: number | null | undefined;
  naming: Naming;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const { read } = useTenantReads();
  // Where each page shown so far starts; the last is this page's.
  const [afters, setAfters] = useState<PageAt[]>([
    cited ? { from: cited } : { after: null },
  ]);
  const after = afters.at(-1)!;
  const fromCited = "from" in afters[0]!;
  const page = useQuery({
    queryKey: [
      ws,
      "ontology",
      o,
      "read",
      loadId,
      "rule-breaks",
      constraint.name,
      after,
      naming.version,
    ],
    queryFn: async ({ signal }) => {
      const p = parseRuleBreakPage(
        await runRead(
          read,
          ruleBreakPageRequest(
            loadId,
            constraint.name,
            after,
            namingKeys(naming),
          ),
          signal,
        ),
      );
      return {
        ...p,
        breaks: p.breaks.map((b) => ({
          ...b,
          entities: b.entities.map((e) => {
            const r = readable(naming, e.type, e.props, e.dbId);
            return r.known ? { ...e, name: r.name } : e;
          }),
        })),
      };
    },
    ...readOptions,
  });
  const entityPath = (id: string) =>
    withLoad(paths.entity(o, id), current ? null : loadId);
  const view = (b: RuleBreak): RefView => ({
    loadId,
    naming,
    name: (r) =>
      b.entities.find((e) => e.type === r.type && refOf(e.identity).id === r.id)
        ?.name,
    to: (r) => {
      const i = identityOf(r, naming);
      return i && entityPath(entityId(i));
    },
  });
  const from = (afters.length - 1) * RULE_BREAK_LIMITS.page;
  return (
    <Panel
      title={`Breaks of ${constraint.name}`}
      subtitle={constraint.description}
    >
      {page.isPending ? (
        <SkeletonText lines={4} />
      ) : page.error ? (
        <p role="alert" className="on-error-text">
          Couldn’t read the breaks: {page.error.message}{" "}
          <Button size="sm" onClick={() => void page.refetch()}>
            Retry
          </Button>
        </p>
      ) : !page.data.breaks.length ? (
        <EmptyState
          size="sm"
          icon={ShieldAlert}
          title="No breaks"
          text="Everything the load holds keeps to this constraint, apart from what wasn’t checked."
        />
      ) : (
        <div className="on-stack">
          <ul className="ex-facts" aria-label="Rule breaks">
            {page.data.breaks.map((b) => (
              <li
                key={b.key}
                className={b.key === cited ? "is-cited" : undefined}
                aria-current={b.key === cited || undefined}
              >
                <div className="ex-fact-head">
                  {b.entities.length ? (
                    b.entities.flatMap((e, i) => [
                      ...(i ? [", "] : []),
                      <Link key={e.id} to={entityPath(e.id)}>
                        {e.name}
                      </Link>,
                      <span key={`${e.id}-type`} className="on-muted">
                        {e.type}
                      </span>,
                    ])
                  ) : (
                    <span className="on-muted">
                      Entities this load no longer holds
                    </span>
                  )}
                  {b.entities.length > 0 && (
                    <Why
                      pred={{ kind: "violation", name: constraint.name }}
                      args={b.entities.map((e) => refOf(e.identity))}
                      view={view(b)}
                    />
                  )}
                </div>
              </li>
            ))}
          </ul>
          <div className="on-card-actions">
            <span className="on-muted">
              {fromCited
                ? `From the cited break${typeof total === "number" ? `, of ${plural(total, "break")}` : ""}`
                : `${from + 1}–${from + page.data.breaks.length}${typeof total === "number" ? ` of ${plural(total, "break")}` : ""}`}
            </span>
            {fromCited && (
              <Button size="sm" onClick={() => setAfters([{ after: null }])}>
                From the first
              </Button>
            )}
            <Button
              size="sm"
              icon={ArrowLeft}
              disabled={afters.length < 2}
              onClick={() => setAfters(afters.slice(0, -1))}
            >
              Previous
            </Button>
            <Button
              size="sm"
              iconRight={ArrowRight}
              disabled={!page.data.next}
              onClick={() => setAfters([...afters, { after: page.data.next }])}
            >
              Next
            </Button>
          </div>
        </div>
      )}
    </Panel>
  );
}
