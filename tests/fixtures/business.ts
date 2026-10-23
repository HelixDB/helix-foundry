// Deterministic business data for Home insights. Dates count back from the
// latest complete UTC day, so a scan always compares the last two whole weeks
// ("current" is week 0, "baseline" week 1) and fixes cohorts from weeks 2–3.
// Every count below is exact, so tests can check shares and deltas by hand.
const DAY = 86_400_000;
// The start (UTC midnight) of week w, counting back from today.
export const weekStart = (
  w: number,
  today = Math.floor(Date.now() / DAY) * DAY,
) => today - (w + 1) * 7 * DAY;
const at = (w: number, i: number, today?: number) =>
  new Date(
    weekStart(w, today) +
      (i % 7) * DAY +
      ((i * 37) % 24) * 3_600_000 +
      ((i * 13) % 60) * 60_000,
  ).toISOString();
const id = (prefix: string, n: number, width = 4) =>
  prefix + String(n).padStart(width, "0");
// Shares as exact counts out of `total`, spread evenly over the week's rows.
function assign(total: number, shares: [string, number][]) {
  const out: string[] = [];
  for (const [label, share] of shares)
    for (let i = 0; i < Math.round(share * total); i++) out.push(label);
  // Interleave deterministically so every customer group and day gets a mix.
  return out.map((_, i) => out[(i * 7919) % out.length]);
}

export const SOURCES_BEFORE: [string, number][] = [
  ["organic", 0.4],
  ["paid", 0.25],
  ["referral", 0.1],
  ["direct", 0.15],
  ["email", 0.1],
];
export const SOURCES_AFTER: [string, number][] = [
  ["organic", 0.3],
  ["paid", 0.25],
  ["referral", 0.25],
  ["direct", 0.12],
  ["email", 0.08],
];
const WEEKS = 8;

export type CommerceOptions = {
  // Orders record the session they came from (sessions → orders).
  sessionLinks?: boolean;
  // Paid sessions convert less in the latest week while referrals convert
  // more, keeping overall conversion flat; phone orders (no session) fall
  // while their value rises, keeping revenue flat.
  hiddenShifts?: boolean;
  // The latest week has no customer on most sessions (a tracking change).
  trackingBreak?: boolean;
  weeks?: number;
};

// A shop: customers, their sessions (2,000 a week, unchanged) and orders
// ($30,000 a week, unchanged). In the latest week:
// - 120 regular customers (5 sessions a week before) drop to 420 sessions
//   from 600 (−30%); occasional customers make up the difference;
// - referral grows from 10% to 25% of sessions (200 → 500);
// - the top 10 buyers grow from 22% to 45% of net order value.
export function commerce(opts: CommerceOptions = {}) {
  const weeks = opts.weeks ?? WEEKS;
  const customers = Array.from({ length: 1000 }, (_, i) => ({
    customer_id: id("c", i + 1),
    created_at: new Date(weekStart(weeks + 4) + i * 3_600_000).toISOString(),
    region: ["north", "south", "east", "west"][i % 4],
  }));
  const sessions: Record<string, unknown>[] = [],
    orders: Record<string, unknown>[] = [];
  let occasional = 0,
    sessionNo = 0,
    orderNo = 0;
  for (let w = weeks - 1; w >= 0; w--) {
    const latest = w === 0;
    // Hidden shifts use 4,000 sessions a week so per-source rates have power.
    const total = opts.hiddenShifts ? 4000 : 2000;
    const regular = opts.hiddenShifts ? 0 : latest ? 420 : 600,
      occasionalCount = opts.hiddenShifts ? 0 : latest ? 480 : 300;
    const mix = opts.hiddenShifts
      ? latest
        ? ([
            ["organic", 0.525],
            ["paid", 0.3],
            ["referral", 0.175],
          ] as [string, number][])
        : ([
            ["organic", 0.6],
            ["paid", 0.3],
            ["referral", 0.1],
          ] as [string, number][])
      : latest
        ? SOURCES_AFTER
        : SOURCES_BEFORE;
    const sources = assign(total, mix);
    const week: Record<string, unknown>[] = [];
    for (let i = 0; i < total; i++) {
      let customer: string | null = null;
      if (i < regular)
        // 120 regulars, 5 a week before and 3 or 4 in the latest week.
        customer = id("c", (i % 120) + 1);
      else if (i < regular + occasionalCount)
        customer = id("c", 121 + (occasional++ % 880));
      if (opts.trackingBreak && latest && i % 10 !== 0) customer = null;
      week.push({
        session_id: id("s", ++sessionNo, 6),
        customer_id: customer,
        started_at: at(w, i),
        traffic_source: sources[i],
      });
    }
    sessions.push(...week);
    if (opts.hiddenShifts) {
      // Organic converts 10%, referral 30%; paid 20% before and 15% after,
      // so 600 sessions convert each week (15%) and the mix hides the drop.
      const rate: Record<string, number> = {
        organic: 0.1,
        paid: latest ? 0.15 : 0.2,
        referral: 0.3,
      };
      const seen: Record<string, number> = {};
      const linked = week.filter((s) => {
        const src = s.traffic_source as string;
        seen[src] = (seen[src] || 0) + 1;
        // Exactly rate × sessions of each source convert.
        return (
          Math.floor(seen[src] * rate[src]) >
          Math.floor((seen[src] - 1) * rate[src])
        );
      });
      // 600 web orders at $50 a week, plus phone orders: 200 at $50 before,
      // 40 at $250 in the latest week. Revenue stays $40,000 while orders
      // fall 20% and value per order rises 25%.
      const phone = latest ? 40 : 200,
        phoneValue = latest ? 250 : 50;
      linked.forEach((s, i) =>
        orders.push({
          order_id: id("o", ++orderNo, 6),
          customer_id: id("c", (i % 900) + 1),
          session_id: s.session_id,
          ordered_at: s.started_at,
          net_amount: 50 * [0.7, 1.3, 0.9, 1.1, 1, 1][i % 6],
        }),
      );
      for (let i = 0; i < phone; i++)
        orders.push({
          order_id: id("o", ++orderNo, 6),
          customer_id: id("c", (i % 900) + 1),
          session_id: null,
          ordered_at: at(w, i * 3),
          net_amount: phoneValue * [0.7, 1.3, 0.9, 1.1, 1, 1][i % 6],
        });
      continue;
    }
    // 30 orders from 10 big buyers and 270 from a fixed set of other buyers.
    const big = latest ? 450 : 220,
      other = latest ? 16500 / 270 : 23400 / 270;
    const spread = [0.7, 1.3, 0.9, 1.1, 1, 1];
    for (let i = 0; i < 300; i++) {
      const whale = i < 30;
      const session = week[(i * 6) % week.length];
      orders.push({
        order_id: id("o", ++orderNo, 6),
        customer_id: whale ? id("c", (i % 10) + 1) : id("c", 11 + (i - 30)),
        ...(opts.sessionLinks ? { session_id: session.session_id } : {}),
        ordered_at: at(w, i * 5),
        net_amount:
          Math.round((whale ? big : other) * spread[i % 6] * 100) / 100,
      });
    }
  }
  return { customers, sessions, orders };
}

// A clinic, nothing like a shop: patients and appointments (600 a week,
// unchanged), booked by phone, online or referral, in four departments. No
// money and no recorded outcome, so revenue and conversion are unsupported.
// In the latest week 80 regular patients (3 a week before) drop to 168
// appointments from 240 (−30%) while new patients make up the difference,
// and online booking grows from 20% to 40%.
export function clinic(weeks = WEEKS) {
  const patients = Array.from({ length: 2000 }, (_, i) => ({
    patient_id: id("p", i + 1),
    registered_at: new Date(weekStart(weeks + 4) + i * 600_000).toISOString(),
  }));
  const appointments: Record<string, unknown>[] = [];
  let n = 0,
    walkIn = 0;
  for (let w = weeks - 1; w >= 0; w--) {
    const latest = w === 0;
    const regular = latest ? 168 : 240;
    const channels = assign(
      600,
      latest
        ? [
            ["phone", 0.4],
            ["online", 0.4],
            ["referral", 0.2],
          ]
        : [
            ["phone", 0.6],
            ["online", 0.2],
            ["referral", 0.2],
          ],
    );
    const departments = assign(600, [
      ["general", 0.4],
      ["cardiology", 0.2],
      ["dermatology", 0.2],
      ["pediatrics", 0.2],
    ]);
    for (let i = 0; i < 600; i++)
      appointments.push({
        appointment_id: id("a", ++n, 6),
        patient_id:
          i < regular ? id("p", (i % 80) + 1) : id("p", 81 + (walkIn++ % 1900)),
        scheduled_at: at(w, i),
        booking_channel: channels[i],
        department: departments[i],
      });
  }
  return { patients, appointments };
}
