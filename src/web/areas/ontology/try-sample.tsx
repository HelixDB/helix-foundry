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
      text="Watch Foundry answer a question about a made-up company’s data."
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
