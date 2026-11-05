import type {
  HealFindingResponse,
  ListHealFindingsResponse,
} from "../../../shared/wfe";
import { api } from "../../api";

// The Suggestions page's routes: the ontology's shown findings and its
// latest audit, and dismissing one.
const enc = encodeURIComponent;
const base = (ws: string, o: string) =>
  `/v1/workspaces/${enc(ws)}/foundry/ontologies/${enc(o)}/heal-findings`;

export const suggestionQueries = {
  list: (ws: string, o: string) => ({
    queryKey: [ws, "ontology", o, "heal-findings"],
    staleTime: 30_000,
    queryFn: () => api<ListHealFindingsResponse>(base(ws, o)),
  }),
};

export const dismissSuggestion = (ws: string, o: string, fingerprint: string) =>
  api<HealFindingResponse>(`${base(ws, o)}/${enc(fingerprint)}:dismiss`, {});
