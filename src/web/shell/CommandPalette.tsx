import { useEffect, useId, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useQueries } from "@tanstack/react-query";
import { LOCAL } from "../../shared/deployment";
import {
  ArrowLeftRight,
  ArrowRight,
  Braces,
  CornerDownLeft,
  History,
  LoaderCircle,
  Monitor,
  Moon,
  Plus,
  Search,
  Sparkles,
  Sun,
  type LucideIcon,
} from "lucide-react";
import { useWorkspace, type ShellWorkspace } from "../ui";
import { cx, useMedia } from "../kit";
import { paths } from "../paths";
import { paletteSources, primaryNav, secondaryNav } from "./nav";
import { readRecents, recordRecent, sectionIcon } from "./recents";
import { shortcutKeys } from "./shortcut";
import { themeOptions, useTheme } from "./theme";

const themeIcons = { system: Monitor, light: Sun, dark: Moon };
// Set per stamp in next.config.ts: empty where API sources take pieces only
// over the Helix MCP (prod, local mode).
const OVER_HTTP = !!process.env.NEXT_PUBLIC_HELIX_EVENTS_URL;

type Entry = {
  key: string;
  group: string;
  title: string;
  subtitle?: string;
  icon: LucideIcon;
  run: () => void;
  disabled?: boolean;
};
const score = (text: string, q: string) => {
  const t = text.toLowerCase();
  if (!q) return 1;
  if (t.startsWith(q)) return 3;
  if (t.split(/[\s_·›-]+/).some((w) => w.startsWith(q))) return 2;
  return t.includes(q) ? 1 : 0;
};
// Global quick search (Cmd/Ctrl+K) over pages, actions and what areas list.
export function CommandPalette({
  initialQuery = "",
  workspaces,
  current,
  onSwitch,
  onClose,
}: {
  initialQuery?: string;
  workspaces: ShellWorkspace[];
  current: ShellWorkspace;
  onSwitch: (id: string) => void;
  onClose: () => void;
}) {
  const { id, role } = useWorkspace(),
    navigate = useNavigate(),
    { pathname } = useLocation(),
    [theme, setTheme] = useTheme(),
    small = useMedia("(max-width: 800px)");
  const [query, setQuery] = useState(initialQuery),
    [active, setActive] = useState(0);
  const dialog = useRef<HTMLDialogElement>(null),
    list = useRef<HTMLDivElement>(null);
  const listId = useId();
  const recents = useMemo(() => readRecents(id), [id]);
  useEffect(() => {
    const el = dialog.current!,
      previous = document.activeElement as HTMLElement | null;
    el.showModal();
    const cancel = (e: Event) => {
      e.preventDefault();
      onClose();
    };
    el.addEventListener("cancel", cancel);
    return () => {
      el.removeEventListener("cancel", cancel);
      el.close();
      previous?.focus();
    };
    // Mount-only: the dialog opens once per palette instance.
  }, []);
  const results = useQueries({
    queries: paletteSources.map((s) => ({
      queryKey: [id, "palette", s.id],
      staleTime: 30000,
      queryFn: () => s.load(id),
    })),
  });
  const loading = results.some((r) => r.isPending);
  const q = query.trim().toLowerCase();
  const go = (to: string) => () => {
    onClose();
    navigate(to);
  };
  const entries = useMemo(() => {
    const out: Entry[] = [];
    if (!q)
      for (const r of recents.slice(0, 5))
        out.push({
          key: "recent:" + r.path,
          group: "Recent",
          title: r.title,
          icon: sectionIcon(r.section) || History,
          run: go(r.path),
        });
    // With no query, only the sections; typing also finds their sub-pages.
    const pages = [...primaryNav, ...secondaryNav].flatMap((n) => [
      { title: n.label, to: n.to, icon: n.icon },
      ...(q ? n.children(pathname) : [])
        .filter((c) => c.to !== n.to)
        .map((c) => ({
          title: `${n.label} › ${c.label}`,
          to: c.to,
          icon: n.icon,
        })),
    ]);
    const actions: (Entry & { terms: string })[] = [
      // Owners and admins connect data, make API sources (not in a local
      // build, which has no Helix MCP for them) and create ontologies.
      ...(role !== "member"
        ? [
            {
              key: "action:add-data",
              group: "Actions",
              title: "Add data",
              subtitle: "Connect a database or an app",
              terms: "add data connect source connection",
              icon: Plus,
              run: go(paths.connections(true)),
            },
            ...(LOCAL
              ? []
              : [
                  {
                    key: "action:new-api-source",
                    group: "Actions",
                    title: "New API source",
                    subtitle: OVER_HTTP
                      ? "Send pieces of data from your app or agent"
                      : "Send pieces of data from your agent",
                    terms: "new api source event agent pieces send mcp json",
                    icon: Braces,
                    run: go(paths.connections("events")),
                  },
                ]),
            {
              key: "action:new-ontology",
              group: "Actions",
              title: "Create an ontology",
              subtitle: "On a Helix database, then draft it from your tables",
              terms:
                "create ontology new ontology generate draft agent database",
              icon: Sparkles,
              run: go(paths.newOntology()),
            },
          ]
        : []),
      // The themes not in use: System follows the OS.
      ...themeOptions
        .filter((t) => t.value !== theme)
        .map((t) => ({
          key: "action:theme:" + t.value,
          group: "Actions",
          title:
            t.value === "system"
              ? "Use the system theme"
              : `Use the ${t.label.toLowerCase()} theme`,
          subtitle: "Theme",
          terms: `theme appearance mode ${t.value}${t.value === "system" ? " os auto" : ""}`,
          icon: themeIcons[t.value],
          run: () => {
            setTheme(t.value);
            onClose();
          },
        })),
      ...workspaces
        .filter((w) => w.id !== current.id)
        .slice(0, 5)
        .map((w) => ({
          key: "action:workspace:" + w.id,
          group: "Actions",
          title: `Switch to ${w.name}`,
          subtitle: "Workspace",
          terms: "switch workspace " + w.name,
          icon: ArrowLeftRight,
          run: () => {
            onClose();
            onSwitch(w.id);
            navigate(paths.home());
          },
        })),
    ];
    if (!q) out.push(...actions);
    for (const p of pages)
      if (score(p.title, q))
        out.push({
          key: "page:" + p.title,
          group: "Go to",
          title: p.title,
          icon: p.icon,
          run: go(p.to),
        });
    if (q)
      paletteSources.forEach((s, i) => {
        const ranked = (results[i]?.data || [])
          .map((x) => ({
            ...x,
            rank: Math.max(
              score(x.title, q) * 2,
              score(x.subtitle || "", q),
              score(x.description || "", q) ? 1 : 0,
            ),
          }))
          .filter((x) => x.rank > 0)
          .sort((x, y) => y.rank - x.rank)
          .slice(0, 6);
        for (const x of ranked)
          out.push({
            key: s.id + ":" + x.id,
            group: s.label,
            title: x.title,
            subtitle: x.subtitle,
            icon: s.icon,
            run: () => {
              recordRecent(id, x.href, x.title);
              go(x.href)();
            },
          });
      });
    if (q)
      out.push(...actions.filter((a) => score(a.terms + " " + a.title, q)));
    return out;
    // `go` is recreated each render but only closes over stable values.
  }, [query, results.map((r) => r.dataUpdatedAt).join(), theme, role]);
  useEffect(() => setActive(0), [q]);
  useEffect(() => {
    list.current
      ?.querySelector(`[data-index="${active}"]`)
      ?.scrollIntoView({ block: "nearest" });
  }, [active]);
  const groups = entries.reduce<Record<string, [Entry, number][]>>(
    (acc, e, i) => ((acc[e.group] ||= []).push([e, i]), acc),
    {},
  );
  return (
    <dialog
      ref={dialog}
      className="hf-palette"
      aria-label="Search your workspace"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="hf-palette-card">
        <div className="hf-palette-input">
          {loading && q ? (
            <LoaderCircle size={17} className="hf-spin" aria-hidden />
          ) : (
            <Search size={17} aria-hidden />
          )}
          <input
            autoFocus
            role="combobox"
            aria-expanded="true"
            aria-controls={listId}
            aria-activedescendant={
              entries[active] ? `${listId}-${active}` : undefined
            }
            aria-autocomplete="list"
            aria-label="Search pages, data and ontology"
            placeholder={small ? "Search…" : "Search pages, data and ontology…"}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setActive((a) => Math.min(a + 1, entries.length - 1));
              } else if (e.key === "ArrowUp") {
                e.preventDefault();
                setActive((a) => Math.max(a - 1, 0));
              } else if (e.key === "Enter") {
                e.preventDefault();
                const entry = entries[active];
                if (entry && !entry.disabled) entry.run();
              }
            }}
          />
          <kbd className="hf-palette-esc" aria-hidden>
            esc
          </kbd>
          <button type="button" className="hf-palette-cancel" onClick={onClose}>
            Cancel
          </button>
        </div>
        <div
          ref={list}
          id={listId}
          role="listbox"
          aria-label="Results"
          className="hf-palette-list"
        >
          {Object.entries(groups).map(([group, items]) => (
            <div key={group} role="group" aria-label={group}>
              <div className="hf-palette-group" aria-hidden>
                {group}
              </div>
              {items.map(([e, i]) => (
                <div
                  key={e.key}
                  id={`${listId}-${i}`}
                  data-index={i}
                  role="option"
                  aria-selected={i === active}
                  aria-disabled={e.disabled || undefined}
                  className={cx(
                    "hf-palette-option",
                    i === active && "is-active",
                    e.disabled && "is-disabled",
                  )}
                  onMouseMove={() => setActive(i)}
                  onClick={() => !e.disabled && e.run()}
                >
                  <e.icon size={16} aria-hidden />
                  <span className="hf-palette-title">{e.title}</span>
                  {e.subtitle && (
                    <span className="hf-palette-subtitle">{e.subtitle}</span>
                  )}
                  {i === active && !e.disabled ? (
                    <CornerDownLeft size={14} aria-hidden />
                  ) : (
                    <ArrowRight size={14} aria-hidden className="is-quiet" />
                  )}
                </div>
              ))}
            </div>
          ))}
          {!entries.length && (
            <p className="hf-palette-empty">
              {loading ? "Searching…" : `No results for “${query}”`}
            </p>
          )}
        </div>
        <div className="hf-palette-foot" aria-hidden>
          <span>
            <kbd>↑</kbd>
            <kbd>↓</kbd> to navigate
          </span>
          <span>
            <kbd>↵</kbd> to open
          </span>
          <span>
            {shortcutKeys.map((k) => (
              <kbd key={k}>{k}</kbd>
            ))}{" "}
            to toggle
          </span>
        </div>
      </div>
    </dialog>
  );
}
