import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { ArrowRight, Search, X } from "lucide-react";
import { number } from "./vendor/web/api";
import { attach } from "./field/field";
import { demo } from "./field/demo";
import { hue, neighbours, records, totals, types } from "./field/records";

// The Explorer's graph view with the same bar, legend and record panel as the
// app's ObjectGraph. It is a demo, not a control: the field draws the records
// and a pointer that opens one after another, and this panel follows it.
// The figure it is rendered in is marked live once the records have landed.
export function Explorer() {
  const stage = useRef<HTMLDivElement>(null);
  const [selected, setSelected] = useState(-1);
  useEffect(() => demo.subscribe(setSelected), []);
  useLayoutEffect(() => {
    attach("explorer", stage.current, stage.current!.closest("figure"));
    return () => attach("explorer", null);
  }, []);

  const counts = new Map<string, number>();
  for (const r of records) counts.set(r.type, (counts.get(r.type) || 0) + 1);
  const current = selected >= 0 ? records[selected] : undefined;
  // Linked records by type and link, e.g. "User · belongs to" on an account.
  const grouped = current
    ? [
        ...neighbours[selected]
          .reduce((m, nb) => {
            const key = records[nb.index].type + "\u0000" + nb.name;
            if (!m.has(key)) m.set(key, []);
            m.get(key)!.push(nb.index);
            return m;
          }, new Map<string, number[]>())
          .entries(),
      ].map(([key, list]) => {
        const [type, name] = key.split("\u0000");
        const verb = [type, current.type].reduce(
          (v, t) =>
            v.toLowerCase().startsWith(t.toLowerCase() + " ")
              ? v.slice(t.length + 1)
              : v,
          name,
        );
        return { key, type, verb, list };
      })
    : [];

  return (
    <div className="og" aria-hidden>
      <div className="og-bar">
        <div className="og-search">
          <Search size={14} />
          <span>Find a record</span>
        </div>
        <ul className="og-legend">
          {types.map((t) => (
            <li key={t}>
              <span
                className={`og-type${current?.type === t ? " is-current" : ""}`}
              >
                <span className="og-dot" style={{ background: hue(t) }} />
                {t}
                <span className="og-count">{number(counts.get(t))}</span>
              </span>
            </li>
          ))}
        </ul>
      </div>
      <p className="og-scope">
        {number(records.length)} of {number(totals.records)} records: ten
        accounts and the records around them. Double-click a record to see
        everything around it.
      </p>
      <div className="og-stage" ref={stage}>
        <canvas className="og-canvas" aria-hidden />
        {current && (
          <aside className="og-panel" key={selected}>
            <header>
              <span
                className="og-dot"
                style={{ background: hue(current.type) }}
              />
              <div>
                <h2>{current.label}</h2>
                <p>
                  {current.type} {current.key} · {current.degree}{" "}
                  {current.degree === 1 ? "link" : "links"}
                </p>
              </div>
              <X size={14} className="og-close" />
            </header>
            <div className="og-actions">
              <span className="og-button is-primary">
                Open record <ArrowRight size={13} />
              </span>
              <span className="og-button">Show what’s around it</span>
            </div>
            <section className="og-props">
              <h3>Properties</h3>
              <dl>
                {current.props.map(([k, v]) => (
                  <div key={k}>
                    <dt>{k}</dt>
                    <dd>{v}</dd>
                  </div>
                ))}
              </dl>
            </section>
            {grouped.map(({ key, type, verb, list }) => (
              <section key={key}>
                <h3>
                  {type}{" "}
                  <span>
                    · {verb} · {list.length}
                  </span>
                </h3>
                <ul>
                  {list.slice(0, 5).map((i) => (
                    <li key={i}>
                      <span
                        className="og-dot"
                        style={{ background: hue(records[i].type) }}
                      />
                      <span className="og-match-name">{records[i].label}</span>
                      <small>{records[i].type}</small>
                    </li>
                  ))}
                  {list.length > 5 && (
                    <li className="og-more">and {list.length - 5} more</li>
                  )}
                </ul>
              </section>
            ))}
          </aside>
        )}
      </div>
    </div>
  );
}
