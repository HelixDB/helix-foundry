// Builds the sample company the new-ontology wizard offers
// (src/web/areas/ontology/sample.ts) into public/samples/kestrel, and its
// tables as PostgreSQL into local/kestrel/kestrel.sql, which a local
// install's kestrel-db serves (local/compose.yaml): Kestrel, a
// made-up field-service scheduling software company, as its own records
// would hold it. Six tables (accounts, contacts, subscriptions, invoices,
// support tickets, monthly usage) and nine documents (three contracts as
// PDFs, call notes, a support escalation, an incident report and the price
// list) that name the same accounts, people, tickets and invoices, so an
// ontology drafted from them has to link the documents to the rows.
//
// Every value comes from a seeded generator, so a run writes the same files
// (but for the PDFs' creation dates). The data is as of 27 October 2026.
// Every name, company and address is invented; emails use .example domains.
//
//   pnpm exec playwright install chromium   # the PDFs are Chromium's print
//   node scripts/sample-company.mjs

import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import process from "node:process";
import { chromium } from "@playwright/test";

const OUT = join(import.meta.dirname, "..", "public", "samples", "kestrel");
const SQL = join(import.meta.dirname, "..", "local", "kestrel", "kestrel.sql");
const TODAY = "2026-10-27";

// ---------------------------------------------------------------- Helpers

// mulberry32: small, seeded, and the same on every machine.
let seed = 20261027;
function rand() {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
}
const int = (lo, hi) => lo + Math.floor(rand() * (hi - lo + 1));
const between = (lo, hi) => lo + rand() * (hi - lo);
const pick = (xs) => xs[Math.floor(rand() * xs.length)];
/** One key of `weights`, chosen in proportion to its weight. */
function weighted(weights) {
  const entries = Object.entries(weights),
    total = entries.reduce((n, [, w]) => n + w, 0);
  let r = rand() * total;
  for (const [key, w] of entries) if ((r -= w) < 0) return key;
  return entries.at(-1)[0];
}

const date = (s) => new Date(s.slice(0, 10) + "T00:00:00Z");
const iso = (d) => d.toISOString().slice(0, 10);
const addDays = (s, n) => iso(new Date(date(s).getTime() + n * 86_400_000));
// A day before today at the latest.
const earlier = (s) => (s >= TODAY ? addDays(TODAY, -1) : s);
// Only ever from the 1st of a month, so no month overflows.
function addMonths(s, n) {
  const d = date(s);
  d.setUTCMonth(d.getUTCMonth() + n);
  return iso(d);
}
const monthsBetween = (a, b) =>
  (date(b).getUTCFullYear() - date(a).getUTCFullYear()) * 12 +
  date(b).getUTCMonth() -
  date(a).getUTCMonth();
const money = (n) => (Math.round(n * 100) / 100).toFixed(2);
const usd = (n) =>
  "$" +
  Number(n).toLocaleString("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });

function csv(rows) {
  const cols = Object.keys(rows[0]);
  const cell = (v) => {
    const s = v === null || v === undefined ? "" : String(v);
    return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  return (
    [
      cols.join(","),
      ...rows.map((r) => cols.map((c) => cell(r[c])).join(",")),
    ].join("\n") + "\n"
  );
}

// ---------------------------------------------------------------- Kestrel

const STAFF = {
  jordan: { name: "Jordan Reyes", email: "jordan.reyes@kestrel.example" },
  sam: { name: "Sam Whitfield", email: "sam.whitfield@kestrel.example" },
  aisha: { name: "Aisha Karimi", email: "aisha.karimi@kestrel.example" },
  leo: { name: "Leo Marchetti", email: "leo.marchetti@kestrel.example" },
};
const MANAGERS = Object.values(STAFF);
const PRICE = { Starter: 29, Growth: 49 };

// ---------------------------------------------------------------- Accounts

// The four accounts the documents are about, as they are.
const KEY = {
  pinecrest: {
    name: "Pinecrest Home Health",
    legal: "Pinecrest Home Health, Inc.",
    address: "2210 NW Raleigh Street, Portland, OR 97210, United States",
    industry: "Home healthcare",
    country: "United States",
    city: "Portland",
    employees: 1800,
    manager: STAFF.jordan,
    since: "2024-01-01",
    domain: "pinecresthh.example",
    sub: { plan: "Enterprise", seats: 420, price: 42, discount: 0, term: 12 },
    contacts: [
      ["Dana Okafor", "VP Operations", "Champion"],
      ["Miguel Serrano", "CFO", "Economic buyer"],
      ["Raj Patel", "IT Manager", "Admin"],
      ["Leah Whitcombe", "Scheduling Lead", "User"],
    ],
    usage: [366, 371, 368, 372, 298, 331],
  },
  halvorsen: {
    name: "Halvorsen Freight",
    legal: "Halvorsen Freight Ltd.",
    address: "4800 72 Avenue SE, Calgary, AB T2C 3H3, Canada",
    industry: "Freight and logistics",
    country: "Canada",
    city: "Calgary",
    employees: 2400,
    manager: STAFF.sam,
    since: "2024-02-01",
    domain: "halvorsenfreight.example",
    sub: { plan: "Enterprise", seats: 600, price: 38, discount: 0, term: 36 },
    contacts: [
      ["Ingrid Halvorsen", "COO", "Economic buyer"],
      ["Carla Mendes", "Dispatch Manager", "Champion"],
      ["Tom Becker", "IT Director", "Admin"],
    ],
    usage: [541, 549, 556, 563, 570, 584],
  },
  brightmoor: {
    name: "Brightmoor Facilities",
    legal: "Brightmoor Facilities Management Ltd",
    address: "14 Whitworth Street West, Manchester M1 5WG, United Kingdom",
    industry: "Facilities management",
    country: "United Kingdom",
    city: "Manchester",
    employees: 650,
    manager: STAFF.sam,
    since: "2024-12-01",
    domain: "brightmoorfm.example",
    sub: { plan: "Growth", seats: 120, price: 49, discount: 10, term: 12 },
    contacts: [
      ["Priya Natarajan", "Head of Operations", "Champion"],
      ["Gareth Lloyd", "Finance Director", "Economic buyer"],
      ["Owen Pryce", "Systems Administrator", "Admin"],
    ],
    usage: [104, 101, 96, 88, 80, 71],
  },
  sable: {
    name: "Sable Solar",
    industry: "Solar installation",
    country: "United States",
    city: "Phoenix",
    employees: 420,
    manager: STAFF.aisha,
    since: "2025-03-01",
    domain: "sablesolar.example",
    sub: { plan: "Growth", seats: 140, price: 49, discount: 0, term: 12 },
    contacts: [
      ["Marcus Bell", "Operations Director", "Champion"],
      ["Tessa Grant", "Controller", "Economic buyer"],
      ["Devon Price", "Field Supervisor", "User"],
    ],
    usage: [118, 121, 124, 126, 129, 131],
  },
};

const OTHERS = [
  ["Corbel HVAC Services", "HVAC"],
  ["Northgate Pest Control", "Pest control"],
  ["Riverside Elevator Co.", "Elevator maintenance"],
  ["Ashgrove Plumbing & Heating", "Plumbing"],
  ["Calder Telecom Installations", "Telecoms installation"],
  ["Fenwick Fire Protection", "Fire safety"],
  ["Greywater Environmental", "Environmental services"],
  ["Juniper Home Care", "Home healthcare"],
  ["Kettering Appliance Repair", "Appliance repair"],
  ["Larkspur Landscaping", "Landscaping"],
  ["Merrow Security Systems", "Security systems"],
  ["Oakhurst Roofing", "Roofing"],
  ["Pemberton Medical Equipment", "Medical equipment servicing"],
  ["Quarry Lane Electric", "Electrical contracting"],
  ["Redfern Waste Solutions", "Waste management"],
  ["Saltmarsh Marine Services", "Marine servicing"],
  ["Thornbury Locksmiths", "Locksmiths"],
  ["Upland Water Utilities", "Water utilities"],
  ["Vantage Lift Trucks", "Equipment rental"],
  ["Westbrook Cleaning Group", "Commercial cleaning"],
  ["Yarrow Mobile Veterinary", "Veterinary care"],
  ["Alder Glass & Glazing", "Glazing"],
  ["Bramble Pool Care", "Pool maintenance"],
  ["Cobalt Fiber Networks", "Telecoms installation"],
  ["Driftwood Moving Co.", "Moving and storage"],
  ["Elmstead Solar", "Solar installation"],
  ["Foxglove Home Nursing", "Home healthcare"],
  ["Granite Peak Generators", "Power generation servicing"],
  ["Hollis Garage Doors", "Garage doors"],
  ["Ironwood Tree Surgeons", "Arboriculture"],
  ["Kingfisher Irrigation", "Irrigation"],
  ["Lowmoor Boiler Services", "Heating"],
  ["Millbank Signage", "Signage installation"],
  ["Nettlefield Septic", "Septic services"],
  ["Orchard Street Clinics", "Community clinics"],
  ["Tidewater Plumbing Co.", "Plumbing"],
];
// Two the incident hit hard enough to name: never churned or past due.
const NAMED_IN_INCIDENT = ["Corbel HVAC Services", "Fenwick Fire Protection"];

const CITIES = {
  "United States": [
    "Denver",
    "Austin",
    "Columbus",
    "Raleigh",
    "Sacramento",
    "Tampa",
    "Minneapolis",
    "Boise",
  ],
  Canada: ["Toronto", "Edmonton", "Halifax", "Winnipeg"],
  "United Kingdom": ["Leeds", "Bristol", "Glasgow", "Norwich", "Cardiff"],
  Ireland: ["Dublin", "Cork"],
  Australia: ["Brisbane", "Perth", "Adelaide"],
};
const FIRST = [
  "Alex",
  "Bea",
  "Chris",
  "Dee",
  "Eamon",
  "Farah",
  "Glen",
  "Hattie",
  "Imran",
  "Jess",
  "Kofi",
  "Lena",
  "Mateo",
  "Nia",
  "Oscar",
  "Pia",
  "Quinn",
  "Rosa",
  "Seth",
  "Tara",
  "Umar",
  "Vera",
  "Will",
  "Xena",
  "Yusuf",
  "Zoe",
  "Bruno",
  "Carys",
  "Dmitri",
  "Elise",
  "Femi",
  "Greta",
  "Hugo",
  "Ines",
  "Jonah",
  "Kira",
];
const LAST = [
  "Abbott",
  "Brennan",
  "Castillo",
  "Doyle",
  "Ekwueme",
  "Fraser",
  "Gallagher",
  "Hughes",
  "Iqbal",
  "Jensen",
  "Kowalski",
  "Lindqvist",
  "Moreau",
  "Nakamura",
  "O'Neill",
  "Pereira",
  "Quigley",
  "Rasmussen",
  "Sato",
  "Thorne",
  "Underwood",
  "Varga",
  "Wilde",
  "Xu",
  "Yilmaz",
  "Zielinski",
  "Ahn",
  "Bianchi",
  "Chowdhury",
  "Delaney",
];
const TITLES = {
  Champion: [
    "Operations Manager",
    "Head of Operations",
    "Service Manager",
    "Field Operations Lead",
  ],
  "Economic buyer": [
    "CFO",
    "Finance Director",
    "Owner",
    "Managing Director",
    "Controller",
  ],
  Admin: ["IT Manager", "Office Manager", "Systems Administrator"],
  User: ["Dispatch Lead", "Scheduler", "Field Supervisor"],
};

const slug = (s) =>
  s
    .toLowerCase()
    .replace(/&/g, "and")
    .replace(/[^a-z0-9]+/g, " ")
    .trim();
const domainOf = (name) =>
  slug(name)
    .split(" ")
    .filter((w) => w !== "co")
    .slice(0, 2)
    .join("") + ".example";

const segment = (employees) =>
  employees < 200 ? "SMB" : employees < 1000 ? "Mid-market" : "Enterprise";

// Every account, its contacts and its subscriptions (oldest first), before
// ids are given.
const accounts = [
  ...Object.values(KEY).map((k) => ({ ...k, key: true })),
  ...OTHERS.map(([name, industry], i) => {
    const country = weighted({
      "United States": 50,
      "United Kingdom": 20,
      Canada: 15,
      Australia: 10,
      Ireland: 5,
    });
    const employees =
      Math.round(Math.exp(between(Math.log(25), Math.log(3000))) / 5) * 5;
    const since = addMonths("2022-03-01", int(0, 51));
    let plan =
      segment(employees) === "Enterprise"
        ? "Enterprise"
        : segment(employees) === "Mid-market" || rand() < 0.45
          ? "Growth"
          : "Starter";
    let seats = Math.round(employees * between(0.25, 0.55));
    if (plan === "Enterprise" && seats < 250) plan = "Growth";
    seats =
      plan === "Starter"
        ? Math.max(3, Math.min(25, seats))
        : plan === "Growth"
          ? Math.max(10, seats)
          : seats;
    const discount =
      plan === "Starter"
        ? 0
        : Number(
            weighted(
              plan === "Growth"
                ? { 0: 70, 5: 15, 10: 15 }
                : { 0: 30, 5: 30, 10: 30, 15: 10 },
            ),
          );
    return {
      name,
      industry,
      country,
      city: pick(CITIES[country]),
      employees,
      manager: MANAGERS[i % MANAGERS.length],
      since,
      domain: domainOf(name),
      sub: {
        plan,
        seats,
        price: PRICE[plan] ?? int(36, 44),
        discount,
        term: plan === "Enterprise" ? pick([12, 12, 24, 36]) : 12,
      },
    };
  }),
].sort(
  (a, b) => a.since.localeCompare(b.since) || a.name.localeCompare(b.name),
);

accounts.forEach((a, i) => (a.id = `ACC-${1001 + i}`));
const byName = Object.fromEntries(accounts.map((a) => [a.name, a]));
for (const [k, v] of Object.entries(KEY)) KEY[k] = byName[v.name];

// Churn, upgrades and late payers, among the accounts no document names.
const plain = accounts.filter(
  (a) => !a.key && !NAMED_IN_INCIDENT.includes(a.name),
);
const churned = plain
  .filter((a) => a.since < "2025-09-01")
  .slice(0, 9)
  .filter((_, i) => i % 3 === 0);
churned.forEach((a, i) => {
  a.canceledOn = ["2026-03-01", "2026-06-01", "2026-08-01"][i];
  a.cancelReason = [
    "Switched to a competitor",
    "Business closed",
    "Budget cuts",
  ][i];
});
const upgraded = plain
  .filter(
    (a) =>
      !a.canceledOn &&
      a.sub.plan === "Growth" &&
      a.since <= "2025-06-01" &&
      a.employees < 500,
  )
  .slice(0, 5);
for (const a of upgraded) a.upgradedOn = addMonths(a.since, int(4, 12));
const lateGenerated = plain.find(
  (a) => !a.canceledOn && !a.upgradedOn && a.sub.plan === "Starter",
);
lateGenerated.late = true;
KEY.brightmoor.late = true;

// ---------------------------------------------------------------- Contacts

const contacts = [];
for (const a of accounts) {
  const people =
    a.contacts ??
    (() => {
      const roles = ["Champion", "Economic buyer", "Admin", "User"].slice(
        0,
        a.employees < 200 ? int(2, 3) : int(3, 4),
      );
      const used = new Set();
      return roles.map((role) => {
        let name;
        do name = `${pick(FIRST)} ${pick(LAST)}`;
        while (used.has(name));
        used.add(name);
        return [name, pick(TITLES[role]), role];
      });
    })();
  a.people = people.map(([full_name, title, role]) => {
    const c = {
      contact_id: "",
      account_id: a.id,
      full_name,
      title,
      email:
        full_name
          .toLowerCase()
          .replace(/'/g, "")
          .replace(/[^a-z]+/g, ".") +
        "@" +
        a.domain,
      role,
    };
    contacts.push(c);
    return c;
  });
}
contacts.forEach((c, i) => (c.contact_id = `CON-${5001 + i}`));
const person = (a, name) => a.people.find((c) => c.full_name === name);

// ---------------------------------------------------------------- Subscriptions

const monthly = (s) => s.seats * s.price * (1 - s.discount / 100);
const subscriptions = [];
for (const a of accounts) {
  if (a.upgradedOn)
    subscriptions.push({
      account: a,
      plan: "Starter",
      seats: Math.min(25, Math.round(a.sub.seats * 0.6)),
      price: PRICE.Starter,
      discount: 0,
      term: 12,
      started_on: a.since,
      ended_on: a.upgradedOn,
      status: "canceled",
      cancel_reason: "Upgraded to Growth",
    });
  subscriptions.push({
    account: a,
    ...a.sub,
    started_on: a.upgradedOn ?? a.since,
    ended_on: a.canceledOn ?? "",
    status: a.canceledOn ? "canceled" : a.late ? "past_due" : "active",
    cancel_reason: a.cancelReason ?? "",
  });
}
subscriptions.sort(
  (a, b) =>
    a.started_on.localeCompare(b.started_on) ||
    a.account.id.localeCompare(b.account.id),
);
subscriptions.forEach((s, i) => {
  s.id = `SUB-${2001 + i}`;
  if (s.ended_on) return;
  // The current term's last day: terms run from the start, back to back.
  let end = s.started_on;
  while (end <= TODAY) end = addMonths(end, s.term);
  s.term_end = addDays(end, -1);
});
const subOf = (a) =>
  subscriptions.find(
    (s) => s.account === a && !s.cancel_reason.startsWith("Up"),
  );

// ---------------------------------------------------------------- Invoices

const ISSUE_MONTHS = Array.from({ length: 12 }, (_, i) =>
  addMonths("2025-11-01", i),
);
const invoices = [];
for (const s of subscriptions)
  for (const issued of ISSUE_MONTHS) {
    if (issued < s.started_on || (s.ended_on && issued >= s.ended_on)) continue;
    const quarterly = s.plan === "Enterprise";
    if (quarterly && monthsBetween(s.started_on, issued) % 3) continue;
    const due = addDays(issued, quarterly ? 30 : 14);
    const late = s.account.late && issued >= "2026-09-01";
    // Brightmoor paid September's late and holds October's.
    const [status, paid_on] =
      s.account === KEY.brightmoor && issued === "2026-09-01"
        ? ["paid", "2026-10-09"]
        : late && (s.account !== KEY.brightmoor || issued === "2026-10-01")
          ? ["overdue", ""]
          : due < TODAY || rand() < 0.5
            ? ["paid", earlier(addDays(issued, int(1, 20)))]
            : ["open", ""];
    invoices.push({
      sub: s,
      issued_on: issued,
      due_on: due,
      amount_usd: money(monthly(s) * (quarterly ? 3 : 1)),
      status,
      paid_on,
    });
  }
invoices.sort(
  (a, b) =>
    a.issued_on.localeCompare(b.issued_on) ||
    a.sub.account.id.localeCompare(b.sub.account.id),
);
invoices.forEach((v, i) => (v.id = `INV-${30001 + i}`));

// ---------------------------------------------------------------- Tickets

const SUBJECTS = {
  Scheduling: [
    "Recurring visits created twice",
    "Can't move a job to next week",
    "Technician availability not respected",
    "Calendar shows the wrong time zone",
    "Bulk reschedule times out",
  ],
  "Dispatch and routing": [
    "Route order looks wrong for the north crew",
    "Dispatch board slow to load in the morning",
    "Travel time estimates too short",
    "Emergency job not offered to the nearest technician",
  ],
  "Mobile app": [
    "App logs technicians out mid-shift",
    "Photos fail to upload on job completion",
    "Signature capture blank on Android",
    "Offline mode loses notes",
    "Push notifications arrive late",
  ],
  Integrations: [
    "QuickBooks sync duplicated customers",
    "Salesforce sync stopped overnight",
    "Xero invoices missing line items",
    "Webhook retries flooding our endpoint",
  ],
  Billing: [
    "Invoice shows the wrong seat count",
    "Need a copy of last quarter's invoices",
    "Update billing contact",
    "Charged for deactivated technicians",
  ],
  "SSO and sign-in": [
    "New hires can't sign in with SSO",
    "SAML assertion rejected after IdP change",
  ],
  Reporting: [
    "First-time fix rate report empty",
    "Export of completed jobs missing columns",
    "Utilisation dashboard doesn't match payroll",
  ],
};
const HOURS = {
  urgent: [1, 8],
  high: [4, 30],
  normal: [8, 96],
  low: [24, 200],
};
const stamp = (day, minutes) =>
  new Date(date(day).getTime() + minutes * 60_000).toISOString().slice(0, 16) +
  ":00Z";
const later = (at, hours) =>
  new Date(new Date(at).getTime() + hours * 3_600_000)
    .toISOString()
    .slice(0, 16) + ":00Z";

const tickets = [];
// A month's tickets, in proportion to seats. The documents' accounts have
// their sign-in tickets only as the documents say, and nobody's ticket but
// those the incident report names falls on the incident's day.
for (const a of accounts)
  for (const month of ISSUE_MONTHS) {
    const s = subscriptions.find(
      (x) =>
        x.account === a &&
        x.started_on <= month &&
        (!x.ended_on || month < x.ended_on),
    );
    if (!s) continue;
    const expected = 0.15 + s.seats / 400,
      count = Math.floor(expected) + (rand() < expected % 1 ? 1 : 0);
    for (let n = 0; n < count; n++) {
      const category = weighted({
        Scheduling: 25,
        "Dispatch and routing": 20,
        "Mobile app": 20,
        Integrations: 12,
        Billing: 8,
        ...(s.plan === "Starter" || a.key ? {} : { "SSO and sign-in": 5 }),
        Reporting: 10,
      });
      const day = addDays(month, int(0, month === "2026-10-01" ? 25 : 27));
      if (day === "2026-10-07") continue;
      const opened = stamp(day, int(14, 23) * 60 + int(0, 59));
      tickets.push({
        account: a,
        contact: pick(a.people),
        opened_at: opened,
        priority: weighted({ low: 20, normal: 55, high: 20, urgent: 5 }),
        category,
        subject: pick(SUBJECTS[category]),
      });
    }
  }
// What closes each: recent ones may still be open or waiting on the customer.
for (const t of tickets) {
  const recent = t.opened_at >= addDays(TODAY, -10);
  t.status = recent
    ? weighted({ open: 40, pending: 20, solved: 40 })
    : "solved";
  if (t.status === "solved") {
    t.closed_at = later(t.opened_at, int(...HOURS[t.priority]));
    if (t.closed_at > TODAY + "T23:59") t.status = "pending";
  }
}

// The tickets the documents name, as they were.
const named = (
  account,
  contactName,
  opened_at,
  priority,
  category,
  subject,
  status,
  closed_at = "",
) => {
  const t = {
    account,
    contact: person(account, contactName),
    opened_at,
    priority,
    category,
    subject,
    status,
    closed_at,
  };
  tickets.push(t);
  return t;
};
const { pinecrest, halvorsen, brightmoor, sable } = KEY;
const corbel = byName["Corbel HVAC Services"],
  fenwick = byName["Fenwick Fire Protection"];
const T = {
  sso1: named(
    pinecrest,
    "Raj Patel",
    "2026-08-05T13:58:00Z",
    "urgent",
    "SSO and sign-in",
    "Nurses can't sign in with SSO this morning",
    "solved",
    "2026-08-05T17:20:00Z",
  ),
  sso2: named(
    pinecrest,
    "Raj Patel",
    "2026-08-11T14:06:00Z",
    "high",
    "SSO and sign-in",
    "SAML login fails after certificate change",
    "solved",
    "2026-08-12T19:45:00Z",
  ),
  sso3: named(
    pinecrest,
    "Leah Whitcombe",
    "2026-08-19T13:31:00Z",
    "high",
    "SSO and sign-in",
    "SSO sign-in loop on the mobile app",
    "solved",
    "2026-08-21T16:10:00Z",
  ),
  audit: named(
    pinecrest,
    "Dana Okafor",
    "2026-09-30T16:22:00Z",
    "high",
    "Reporting",
    "Audit log export needed for state audit in January",
    "open",
  ),
  sable1: named(
    sable,
    "Devon Price",
    "2026-10-07T14:51:00Z",
    "urgent",
    "Dispatch and routing",
    "Crews seeing yesterday's schedule",
    "solved",
    "2026-10-07T17:40:00Z",
  ),
  sable2: named(
    sable,
    "Marcus Bell",
    "2026-10-07T15:20:00Z",
    "urgent",
    "Mobile app",
    "Job updates not reaching phones: 40 installs at risk",
    "pending",
  ),
  halvorsen1: named(
    halvorsen,
    "Carla Mendes",
    "2026-10-07T15:02:00Z",
    "high",
    "Dispatch and routing",
    "Dispatch board changes not reaching drivers",
    "solved",
    "2026-10-07T17:35:00Z",
  ),
  pinecrest1: named(
    pinecrest,
    "Leah Whitcombe",
    "2026-10-07T15:09:00Z",
    "urgent",
    "Mobile app",
    "Visit changes not syncing to nurses' phones",
    "solved",
    "2026-10-07T17:38:00Z",
  ),
  corbel1: named(
    corbel,
    corbel.people[0].full_name,
    "2026-10-07T15:33:00Z",
    "high",
    "Dispatch and routing",
    "Technicians not getting reassigned jobs",
    "solved",
    "2026-10-07T17:50:00Z",
  ),
  fenwick1: named(
    fenwick,
    fenwick.people[0].full_name,
    "2026-10-07T16:05:00Z",
    "high",
    "Mobile app",
    "Inspection jobs missing from the app",
    "solved",
    "2026-10-07T18:02:00Z",
  ),
  seats: named(
    brightmoor,
    "Priya Natarajan",
    "2026-10-02T09:14:00Z",
    "normal",
    "Billing",
    "Reduce to 80 seats at renewal",
    "pending",
  ),
  discount: named(
    brightmoor,
    "Gareth Lloyd",
    "2026-10-14T10:40:00Z",
    "normal",
    "Billing",
    "Will the 10% discount carry over at renewal?",
    "open",
  ),
  netsuite: named(
    halvorsen,
    "Tom Becker",
    "2026-10-09T16:12:00Z",
    "normal",
    "Integrations",
    "NetSuite integration timeline",
    "open",
  ),
};
tickets.sort((a, b) => a.opened_at.localeCompare(b.opened_at));
tickets.forEach((t, i) => (t.id = `T-${4101 + i}`));

// ---------------------------------------------------------------- Usage

const USAGE_MONTHS = Array.from({ length: 6 }, (_, i) =>
  addMonths("2026-04-01", i),
);
const usage = [];
for (const a of accounts) {
  let use = between(0.62, 0.93);
  const drift = between(-0.025, 0.02);
  USAGE_MONTHS.forEach((month, i) => {
    const s = subscriptions.find(
      (x) =>
        x.account === a &&
        x.started_on <= month &&
        (!x.ended_on || addMonths(month, 1) <= x.ended_on),
    );
    if (!s) return;
    use = Math.min(0.98, Math.max(0.3, use + drift + between(-0.03, 0.03)));
    const active = a.usage?.[i] ?? Math.max(1, Math.round(s.seats * use));
    usage.push({
      account_id: a.id,
      month: month.slice(0, 7),
      active_users: active,
      jobs_scheduled: Math.round(active * between(16, 34)),
    });
  });
}

// ---------------------------------------------------------------- Documents

const usageOf = (a, month) =>
  usage.find((u) => u.account_id === a.id && u.month === month).active_users;
const invoiceOf = (a, issued) =>
  invoices.find((v) => v.sub.account === a && v.issued_on === issued);

const pricing = `# Kestrel plans and pricing, 2026

Kestrel is scheduling and dispatch software for field-service teams: the office plans jobs on the dispatch board, and technicians get them in the Kestrel mobile app. Prices are per technician seat per month, in US dollars.

| Plan | Price | Seats | Billing | Support | Uptime commitment |
| --- | --- | --- | --- | --- | --- |
| Starter | $29 per seat | 1 to 25 | Monthly, due in 14 days | Email, next business day | None |
| Growth | $49 per seat | 10 or more | Monthly, due in 14 days | Standard: 4-hour response to urgent tickets | 99.9% target, no service credits |
| Enterprise | Quoted, usually $36 to $44 per seat | 250 or more | Quarterly in advance, net 30 | Premium: 1-hour response to urgent tickets | Set in the order form, with service credits |

An account has one plan: seats can't be split between plans.

## What each plan includes

- **Starter**: scheduling, the mobile app, customer notifications.
- **Growth**: everything in Starter, plus the dispatch board with route optimisation, and the QuickBooks, Xero and Salesforce integrations. SSO (SAML) is an add-on at $6 per seat.
- **Enterprise**: everything in Growth, plus SSO, a business associate agreement (BAA) for healthcare customers, a sandbox, and the NetSuite integration (beta).

Audit logs are on the roadmap for Enterprise. They are not generally available, and any date promised to a customer must be in their order form.

## Discounts and renewals

- Account managers may discount up to 15% without approval. Anything more, or any discount carried into a renewal, needs the Chief Revenue Officer, Elena Vasquez.
- Starter and Growth renew every 12 months. Seat reductions take effect only at renewal, with the notice the order form sets.
- Enterprise renewal increases are capped at what the order form says.

## Service credits

Only an Enterprise order form can grant service credits. Starter and Growth customers have no credits for downtime, whatever the cause; goodwill credits need the CRO's approval.
`;

const pinecrestQbr = `# Pinecrest Home Health: quarterly business review

**Date:** 15 September 2026, video call
**Pinecrest:** Dana Okafor (VP Operations), Raj Patel (IT Manager), Miguel Serrano (CFO, last 20 minutes)
**Kestrel:** ${STAFF.jordan.name} (account manager), Elena Vasquez (Chief Revenue Officer), Hana Lee (solutions engineer)

## Summary

A hard quarter. Usage fell after a run of SSO sign-in failures in August, and Pinecrest is looking at a competitor before their renewal on 31 December. Dana still backs Kestrel, but Miguel wants a price that reflects the problems.

## Usage

Active nurses and schedulers fell from ${usageOf(pinecrest, "2026-07")} in July to ${usageOf(pinecrest, "2026-08")} in August: on the worst mornings nurses fell back to paper rosters. Dana expects September to recover to around 330.

## Issues raised

- **SSO sign-in.** Nurses were locked out on three mornings in August: tickets ${T.sso1.id} (5 August), ${T.sso2.id} (11 August) and ${T.sso3.id} (19 August). Raj says nobody at Pinecrest was told about the SAML certificate rotation on 4 August, which caused the first two. Jordan apologised and committed to a written post-mortem.
- **Audit logs.** Pinecrest's order form commits Kestrel to audit logs by 31 December 2026. Dana needs them for a state audit in January and asked for early access. Hana offered the beta.
- **Scheduling.** Leah Whitcombe's team wants recurring visits that skip public holidays. Logged as a feature request.

## Commercial

- Miguel said Routewell quoted $35 per seat for the same nurses, against Pinecrest's $42.
- Renewal is on 31 December 2026, and the order form asks for 60 days' notice, so Pinecrest must decide by 1 November.
- Elena said she could look at the price if Pinecrest commits to two years, but made no offer.

## Next steps

- ${STAFF.jordan.name}: send the SSO post-mortem by 22 September.
- Hana Lee: audit-log beta access for Raj by 15 October.
- Elena Vasquez: a two-year renewal proposal before 20 October.
- Follow-up call on 20 October.
`;

const halvorsenCall = `# Halvorsen Freight: expansion call

**Date:** 6 October 2026
**Halvorsen:** Ingrid Halvorsen (COO), Tom Becker (IT Director), Carla Mendes (Dispatch Manager)
**Kestrel:** ${STAFF.sam.name} (account manager)

## Why they called

Halvorsen opens a new depot at Balzac, north of Calgary, in February 2027, and wants 150 more seats from 1 January 2027 for its drivers and dispatchers: 750 seats in all.

## What they asked for

- Ingrid asked for $36 per seat across all 750 seats if Halvorsen extends by two years past the current term, which ends on 31 January 2027.
- The master agreement prices additional seats at the subscription's per-seat price, $38, for the rest of the term, and caps the renewal increase at 5%. Sam explained that $36 needs Elena's approval.
- Tom needs the NetSuite integration live before the depot opens, for driver timesheets and fuel invoices. It is in beta for Enterprise.

## Usage

Halvorsen had ${usageOf(halvorsen, "2026-09")} active users in September, of 600 seats: they are close to full, which is why they're expanding now.

## Notes

- Carla is happy with dispatch; her only ask is a faster dispatch board in the 6am rush.
- No complaints about billing; invoices are paid on time.

## Next steps

- ${STAFF.sam.name}: take the $36, 750-seat, two-year extension to Elena Vasquez by 13 October.
- ${STAFF.sam.name}: get a NetSuite integration date from product for Tom.
`;

const brightmoorCall = `# Brightmoor Facilities: renewal call

**Date:** 21 October 2026
**Brightmoor:** Priya Natarajan (Head of Operations), Gareth Lloyd (Finance Director)
**Kestrel:** ${STAFF.sam.name} (account manager)

## Summary

Brightmoor's renewal is at risk. They lost their largest hospital facilities contract in August, and Gareth has to cut software spend by 30%. They want to stay on Kestrel, but smaller and at the same discount.

## What they asked for

- Go from 120 seats to 80 at the renewal on 1 December, and keep the 10% discount (ticket ${T.seats.id}, and Gareth's question in ${T.discount.id}).
- Gareth asked whether the 40 seats they keep for occasional subcontractors could go on Starter. Sam explained an account can't split seats between plans, and Starter stops at 25 seats.

## Risks

- **Champion leaving.** Priya leaves Brightmoor on 28 November. Nobody replaces her yet; Owen Pryce stays as admin but has no say in the budget.
- **Unpaid invoice.** Gareth is holding the October invoice, ${invoiceOf(brightmoor, "2026-10-01").id} for ${usd(invoiceOf(brightmoor, "2026-10-01").amount_usd)}, until the renewal terms are agreed. September's was paid late too.
- **Usage.** Active users fell from ${usageOf(brightmoor, "2026-04")} in April to ${usageOf(brightmoor, "2026-09")} in September.
- **Notice.** Their order form needs 30 days' written notice of any seat change, so by 31 October.

## Next steps

- ${STAFF.sam.name}: ask Elena Vasquez whether the 10% can carry into the renewal at 80 seats, and answer Gareth by 24 October.
- Gareth Lloyd: pay the October invoice once the terms are agreed.
- Priya Natarajan: name who owns Kestrel at Brightmoor after she leaves.
`;

const sableThread = `# Sable Solar: escalation after the 7 October incident

---

**From:** Marcus Bell <${person(sable, "Marcus Bell").email}>
**To:** Kestrel Support <support@kestrel.example>
**Cc:** ${STAFF.aisha.name} <${STAFF.aisha.email}>
**Date:** 8 October 2026, 07:55 MST
**Subject:** Yesterday's outage (${T.sable1.id}, ${T.sable2.id})

Hi,

Yesterday morning our install crews were working from the previous day's schedule for over two hours. Devon logged ${T.sable1.id} at 07:51 and I logged ${T.sable2.id} half an hour later. Forty installs were at risk; we rescheduled eleven, and two customers cancelled.

We pay for 140 seats. I want to know what went wrong, and I expect a service credit for the morning.

Marcus Bell
Operations Director, Sable Solar

---

**From:** Nora Quinn <nora.quinn@kestrel.example>
**To:** Marcus Bell
**Cc:** ${STAFF.aisha.name}
**Date:** 8 October 2026, 10:40 PDT

Hi Marcus,

I'm sorry. Sable Solar was the account hit hardest by yesterday's incident: job changes made on the dispatch board didn't reach your crews' phones between 07:42 and 10:15 Pacific. Our incident report has the cause and what we are changing; Aisha will walk you through it.

On credits: your Growth plan has no service credits, but I have asked our CRO, Elena Vasquez, to approve a goodwill credit of one month for 20 seats. I'll keep ${T.sable2.id} open until you have an answer.

Nora Quinn
Support Lead, Kestrel

---

**From:** Marcus Bell
**To:** Nora Quinn
**Cc:** ${STAFF.aisha.name}, Tessa Grant
**Date:** 9 October 2026, 08:20 MST

Thanks, Nora. A credit would help. To be straight with you: if mornings like that can happen without consequences, we need an uptime commitment with credits. Aisha, can you price Enterprise for us? Tessa (our controller) will want the numbers before our planning meeting in November.

Marcus
`;

const incident = `# Incident report: dispatch sync delays, 7 October 2026

**Severity:** SEV-1
**Incident commander:** Victor Osei, VP Engineering
**Status:** resolved; follow-ups open
**Written:** 9 October 2026

## Summary

Between 07:42 and 10:15 Pacific on 7 October 2026 (2 hours 33 minutes), changes made on the dispatch board did not reach technicians' mobile apps. 312 job updates across 11 customer accounts were delayed by up to 2 hours 20 minutes. Scheduling, the dispatch board itself and the rest of the mobile app kept working.

## Impact

The most affected accounts, by delayed job updates:

| Account | Delayed updates | Tickets |
| --- | --- | --- |
| Sable Solar | 96 | ${T.sable1.id}, ${T.sable2.id} |
| Halvorsen Freight | 71 | ${T.halvorsen1.id} |
| Pinecrest Home Health | 58 | ${T.pinecrest1.id} |
| Corbel HVAC Services | 24 | ${T.corbel1.id} |
| Fenwick Fire Protection | 19 | ${T.fenwick1.id} |

Six other accounts had fewer than ten delayed updates each and opened no tickets.

Under our service definitions the delay counts as downtime of the dispatch service. With no other downtime, the dispatch service's uptime for October 2026 is 99.66%. Customers whose order forms grant service credits are owed them at their order form's rates; Starter and Growth plans carry none.

## Cause

A release at 07:35 changed how the sync service acknowledges job updates. Under the morning load, two consumers of the update queue waited on each other's locks, and updates queued without being delivered. Our alerts watched errors, not lag, so nothing paged until customers called.

## Timeline (Pacific)

- 07:35: release 2026.10.07-1 deployed.
- 07:42: updates stop reaching the mobile apps.
- 07:51: first ticket, from Sable Solar.
- 08:20: support escalates to engineering; Victor Osei takes command.
- 09:58: release rolled back.
- 10:15: the queue drains; all updates delivered.

## Follow-ups

- Alert when sync lag passes 5 minutes. Owner: Victor Osei. Due 31 October.
- Load test the sync service at peak morning volume before each release. Owner: Victor Osei. Due 15 November.
- Credit notes to every customer whose order form grants credits. Owner: Elena Vasquez. Due 15 November.
`;

// ---------------------------------------------------------------- Contracts

const css = `
  @page { size: Letter; margin: 22mm 20mm; }
  body { font: 10.5pt/1.45 Georgia, "Times New Roman", serif; color: #1a1a1a; }
  header { display: flex; justify-content: space-between; align-items: baseline; border-bottom: 2px solid #1a1a1a; padding-bottom: 6px; margin-bottom: 18px; }
  header strong { font: 700 15pt/1 Helvetica, Arial, sans-serif; letter-spacing: 0.08em; }
  header span { font: 9pt Helvetica, Arial, sans-serif; color: #555; }
  h1 { font: 700 16pt/1.2 Helvetica, Arial, sans-serif; margin: 0 0 4px; }
  h2 { font: 700 11pt/1.2 Helvetica, Arial, sans-serif; margin: 18px 0 6px; text-transform: uppercase; letter-spacing: 0.04em; }
  table { border-collapse: collapse; width: 100%; margin: 6px 0; }
  th, td { border: 1px solid #999; padding: 5px 7px; text-align: left; vertical-align: top; }
  th { background: #eee; font: 700 9.5pt Helvetica, Arial, sans-serif; width: 34%; }
  ol li { margin-bottom: 5px; }
  .sign { display: flex; gap: 24px; margin-top: 28px; page-break-inside: avoid; }
  .sign div { flex: 1; border-top: 1px solid #1a1a1a; padding-top: 6px; }
  footer { margin-top: 26px; font: 8.5pt Helvetica, Arial, sans-serif; color: #666; }
`;
const rows = (pairs) =>
  `<table>${pairs.map(([k, v]) => `<tr><th>${k}</th><td>${v}</td></tr>`).join("")}</table>`;
const contract = ({
  title,
  number,
  customer,
  parties,
  terms,
  clauses,
  signed,
}) => `<!doctype html>
<html><head><meta charset="utf-8"><title>${title}</title><style>${css}</style></head><body>
<header><strong>KESTREL</strong><span>Kestrel Software, Inc. · 600 Alder Street, Suite 400, Seattle, WA 98104</span></header>
<h1>${title}</h1>
<p>${number} · between Kestrel Software, Inc. (“Kestrel”) and ${customer.legal} (“Customer”), ${customer.address}.</p>
<h2>Parties and contacts</h2>
${rows(parties)}
<h2>Subscription</h2>
${rows(terms)}
<h2>Terms</h2>
<ol>${clauses.map((c) => `<li>${c}</li>`).join("")}</ol>
<p>This order form is governed by Kestrel's Master Subscription Agreement in force on the effective date. Where they differ, this order form governs.</p>
<div class="sign">
  <div><strong>For Kestrel Software, Inc.</strong><br>Elena Vasquez, Chief Revenue Officer<br>Signed ${signed.kestrel}</div>
  <div><strong>For ${customer.legal}</strong><br>${signed.by}<br>Signed ${signed.customer}</div>
</div>
<footer>Confidential. Kestrel Software, Inc. ${number}.</footer>
</body></html>`;

const contact = (a, name) => {
  const c = person(a, name);
  return `${c.full_name}, ${c.title} (${c.email})`;
};
const pinecrestSub = subOf(pinecrest),
  halvorsenSub = subOf(halvorsen),
  brightmoorSub = subOf(brightmoor);

const contracts = {
  "Pinecrest Home Health order form.pdf": contract({
    title: "Order Form: Kestrel Enterprise renewal",
    number: "Order form KF-2025-118",
    customer: pinecrest,
    parties: [
      ["Business contact", contact(pinecrest, "Dana Okafor")],
      ["Technical contact", contact(pinecrest, "Raj Patel")],
      ["Billing contact", contact(pinecrest, "Miguel Serrano")],
      [
        "Kestrel account manager",
        `${STAFF.jordan.name} (${STAFF.jordan.email})`,
      ],
    ],
    terms: [
      ["Plan", "Kestrel Enterprise"],
      ["Seats", `${pinecrestSub.seats} named technician seats`],
      ["Price", `${usd(pinecrestSub.price)} per seat per month`],
      ["Term", "1 January 2026 to 31 December 2026 (12 months)"],
      ["Annual fees", usd(monthly(pinecrestSub) * 12)],
      [
        "Invoicing",
        `Quarterly in advance, ${usd(monthly(pinecrestSub) * 3)} per quarter, net 30`,
      ],
    ],
    clauses: [
      "<strong>Renewal.</strong> This order form renews for a further 12 months unless either party gives written notice at least 60 days before the end of the term.",
      "<strong>Renewal price.</strong> Kestrel may raise the per-seat price at renewal by no more than 7%.",
      "<strong>Service levels.</strong> Kestrel commits to 99.95% monthly uptime for scheduling, the dispatch service and the mobile app. For each 0.1% (or part of it) by which a month falls short, Customer is credited 10% of that month's fees, up to 30%.",
      "<strong>Support.</strong> Premium support: a response within 1 hour to urgent tickets, at any hour.",
      "<strong>Health data.</strong> The parties' Business Associate Agreement of 12 December 2025 applies. Customer data is stored in the United States only.",
      "<strong>Single sign-on.</strong> SAML single sign-on is included. Kestrel will give Customer at least 14 days' notice of any certificate rotation.",
      "<strong>Audit logs.</strong> Kestrel will make audit logs of user sign-ins and schedule changes available to Customer by 31 December 2026. If it does not, Customer may end this order form on 30 days' notice with a refund of prepaid fees for the rest of the term.",
    ],
    signed: {
      kestrel: "16 December 2025",
      by: "Miguel Serrano, CFO",
      customer: "18 December 2025",
    },
  }),
  "Halvorsen Freight master agreement.pdf": contract({
    title: "Master Subscription Agreement and Order Form",
    number: "Agreement KF-2024-009",
    customer: halvorsen,
    parties: [
      ["Executive sponsor", contact(halvorsen, "Ingrid Halvorsen")],
      ["Operations contact", contact(halvorsen, "Carla Mendes")],
      ["Technical contact", contact(halvorsen, "Tom Becker")],
      ["Kestrel account manager", `${STAFF.sam.name} (${STAFF.sam.email})`],
    ],
    terms: [
      ["Plan", "Kestrel Enterprise"],
      [
        "Seats",
        `${halvorsenSub.seats} named seats for drivers and dispatchers`,
      ],
      [
        "Price",
        `${usd(halvorsenSub.price)} per seat per month, fixed for the initial term`,
      ],
      ["Initial term", "1 February 2024 to 31 January 2027 (36 months)"],
      ["Annual fees", usd(monthly(halvorsenSub) * 12)],
      [
        "Invoicing",
        `Quarterly in advance, ${usd(monthly(halvorsenSub) * 3)} per quarter, net 30`,
      ],
    ],
    clauses: [
      "<strong>Additional seats.</strong> Customer may add seats at any time at the per-seat price above, prorated to the end of the current term.",
      "<strong>Renewal.</strong> After the initial term this agreement renews for 12 months at a time unless either party gives 90 days' written notice. Kestrel may raise the per-seat price at each renewal by no more than 5%.",
      "<strong>Termination for convenience.</strong> After 31 January 2025, Customer may end this agreement on 90 days' written notice, paying fees to the end of the notice period.",
      "<strong>Service levels.</strong> Kestrel commits to 99.9% monthly uptime for the dispatch service and the mobile app. For each 0.1% (or part of it) by which a month falls short, Customer is credited 5% of that month's fees, up to 20%.",
      "<strong>Support.</strong> Premium support: a response within 1 hour to urgent tickets, at any hour.",
      "<strong>Data.</strong> Customer data is stored in Canada. Kestrel may not use it to train models.",
    ],
    signed: {
      kestrel: "22 January 2024",
      by: "Ingrid Halvorsen, COO",
      customer: "25 January 2024",
    },
  }),
  "Brightmoor Facilities order form.pdf": contract({
    title: "Order Form: Kestrel Growth renewal",
    number: "Order form KF-2025-104",
    customer: brightmoor,
    parties: [
      ["Business contact", contact(brightmoor, "Priya Natarajan")],
      ["Billing contact", contact(brightmoor, "Gareth Lloyd")],
      ["Administrator", contact(brightmoor, "Owen Pryce")],
      ["Kestrel account manager", `${STAFF.sam.name} (${STAFF.sam.email})`],
    ],
    terms: [
      ["Plan", "Kestrel Growth"],
      ["Seats", `${brightmoorSub.seats} technician seats`],
      [
        "Price",
        `${usd(brightmoorSub.price)} per seat per month, less a ${brightmoorSub.discount}% discount: ${usd(monthly(brightmoorSub))} per month`,
      ],
      ["Term", "1 December 2025 to 30 November 2026 (12 months)"],
      ["Invoicing", "Monthly on the 1st, due in 14 days"],
    ],
    clauses: [
      "<strong>Renewal.</strong> This order form renews for a further 12 months unless either party gives written notice at least 30 days before the end of the term.",
      "<strong>Seat changes.</strong> Customer may add seats at any time. Seats can be removed only at renewal, with the notice in clause 1.",
      `<strong>Discount.</strong> The ${brightmoorSub.discount}% discount applies to this term only. A renewal is at list price unless the parties agree otherwise in writing.`,
      "<strong>Service levels.</strong> Kestrel targets 99.9% monthly uptime. The Growth plan carries no service credits.",
      "<strong>Late payment.</strong> Kestrel may suspend access after written notice if an invoice is more than 30 days overdue.",
    ],
    signed: {
      kestrel: "20 November 2025",
      by: "Gareth Lloyd, Finance Director",
      customer: "24 November 2025",
    },
  }),
};

// ---------------------------------------------------------------- Write

rmSync(OUT, { recursive: true, force: true });
mkdirSync(OUT, { recursive: true });
const write = (name, body) => writeFileSync(join(OUT, name), body);
// Each table's rows, as its CSV holds them, for kestrel.sql.
const rowsOf = {};
const table = (name, rows) => {
  rowsOf[name] = rows;
  write(`${name}.csv`, csv(rows));
};

table(
  "accounts",
  accounts.map((a) => ({
    account_id: a.id,
    name: a.name,
    industry: a.industry,
    country: a.country,
    city: a.city,
    employees: a.employees,
    segment: segment(a.employees),
    account_manager: a.manager.email,
    customer_since: a.since,
  })),
);
table("contacts", contacts);
table(
  "subscriptions",
  subscriptions.map((s) => ({
    subscription_id: s.id,
    account_id: s.account.id,
    plan: s.plan,
    seats: s.seats,
    price_per_seat_usd: money(s.price),
    discount_pct: s.discount,
    monthly_amount_usd: money(monthly(s)),
    billing: s.plan === "Enterprise" ? "quarterly" : "monthly",
    status: s.status,
    started_on: s.started_on,
    term_months: s.term,
    current_term_end: s.term_end ?? "",
    ended_on: s.ended_on,
    cancel_reason: s.cancel_reason,
  })),
);
table(
  "invoices",
  invoices.map((v) => ({
    invoice_id: v.id,
    subscription_id: v.sub.id,
    account_id: v.sub.account.id,
    issued_on: v.issued_on,
    due_on: v.due_on,
    amount_usd: v.amount_usd,
    status: v.status,
    paid_on: v.paid_on,
  })),
);
table(
  "support_tickets",
  tickets.map((t) => ({
    ticket_id: t.id,
    account_id: t.account.id,
    requester_id: t.contact.contact_id,
    opened_at: t.opened_at,
    closed_at: t.status === "solved" ? t.closed_at : "",
    priority: t.priority,
    status: t.status,
    category: t.category,
    subject: t.subject,
  })),
);
table("product_usage", usage);
write("Kestrel plans and pricing 2026.md", pricing);
write("Pinecrest QBR 2026-09-15.md", pinecrestQbr);
write("Halvorsen expansion call 2026-10-06.md", halvorsenCall);
write("Brightmoor renewal call 2026-10-21.md", brightmoorCall);
write("Sable Solar escalation 2026-10-08.md", sableThread);
write("Incident report 2026-10-07 dispatch sync.md", incident);

const browser = await chromium.launch();
try {
  const page = await browser.newPage();
  for (const [name, html] of Object.entries(contracts)) {
    await page.setContent(html, { waitUntil: "load" });
    write(name, await page.pdf({ format: "Letter", printBackground: true }));
  }
} finally {
  await browser.close();
}

// ---------------------------------------------------------------- SQL

// The tables as kestrel-db serves them: typed, each keyed, and linked by
// foreign keys as the CSVs' ids link them. An empty cell is NULL. Foundry
// reads them as kestrel_reader, which can only select.
const SCHEMA = {
  accounts: {
    key: ["account_id"],
    types: { employees: "integer", customer_since: "date" },
  },
  contacts: { key: ["contact_id"], refs: { account_id: "accounts" } },
  subscriptions: {
    key: ["subscription_id"],
    refs: { account_id: "accounts" },
    types: {
      seats: "integer",
      price_per_seat_usd: "numeric(10,2)",
      discount_pct: "integer",
      monthly_amount_usd: "numeric(12,2)",
      started_on: "date",
      term_months: "integer",
      current_term_end: "date",
      ended_on: "date",
    },
  },
  invoices: {
    key: ["invoice_id"],
    refs: { subscription_id: "subscriptions", account_id: "accounts" },
    types: {
      issued_on: "date",
      due_on: "date",
      amount_usd: "numeric(12,2)",
      paid_on: "date",
    },
  },
  support_tickets: {
    key: ["ticket_id"],
    refs: { account_id: "accounts", requester_id: "contacts" },
    types: { opened_at: "timestamptz", closed_at: "timestamptz" },
  },
  product_usage: {
    key: ["account_id", "month"],
    refs: { account_id: "accounts" },
    types: { active_users: "integer", jobs_scheduled: "integer" },
  },
};
const literal = (v, type) =>
  v === null || v === undefined || v === ""
    ? "NULL"
    : type === "integer" || type.startsWith("numeric")
      ? String(v)
      : `'${String(v).replace(/'/g, "''")}'`;
const sql = [
  "-- Kestrel, the sample company, as a local install's kestrel-db serves it.",
  "-- Written by scripts/sample-company.mjs; edit that, not this.",
  "",
  ...Object.entries(SCHEMA).flatMap(([name, t]) => {
    const rows = rowsOf[name],
      cols = Object.keys(rows[0]),
      type = (c) => t.types?.[c] ?? "text";
    const keyOf = (ref) => SCHEMA[ref].key[0];
    return [
      `CREATE TABLE ${name} (`,
      [
        ...cols.map(
          (c) => `  ${c} ${type(c)}${t.key.includes(c) ? " NOT NULL" : ""}`,
        ),
        `  PRIMARY KEY (${t.key.join(", ")})`,
        ...Object.entries(t.refs ?? {}).map(
          ([c, ref]) => `  FOREIGN KEY (${c}) REFERENCES ${ref} (${keyOf(ref)})`,
        ),
      ].join(",\n"),
      ");",
      `INSERT INTO ${name} (${cols.join(", ")}) VALUES`,
      rows
        .map((r) => `  (${cols.map((c) => literal(r[c], type(c))).join(", ")})`)
        .join(",\n") + ";",
      "",
    ];
  }),
  "CREATE ROLE kestrel_reader LOGIN PASSWORD 'kestrel';",
  "GRANT USAGE ON SCHEMA public TO kestrel_reader;",
  "GRANT SELECT ON ALL TABLES IN SCHEMA public TO kestrel_reader;",
  "",
].join("\n");
mkdirSync(join(SQL, ".."), { recursive: true });
writeFileSync(SQL, sql);

process.stdout.write(
  `${accounts.length} accounts, ${contacts.length} contacts, ${subscriptions.length} subscriptions, ${invoices.length} invoices, ${tickets.length} tickets, ${usage.length} usage rows, ${Object.keys(contracts).length} contracts → ${OUT}\n`,
);
