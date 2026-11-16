import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  LayoutDashboard,
  type LucideIcon,
  MessageCircleQuestion,
  Network,
  Workflow,
} from "lucide-react";
import { trackEvent } from "../../analytics";
import { readStorage, Skeleton, TabPanel, Tabs, writeStorage } from "../../kit";
import { useWorkspace } from "../../ui";
import { automationRefusal } from "../automations/model";
import { automationQueries } from "../automations/queries";
import {
  chosenKind,
  founderSuggestionsView,
  SUGGESTION_CARDS,
  SUGGESTION_KIND_KEY,
  type FounderSuggestion,
  type SuggestionKind,
} from "./founder-model";

// Founder's new chat's suggestions (founder-page.tsx, under its composer):
// a tab per kind (Questions, Dashboards, Ontologies, Workflows: the kinds
// GET .../automation-suggestions holds for the page's ontology), then the
// chosen kind's 2 cards, written for the ontology. A card's prompt is
// clamped; a mouse's hover or the keyboard's focus previews it whole as the
// composer's placeholder (`onPreview`), and a click puts it in the composer
// (`onPick`), never sends it. The kind chosen is kept per browser. A
// grantee never reads the route: the load's competency questions, as cards.
// founder-model.ts decides what shows (founderSuggestionsView).

const LABEL: Record<SuggestionKind, string> = {
  question: "Questions",
  dashboard: "Dashboards",
  ontology: "Ontologies",
  workflow: "Workflows",
};
const ICON: Record<SuggestionKind, LucideIcon> = {
  question: MessageCircleQuestion,
  dashboard: LayoutDashboard,
  ontology: Network,
  workflow: Workflow,
};

export function FounderSuggestions({
  ontologyId,
  name,
  versioned,
  questions,
  onPreview,
  onPick,
}: {
  ontologyId: string;
  // The ontology's name, as the cards' pill shows it.
  name: string;
  // Whether the ontology has a current version (the fixed examples differ).
  versioned: boolean;
  // The load's competency questions: a grantee's cards.
  questions: readonly string[];
  onPreview: (prompt: string) => void;
  onPick: (prompt: string) => void;
}) {
  const me = useWorkspace();
  const read = useQuery({
    ...automationQueries.suggestions(me.id, ontologyId),
    enabled: !me.grantee,
  });
  const view = founderSuggestionsView({
    grantee: me.grantee,
    questions,
    name,
    versioned,
    pending: read.isPending,
    failed: read.isError,
    refused: automationRefusal(read.error) === "FOUNDRY_AUTOMATIONS_DISABLED",
    data: read.data,
  });
  const [stored, setStored] = useState(() => readStorage(SUGGESTION_KIND_KEY));
  const tabs = view.kind === "tabs" ? view.tabs : [];
  const chosen = chosenKind(
    stored,
    tabs.map((t) => t.kind),
  );
  const shown =
    view.kind === "questions"
      ? view.cards
      : tabs.find((t) => t.kind === chosen)?.cards;
  // Cards replaced under the pointer or the focus (Founder's in place of a
  // placeholder, another kind's) leave no stale preview, and a screen
  // reader hears that they changed.
  const signature = (shown || []).map((x) => x.prompt).join("\n"),
    before = useRef(signature),
    [updated, setUpdated] = useState(false);
  useEffect(() => {
    if (before.current === signature) return;
    const had = before.current !== "";
    before.current = signature;
    onPreview("");
    setUpdated(had && signature !== "");
  }, [signature]);
  if (view.kind === "none") return null;
  const cards = (list: readonly FounderSuggestion[], label?: string) => (
    <ul className="fd-cards" aria-label={label}>
      {list.map((x, i) => {
        const Icon = ICON[x.kind];
        return (
          <li key={i}>
            <button
              type="button"
              className="fd-card"
              // A mouse's hover or the keyboard's focus previews it; a
              // tap, or a click's focus, only fills it.
              onPointerEnter={(e) =>
                e.pointerType === "mouse" && onPreview(x.prompt)
              }
              onPointerLeave={() => onPreview("")}
              onFocus={(e) =>
                e.currentTarget.matches(":focus-visible") && onPreview(x.prompt)
              }
              onBlur={() => onPreview("")}
              onClick={() => {
                trackEvent("agent_suggestion_picked", {
                  kind: x.kind,
                  source: x.source,
                });
                onPick(x.prompt);
              }}
            >
              <span className="fd-card-prompt">{x.prompt}</span>
              <span className="fd-card-foot">
                <span className="fd-card-topic">
                  <Icon size={13} aria-hidden />
                  <span>{x.topic}</span>
                </span>
                <span className="fd-card-ontology">{name}</span>
              </span>
            </button>
          </li>
        );
      })}
    </ul>
  );
  const status = (
    <span role="status" className="hf-sr-only">
      {read.data?.generating
        ? "Writing suggestions…"
        : view.kind === "tabs" && !shown
          ? "Loading suggestions…"
          : updated
            ? "Suggestions updated"
            : ""}
    </span>
  );
  if (view.kind === "questions")
    return (
      <div className="fd-suggest">
        {cards(view.cards, "Suggested questions")}
      </div>
    );
  return (
    <div className="fd-suggest">
      <Tabs
        items={tabs.map((t) => ({
          id: t.kind,
          label: LABEL[t.kind],
          icon: ICON[t.kind],
        }))}
        label="Suggestions"
        value={chosen}
        onChange={(id) => {
          setStored(id);
          writeStorage(SUGGESTION_KIND_KEY, id);
        }}
        idPrefix="fd-kind"
        className="fd-kinds"
      />
      {tabs.map((t) => (
        <TabPanel
          key={t.kind}
          id={t.kind}
          value={chosen || ""}
          idPrefix="fd-kind"
          className="fd-kind-panel"
        >
          {t.cards ? (
            cards(t.cards)
          ) : (
            <ul className="fd-cards" aria-hidden>
              {Array.from({ length: SUGGESTION_CARDS }, (_, i) => (
                <li key={i}>
                  <Skeleton className="fd-card-skeleton" height={112} />
                </li>
              ))}
            </ul>
          )}
        </TabPanel>
      ))}
      {status}
    </div>
  );
}
