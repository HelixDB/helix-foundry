import { it, expect } from "vitest";
import { normalizeStripe, stripeObjects } from "../apps/api/src/stripe.js";
it("keeps nullable relationship columns canonical across sparse invoices", () => {
  const obj = stripeObjects.find((o) => o.name === "invoices")!;
  expect(
    normalizeStripe(obj, { id: "in_1", subscription: null, customer: null }),
  ).toEqual({ invoice_id: "in_1", subscription_id: null, customer_id: null });
  expect(normalizeStripe(obj, { subscription: "sub_1" })).toEqual({
    subscription_id: "sub_1",
  });
});
