import {
  memo,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import type { Resource } from "./vendor/shared";
import { Reply } from "./vendor/web/areas/analyst/reply";
import "./vendor/web/workspace.css";
import "./vendor/web/kit/kit.css";
import "./vendor/web/areas/analyst/analyst.css";
import recorded from "./data/loopwell-analyst.json";

// Three of the Analyst's real runs on the Loopwell sample, exported from the
// local app's Analyst (./vendor), asked one after another in its own chat.
const runs = recorded as unknown as Resource[];
// A question's timeline: typed out at the top of the chat, answered at once,
// then read for a while before the next is typed.
const TYPED = 1100,
  ANSWERED = TYPED + 150,
  NEXT = ANSWERED + 4800;

// The finished run, as if it had just been answered: its events are moved
// to now, so the chat shows it as complete rather than from the past.
function snapshot(run: Resource, now: number): Resource {
  const at = (i: number) => new Date(now + i).toISOString();
  return {
    ...run,
    createdAt: at(0),
    updatedAt: at(run.data.events.length),
    data: {
      ...run.data,
      events: run.data.events.map((e: object, i: number) => ({
        ...e,
        at: at(i),
      })),
    },
  };
}

const client = new QueryClient();

// One question and its reply. A turn only renders again when its question or
// its run changes, not on every tick of the demo's clock.
const Turn = memo(function Turn({
  goal,
  typing,
  run,
}: {
  goal: string;
  typing?: boolean;
  run?: Resource;
}) {
  return (
    <div className="an-turn">
      <div className={`an-question${typing ? " is-typing" : ""}`}>
        <p>{goal}</p>
      </div>
      {run && <Reply run={run} />}
    </div>
  );
});

// The Analyst's chat, playing by itself while it is on screen: each question
// types itself out at the top, and its answer comes in under it. Nothing in
// it takes the pointer. With reduced motion it shows the first answer.
export function Analyst() {
  const calm = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const scroller = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);
  const [time, setTime] = useState(calm ? NEXT - 1 : 0);
  useEffect(() => {
    const watcher = new IntersectionObserver(([entry]) =>
      setVisible(entry.isIntersecting),
    );
    watcher.observe(scroller.current!);
    return () => watcher.disconnect();
  }, []);
  useEffect(() => {
    if (calm || !visible) return;
    let last = performance.now();
    const timer = setInterval(() => {
      const now = performance.now(),
        step = now - last;
      last = now;
      setTime((t) => t + step);
    }, 50);
    return () => clearInterval(timer);
  }, [calm, visible]);

  // Which question is being asked, how far along it is, and the questions
  // asked before it since the conversation last started over.
  const round = Math.floor(time / NEXT),
    within = time - round * NEXT,
    first = round - (round % runs.length);
  const goal: string = runs[round % runs.length].data.goal,
    typed = Math.min(goal.length, Math.ceil((goal.length * within) / TYPED)),
    answered = within >= ANSWERED;
  // Each answer made once, so a turn that hasn't changed gets the same object
  // and doesn't render again.
  const shots = useRef(new Map<number, Resource>());
  const shot = (r: number) => {
    if (!shots.current.has(r)) {
      for (const k of shots.current.keys())
        if (k < first) shots.current.delete(k);
      shots.current.set(r, snapshot(runs[r % runs.length], Date.now()));
    }
    return shots.current.get(r)!;
  };
  const turns = useMemo(() => {
    const list: {
      key: number;
      goal: string;
      typing?: boolean;
      run?: Resource;
    }[] = [];
    for (let r = first; r < round; r++)
      list.push({
        key: r,
        goal: runs[r % runs.length].data.goal,
        run: shot(r),
      });
    list.push({
      key: round,
      goal: goal.slice(0, typed),
      typing: within < TYPED,
      run: answered ? shot(round) : undefined,
    });
    return list;
  }, [round, typed, within < TYPED, answered]);

  // The newest answer writes itself in: its title, summary and highlights
  // fade in word by word, then the rest of it rises in (analyst.css). The
  // words are wrapped once, before the answer is first painted; its text
  // never changes after that, so React has nothing to put back.
  useLayoutEffect(() => {
    const answer = scroller.current?.querySelector<HTMLElement>(
      ".an-turn:last-child .an-answer",
    );
    const head = answer?.querySelector(".an-answer-head");
    if (calm || !answer || !head || answer.dataset.written) return;
    answer.dataset.written = "true";
    const texts: Text[] = [],
      walk = document.createTreeWalker(head, NodeFilter.SHOW_TEXT);
    while (walk.nextNode()) texts.push(walk.currentNode as Text);
    let words = 0;
    for (const text of texts) {
      const parts = document.createDocumentFragment();
      for (const part of text.data.split(/(\s+)/)) {
        if (!part.trim()) {
          if (part) parts.append(part);
          continue;
        }
        const word = document.createElement("span");
        word.className = "an-word";
        word.style.setProperty("--w", String(words++));
        word.textContent = part;
        parts.append(word);
      }
      text.replaceWith(parts);
    }
    // A highlight's bullet comes in with its first word.
    for (const item of head.querySelectorAll("li")) {
      const word = item.querySelector<HTMLElement>(".an-word");
      if (!word) continue;
      item.classList.add("an-word");
      item.style.setProperty("--w", word.style.getPropertyValue("--w"));
    }
    answer.style.setProperty("--words", String(words));
  }, [round, answered]);
  // The newest question is kept at the top of the conversation, as its
  // answer grows under it. Once the answer has written itself in, the chat
  // reads on down to its chart and sources.
  const reading = !calm && within >= ANSWERED + 1700;
  useLayoutEffect(() => {
    const log = scroller.current;
    if (!log) return;
    const turn = log.querySelector<HTMLElement>(".an-turn:last-child"),
      answer = turn?.querySelector<HTMLElement>(".an-answer");
    const top = !turn
      ? 0
      : reading && answer
        ? log.scrollTop +
          answer.getBoundingClientRect().bottom -
          log.getBoundingClientRect().bottom +
          16
        : turn.offsetTop - 16;
    log.scrollTo({
      top: Math.max(0, top),
      behavior: calm || !turn ? "auto" : "smooth",
    });
  }, [turns.length, first, answered, reading]);

  return (
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <div className="an-chat" aria-hidden>
          <div className="an-scroll" ref={scroller}>
            <div className="an-log">
              {turns.map(({ key, ...turn }) => (
                <Turn key={key} {...turn} />
              ))}
            </div>
          </div>
        </div>
      </MemoryRouter>
    </QueryClientProvider>
  );
}
