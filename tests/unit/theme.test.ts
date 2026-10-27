import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  THEME_BACKGROUNDS,
  THEME_BOOT,
  THEME_KEY,
  parseTheme,
  resolveTheme,
} from "@/shared/theme";

// The theme: what a stored value means, which theme a preference shows, how
// it is stored, the boot script the layout runs before first paint, and the
// watcher that tells canvas, SVG and logos when the page's theme changes.

/** A localStorage that holds values in a Map, or throws like a blocked one. */
function fakeStorage(blocked = false) {
  const values = new Map<string, string>();
  const guard = () => {
    if (blocked) throw new DOMException("blocked", "SecurityError");
  };
  return {
    values,
    getItem: (k: string) => (guard(), values.get(k) ?? null),
    setItem: (k: string, v: string) => (guard(), void values.set(k, v)),
    removeItem: (k: string) => (guard(), void values.delete(k)),
  };
}

type StorageChange = { key: string | null; newValue: string | null };

/**
 * <html> with data-theme and two theme-color metas, an OS dark setting that
 * can change, and a MutationObserver that reports attribute writes.
 */
function fakePage(prefersDark = false) {
  const observers: (() => void)[] = [];
  const media = new Set<() => void>();
  const storageEvents = new Set<(e: StorageChange) => void>();
  const dataset = new Proxy({} as Record<string, string>, {
    set(target, key: string, value) {
      target[key] = value;
      observers.forEach((o) => o());
      return true;
    },
    deleteProperty(target, key: string) {
      delete target[key];
      observers.forEach((o) => o());
      return true;
    },
  });
  const metas = [
    { media: "(prefers-color-scheme: light)", content: "" },
    { media: "(prefers-color-scheme: dark)", content: "" },
  ];
  const page = {
    dark: prefersDark,
    dataset,
    metas,
    setOsDark(dark: boolean) {
      page.dark = dark;
      media.forEach((m) => m());
    },
    // Another tab writes the choice.
    otherTab(key: string | null, newValue: string | null) {
      storageEvents.forEach((f) => f({ key, newValue }));
    },
  };
  vi.stubGlobal("window", {
    addEventListener: (type: string, f: (e: StorageChange) => void) => {
      if (type === "storage") storageEvents.add(f);
    },
  });
  vi.stubGlobal("document", {
    documentElement: { dataset },
    querySelectorAll: () => metas,
  });
  vi.stubGlobal("matchMedia", (query: string) => ({
    get matches() {
      return query.includes("dark") && page.dark;
    },
    addEventListener: (_: string, f: () => void) => media.add(f),
  }));
  vi.stubGlobal(
    "MutationObserver",
    class {
      constructor(private callback: () => void) {}
      observe() {
        observers.push(this.callback);
      }
    },
  );
  return page;
}

// shell/theme.ts keeps one watcher per page: each test loads it afresh.
const loadShellTheme = async () => {
  vi.resetModules();
  return import("@/web/shell/theme");
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("parseTheme and resolveTheme", () => {
  it("reads light and dark as stored, anything else as System", () => {
    expect(parseTheme("light")).toBe("light");
    expect(parseTheme("dark")).toBe("dark");
    for (const raw of [null, undefined, "", "system", "Dark", "sepia"])
      expect(parseTheme(raw)).toBe("system");
  });

  it("shows the OS's theme for System and the chosen one otherwise", () => {
    expect(resolveTheme("system", true)).toBe("dark");
    expect(resolveTheme("system", false)).toBe("light");
    for (const os of [true, false]) {
      expect(resolveTheme("light", os)).toBe("light");
      expect(resolveTheme("dark", os)).toBe("dark");
    }
  });
});

describe("readTheme and writeTheme", () => {
  let storage: ReturnType<typeof fakeStorage>;
  beforeEach(() => {
    storage = fakeStorage();
    vi.stubGlobal("localStorage", storage);
  });

  it("stores light and dark, and clears the key for System", async () => {
    const { readTheme, writeTheme } = await loadShellTheme();
    expect(readTheme()).toBe("system");
    writeTheme("dark");
    expect(storage.values.get(THEME_KEY)).toBe("dark");
    expect(readTheme()).toBe("dark");
    writeTheme("light");
    expect(readTheme()).toBe("light");
    writeTheme("system");
    expect(storage.values.has(THEME_KEY)).toBe(false);
    expect(readTheme()).toBe("system");
  });

  it("reads an unknown stored value as System", async () => {
    const { readTheme } = await loadShellTheme();
    storage.values.set(THEME_KEY, "solarized");
    expect(readTheme()).toBe("system");
  });

  it("keeps working when storage is blocked", async () => {
    vi.stubGlobal("localStorage", fakeStorage(true));
    const { readTheme, writeTheme } = await loadShellTheme();
    expect(() => writeTheme("dark")).not.toThrow();
    expect(readTheme()).toBe("system");
  });
});

describe("THEME_BOOT", () => {
  const boot = (storage: ReturnType<typeof fakeStorage>) => {
    const root = { dataset: {} as Record<string, string> };
    new Function("localStorage", "document", THEME_BOOT)(storage, {
      documentElement: root,
    });
    return root.dataset.theme;
  };

  it("puts a stored light or dark on <html> before first paint", () => {
    for (const theme of ["light", "dark"]) {
      const storage = fakeStorage();
      storage.values.set(THEME_KEY, theme);
      expect(boot(storage)).toBe(theme);
    }
  });

  it("leaves System, an unknown value and blocked storage to the OS", () => {
    const system = fakeStorage();
    system.values.set(THEME_KEY, "system");
    const unknown = fakeStorage();
    unknown.values.set(THEME_KEY, "<script>");
    expect(boot(fakeStorage())).toBeUndefined();
    expect(boot(system)).toBeUndefined();
    expect(boot(unknown)).toBeUndefined();
    expect(() => boot(fakeStorage(true))).not.toThrow();
    expect(boot(fakeStorage(true))).toBeUndefined();
  });
});

describe("applyTheme", () => {
  it("sets data-theme for a choice and removes it for System", async () => {
    const page = fakePage();
    const { applyTheme } = await loadShellTheme();
    applyTheme("dark");
    expect(page.dataset.theme).toBe("dark");
    applyTheme("light");
    expect(page.dataset.theme).toBe("light");
    applyTheme("system");
    expect(page.dataset.theme).toBeUndefined();
  });

  it("gives the browser chrome the background the page shows", async () => {
    const page = fakePage();
    const { applyTheme } = await loadShellTheme();
    applyTheme("dark");
    expect(page.metas.map((m) => m.content)).toEqual([
      THEME_BACKGROUNDS.dark,
      THEME_BACKGROUNDS.dark,
    ]);
    applyTheme("light");
    expect(page.metas.map((m) => m.content)).toEqual([
      THEME_BACKGROUNDS.light,
      THEME_BACKGROUNDS.light,
    ]);
    // System: each meta's media query picks its own.
    applyTheme("system");
    expect(page.metas.map((m) => m.content)).toEqual([
      THEME_BACKGROUNDS.light,
      THEME_BACKGROUNDS.dark,
    ]);
  });
});

describe("currentTheme and subscribeTheme", () => {
  it("is light, and the choice System, without a document", async () => {
    const { currentChoice, currentTheme, subscribeChoice, subscribeTheme } =
      await loadShellTheme();
    expect(currentTheme()).toBe("light");
    expect(currentChoice()).toBe("system");
    for (const subscribe of [subscribeTheme, subscribeChoice]) {
      const unsubscribe = subscribe(() => {});
      expect(unsubscribe).toBeTypeOf("function");
      unsubscribe();
    }
  });

  it("follows the OS until a choice is set, then the choice", async () => {
    const page = fakePage(true);
    const { currentTheme, applyTheme } = await loadShellTheme();
    expect(currentTheme()).toBe("dark");
    page.setOsDark(false);
    expect(currentTheme()).toBe("light");
    applyTheme("dark");
    expect(currentTheme()).toBe("dark");
    // An explicit choice wins over the OS.
    page.setOsDark(false);
    expect(currentTheme()).toBe("dark");
    applyTheme("system");
    expect(currentTheme()).toBe("light");
  });

  it("tells listeners once per change of the theme shown", async () => {
    const page = fakePage(false);
    const { applyTheme, subscribeTheme } = await loadShellTheme();
    const listener = vi.fn();
    const unsubscribe = subscribeTheme(listener);
    applyTheme("dark");
    expect(listener).toHaveBeenCalledTimes(1);
    // Dark chosen, then the OS turns dark too: the page stays dark.
    page.setOsDark(true);
    applyTheme("system");
    expect(listener).toHaveBeenCalledTimes(1);
    page.setOsDark(false);
    expect(listener).toHaveBeenCalledTimes(2);
    unsubscribe();
    applyTheme("dark");
    expect(listener).toHaveBeenCalledTimes(2);
  });
});

describe("currentChoice and subscribeChoice", () => {
  it("tells every menu of a change of choice, even one the page shows alike", async () => {
    const page = fakePage(true);
    const { applyTheme, currentChoice, subscribeChoice, subscribeTheme } =
      await loadShellTheme();
    const choice = vi.fn(),
      shown = vi.fn();
    subscribeChoice(choice);
    subscribeTheme(shown);
    expect(currentChoice()).toBe("system");
    // The OS is dark: choosing Dark changes the choice, not the page.
    applyTheme("dark");
    expect(currentChoice()).toBe("dark");
    expect(choice).toHaveBeenCalledTimes(1);
    expect(shown).not.toHaveBeenCalled();
    // The OS changing is no change of choice.
    page.setOsDark(false);
    expect(choice).toHaveBeenCalledTimes(1);
    applyTheme("system");
    expect(currentChoice()).toBe("system");
    expect(choice).toHaveBeenCalledTimes(2);
    expect(shown).toHaveBeenCalledTimes(1);
  });

  it("applies a choice made in another tab", async () => {
    const page = fakePage(false);
    const { currentChoice, currentTheme } = await loadShellTheme();
    expect(currentTheme()).toBe("light");
    page.otherTab(THEME_KEY, "dark");
    expect(page.dataset.theme).toBe("dark");
    expect(currentChoice()).toBe("dark");
    expect(currentTheme()).toBe("dark");
    // Other keys, and storage cleared (no key), leave it.
    page.otherTab("foundry.sidebar.v1", "collapsed");
    page.otherTab(null, null);
    expect(currentChoice()).toBe("dark");
    // System there clears the key: System here.
    page.otherTab(THEME_KEY, null);
    expect(page.dataset.theme).toBeUndefined();
    expect(currentChoice()).toBe("system");
  });
});
