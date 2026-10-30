import {
  Braces,
  Database,
  FileUp,
  Globe2,
  Workflow,
  type LucideIcon,
  Waypoints,
} from "lucide-react";
import type { Theme } from "../../shared/theme";
import { useCurrentTheme } from "../shell/theme";
import { cx } from "./util";

// Same logo files and names the onboarding connector cards use.
const logoFiles: Record<string, string> = {
  planetscale: "planetscale",
  workos: "workos",
  posthog: "posthog",
  github: "github",
  linear: "linear",
  calcom: "calcom",
  granola: "granola",
  slack: "slack",
  notion: "notion",
  google: "google",
  stripe: "stripe",
  s3: "s3",
  postgres: "postgresql",
  mysql: "mysql-wordmark",
  neon: "neon",
  supabase: "supabase",
};
const labels: Record<string, string> = {
  planetscale: "PlanetScale",
  workos: "WorkOS",
  posthog: "PostHog",
  github: "GitHub",
  linear: "Linear",
  calcom: "Cal.com",
  granola: "Granola",
  slack: "Slack",
  notion: "Notion",
  google: "Google Workspace",
  stripe: "Stripe",
  s3: "Amazon S3",
  events: "API source",
  postgres: "PostgreSQL",
  mysql: "MySQL",
  neo4j: "Neo4j",
  neon: "Neon",
  supabase: "Supabase",
  upload: "Files",
  rest: "REST API",
  api: "Ingestion API",
  pipeline: "Pipeline output",
};
const generic: Record<string, LucideIcon> = {
  events: Braces,
  neo4j: Waypoints,
  upload: FileUp,
  rest: Globe2,
  api: Braces,
  pipeline: Workflow,
};
const aliases: Record<string, string> = {
  postgresql: "postgres",
  "amazon s3": "s3",
  "blob storage": "s3",
  files: "upload",
  file: "upload",
  "rest api": "rest",
  "ingestion api": "api",
};

// Logos drawn for light backgrounds that have a variant for dark ones,
// <file>-dark.svg, whose dark parts are light (public/connectors/README.md).
const darkLogos = new Set([
  "planetscale",
  "mysql-wordmark",
  "posthog",
  "github",
  "calcom",
  "granola",
  "slack",
  "notion",
]);
/** The URL of a logo file in public/connectors, in the page's theme. */
export const logoUrl = (file: string, theme: Theme) =>
  `/connectors/${file}${theme === "dark" && darkLogos.has(file) ? "-dark" : ""}.svg`;

// Normalises connector names and kinds ("PostgreSQL", "Stripe") to a key.
export function connectorKey(...candidates: unknown[]): string {
  for (const c of candidates) {
    if (typeof c !== "string" || !c) continue;
    const k = c.toLowerCase();
    if (aliases[k]) return aliases[k];
    if (labels[k]) return k;
    const byLabel = Object.keys(labels).find(
      (key) => labels[key].toLowerCase() === k,
    );
    if (byLabel) return byLabel;
  }
  const first = candidates.find((c) => typeof c === "string" && c);
  return typeof first === "string" ? first.toLowerCase() : "upload";
}
export const connectorLabel = (key: string) =>
  labels[connectorKey(key)] || key.charAt(0).toUpperCase() + key.slice(1);

export function SourceLogo({
  source,
  size = 20,
  showLabel = false,
  className,
}: {
  // Connector key or name: "stripe", "postgres", "PostgreSQL", "upload"...
  source: string;
  size?: number;
  showLabel?: boolean;
  className?: string;
}) {
  const theme = useCurrentTheme();
  const key = connectorKey(source),
    file = logoFiles[key],
    label = connectorLabel(key),
    Icon = generic[key] || Database;
  const logo = (
    <span
      className={cx(
        "hf-logo",
        key === "mysql" && "is-wordmark",
        !file && "is-generic",
        !showLabel && className,
      )}
      style={{ width: size, height: size }}
      title={showLabel ? undefined : label}
      role={showLabel ? undefined : "img"}
      aria-label={showLabel ? undefined : label}
    >
      {file ? (
        <img src={logoUrl(file, theme)} alt="" />
      ) : (
        <Icon size={Math.round(size * 0.6)} aria-hidden />
      )}
    </span>
  );
  return showLabel ? (
    <span className={cx("hf-logo-label", className)}>
      {logo}
      <span>{label}</span>
    </span>
  ) : (
    logo
  );
}

// Overlapping logos, e.g. every connector in a workspace.
export function SourceStack({
  sources,
  max = 5,
  size = 22,
}: {
  sources: string[];
  max?: number;
  size?: number;
}) {
  const unique = [...new Set(sources.map((s) => connectorKey(s)))];
  const shown = unique.slice(0, max),
    rest = unique.length - shown.length;
  return (
    <span
      className="hf-logo-stack"
      role="img"
      aria-label={unique.map(connectorLabel).join(", ")}
    >
      {shown.map((s) => (
        <SourceLogo key={s} source={s} size={size} />
      ))}
      {rest > 0 && (
        <span className="hf-logo hf-logo-more" style={{ height: size }}>
          +{rest}
        </span>
      )}
    </span>
  );
}
