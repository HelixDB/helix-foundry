import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type Ref,
} from "react";
import { Database, Loader2, Search, X } from "lucide-react";

import { cn } from "./ui";

import {
  matchLoadedNodes,
  parseIdCandidate,
  snippetFor,
  type QueryFailure,
} from "./lib/graph-store";
import type { GraphNode, SearchHit } from "./lib/types";
import { describeFailure } from "./query/failure";
import type { SearchOptions, SearchOutcome } from "./useExplorerGraph";
import { KBD, MONO } from "./ui";

export interface ExplorerSearchProps {
  inputRef: Ref<HTMLInputElement>;
  /** Nodes on the canvas, matched as you type. */
  nodes: readonly GraphNode[];
  /** Ids of every loaded node, to mark database hits that are already loaded. */
  isLoaded: (nodeId: string) => boolean;
  captionOf: (node: GraphNode) => string;
  colorFor: (label: string) => string;
  search: (
    text: string,
    options?: SearchOptions,
  ) => Promise<SearchOutcome | null>;
  /** Picks a node; `loaded` is false for database hits not in the store yet. */
  onChoose: (node: GraphNode, loaded: boolean) => void;
  disabled?: boolean;
  /** Whether the database has full-text indexes to search (changes the hint). */
  hasTextIndexes: boolean;
  /**
   * Labels the database search reads node by node for exact-case text (the
   * `contains` scan): it runs only on Enter, and its cost grows with these
   * labels' sizes, so the hint names them.
   */
  scanLabels?: string[];
  className?: string;
}

/** "User", "User and Company", "User, Company, Tag and 3 more labels" */
function labelList(labels: string[], max = 3): string {
  if (labels.length === 1) return labels[0];
  if (labels.length <= max)
    return `${labels.slice(0, -1).join(", ")} and ${labels.at(-1)}`;
  return `${labels.slice(0, max).join(", ")} and ${labels.length - max} more labels`;
}

type Item =
  | {
      kind: "canvas";
      node: GraphNode;
      matchedKey: string | null;
      snippet: string | null;
    }
  | { kind: "run" }
  | { kind: "hit"; hit: SearchHit };

interface DatabaseSearch {
  text: string;
  status: "loading" | "done";
  hits: SearchHit[];
  failure: QueryFailure | null;
}

const DROPDOWN_MAX_WIDTH = 440;
const DROPDOWN_MIN_WIDTH = 320;
const VIEWPORT_GUTTER = 8;

const VIA_LABEL: Record<SearchHit["via"], string> = {
  id: "id",
  text: "text index",
  contains: "contains",
};

export function ExplorerSearch({
  inputRef,
  nodes,
  isLoaded,
  captionOf,
  colorFor,
  search,
  onChoose,
  disabled,
  hasTextIndexes,
  scanLabels = [],
  className,
}: ExplorerSearchProps) {
  const [text, setText] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const [database, setDatabase] = useState<DatabaseSearch | null>(null);
  const [position, setPosition] = useState<{
    left: number;
    top: number;
    width: number;
  } | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const localInputRef = useRef<HTMLInputElement | null>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const requestRef = useRef(0);
  const listId = useId();
  const optionId = (index: number) => `${listId}-option-${index}`;

  const trimmed = text.trim();
  const matches = useMemo(
    () => matchLoadedNodes(nodes, trimmed, captionOf),
    [nodes, trimmed, captionOf],
  );
  // An id is looked up directly; only text runs the property scan.
  const scanned = parseIdCandidate(trimmed) === null ? scanLabels : [];
  const current = database && database.text === trimmed ? database : null;
  const items = useMemo<Item[]>(() => {
    if (!trimmed) return [];
    const list: Item[] = matches.map((match) => ({ kind: "canvas", ...match }));
    // While the database search runs, its row stays as the spinner ("Searching the database…").
    if (!current || current.failure || current.status === "loading")
      list.push({ kind: "run" });
    // Id and text-index hits arrive before the slower property scan finishes.
    if (current && current.hits.length > 0) {
      const onCanvas = new Set(matches.map((match) => match.node.id));
      for (const hit of current.hits)
        if (!onCanvas.has(hit.node.id)) list.push({ kind: "hit", hit });
    }
    return list;
  }, [trimmed, matches, current]);

  useEffect(() => {
    setActive(0);
  }, [trimmed]);
  useEffect(() => {
    if (active >= items.length) setActive(Math.max(0, items.length - 1));
  }, [active, items.length]);

  const setInputRef = useCallback(
    (element: HTMLInputElement | null) => {
      localInputRef.current = element;
      if (typeof inputRef === "function") inputRef(element);
      else if (inputRef)
        (inputRef as { current: HTMLInputElement | null }).current = element;
    },
    [inputRef],
  );

  const showDropdown = open && trimmed.length > 0;

  // The dropdown is fixed to the viewport (so it can be wider than a narrow
  // input and still stay on screen), anchored under the input.
  const place = useCallback(() => {
    const input = localInputRef.current;
    if (!input) return;
    const rect = input.getBoundingClientRect();
    const viewport = window.innerWidth;
    const width = Math.min(
      DROPDOWN_MAX_WIDTH,
      viewport - VIEWPORT_GUTTER * 2,
      Math.max(rect.width, DROPDOWN_MIN_WIDTH),
    );
    const left = Math.min(
      Math.max(VIEWPORT_GUTTER, rect.left),
      viewport - width - VIEWPORT_GUTTER,
    );
    setPosition({ left, top: rect.bottom + 4, width });
  }, []);
  useLayoutEffect(() => {
    if (!showDropdown) return;
    place();
    window.addEventListener("resize", place);
    return () => window.removeEventListener("resize", place);
  }, [showDropdown, place]);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target as Node;
      if (
        !rootRef.current?.contains(target) &&
        !listRef.current?.contains(target)
      )
        setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  useEffect(() => {
    if (!showDropdown) return;
    document
      .getElementById(`${listId}-option-${active}`)
      ?.scrollIntoView?.({ block: "nearest" });
  }, [active, showDropdown, listId]);

  const runDatabaseSearch = useCallback(async () => {
    if (!trimmed) return;
    const request = (requestRef.current += 1);
    setDatabase({ text: trimmed, status: "loading", hits: [], failure: null });
    const outcome = await search(trimmed, {
      onPartial: (partial) => {
        if (request !== requestRef.current) return;
        setDatabase({
          text: trimmed,
          status: "loading",
          hits: partial.hits,
          failure: null,
        });
      },
    });
    if (request !== requestRef.current) return;
    if (!outcome) {
      setDatabase(null);
      return;
    }
    setDatabase({
      text: trimmed,
      status: "done",
      hits: outcome.hits,
      failure: outcome.failure,
    });
  }, [search, trimmed]);

  const choose = useCallback(
    (item: Item) => {
      if (item.kind === "run") {
        if (current?.status !== "loading") void runDatabaseSearch();
        return;
      }
      const node = item.kind === "canvas" ? item.node : item.hit.node;
      setOpen(false);
      localInputRef.current?.blur();
      onChoose(node, item.kind === "canvas" || isLoaded(node.id));
    },
    [current, isLoaded, onChoose, runDatabaseSearch],
  );

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      if (items.length === 0) return;
      event.preventDefault();
      setOpen(true);
      setActive((index) =>
        event.key === "ArrowDown"
          ? (index + 1) % items.length
          : (index - 1 + items.length) % items.length,
      );
      return;
    }
    if (event.key === "Enter") {
      event.preventDefault();
      setOpen(true);
      const item = items[active];
      if (item) choose(item);
      return;
    }
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      if (showDropdown) setOpen(false);
      else if (text) setText("");
      else localInputRef.current?.blur();
    }
  };

  const canvasCount = items.filter((item) => item.kind === "canvas").length;
  const hitCount = items.filter((item) => item.kind === "hit").length;
  const activeItem = showDropdown ? items[active] : undefined;

  return (
    <div ref={rootRef} className={cn("ex-search", className)}>
      <label className="ex-field is-search">
        <span className="hf-sr-only">Jump to a node</span>
        <Search aria-hidden="true" size={15} className="ex-field-icon" />
        <input
          ref={setInputRef}
          type="text"
          role="combobox"
          aria-expanded={showDropdown}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={activeItem ? optionId(active) : undefined}
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
          value={text}
          placeholder="Jump to a node…"
          onChange={(event) => {
            setText(event.target.value);
            setOpen(true);
          }}
          onFocus={() => setOpen(true)}
          onKeyDown={onKeyDown}
        />
        {text ? (
          <button
            type="button"
            onClick={() => {
              setText("");
              localInputRef.current?.focus();
            }}
            aria-label="Clear search"
            title="Clear search"
            className="ex-field-clear"
          >
            <X aria-hidden="true" size={14} />
          </button>
        ) : (
          <kbd aria-hidden="true" className={cn(KBD, "ex-field-kbd")}>
            /
          </kbd>
        )}
      </label>

      {showDropdown && position && (
        <div
          ref={listRef}
          className="ex-menu ex-search-results"
          style={{
            left: position.left,
            top: position.top,
            width: position.width,
          }}
        >
          <div
            id={listId}
            role="listbox"
            aria-label="Search results"
            className="ex-search-list"
          >
            {canvasCount > 0 && <GroupHeading>On canvas</GroupHeading>}
            {items.map((item, index) => {
              const selected = index === active;
              const common = {
                id: optionId(index),
                role: "option" as const,
                "aria-selected": selected,
                onMouseDown: (event: React.MouseEvent) =>
                  event.preventDefault(),
                onMouseMove: () => setActive(index),
                onClick: () => choose(item),
                className: cn("ex-search-option", selected && "is-active"),
              };
              if (item.kind === "run") {
                const loading = current?.status === "loading";
                return (
                  <div key="run">
                    <GroupHeading>In database</GroupHeading>
                    <div {...common} aria-disabled={loading || undefined}>
                      <span className="ex-search-run-icon">
                        {loading ? (
                          <Loader2
                            aria-hidden="true"
                            size={14}
                            className="hf-spin ex-busy"
                          />
                        ) : (
                          <Database aria-hidden="true" size={14} />
                        )}
                      </span>
                      <span className="ex-search-text">
                        <span className="ex-search-title ex-truncate">
                          {loading
                            ? "Searching the database…"
                            : `Search the database for “${trimmed}”`}
                        </span>
                        {scanned.length > 0 && (
                          <span className="ex-search-meta ex-truncate">
                            {loading ? "Reading" : "Reads"} every node of{" "}
                            {labelList(scanned)} for exact-case text
                          </span>
                        )}
                      </span>
                      {!loading && <kbd className={KBD}>↵</kbd>}
                    </div>
                    {current?.failure && (
                      <p className="ex-search-error">
                        {describeFailure(current.failure, "read").title}.{" "}
                        <span>{current.failure.message}</span>
                      </p>
                    )}
                  </div>
                );
              }
              const node = item.kind === "canvas" ? item.node : item.hit.node;
              const hitKey =
                item.kind === "canvas"
                  ? item.matchedKey
                  : (item.hit.matchedKey ?? null);
              // A normalized name (`<prop>__nn`) shows as the prop it folds.
              const folded =
                !!hitKey?.endsWith("__nn") &&
                typeof node.properties[hitKey.slice(0, -4)] === "string";
              const matchedKey = folded ? hitKey!.slice(0, -4) : hitKey;
              const caption = captionOf(node);
              const matchedValue = matchedKey
                ? node.properties[matchedKey]
                : undefined;
              // A hit on the property the caption already shows needs no snippet.
              const repeatsCaption =
                typeof matchedValue === "string" &&
                matchedValue
                  .replace(/\s+/g, " ")
                  .trim()
                  .startsWith(caption.replace(/…$/, ""));
              const snippet =
                item.kind === "canvas" && !folded
                  ? item.snippet
                  : typeof matchedValue === "string" && !repeatsCaption
                    ? snippetFor(matchedValue, trimmed)
                    : null;
              const firstHit =
                item.kind === "hit" && items[index - 1]?.kind !== "hit";
              return (
                <div key={`${item.kind}:${node.id}`}>
                  {firstHit && <GroupHeading>In database</GroupHeading>}
                  <div {...common}>
                    <span
                      aria-hidden="true"
                      className="ex-swatch is-round"
                      style={{ background: colorFor(node.label) }}
                    />
                    <span className="ex-search-text">
                      <span className="ex-search-title ex-truncate">
                        {caption}
                      </span>
                      <span className="ex-search-meta">
                        <span>{node.label}</span>
                        <span className={cn(MONO, "ex-num ex-dim")}>
                          #{node.dbId}
                        </span>
                        {matchedKey && snippet && (
                          <span className="ex-truncate">
                            · <span className={MONO}>{matchedKey}</span>:{" "}
                            {snippet}
                          </span>
                        )}
                      </span>
                    </span>
                    {item.kind === "hit" && (
                      <span className="ex-value-chip">
                        {isLoaded(node.id) ? "loaded" : VIA_LABEL[item.hit.via]}
                      </span>
                    )}
                  </div>
                </div>
              );
            })}
            {current?.status === "done" &&
              !current.failure &&
              hitCount === 0 && (
                <p className="ex-empty-note">
                  {current.hits.length > 0
                    ? "Every database match is already on the canvas."
                    : "No matches in the database."}
                </p>
              )}
            {canvasCount === 0 && !current && (
              <p className="ex-empty-note">Nothing on the canvas matches.</p>
            )}
          </div>
          <div className="ex-search-foot">
            <span>↑↓ move · ↵ open · esc close</span>
            <span>
              Database search: ids{hasTextIndexes ? ", text indexes" : ""} and
              exact-case text
              {scanLabels.length > 0
                ? " (a scan of each label, slow on large ones)"
                : ""}
            </span>
          </div>
        </div>
      )}
    </div>
  );
}

function GroupHeading({ children }: { children: React.ReactNode }) {
  return <div className="ex-menu-title">{children}</div>;
}
