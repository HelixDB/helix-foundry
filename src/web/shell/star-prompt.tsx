import { useEffect, useState, useSyncExternalStore } from "react";
import { Star } from "lucide-react";
import { LOCAL } from "../../shared/deployment";
import { STAR_REPOSITORY_URL, type StarCheck } from "../../shared/github-star";
import { trackEvent } from "../analytics";
import { Button } from "../kit";

// The GitHub star prompt, after T3 Code's: once a few questions have been
// asked in Analyst, a card asks for a star on GitHub, once a session until
// it is answered or dismissed. A local build stars in place with the
// install's GitHub token (/github-star, src/server/github-star.ts) and opens
// the repository when it can't; the hosted site opens the repository.

// Separate keys, so a count another tab writes can never undo a dismissal.
const ASKED = "foundry.star-prompt.asked";
const DONE = "foundry.star-prompt.done";
// Asks from the third question on.
const QUESTIONS_BEFORE_ASKING = 2;
// Waits this long without input in this window, so it never interrupts.
const IDLE_MS = 30_000;
// What a person does (ask, navigate, type, scroll) starts with one of these.
const ACTIVITY = ["pointerdown", "keydown", "wheel", "focus"] as const;

/** How the prompt's button stars: through the site, or on GitHub. */
type Via = Exclude<StarCheck, "starred">;

let shownThisSession = false;
let showing: Via | null = null;
const listeners = new Set<() => void>();
const show = (via: Via | null) => {
  showing = via;
  for (const listener of listeners) listener();
};
const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => void listeners.delete(listener);
};

// Storage blocked: done() keeps nothing, and isDone() says done, so it never asks.
function done() {
  try {
    localStorage.setItem(DONE, "1");
  } catch {
    // Nothing kept: isDone() reads blocked storage as done anyway.
  }
}
function isDone() {
  try {
    return !!localStorage.getItem(DONE);
  } catch {
    return true;
  }
}

// GET /github-star's answer, parsed without trust; anything else links.
const check = (): Promise<StarCheck> =>
  fetch("/github-star", { cache: "no-store" })
    .then((res) => (res.ok ? res.json() : null))
    .then((body: { star?: unknown } | null): StarCheck => {
      const star = body?.star;
      return star === "starred" || star === "in_place" ? star : "on_github";
    })
    .catch((): StarCheck => "on_github");

// Resolves once this window, focused, has had IDLE_MS without input.
function idle() {
  return new Promise<void>((resolve) => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const restart = () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        // Input in another app or a frame never reaches us: the next focus restarts.
        if (!document.hasFocus() || document.activeElement?.matches("iframe"))
          return;
        for (const type of ACTIVITY)
          window.removeEventListener(type, restart, true);
        resolve();
      }, IDLE_MS);
    };
    for (const type of ACTIVITY)
      window.addEventListener(type, restart, { capture: true, passive: true });
    restart();
  });
}

/**
 * Counts a question asked in Analyst. From the third, once a session until
 * the prompt is answered or dismissed in this browser, shows it once the
 * window has gone 30 s without input, unless the install's GitHub token has
 * starred the repository already.
 */
export async function recordQuestionAsked(): Promise<void> {
  if (shownThisSession) return;
  try {
    if (localStorage.getItem(DONE)) return;
    const asked = (Number(localStorage.getItem(ASKED)) || 0) + 1;
    localStorage.setItem(ASKED, String(asked));
    if (asked <= QUESTIONS_BEFORE_ASKING) return;
  } catch {
    return;
  }
  shownThisSession = true;
  const star = LOCAL ? await check() : "on_github";
  if (star === "starred") return done();
  await idle();
  // Another tab may have been answered meanwhile.
  if (isDone()) return;
  show(star);
  trackEvent("star_prompt_shown", { via: star });
}

/** The prompt, where the toast shows; nothing until recordQuestionAsked shows it. */
export function StarPrompt() {
  const via = useSyncExternalStore(
    subscribe,
    () => showing,
    () => null,
  );
  // Starring in place, then thanked; or the star failed after the browser
  // stopped letting the click open a tab, so the button links to GitHub.
  const [step, setStep] = useState<"ask" | "starring" | "thanked" | "link">(
    "ask",
  );
  useEffect(() => {
    if (step !== "thanked") return;
    const timer = setTimeout(() => show(null), 5000);
    return () => clearTimeout(timer);
  }, [step]);
  if (!via) return null;
  const close = () => {
    done();
    show(null);
  };
  const opened = () => {
    trackEvent("star_prompt_clicked", { outcome: "opened_github" });
    close();
  };
  const starInPlace = async () => {
    setStep("starring");
    const starred = await fetch("/github-star", { method: "POST" })
      .then((res) => (res.ok ? res.json() : null))
      .then((body: { starred?: unknown } | null) => body?.starred === true)
      .catch(() => false);
    if (starred) {
      done();
      trackEvent("star_prompt_clicked", { outcome: "starred" });
      return setStep("thanked");
    }
    // A browser blocks new tabs once the click's activation has expired.
    if (navigator.userActivation?.isActive === false) return setStep("link");
    window.open(STAR_REPOSITORY_URL, "_blank", "noopener,noreferrer");
    opened();
  };
  return (
    <aside className="hf-star-prompt" aria-label="Star Helix Foundry">
      <Star className="hf-star-prompt-icon" size={16} aria-hidden />
      {step === "thanked" ? (
        <div role="status" className="hf-star-prompt-text">
          <strong>Thanks for the star!</strong>
          <p>It really helps Helix Foundry grow.</p>
        </div>
      ) : (
        <>
          <div className="hf-star-prompt-text">
            <strong>Enjoying Helix Foundry?</strong>
            <p>Give us a star on GitHub to let us know!</p>
          </div>
          <div className="hf-star-prompt-actions">
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                trackEvent("star_prompt_dismissed");
                close();
              }}
            >
              Dismiss
            </Button>
            {via === "in_place" && step !== "link" ? (
              <Button
                variant="primary"
                size="sm"
                pending={step === "starring"}
                onClick={() => void starInPlace()}
              >
                {step === "starring" ? "Starring…" : "Star on GitHub"}
              </Button>
            ) : (
              <a
                className="hf-btn hf-btn--primary hf-btn--sm"
                href={STAR_REPOSITORY_URL}
                target="_blank"
                rel="noopener noreferrer"
                onClick={opened}
              >
                {step === "link" ? "Open GitHub" : "Star on GitHub"}
              </a>
            )}
          </div>
        </>
      )}
    </aside>
  );
}
