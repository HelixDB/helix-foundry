import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { FlaskConical } from "lucide-react";
import { trackEvent } from "../../analytics";
import { Button } from "../../kit";
import { paths } from "../../paths";
import { AiGate, useAiConfigured } from "../workspace/ai-gate";
import { Callout } from "./parts";

// "Start with a sample company", the first choice of a new workspace's
// onboarding (./new-ontology.tsx): it opens
// the sample's own page (./sample-tour.tsx), which connects Acme Corp's
// database (a local install's acme-db, or the stamp's) and shows each step.
// The page uploads the sample's documents, which needs an AI provider: until
// one is set up, the card says that instead.
export function TrySample() {
  const navigate = useNavigate(),
    aiReady = useAiConfigured();
  const [gated, setGated] = useState(false);
  if (gated && aiReady === false) return <AiGate action="Adding the sample" />;
  return (
    <Callout
      icon={FlaskConical}
      title="Start with a sample company"
      text="Acme Corp is a made-up software company: its customers, subscriptions, invoices and support tickets in a PostgreSQL database, and its contracts and call notes as documents. Watch Foundry turn them into an ontology and answer a question about it."
      action={
        <Button
          variant="primary"
          onClick={() => {
            trackEvent("sample_clicked");
            if (aiReady === false) setGated(true);
            else navigate(paths.sampleTour());
          }}
        >
          See it on a sample company (1 min)
        </Button>
      }
    />
  );
}
