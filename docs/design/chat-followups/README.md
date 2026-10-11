# Chat follow-ups and inline completion — design reference

Related implementation issue: [HelixDB/helix-foundry#72](https://github.com/HelixDB/helix-foundry/issues/72). See [requirements.md](requirements.md) for why, UX, ontology-backed generation, integration and production acceptance checks.

Open `index.html#followups` in a browser to show a completed example answer. The full Home composition is retained as context. Click Home to return. Existing scripted voice interaction is included for compatibility with #54; production requires GPT-Live (`gpt-live-1`) with actual tool calling/delegation.

## Interaction

- Up to three subtle pills below the latest completed answer. Clicking fills and focuses the composer, without submitting.
- Type `Which` after the customer answer to see an inline completion. Tab accepts; Enter sends the completed question once.
- After the traffic answer, the list changes to daily traffic, customer groups and weekly orders. Type `Compare` for the new contextual completion.
- Escape dismisses the current completion. Shift+Tab and Tab without a suggestion continue ordinary focus navigation. Suggestions only complete at the end of a draft with no selection; composition events are ignored by keyboard acceptance.
- Suggestions are hidden while the next answer is pending; prior pill rows are removed. Only the latest answer receives the new set.
- Explicit pill selection/typing stops scripted voice. A non-empty typed draft blocks voice activation until sent or cleared, preserving user text. Predictions do not enter microphone transcripts.
- Paper/olive and dark theme tokens are retained. Pills wrap at 390px; accessible labels contain the full question. The live orb remains above the chat composer.

## What is and is not implemented

This is a **standalone interactive HTML prototype**. Example candidates are grouped by customer, traffic and order context. Answers and voice transcripts are scripted. There are no backend requests, live ontology lookups, real microphone capture, model calls, tool calls or audio playback. Production generation/validation and GPT-Live remain in issues #72/#54. Do not copy the scripted timers or static catalog as production inference.

This PR is limited to the design reference and handoff. It does not modify production React/API code, connect data, reset Docker, or deploy the app.

## Verification performed

Browser checks covered three pills, pill selection with no submission, inline completion, Tab acceptance and Enter submission exactly once, Escape dismissal, Shift+Tab, ordinary Tab with an unmatched draft, mid-text caret behavior, pending suppression and contextual refresh. Checked a 390px phone layout without horizontal overflow, both light/dark themes, font alignment and accessible question descriptions. No browser errors during these interactions. Additional check: starting voice cannot automatically send a prepared suggested draft, and selecting a pill ends voice before changing the draft.

JavaScript syntax checked with Node's `vm.Script`. These checks verify prototype interactions; they do not establish real ontology ranking, real voice latency, backend security, actual IME platform behavior or server integration. Those require the production acceptance checks in the linked issue.

## Views

![Three follow-up pills and Tab completion](desktop.jpg)

![390px phone layout with wrapping pills](mobile.jpg)

![Dark theme with contextual inline completion](dark.jpg)
