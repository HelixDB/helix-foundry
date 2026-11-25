// Apps' built-in browsers (X, LinkedIn, Instagram, …), told by their
// User-Agent. Their web views can drop the sign-in's cookie on the way to
// WorkOS and back, and Google refuses to sign in inside them, so the sign-in
// page asks to open it in the phone's browser. Each app adds its own token to
// the system web view's User-Agent; an Android web view without one still
// says `; wv)`. An iOS web view without a token looks like Safari, so it
// isn't told.

/** The app whose browser a page is in: a fixed vocabulary for analytics. */
export type InApp =
  | "x"
  | "linkedin"
  | "facebook"
  | "messenger"
  | "instagram"
  | "threads"
  | "tiktok"
  | "snapchat"
  | "pinterest"
  | "line"
  | "wechat"
  | "webview";

export type InAppBrowser = {
  app: InApp;
  /** How the app names itself to the reader; null for an unknown web view. */
  name: string | null;
  /** Android offers Chrome by an intent link; iOS only by the app's menu. */
  os: "ios" | "android" | "other";
};

// First match wins: Messenger, Instagram and Threads also say FBAV.
const APPS: readonly { app: InApp; name: string | null; ua: RegExp }[] = [
  { app: "messenger", name: "Messenger", ua: /FBAN\/Messenger|MessengerFor/ },
  { app: "instagram", name: "Instagram", ua: /\bInstagram\b/ },
  { app: "threads", name: "Threads", ua: /\bBarcelona\b/ },
  { app: "facebook", name: "Facebook", ua: /\bFB(?:AN|AV|_IAB|IOS)\b/ },
  { app: "x", name: "X", ua: /\bTwitter(?!bot)/ },
  { app: "linkedin", name: "LinkedIn", ua: /\bLinkedInApp\b/ },
  {
    app: "tiktok",
    name: "TikTok",
    ua: /\b(?:musical_ly|trill)_|BytedanceWebview|\bTikTok\b/,
  },
  { app: "snapchat", name: "Snapchat", ua: /\bSnapchat\b/ },
  { app: "pinterest", name: "Pinterest", ua: /\bPinterest\b/ },
  { app: "line", name: "LINE", ua: /\bLine\/\d/ },
  { app: "wechat", name: "WeChat", ua: /\bMicroMessenger\b/ },
  { app: "webview", name: null, ua: /\bAndroid\b.*; wv\)/ },
];

/**
 * The in-app browser `userAgent` comes from, or null for a browser of its own.
 *
 * @example
 * inAppBrowser("… Mobile Safari/537.36 TwitterAndroid"); // { app: "x", name: "X", os: "android" }
 * inAppBrowser("… Version/18.0 Mobile/15E148 Safari/604.1"); // null
 */
export function inAppBrowser(userAgent: string): InAppBrowser | null {
  const found = APPS.find(({ ua }) => ua.test(userAgent));
  if (!found) return null;
  return {
    app: found.app,
    name: found.name,
    os: /\biP(?:hone|ad|od)\b/.test(userAgent)
      ? "ios"
      : /\bAndroid\b/.test(userAgent)
        ? "android"
        : "other",
  };
}

/**
 * An Android intent link that opens `url` in Chrome: apps' web views hand
 * `intent:` links to the system rather than load them.
 *
 * @example
 * chromeIntent(new URL("https://foundry.helix-db.com/login?return_to=%2Fask"));
 * // "intent://foundry.helix-db.com/login?return_to=%2Fask#Intent;scheme=https;package=com.android.chrome;end"
 */
export function chromeIntent(url: URL): string {
  return `intent://${url.host}${url.pathname}${url.search}#Intent;scheme=${url.protocol.slice(0, -1)};package=com.android.chrome;end`;
}
