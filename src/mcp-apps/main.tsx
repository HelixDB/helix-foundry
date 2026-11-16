import "../web/workspace.css";
import "../web/kit/kit.css";
import "../web/areas/ontology/ontology.css";
import "../web/areas/ontology/ask.css";
import "../web/areas/ontology/ask-visual.css";
import "./answer.css";
import { createRoot } from "react-dom/client";
import { App } from "@modelcontextprotocol/ext-apps";
import { AnswerView } from "./answer";
import { answerCard, type AnswerCard } from "./answer-model";

// The answer view's entry (backend/integration: answerAppURI). The host
// hands it the tool's input as the question is asked and the tool's result
// once it returns, and its theme; links open through the host. Built into one
// HTML document by scripts/mcp-apps.mjs.

const app = new App(
  { name: "Helix Foundry", version: "1.0.0" },
  {},
  { autoResize: true },
);
const root = createRoot(document.getElementById("root")!);
let card: AnswerCard | undefined, asking: string | undefined;

const render = () =>
  root.render(
    <AnswerView
      card={card}
      asking={asking}
      open={(url) => void app.openLink({ url })}
    />,
  );
// The host's light or dark, as the website's own choice (data-theme).
const theme = (t?: string) => {
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
};

// Handlers first: the host may send these as soon as the handshake ends.
app.ontoolinput = ({ arguments: input }) => {
  if (typeof input?.question === "string") asking = input.question;
  render();
};
app.ontoolresult = (result) => {
  card = answerCard(result);
  render();
};
app.onhostcontextchanged = (context) => theme(context.theme);

render();
void app.connect().then(() => theme(app.getHostContext()?.theme));
