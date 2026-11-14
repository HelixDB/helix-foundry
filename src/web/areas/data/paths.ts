// Data area URLs, merged into `paths` (src/web/paths.ts). Owned by the data
// area; do not import ../../paths here (it imports this file).
const enc = encodeURIComponent;
// The history state a link to the picker carries when it pushes an entry of
// its own: closing the picker then goes back to where it was opened (the
// Data page, a connection's page, any page the palette was on), and Back
// closes it. Without it (an agent's link, a reload) closing replaces the
// entry with /data.
export const PICKER_OPENED = { picker: true } as const;
export const pickerOpened = (state: unknown) =>
  !!state && (state as { picker?: unknown }).picker === true;
export const dataPaths = {
  data: () => "/data",
  // Connections are cards on the Data page; ?add=1 opens the picker and
  // ?add=<kind> that kind's form (the Helix MCP's connector links, and the
  // palette's New API source, ?add=events).
  connections: (add: boolean | string = false) =>
    "/data" + (add === true ? "?add=1" : add ? "?add=" + enc(add) : ""),
  connection: (connectorId: string) => "/data/connections/" + enc(connectorId),
  syncedTable: (connectorId: string, table: string) =>
    "/data/connections/" + enc(connectorId) + "/tables/" + enc(table),
  // A document of the workspace: its text, its set and the ontologies that
  // read it. The documents themselves are a folder of the Data page.
  dataDocument: (documentId: string) => "/data/documents/" + enc(documentId),
};
