import {
  AppBridge,
  PostMessageTransport,
} from "@modelcontextprotocol/ext-apps/app-bridge";

// A host for an MCP App view in a test page, as Claude, ChatGPT or T3 Code
// draws one with the SDK's own AppBridge: the view in a sandboxed frame of
// an origin of its own, sized to the height it reports, handed a tool's
// input and then its result, its links recorded instead of opened.
// tests/e2e/mcp-apps.spec.ts bundles it into its page.

type Show = {
  html: string;
  input: Record<string, unknown>;
  // The tool's result, or none while the tool runs.
  result?: Record<string, unknown>;
  theme: "light" | "dark";
};

declare global {
  interface Window {
    mcpHost: { show: (s: Show) => Promise<void>; opened: string[] };
  }
}

window.mcpHost = {
  opened: [],
  async show({ html, input, result, theme }) {
    const frame = document.createElement("iframe");
    frame.title = "MCP App";
    frame.setAttribute("sandbox", "allow-scripts");
    frame.style.cssText = "display:block;width:720px;height:80px;border:0";
    document.body.replaceChildren(frame);
    const bridge = new AppBridge(
      null,
      { name: "test host", version: "1.0.0" },
      { openLinks: {} },
      { hostContext: { theme, displayMode: "inline", platform: "web" } },
    );
    bridge.onopenlink = async ({ url }) => {
      window.mcpHost.opened.push(url);
      return {};
    };
    bridge.onsizechange = ({ height }) => {
      if (height) frame.style.height = `${height}px`;
    };
    bridge.oninitialized = () => {
      void bridge.sendToolInput({ arguments: input });
      if (result) void bridge.sendToolResult(result as never);
    };
    // The frame's window outlives its navigation to the view: listen first,
    // so the view's handshake is heard.
    await bridge.connect(
      new PostMessageTransport(frame.contentWindow!, frame.contentWindow!),
    );
    frame.srcdoc = html;
  },
};
