---
id: ecommerce
title: Online retail
version: 1
signals:
  require: [order, amount]
  boost: [order_line, product, customer, session, shipment, payment]
  against: [subscription_plan, invoice_line_seat, patient, policy, ticket_only]
roles:
  order: { entity: true, names: [order, purchase, transaction, sale, checkout] }
  order_line: { entity: true, of: order, names: [order_item, line_item, order_line, basket_item] }
  product: { entity: true, names: [product, sku, item, variant] }
  category: { entity: true, names: [category, collection, department] }
  customer: { entity: true, names: [customer, buyer, shopper, user, account] }
  session: { entity: true, names: [session, visit, pageview_session] }
  shipment: { entity: true, of: order, names: [shipment, fulfillment, delivery] }
  payment: { entity: true, of: order, names: [payment, charge, transaction_payment] }
  refund: { entity: true, of: order, names: [refund, return, rma] }
  spend: { entity: true, names: [ad_spend, marketing_spend, campaign_cost] }
  ticket: { entity: true, names: [ticket, case, conversation, complaint] }
  amount: { of: order, type: number, names: [net_amount, total, grand_total, amount, subtotal] }
  order_date: { of: order, type: date, names: [order_date, created_at, placed_at, ordered_at] }
  order_status: { of: order, type: category, names: [status, state, financial_status] }
  channel: { of: order, type: category, names: [channel, source_name, sales_channel] }
  traffic_source: { of: session, type: category, names: [traffic_source, utm_source, source, medium] }
  campaign: { of: session, type: category, names: [utm_campaign, campaign] }
  line_price: { of: order_line, type: number, names: [unit_price, price] }
  line_discount: { of: order_line, type: number, names: [discount_amount, discount] }
  unit_cost: { of: product, type: number, names: [unit_cost, cost, cogs, cost_price] }
  customer_email: { of: customer, type: text, names: [email] }
  signup_date: { of: customer, type: date, names: [signup_date, created_at, first_seen] }
  shipped_at: { of: shipment, type: date, names: [shipped_date, shipped_at] }
  delivered_at: { of: shipment, type: date, names: [delivered_date, delivered_at] }
areas:
  - id: revenue
    title: Revenue
    metrics: [net_revenue, orders, aov]
  - id: customers
    title: Customers and loyalty
    metrics: [new_customers, repeat_rate, revenue_per_customer]
  - id: experience
    title: Customer experience
    metrics: [delivery_days, return_rate, on_time_rate]
  - id: marketing
    title: Marketing and acquisition
    metrics: [conversion_rate, channel_mix, cac, roas]
  - id: cost
    title: Cost and margin
    metrics: [gross_margin, discount_rate]
metrics:
  net_revenue: { title: Net revenue, value: "sum(order.amount)", where: completed, needs: [order, amount, order_status] }
  orders: { title: Orders, value: "count(order)", where: completed, needs: [order] }
  aov: { title: Average order value, value: "sum(order.amount) / count(order)", where: completed, needs: [order, amount] }
  new_customers: { title: New customers, value: "count(customer) by first order", needs: [customer, order] }
  repeat_rate: { title: Repeat customers, value: "share of customers with 2+ completed orders", needs: [customer, order] }
  revenue_per_customer: { title: Revenue per customer, value: "net_revenue / buyers", needs: [customer, order, amount] }
  delivery_days: { title: Days to deliver, value: "median(delivered_at - order_date)", needs: [shipment, delivered_at] }
  on_time_rate: { title: Delivered on time, value: "share delivered within promise", needs: [shipment, delivered_at, promise] }
  return_rate: { title: Return rate, value: "returned orders / completed orders", needs: [order_status] }
  conversion_rate: { title: Conversion rate, value: "sessions with an order / sessions", needs: [session, order] }
  channel_mix: { title: Orders by source, value: "orders by traffic_source", needs: [session, traffic_source, order] }
  cac: { title: Cost per new customer, value: "spend / new customers", needs: [spend, customer] }
  roas: { title: Return on ad spend, value: "revenue by source / spend by source", needs: [spend, traffic_source] }
  gross_margin: { title: Gross margin, value: "1 - sum(qty * unit_cost) / sum(line net)", needs: [order_line, product, unit_cost] }
  discount_rate: { title: Discount share, value: "sum(line_discount) / sum(qty * line_price)", needs: [order_line, line_discount] }
checks:
  - { id: lines_link_orders, kind: link, from: order_line, to: order, good: 0.99 }
  - { id: lines_link_products, kind: link, from: order_line, to: product, good: 0.98 }
  - { id: orders_link_customers, kind: link, from: order, to: customer, good: 0.95 }
  - { id: sessions_link_orders, kind: link, from: session, to: order, good: 0.9, among: "sessions that converted" }
  - { id: payments_match_orders, kind: reconcile, left: "sum(payment.amount captured)", right: "sum(order.amount completed)", tolerance: 0.02 }
  - { id: lines_sum_to_orders, kind: reconcile, left: "sum(order_line net)", right: "sum(order.amount)", tolerance: 0.01 }
  - { id: duplicate_customers, kind: duplicate, of: customer, by: customer_email }
  - { id: status_meaning, kind: definition, of: order_status, assume: { sale: [completed], undo: [cancelled, returned, refunded], open: [pending] } }
  - { id: amount_units, kind: units, of: amount }
  - { id: test_orders, kind: outliers, of: order, hints: [test, "@example.", zero amount] }
  - { id: date_gaps, kind: continuity, of: order_date, grain: day }
advice:
  - { id: connect_spend, kind: connect, area: marketing, when: "missing spend" }
  - { id: connect_support, kind: connect, area: experience, when: "missing ticket" }
  - { id: connect_analytics, kind: connect, area: marketing, when: "missing session" }
  - { id: utm_tags, kind: change, area: marketing, when: "missing campaign" }
  - { id: session_on_order, kind: change, area: marketing, when: "low sessions_link_orders" }
  - { id: checkout_events, kind: change, area: marketing, when: "missing checkout events" }
  - { id: refund_reasons, kind: change, area: experience, when: "missing refund reason" }
  - { id: delivery_promise, kind: change, area: experience, when: "missing promise" }
  - { id: product_cost, kind: change, area: cost, when: "missing unit_cost" }
  - { id: one_customer_id, kind: change, area: customers, when: "high duplicate_customers" }
branches:
  model: [direct, multi_brand, marketplace, subscription]
  role: [founder, operations, marketing, finance]
  stage: [early, established]
---

# Online retail

Online retail sells products to customers through a store, an app or a marketplace, and ships them. The business works when enough of the right people visit, enough of them buy, they buy again, and each order makes money after products, discounts, shipping and returns. Everything below follows from that sentence.

## Where it shows

The playbook works in the background. Nothing asks the owner to pick an industry, branch or role, and there is no new page. It shows up in three quiet places:

- **Ontology page:** a third notes column, "To understand your customers better", with at most three one-line suggestions, next to the existing "What we merged" and "Worth checking". Data-health findings and assumed definitions join "Worth checking".
- **Home:** key metrics in the playbook's order, and one small line linking to the suggestions.
- **Analyst:** a one-line footnote when missing data limited an answer.

## 1. Signals

A workspace is online retail when it has **orders with amounts**, and most of: order lines, products, customers, sessions, shipments and payments. Confidence rises with each linked entity. It is **not** online retail when the main object is a recurring subscription invoice with seats or plans (SaaS), or when there are orders but no products and no shipping (services); those go to other playbooks or to the generic reading.

> **Meridian example.** `orders`, `order_items`, `products`, `customers`, `sessions`, `shipments` and `payments` are all present and linked by IDs. Detected as online retail with high confidence; the `marketplace` value in `orders.channel` quietly selects the multi-brand branch; nothing is asked.

## 2. Concepts and how to map them

The playbook never names a column. It talks about **roles**: order, order line, product, customer, session, shipment, payment, refund, spend, and fields such as amount, order date, order status, channel and traffic source. Mapping finds each role in the ontology by entity and column names, types and link evidence, and picks the best match on its own. It never asks: an uncertain mapping that matters appears as one line under **Worth checking** on the Ontology page, with a small **Change** link. A mapping the owner changes is stored in the workspace profile and never guessed again.

Order amount needs the most care. Prefer the net amount after discounts and before tax and shipping, so revenue matches what the business keeps. When only a gross total exists, say so on every revenue figure.

## 3. Business areas and what matters

Five areas, in this default order. The order changes with the branch and with what people ask about (see section 7 and the README).

| Area | Main question | Metrics |
| --- | --- | --- |
| **Revenue** | Are we selling more, and is each order worth more? | Net revenue, orders, average order value |
| **Customers and loyalty** | Do people come back? | New customers, repeat-customer rate, revenue per customer |
| **Customer experience** | Do orders arrive as promised, and stay sold? | Days to deliver, delivered on time, return rate |
| **Marketing and acquisition** | Which sources bring buyers, at what cost? | Conversion rate, orders by source, cost per new customer, return on ad spend |
| **Cost and margin** | Is the growth profitable? | Gross margin, discount share |

**The main number** is net revenue from completed orders. It is always shown next to its two drivers, orders and average order value, because revenue alone hides whether the business is winning more buyers or squeezing more out of fewer.

## 4. Tracking coverage

For each area, Foundry checks whether the data needed is there. Each item is **Tracked**, **Partial** (present but missing a field) or **Missing**, and says what it unlocks.

| Item | Tracked when | Unlocks |
| --- | --- | --- |
| Transactions | Orders with amount, date and status | Revenue, AOV |
| Order lines | Lines with product, quantity, price | Product and category mix |
| Products and cost | Products with category and unit cost | Gross margin |
| Customers | Customers linked to orders, with a first-seen date | New vs repeat, cohorts |
| Traffic sources | Sessions with source, linked to orders | Conversion, orders by source |
| Campaigns | Sessions or orders with a campaign | Which campaign works |
| Checkout steps | Cart and checkout-started events | Where buyers drop off |
| Fulfilment | Shipments with shipped and delivered dates | Delivery time |
| Delivery promise | A promised date or service level per order | On-time rate |
| Refunds and returns | Refund amounts and reasons | Net revenue, why products come back |
| Marketing spend | Spend per channel per day | Cost per new customer, return on ad spend |
| Support | Tickets linked to customers or orders | Experience problems by product or carrier |

> **Meridian example.** Tracked: transactions, order lines, products and cost (`unit_cost`), customers, traffic sources (`sessions.traffic_source` with `order_id`), fulfilment (`shipped_date`, `delivered_date`). Partial: refunds (statuses `returned` and `refunded` exist, but no reason). Missing: campaigns, checkout steps, delivery promise, marketing spend, support. Coverage alone points at the next two recommendations: connect ad spend, and connect support tickets.

## 5. Data health

Coverage asks whether data exists; health asks whether it can be trusted. Each check reports a real number and a plain fix.

- **Links.** Order lines to orders (aim for 99%), order lines to products (98%), orders to customers (95%), converting sessions to orders (90%). Below the bar: "12% of orders have no customer, so repeat rate is understated. Guest checkouts usually cause this; link them by email."
- **Reconciliation.** Captured payments should match completed order revenue within 2%, and order lines should add up to their orders within 1%. A gap usually means amounts include tax or shipping on one side only.
- **Duplicates.** The same email on several customers splits one person into many and makes loyalty look worse than it is.
- **Meaning of statuses.** Foundry proposes which statuses count as a sale (`completed`), which undo one (`cancelled`, `returned`, `refunded`), and which are not final (`pending`). Foundry applies this on its own and shows it as one line under **Worth checking**, with **Change**; a change becomes a workspace definition.
- **Units.** Amounts that are all whole numbers a hundred times larger than prices elsewhere are probably cents.
- **Test orders.** Zero amounts, test emails, and staff accounts are excluded, and the exclusion is shown under **Worth checking**.
- **Gaps in dates.** A day with no orders in a busy store is usually a broken import, not a bad day.

## 6. How to read the numbers

These are the opinions the Analyst and Home follow unless the owner sets a different definition.

- Count a sale on its **order date** when the order is completed; report cancelled and returned orders separately, never silently netted.
- Report revenue **net of discounts, before tax and shipping**, and label anything else.
- Compare a period with **the same period a year earlier** when there is a year of history; retail is seasonal, and last month is a poor baseline for December. Otherwise compare with the previous equal period and say so.
- Compare **full periods only**. A half-finished week is not a decline.
- Show a change as both an amount and a percentage.
- Treat **business events** (sales, holidays, launches) as context for a change, never as proof of its cause.
- A higher average order value is good only if order count and margin hold. Say what moved underneath.
- Repeat-customer rate depends on the window. Use 90 days for most stores, and say which window was used.

## 7. What each person needs

| Person | Asks first | Leads with |
| --- | --- | --- |
| **Founder** | Are we growing, and is it healthy? | Net revenue with orders and AOV, repeat rate, gross margin |
| **Operations** | Are orders going out on time, and what comes back? | Days to deliver, on-time rate, return rate by product and carrier |
| **Marketing** | Which sources bring buyers, and what do they cost? | Orders by source, conversion rate, cost per new customer, return on ad spend |
| **Finance** | What did we actually keep? | Net revenue after returns, gross margin, discount share, payment reconciliation |

Nobody is asked their role. It is inferred from the questions each person asks, which move areas up and down for them (see the README, "How the profile evolves").

## 8. What to change in your own systems

Some gaps can't be closed by connecting another source, because nobody records the data. Foundry calls these out as concrete changes, each with what it reveals about customers.

- **Tag campaign links with UTM source, medium and campaign.** Reveals which campaign brought each buyer, not just which channel.
- **Save the session or visitor ID on every order.** Reveals conversion: how many visits turn into purchases, by source and device.
- **Send `add_to_cart` and `checkout_started` events.** Reveals where buyers give up between interest and purchase.
- **Record a reason on every return and refund.** Reveals which products, sizes or carriers disappoint customers.
- **Store a promised delivery date or service level on each order.** Reveals whether delivery is late, not just slow.
- **Store product cost next to price, and keep it current.** Reveals margin by product and category.
- **Use one customer ID or email across the store, email tool and support desk.** Reveals the real number of loyal customers.

Each piece of advice appears only when its gap is real for this workspace, and leaves once the data arrives.

## 9. Insight rules

These feed "Some things we noticed" (#53). Each rule names the shift, the check it uses, and what it needs.

| Shift | Check (#53) | Needs |
| --- | --- | --- |
| Regular customers order less, while new ones keep the total flat | Core-cohort shift | Customers linked to orders, 2+ periods |
| Orders depend more on one source or a few customers | Mix and dependency | Traffic source or customers |
| Average order value falls while orders hold, or the reverse | Value per activity | Amounts and orders |
| Visits from a source convert worse than before | Progression | Sessions linked to orders |
| Returns rise in one category or with one carrier | Mix and dependency | Order status, lines, shipments |
| Delivery slows for one carrier or region | Value per activity | Shipments with dates |
| Discounts grow faster than revenue | Value per activity | Line discounts |

A rule that lacks what it needs is skipped and, if people ask about it, becomes a gap.

## 10. Glossary

How the Analyst reads these words in this industry.

- **AOV (average order value):** net revenue divided by completed orders.
- **Basket size:** items per order, not money. Not the same as AOV.
- **GMV (gross merchandise value):** total sales value before returns and fees; used for marketplaces, where revenue is the take.
- **Repeat rate:** share of buyers in the window with two or more completed orders.
- **Conversion rate:** sessions with an order divided by all sessions.
- **CAC:** marketing spend divided by new customers in the same period.
- **ROAS:** revenue attributed to a source divided by spend on it.
- **Return rate:** returned orders divided by completed orders, by count unless asked by value.
- **Gross margin:** net line revenue minus product cost, as a share of net line revenue.

## 11. Branches

Branches change the defaults; only the chosen branch reaches the model. Every branch is inferred from the data and the questions asked, never asked for, and can be changed in Settings.

- **Model**
  - *Direct to consumer:* the default above.
  - *Multi-brand retailer:* adds brand and supplier mix to Revenue and Cost; margin by brand leads Cost.
  - *Marketplace:* GMV leads Revenue, with take rate; sellers become a fourth participant, and seller concentration joins the insight rules.
  - *Subscription commerce:* active subscribers and churn lead Customers; repeat rate is replaced by retention by cohort.
- **Role:** founder, operations, marketing or finance, as in section 7.
- **Stage**
  - *Early:* new customers, conversion and cost per new customer come first.
  - *Established:* repeat rate, margin and concentration come first.
