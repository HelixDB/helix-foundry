import {
  Analyst,
  Behaviours,
  Connect,
  Explorer,
  Scene,
  Schema,
} from "./figures";

// The hosted site's landing page: static markup, with React rendering only
// the four product figures and the grid of connectors, and running the field
// drawn behind it and the page's small behaviours (./figures). Every way in to the app goes through
// sign-in (/login), which a signed-in visitor never sees: / sends them to
// the app (src/app/(landing)/page.tsx).
const SIGN_IN = "/login";
const SOURCE = "https://github.com/helixdb/helix-foundry";
const DOCS = `${SOURCE}#readme`;
const SETUP = `git clone ${SOURCE}.git
cd helix-foundry && docker compose up -d --wait`;

export function Landing() {
  return (
    <>
      {/* One drawing behind the whole page: the graph in the hero, the dots
          behind the schema and the records in the explorer. */}
      <canvas className="field" data-field-canvas aria-hidden="true" />

      <div className="page">
        <header className="nav">
          <div className="wrap">
            <a
              className="mark"
              href="#top"
              aria-label="Helix Foundry, back to the top"
            >
              <img
                src="/landing/logo-dark.svg"
                alt="Helix Foundry"
                width={432}
                height={80}
              />
            </a>
            <nav className="tabs m">
              <a href="#schema">
                Ontology <i>01</i>
              </a>
              <a href="#how">
                Analyst <i>02</i>
              </a>
              <a href="#cloud">
                Cloud <i>03</i>
              </a>
            </nav>
            <div className="nav-right m">
              <a href={DOCS}>Docs</a>
              <a href={SOURCE}>Source</a>
              <a className="btn btn-solid" href={SIGN_IN}>
                Start free
              </a>
            </div>
          </div>
        </header>

        <main id="top">
          <section className="hero" data-field="hero">
            <div className="scrim" aria-hidden="true" />
            <div className="wrap">
              <h1>Your company&#8217;s data, connected into one ontology</h1>
              <p className="sub">
                Connect your database, Stripe, WorkOS, PostHog and your files.
                Foundry suggests one ontology across them that you explore and
                ask questions of.
              </p>
              <div className="cta">
                <div className="row">
                  <a className="btn btn-solid" href="#cloud">
                    Talk to Sales
                  </a>
                  <a className="btn" href={SIGN_IN}>
                    Start on Helix Cloud
                  </a>
                </div>
                <button className="setup" type="button" data-copy="setup-text">
                  <span className="setup-head">
                    <span className="label">Copy local setup</span>
                    <svg
                      className="i-copy"
                      viewBox="0 0 24 24"
                      aria-hidden="true"
                    >
                      <rect x="8" y="8" width="13" height="13" rx="2" />
                      <path d="M4 16V5a1 1 0 0 1 1-1h11" />
                    </svg>
                    <svg
                      className="i-check"
                      viewBox="0 0 24 24"
                      aria-hidden="true"
                    >
                      <path d="M5 12.5l4.5 4.5L19 7.5" />
                    </svg>
                  </span>
                  <span className="cmd">
                    $ <b>git clone github.com/helixdb/helix-foundry.git</b>
                    <br />${" "}
                    <b>
                      cd helix-foundry &amp;&amp; docker compose up -d --wait
                    </b>
                  </span>
                </button>
              </div>
              <pre id="setup-text" hidden>
                {SETUP}
              </pre>
            </div>
          </section>

          <div className="wrap stack">
            <section className="box" id="schema">
              <div className="pad">
                <p className="m">Schema</p>
                <h2 style={{ marginTop: ".7rem" }}>
                  A suggested ontology that matches the same records across
                  sources.
                </h2>
              </div>
              <figure
                className="demo site-schema"
                id="schema-root"
                aria-label="The suggested ontology for Loopwell, a sample company: eight types drawn from eleven tables in PlanetScale, WorkOS, Stripe, PostHog and S3, and the links between them."
              >
                <Schema />
              </figure>
            </section>

            <section className="box" id="explorer">
              <div className="pad">
                <p className="m">Explorer</p>
                <h2 style={{ marginTop: ".7rem" }}>
                  Everything a load wrote: its entities and facts.
                </h2>
                <p className="prose" style={{ marginTop: ".7rem" }}>
                  Open a record to see its properties and every relationship, in
                  and out, with the load and the version that wrote it.
                </p>
              </div>
              <figure
                className="demo site-explorer"
                id="explorer-root"
                role="img"
                aria-label="A demo of the Explorer stepping through Loopwell's records: an account, one of its users, the support ticket they raised, the account's subscription and invoice, and the campaign it came from, each with its properties and links."
              >
                <Explorer />
              </figure>
            </section>

            <section className="box split" id="how">
              <div className="cell up">
                <div className="stage">
                  <div className="site-scene" id="scene-root">
                    <Scene />
                  </div>
                </div>
                <div className="lead-block">
                  <div>
                    <h2>
                      <span>01</span>Ontology
                    </h2>
                    <p className="say">
                      One model of your business that every app builds on.
                    </p>
                  </div>
                </div>
              </div>

              <div className="cell up">
                <div className="stage">
                  <div
                    className="site-analyst"
                    id="analyst-root"
                    data-field="analyst"
                    role="img"
                    aria-label="A demo of the Analyst answering three questions about Loopwell, a sample company: support tickets by month, monthly revenue by plan, and the accounts that raise the most tickets. Each answer comes with a chart and the datasets it read."
                  >
                    <Analyst />
                  </div>
                </div>
                <div className="lead-block">
                  <div>
                    <h2>
                      <span>02</span>Analyst
                    </h2>
                    <p className="say">
                      Ask in plain words, and get an answer you can check.
                    </p>
                  </div>
                </div>
              </div>
            </section>

            <section className="box shout pad is-filled" data-field="shout">
              <p className="m" style={{ marginBottom: "1rem" }}>
                The point
              </p>
              <h2>Nobody has to agree on a schema first.</h2>
              <p style={{ marginTop: ".9rem" }}>
                Connect a source. Foundry suggests the ontology, you keep or
                change it, and nothing is published until you say so.
              </p>
              <div className="row" style={{ marginTop: "1.4rem" }}>
                <a className="btn" href={SIGN_IN}>
                  Start on Helix Cloud
                </a>
              </div>
            </section>

            <section className="box split connect">
              <div className="pad">
                <h2>
                  Connect the data you already run on, and the models
                  you&#8217;re already approved to use.
                </h2>
                <p className="prose" style={{ marginTop: ".8rem" }}>
                  Bring your own keys for Claude, OpenAI, Azure or Bedrock, any
                  OpenAI-compatible endpoint, or a model that runs locally.
                  Self-hosted, your data stays on your own infrastructure, and
                  columns that look sensitive are masked before samples reach a
                  hosted model, so you stay within the policies you already
                  have.
                </p>
              </div>
              <figure
                className="pad site-marks"
                id="connect-root"
                aria-label="Every connector Foundry has, and the AI it works with"
              >
                <Connect />
              </figure>
            </section>

            <section className="box close" id="cloud">
              <div className="pad">
                <h2>
                  Now there is nothing to host.{" "}
                  <span>
                    Helix Cloud runs the same build we publish, so you connect a
                    source and explore the ontology without standing up HelixDB,
                    DuckDB and Ollama yourself.
                  </span>
                </h2>
                <div className="row">
                  <a className="btn btn-solid" href={SIGN_IN}>
                    Start on Helix Cloud
                  </a>
                  <a className="btn" href={SOURCE}>
                    Read the self-hosted setup
                  </a>
                </div>
                <p
                  className="prose m"
                  style={{ marginTop: "1.6rem", maxWidth: "44rem" }}
                >
                  Self-hosted Foundry listens on 127.0.0.1 only and has no
                  sign-in: anyone on that machine is already in.
                </p>
              </div>
            </section>
          </div>
        </main>

        <footer className="foot m" data-field="floor">
          <canvas className="foot-graph" aria-hidden="true" />
          <div className="wrap">
            <span>
              Helix Foundry is Apache 2.0 on your own machine, and Helix Cloud
              when you would rather we ran it.
            </span>
            <nav>
              <a href={SOURCE}>GitHub</a>
              <a href={DOCS}>Docs</a>
              <a href="https://docs.helix-db.com">HelixDB</a>
            </nav>
          </div>
        </footer>
      </div>

      <Behaviours />
    </>
  );
}
