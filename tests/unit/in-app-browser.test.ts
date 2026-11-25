import { describe, expect, it } from "vitest";
import { chromeIntent, inAppBrowser } from "@/web/in-app-browser";

// Real User-Agents of apps' built-in browsers, and of browsers of their own,
// which must never be told to leave.

const ANDROID = (device: string, tail: string) =>
  `Mozilla/5.0 (Linux; Android 14; ${device}; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/120.0.6099.144 Mobile Safari/537.36${tail}`;
const IOS = (tail: string) =>
  `Mozilla/5.0 (iPhone; CPU iPhone OS 17_1_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 ${tail}`;

describe("inAppBrowser", () => {
  it.each([
    [
      "X on Android",
      ANDROID("Pixel 7 Build/TQ3A.230805.001", " TwitterAndroid"),
      "x",
      "X",
      "android",
    ],
    ["X on iOS", IOS("Twitter for iPhone/10.5"), "x", "X", "ios"],
    [
      "LinkedIn on iOS",
      IOS("[LinkedInApp]/9.29.6406"),
      "linkedin",
      "LinkedIn",
      "ios",
    ],
    [
      "LinkedIn on Android",
      ANDROID("SM-S918B Build/UP1A.231005.007", " [LinkedInApp]/4.1.889"),
      "linkedin",
      "LinkedIn",
      "android",
    ],
    [
      "Facebook on iOS",
      IOS(
        "[FBAN/FBIOS;FBAV/442.0.0.33.111;FBBV/543116425;FBDV/iPhone15,2;FBMD/iPhone;FBSN/iOS;FBSV/17.1;FBSS/3;FBID/phone;FBLC/en_US;FBOP/5;FBRV/0]",
      ),
      "facebook",
      "Facebook",
      "ios",
    ],
    [
      "Facebook on Android",
      ANDROID(
        "SM-G991B Build/TP1A.220624.014",
        " [FB_IAB/FB4A;FBAV/442.0.0.32.110;]",
      ),
      "facebook",
      "Facebook",
      "android",
    ],
    [
      "Messenger on iOS",
      IOS(
        "[FBAN/MessengerForiOS;FBAV/385.0.0.37.110;FBBV/398765432;FBDV/iPhone14,2;FBMD/iPhone;FBSN/iOS;FBSV/16.0]",
      ),
      "messenger",
      "Messenger",
      "ios",
    ],
    [
      "Instagram on iOS",
      IOS(
        "Instagram 309.0.2.18.119 (iPhone14,5; iOS 17_1_2; en_US; en; scale=3.00; 1170x2532; 541635890)",
      ),
      "instagram",
      "Instagram",
      "ios",
    ],
    [
      "Instagram on Android",
      ANDROID(
        "SM-A525F Build/SP1A.210812.016",
        " Instagram 309.1.0.41.113 Android (31/12; 450dpi; 1080x2186; samsung; SM-A525F; a52q; qcom; en_GB; 541635863)",
      ),
      "instagram",
      "Instagram",
      "android",
    ],
    [
      "Threads on iOS",
      IOS(
        "Barcelona 289.0.0.77.109 (iPhone15,3; iOS 17_0; en_US; en; scale=3.00; 1290x2796; 489720907)",
      ),
      "threads",
      "Threads",
      "ios",
    ],
    [
      "TikTok on iOS",
      IOS(
        "musical_ly_32.5.0 JsSdk/2.0 NetType/WIFI Channel/App Store ByteLocale/en Region/US WKWebView/1 BytedanceWebview/d8a21c6",
      ),
      "tiktok",
      "TikTok",
      "ios",
    ],
    [
      "TikTok on Android",
      ANDROID(
        "Pixel 6 Build/TQ3A.230901.001",
        " trill_2023109040 JsSdk/1.0 NetType/WIFI Channel/googleplay AppName/trill app_version/31.9.4 BytedanceWebview/d8a21c6",
      ),
      "tiktok",
      "TikTok",
      "android",
    ],
    [
      "Snapchat on iOS",
      IOS("Snapchat/12.60.0.40 (like Safari/8617.2.4.10.8, panda)"),
      "snapchat",
      "Snapchat",
      "ios",
    ],
    [
      "Pinterest on Android",
      ANDROID("Pixel 8 Build/UD1A.230803.041", " [Pinterest/Android]"),
      "pinterest",
      "Pinterest",
      "android",
    ],
    ["LINE on iOS", IOS("Safari Line/13.20.0"), "line", "LINE", "ios"],
    [
      "WeChat on iOS",
      IOS("MicroMessenger/8.0.44(0x18002c2f) NetType/WIFI Language/en"),
      "wechat",
      "WeChat",
      "ios",
    ],
    [
      "another app's Android web view",
      ANDROID("Pixel 8 Build/UD1A.230803.041", ""),
      "webview",
      null,
      "android",
    ],
    [
      "LinkedIn on an iPad",
      "Mozilla/5.0 (iPad; CPU OS 17_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 [LinkedInApp]/9.29.6406",
      "linkedin",
      "LinkedIn",
      "ios",
    ],
    [
      "LinkedIn on neither phone OS",
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 LinkedInApp",
      "linkedin",
      "LinkedIn",
      "other",
    ],
  ] as const)("finds %s", (_, ua, app, name, os) => {
    expect(inAppBrowser(ua)).toEqual({ app, name, os });
  });

  it.each([
    [
      "Safari on iOS",
      "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
    ],
    [
      "Chrome on iOS",
      "Mozilla/5.0 (iPhone; CPU iPhone OS 17_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/120.0.6099.119 Mobile/15E148 Safari/604.1",
    ],
    [
      "Chrome on Android",
      "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
    ],
    [
      "Samsung Internet",
      "Mozilla/5.0 (Linux; Android 13; SAMSUNG SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/23.0 Chrome/115.0.0.0 Mobile Safari/537.36",
    ],
    [
      "Firefox on Android",
      "Mozilla/5.0 (Android 14; Mobile; rv:121.0) Gecko/121.0 Firefox/121.0",
    ],
    [
      "Chrome on a Mac",
      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
    ],
    [
      "Edge on Windows",
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
    ],
    ["X's link crawler", "Twitterbot/1.0"],
    ["nothing", ""],
  ])("leaves %s alone", (_, ua) => {
    expect(inAppBrowser(ua)).toBeNull();
  });
});

describe("chromeIntent", () => {
  it("opens the same page, query and all, in Chrome", () => {
    expect(
      chromeIntent(
        new URL("https://foundry.helix-db.com/login?return_to=%2Fask&x=1#h"),
      ),
    ).toBe(
      "intent://foundry.helix-db.com/login?return_to=%2Fask&x=1#Intent;scheme=https;package=com.android.chrome;end",
    );
  });

  it("keeps the port and scheme of a loopback site", () => {
    expect(chromeIntent(new URL("http://localhost:3000/login"))).toBe(
      "intent://localhost:3000/login#Intent;scheme=http;package=com.android.chrome;end",
    );
  });
});
