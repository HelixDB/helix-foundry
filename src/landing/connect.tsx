import { useEffect, useRef, useState, type CSSProperties } from "react";

type Mark = { name: string; logo: string };
// The products Foundry connects to, then the AI it works with. The all-black
// PlanetScale and OpenAI marks are the site's white copies, and the marks
// the app doesn't have are the site's own (public/landing).
const MARKS: Mark[] = [
  { name: "PostgreSQL", logo: "/connectors/postgresql.svg" },
  { name: "MySQL", logo: "/landing/connectors/mysql.svg" },
  { name: "Neo4j", logo: "/landing/connectors/neo4j.svg" },
  { name: "Neon", logo: "/connectors/neon.svg" },
  { name: "Supabase", logo: "/connectors/supabase.svg" },
  { name: "PlanetScale", logo: "/landing/connectors/planetscale-white.svg" },
  { name: "Stripe", logo: "/connectors/stripe.svg" },
  { name: "WorkOS", logo: "/connectors/workos.svg" },
  { name: "PostHog", logo: "/landing/connectors/posthog.svg" },
  { name: "Amazon S3", logo: "/connectors/s3.svg" },
  { name: "OpenAI", logo: "/landing/providers/openai-white.svg" },
  { name: "Claude", logo: "/providers/claude.svg" },
  { name: "Amazon Bedrock", logo: "/landing/providers/bedrock.svg" },
  { name: "Azure OpenAI", logo: "/landing/providers/azure.svg" },
  { name: "Gemini", logo: "/landing/providers/gemini.svg" },
];

// A placard like the setup scene's tiles: a small isometric block with the
// mark lying on its top. Its top face is centred on (42, 24) in a 84 by 60
// drawing; the mark is drawn flat, then laid onto the face.
const C = Math.cos(Math.PI / 6),
  W = 22,
  DEPTH = 8;
const corner = (u: number, v: number, z = 0) =>
  `${42 + (u - v) * C} ${24 + (u + v) / 2 + z}`;
const face = (...ps: string[]) => `M${ps.join("L")}Z`;
function Placard({ logo }: { logo: string }) {
  return (
    <svg className="placard" viewBox="0 0 84 60" aria-hidden>
      <path
        className="placard-l"
        d={face(
          corner(-W, W),
          corner(W, W),
          corner(W, W, DEPTH),
          corner(-W, W, DEPTH),
        )}
      />
      <path
        className="placard-r"
        d={face(
          corner(W, W),
          corner(W, -W),
          corner(W, -W, DEPTH),
          corner(W, W, DEPTH),
        )}
      />
      <path
        className="placard-t"
        d={face(corner(-W, -W), corner(W, -W), corner(W, W), corner(-W, W))}
      />
      <path
        className="placard-rim"
        d={face(
          corner(4 - W, 4 - W),
          corner(W - 4, 4 - W),
          corner(W - 4, W - 4),
          corner(4 - W, W - 4),
        )}
      />
      <g transform={`matrix(${C} 0.5 ${-C} 0.5 42 24)`}>
        <image href={logo} x={-13} y={-13} width={26} height={26} />
      </g>
    </svg>
  );
}

// The placards rise into place the first time the grid comes into view, and
// then stay. A green wave crosses them now and then, corner to corner: each
// lights up as the setup scene's plates do while they work (connect.css).
export function Connect() {
  const root = useRef<HTMLUListElement>(null);
  const [shown, setShown] = useState(false);
  useEffect(() => {
    const watcher = new IntersectionObserver(
      ([entry]) => {
        if (!entry.isIntersecting) return;
        setShown(true);
        watcher.disconnect();
      },
      { rootMargin: "0px 0px -8% 0px", threshold: 0.05 },
    );
    watcher.observe(root.current!);
    return () => watcher.disconnect();
  }, []);
  return (
    <ul ref={root} className={`marks${shown ? " is-in" : ""}`}>
      {MARKS.map(({ name, logo }, i) => (
        <li
          key={name}
          style={
            { "--i": i, "--d": (i % 5) + Math.floor(i / 5) } as CSSProperties
          }
        >
          <Placard logo={logo} />
          <span>{name}</span>
        </li>
      ))}
    </ul>
  );
}
