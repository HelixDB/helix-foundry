# Online retail: first playbook

This playbook gives a published retail workspace a practical starting order
for Home and a small set of data checks. Its JSON configuration block is the executable
configuration. Each role names a business concept; the runtime maps it to
actual datasets and typed columns. The reading is deliberately conservative.

## Recognising a store

An order dataset with a numeric amount is required. At least three supporting
entities must also be recognised: order lines, products, customers, visits,
shipments or payments. Merged datasets take precedence over their raw parts.
A weak match retains the existing generic Home behaviour. A business description
cannot override this reading; descriptions are outside this first version.

Mapping recognises known dataset and column names, rather than guessing from
sample values. An unrecognised role is a mapping gap, not proof that the data
was never collected. Advice therefore says “add or map”.

## What Home can calculate now

The order is recorded order value, orders, average order value, and purchasing
customers. Each must compile and run through the existing metric compiler.
An unavailable or protected column removes that candidate. Supported AI-picked
trends and breakdowns may follow these metrics. The playbook adds no model
context and makes no additional model calls.

Recorded order value is the sum of the mapped amount; average order value is
its mean; orders counts rows; purchasing customers counts distinct customer
keys on orders. These are recorded totals, including every recorded status.
They are not promises of completed sales, net revenue, profit or loyal people.
Null customer keys do not count as purchasing customers.

An order date, when available, uses Home's existing last-30-days comparison
window, anchored on the latest order date. Without a date, the metric covers
the current snapshot. This version does not introduce seasonal or year-over-year
comparisons or claim that these are complete calendar months.

Amounts display as numbers in source units; only a column explicitly ending
in `_cents` is divided by 100. No currency is inferred from the industry.
Owners should check currency consistency before combining sources or using
recorded amounts as financial measures.

Conversion, repeat rate, delivery time, CAC, ROAS and margin need ratios,
joins, date differences or business definitions the current metric compiler
cannot express. This playbook never asks Home to manufacture those KPIs.
Their required data can still be identified as a useful next step.

## Check the foundations first

The worker runs at most six aggregate queries, sequentially, through the
isolated executor and its normal memory/time limits. Each returns one row;
raw customer details and emails are never saved in the results.

- **Orders to customers:** matching orders divided by all orders, including
  null and unmatched customer IDs. Guest purchases may explain a gap.
- **Lines to orders and products:** each denominator includes every line.
  Repeated keys in a reference table never multiply the numerator.
- **Orders to visits:** orders carrying a matching session divided by all
  orders. This measures link coverage, not conversion. It must not count only
  sessions or only orders that already have a session ID.
- **Customer emails:** distinct normalised emails divided by nonblank email
  rows. Repetition is evidence to review, not proof that people are duplicates.
- **Order statuses:** count cancelled, returned, refunded, voided and failed
  rows. The result explicitly says that recorded Home totals include them.
  It never claims that a status filter was applied to Home.

Missing required fields and empty inputs are reported as skipped. A failed
query fails the scan; it is not silently reported as a successful check.

## Helpful next steps

Ontology shows at most three suggestions in playbook order: connect or map
spend and visits; record or map session IDs, campaigns and return reasons;
then costs, checkout events and support data. A suggestion disappears after
a successful scan recognises its required data. The last successful results
remain visible while a refresh runs or fails, with their original check time.

The checks are observations about recorded data. A linked order does not
establish that an ad caused a purchase. An absent field does not establish
that the business lacks the information in another system. Status labels need
an owner's interpretation before they become sales definitions.

## What remains for later PRs

Business knowledge, editable definitions, owner targets, typical ranges,
learning from questions, custom or AI-drafted industries, role-specific
personalisation and insight-rule integration remain follow-ups under #71.
The Analyst and voice routing are unchanged by this version.

## Executable configuration

```json
{
  "id": "ecommerce",
  "title": "Online retail",
  "version": 2,
  "signals": {
    "require": ["order", "amount"],
    "boost": [
      "order_line",
      "product",
      "customer",
      "session",
      "shipment",
      "payment"
    ],
    "min": 3
  },
  "roles": {
    "order": {
      "title": "Orders",
      "names": ["order", "purchase", "transaction", "sale", "sales_order"]
    },
    "order_line": {
      "title": "Order lines",
      "names": [
        "order_item",
        "line_item",
        "order_line",
        "order_lines",
        "basket_item",
        "order_product"
      ]
    },
    "product": {
      "title": "Products",
      "names": ["product", "sku", "item", "variant"]
    },
    "customer": {
      "title": "Customers",
      "names": ["customer", "buyer", "shopper", "client"]
    },
    "session": {
      "title": "Visits",
      "names": ["session", "visit", "web_session"]
    },
    "shipment": {
      "title": "Shipments",
      "names": ["shipment", "fulfillment", "fulfilment", "delivery"]
    },
    "payment": {
      "title": "Payments",
      "names": ["payment", "charge"]
    },
    "checkout": {
      "title": "Cart and checkout events",
      "names": [
        "checkout",
        "cart",
        "cart_event",
        "checkout_event",
        "add_to_cart",
        "abandoned_checkout"
      ]
    },
    "spend": {
      "title": "Marketing spend",
      "names": [
        "ad_spend",
        "marketing_spend",
        "spend",
        "campaign_cost",
        "ads",
        "ad_performance"
      ]
    },
    "ticket": {
      "title": "Support tickets",
      "names": ["ticket", "support_ticket", "case", "conversation", "complaint"]
    },
    "refund": {
      "title": "Refunds or returns",
      "names": ["refund", "return", "rma"]
    },
    "amount": {
      "title": "Order amount",
      "names": [
        "net_amount",
        "net_revenue",
        "amount_usd",
        "amount_eur",
        "amount_gbp",
        "amount_cents",
        "net_amount_cents",
        "total",
        "grand_total",
        "amount",
        "subtotal",
        "total_price",
        "order_total"
      ],
      "of": "order",
      "type": "number"
    },
    "order_date": {
      "title": "Order date",
      "names": ["order_date", "created_at", "placed_at", "ordered_at", "date"],
      "of": "order",
      "type": "date"
    },
    "order_status": {
      "title": "Order status",
      "names": ["status", "state", "order_status", "financial_status"],
      "of": "order",
      "type": "text"
    },
    "campaign": {
      "title": "Campaign",
      "names": ["utm_campaign", "campaign", "campaign_name", "campaign_id"],
      "of": "session",
      "type": "text"
    },
    "unit_cost": {
      "title": "Product cost",
      "names": ["unit_cost", "cost", "cogs", "cost_price", "product_cost"],
      "of": "product",
      "type": "number"
    },
    "customer_email": {
      "title": "Customer email",
      "names": ["email", "email_address", "customer_email"],
      "of": "customer",
      "type": "text"
    },
    "return_reason": {
      "title": "Return reason",
      "names": ["return_reason", "refund_reason", "reason", "reason_code"],
      "of": "order",
      "type": "text"
    },
    "order_key": {
      "title": "Order key",
      "of": "order",
      "type": "key",
      "names": ["order_id", "id", "order_key"]
    },
    "customer_key": {
      "title": "Customer key",
      "of": "customer",
      "type": "key",
      "names": ["customer_id", "id", "customer_key"]
    },
    "product_key": {
      "title": "Product key",
      "of": "product",
      "type": "key",
      "names": ["product_id", "id", "sku"]
    },
    "session_key": {
      "title": "Session key",
      "of": "session",
      "type": "key",
      "names": ["session_id", "id", "visit_id"]
    },
    "order_customer": {
      "title": "Customer on an order",
      "of": "order",
      "type": "key",
      "names": ["customer_id", "buyer_id", "client_id"]
    },
    "order_session": {
      "title": "Session on an order",
      "of": "order",
      "type": "key",
      "names": ["session_id", "visit_id", "visitor_session_id"]
    },
    "line_order": {
      "title": "Order on a line",
      "of": "order_line",
      "type": "key",
      "names": ["order_id", "purchase_id"]
    },
    "line_product": {
      "title": "Product on a line",
      "of": "order_line",
      "type": "key",
      "names": ["product_id", "sku", "item_id"]
    }
  },
  "metrics": [
    {
      "title": "Recorded order value",
      "measure": "sum",
      "dataset": "order",
      "column": "amount",
      "time": "order_date"
    },
    {
      "title": "Orders",
      "measure": "count",
      "dataset": "order",
      "time": "order_date"
    },
    {
      "title": "Average order value",
      "measure": "avg",
      "dataset": "order",
      "column": "amount",
      "time": "order_date"
    },
    {
      "title": "Purchasing customers",
      "measure": "count_distinct",
      "dataset": "order",
      "column": "order_customer",
      "time": "order_date"
    }
  ],
  "checks": [
    {
      "id": "orders_customers",
      "kind": "link",
      "title": "Orders linked to customers",
      "from": "order",
      "column": "order_customer",
      "to": "customer",
      "key": "customer_key",
      "good": 0.95,
      "fix": "Review missing or unmatched customer keys, including guest orders."
    },
    {
      "id": "lines_orders",
      "kind": "link",
      "title": "Order lines linked to orders",
      "from": "order_line",
      "column": "line_order",
      "to": "order",
      "key": "order_key",
      "good": 0.99,
      "fix": "Review missing or unmatched order keys on line items."
    },
    {
      "id": "lines_products",
      "kind": "link",
      "title": "Order lines linked to products",
      "from": "order_line",
      "column": "line_product",
      "to": "product",
      "key": "product_key",
      "good": 0.98,
      "fix": "Review missing or unmatched product keys on line items."
    },
    {
      "id": "orders_sessions",
      "kind": "link",
      "title": "Orders linked to sessions",
      "from": "order",
      "column": "order_session",
      "to": "session",
      "key": "session_key",
      "good": 0.9,
      "fix": "Record the session ID on each order and review unmatched IDs before measuring conversion."
    },
    {
      "id": "customer_emails",
      "kind": "unique",
      "title": "Unique customer emails",
      "column": "customer_email",
      "good": 0.98,
      "fix": "Review repeated emails before treating customer records as distinct people."
    },
    {
      "id": "order_statuses",
      "kind": "status",
      "title": "Recorded order statuses",
      "column": "order_status",
      "values": [
        "cancelled",
        "canceled",
        "returned",
        "refunded",
        "voided",
        "void",
        "failed"
      ]
    }
  ],
  "suggestions": [
    {
      "id": "spend",
      "missing": "spend",
      "kind": "connect",
      "text": "Add or map ad spend by channel to investigate customer acquisition costs."
    },
    {
      "id": "visits",
      "missing": "session",
      "kind": "connect",
      "text": "Add or map storefront visits to investigate which traffic leads to orders."
    },
    {
      "id": "session_ids",
      "missing": "order_session",
      "kind": "change",
      "text": "Save or map the session ID on each order to link visits to purchases."
    },
    {
      "id": "campaigns",
      "missing": "campaign",
      "kind": "change",
      "text": "Tag or map campaign links with UTM codes to distinguish campaigns within a channel."
    },
    {
      "id": "returns",
      "missing": "return_reason",
      "kind": "change",
      "text": "Record or map return reasons to investigate which products disappoint customers."
    },
    {
      "id": "cost",
      "missing": "unit_cost",
      "kind": "change",
      "text": "Add or map product costs before investigating margin."
    },
    {
      "id": "checkout",
      "missing": "checkout",
      "kind": "connect",
      "text": "Add or map cart and checkout events to investigate where buyers drop off."
    },
    {
      "id": "support",
      "missing": "ticket",
      "kind": "connect",
      "text": "Add or map support tickets to investigate complaints about products and deliveries."
    }
  ]
}
```
