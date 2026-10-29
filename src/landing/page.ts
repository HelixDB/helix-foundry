import { trackEvent } from "@/web/analytics";

// What each copy button's text sets up, for analytics: the agent's prompt
// (Helix Cloud through the Helix MCP) or a local install.
const SETUP = { "agent-prompt": "agent", "setup-text": "local" } as const;

// The page's small behaviours: cells that rise into view, and the copy
// button. Returns what undoes them.
export function page() {
  const calm = matchMedia("(prefers-reduced-motion: reduce)").matches;

  const rising = document.querySelectorAll(".up");
  const watcher =
    calm || !("IntersectionObserver" in window)
      ? null
      : new IntersectionObserver(
          (entries, self) =>
            entries.forEach((entry) => {
              if (!entry.isIntersecting) return;
              entry.target.classList.add("in");
              self.unobserve(entry.target);
            }),
          { rootMargin: "0px 0px -8% 0px", threshold: 0.05 },
        );
  rising.forEach((el) =>
    watcher ? watcher.observe(el) : el.classList.add("in"),
  );

  // The clipboard API needs a secure context, and rejects when the document is
  // not focused, so a rejection falls through rather than failing.
  const selectAndCopy = (text: string) => {
    const field = document.createElement("textarea");
    field.value = text;
    field.setAttribute("readonly", "");
    field.style.cssText = "position:fixed;top:-1000px;opacity:0";
    document.body.appendChild(field);
    field.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(field);
    return ok ? Promise.resolve() : Promise.reject(new Error("copy refused"));
  };
  const copy = (text: string) =>
    navigator.clipboard && window.isSecureContext
      ? navigator.clipboard.writeText(text).catch(() => selectAndCopy(text))
      : selectAndCopy(text);

  const undo = [() => watcher?.disconnect()];
  document
    .querySelectorAll<HTMLButtonElement>("[data-copy]")
    .forEach((button) => {
      const source = document.getElementById(button.dataset.copy!);
      if (!source) return;
      const slot = button.querySelector(".label") || button;
      const resting = slot.textContent;
      let revert = 0;
      const say = (word: string) => {
        slot.textContent = word;
        button.dataset.copied = "true";
        clearTimeout(revert);
        revert = window.setTimeout(() => {
          slot.textContent = resting;
          button.dataset.copied = "false";
        }, 2200);
      };
      const setup = SETUP[button.dataset.copy as keyof typeof SETUP];
      const click = () => {
        if (setup) trackEvent("setup_prompt_copied", { setup });
        return copy(source.textContent!.trim()).then(
          () => say("Copied"),
          () => say("Select it and copy"),
        );
      };
      button.addEventListener("click", click);
      undo.push(() => {
        clearTimeout(revert);
        button.removeEventListener("click", click);
      });
    });
  return () => undo.forEach((u) => u());
}
