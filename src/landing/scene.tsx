import { useEffect, useRef, useState } from "react";
import { AiWorkingScene, type SceneStage } from "./vendor/web/ontology-scene";
import data from "./data/loopwell-ontology.json";

// Fig. 1: the top of the scene setup shows while it builds an ontology, with
// Loopwell's types taking shape on the ontology and feeding the analytics,
// workflows and integrations above it. The data and models plates under it
// are hidden (styles/scene.css).
const ENTITIES = data.ontology.entities.map((e) => e.name);
// The stages, with how long each holds.
const STEPS: [SceneStage, number][] = [
  ["reading", 1200],
  ["matching", 1600],
  ["proposing", 1600],
  ["describing", 2000],
  ["done", 3200],
];

// Plays through its stages while it is on screen, then starts again from the
// top, drawn afresh so its entrances play again too. With reduced motion it
// shows the finished ontology.
export function Scene() {
  const root = useRef<HTMLDivElement>(null);
  const calm = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const [seen, setSeen] = useState(calm);
  const [visible, setVisible] = useState(false);
  const [step, setStep] = useState(calm ? STEPS.length - 1 : 0);
  const [round, setRound] = useState(0);
  useEffect(() => {
    const watcher = new IntersectionObserver(
      ([entry]) => {
        setVisible(entry.isIntersecting);
        if (entry.isIntersecting) setSeen(true);
      },
      { threshold: 0.3 },
    );
    watcher.observe(root.current!);
    return () => watcher.disconnect();
  }, []);
  useEffect(() => {
    if (calm || !visible) return;
    const timer = setTimeout(() => {
      if (step < STEPS.length - 1) setStep(step + 1);
      else {
        setStep(0);
        setRound((r) => r + 1);
      }
    }, STEPS[step][1]);
    return () => clearTimeout(timer);
  }, [calm, visible, step]);
  return (
    <div ref={root} className="site-scene-frame">
      {seen && (
        <AiWorkingScene
          key={round}
          sources={[]}
          entities={ENTITIES}
          stage={STEPS[step][0]}
          label={`Foundry building Loopwell's ontology: ${ENTITIES.join(", ")} take shape, link up and are named, ready for analytics, workflows and integrations.`}
        />
      )}
    </div>
  );
}
