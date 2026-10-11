---
{
  "id": "ecommerce",
  "title": "Online retail",
  "version": 1,
  "match": [
    "retail",
    "e-commerce",
    "ecommerce",
    "online store",
    "online shop",
    "shop",
    "store",
    "d2c",
    "dtc",
    "direct to consumer",
    "marketplace",
    "apparel",
    "fashion",
    "boutique",
    "webshop",
    "shopify"
  ],
  "signals": {
    "require": [
      "order",
      "amount"
    ],
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
      "entity": true,
      "names": [
        "order",
        "purchase",
        "transaction",
        "sale",
        "sales_order"
      ]
    },
    "order_line": {
      "title": "Order lines",
      "entity": true,
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
      "entity": true,
      "names": [
        "product",
        "sku",
        "item",
        "variant"
      ]
    },
    "category": {
      "title": "Categories",
      "entity": true,
      "names": [
        "category",
        "collection",
        "department"
      ]
    },
    "customer": {
      "title": "Customers",
      "entity": true,
      "names": [
        "customer",
        "buyer",
        "shopper",
        "client"
      ]
    },
    "session": {
      "title": "Visits",
      "entity": true,
      "names": [
        "session",
        "visit",
        "web_session"
      ],
      "keywords": [
        "visit",
        "session",
        "traffic"
      ]
    },
    "shipment": {
      "title": "Shipments",
      "entity": true,
      "names": [
        "shipment",
        "fulfillment",
        "fulfilment",
        "delivery"
      ]
    },
    "payment": {
      "title": "Payments",
      "entity": true,
      "names": [
        "payment",
        "charge"
      ]
    },
    "refund": {
      "title": "Refunds or returns",
      "entity": true,
      "names": [
        "refund",
        "return",
        "rma"
      ]
    },
    "checkout": {
      "title": "Cart and checkout events",
      "entity": true,
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
      "entity": true,
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
      "entity": true,
      "names": [
        "ticket",
        "support_ticket",
        "case",
        "conversation",
        "complaint"
      ]
    },
    "amount": {
      "title": "Order amount",
      "of": [
        "order"
      ],
      "type": "number",
      "names": [
        "net_amount",
        "total",
        "grand_total",
        "amount",
        "subtotal",
        "total_price",
        "order_total"
      ]
    },
    "order_date": {
      "title": "Order date",
      "of": [
        "order"
      ],
      "type": "date",
      "names": [
        "order_date",
        "created_at",
        "placed_at",
        "ordered_at",
        "date"
      ]
    },
    "order_status": {
      "title": "Order status",
      "of": [
        "order"
      ],
      "type": "category",
      "names": [
        "status",
        "state",
        "order_status",
        "financial_status"
      ]
    },
    "traffic_source": {
      "title": "Traffic source",
      "of": [
        "session",
        "order"
      ],
      "type": "category",
      "names": [
        "traffic_source",
        "utm_source",
        "source",
        "medium",
        "utm_medium",
        "referrer_source"
      ]
    },
    "campaign": {
      "title": "Campaign",
      "of": [
        "session",
        "order"
      ],
      "type": "category",
      "names": [
        "utm_campaign",
        "campaign",
        "campaign_name",
        "campaign_id"
      ]
    },
    "unit_cost": {
      "title": "Product cost",
      "of": [
        "product",
        "order_line"
      ],
      "type": "number",
      "names": [
        "unit_cost",
        "cost",
        "cogs",
        "cost_price",
        "product_cost"
      ]
    },
    "line_discount": {
      "title": "Line discount",
      "of": [
        "order_line",
        "order"
      ],
      "type": "number",
      "names": [
        "discount_amount",
        "discount",
        "discount_total",
        "total_discounts"
      ]
    },
    "customer_email": {
      "title": "Customer email",
      "of": [
        "customer"
      ],
      "type": "text",
      "names": [
        "email",
        "email_address",
        "customer_email"
      ]
    },
    "delivered_at": {
      "title": "Delivery date",
      "of": [
        "shipment",
        "order"
      ],
      "type": "date",
      "names": [
        "delivered_date",
        "delivered_at",
        "delivery_date"
      ]
    },
    "promise": {
      "title": "Promised delivery date",
      "of": [
        "shipment",
        "order"
      ],
      "type": "any",
      "names": [
        "promised_date",
        "promised_at",
        "delivery_promise",
        "estimated_delivery",
        "sla",
        "service_level"
      ]
    },
    "return_reason": {
      "title": "Return reason",
      "of": [
        "refund",
        "order",
        "shipment",
        "order_line"
      ],
      "type": "any",
      "names": [
        "return_reason",
        "refund_reason",
        "reason",
        "reason_code"
      ]
    }
  },
  "areas": [
    {
      "id": "revenue",
      "title": "Revenue",
      "keywords": [
        "revenue",
        "sales",
        "sold",
        "aov",
        "order value",
        "basket",
        "orders",
        "gmv"
      ],
      "metrics": [
        "net_revenue",
        "orders",
        "aov"
      ]
    },
    {
      "id": "customers",
      "title": "Customers",
      "keywords": [
        "customer",
        "repeat",
        "loyal",
        "retention",
        "churn",
        "cohort",
        "buyer",
        "lifetime"
      ],
      "metrics": [
        "new_customers",
        "repeat_rate",
        "revenue_per_customer"
      ]
    },
    {
      "id": "experience",
      "title": "Experience",
      "keywords": [
        "deliver",
        "shipping",
        "late",
        "return",
        "refund",
        "support",
        "complaint",
        "carrier"
      ],
      "metrics": [
        "delivery_days",
        "return_rate",
        "on_time_rate"
      ]
    },
    {
      "id": "marketing",
      "title": "Marketing",
      "keywords": [
        "marketing",
        "channel",
        "source",
        "campaign",
        "conversion",
        "convert",
        "traffic",
        "ads",
        "ad spend",
        "cac",
        "roas",
        "acquisition"
      ],
      "metrics": [
        "conversion_rate",
        "channel_mix",
        "cac",
        "roas"
      ]
    },
    {
      "id": "cost",
      "title": "Cost and margin",
      "keywords": [
        "margin",
        "cost",
        "profit",
        "discount",
        "cogs"
      ],
      "metrics": [
        "gross_margin",
        "discount_rate"
      ]
    }
  ],
  "metrics": {
    "net_revenue": {
      "title": "Net revenue",
      "needs": [
        "order",
        "amount"
      ],
      "hint": "sum of the order amount over completed orders, with a 30-day comparison"
    },
    "orders": {
      "title": "Orders",
      "needs": [
        "order"
      ],
      "hint": "count of completed orders, with a 30-day comparison"
    },
    "aov": {
      "title": "Average order value",
      "needs": [
        "order",
        "amount"
      ],
      "hint": "average order amount over completed orders"
    },
    "new_customers": {
      "title": "New customers",
      "needs": [
        "customer"
      ],
      "hint": "count of customers by signup or first-order date, with a 30-day comparison"
    },
    "repeat_rate": {
      "title": "Repeat customers",
      "needs": [
        "customer",
        "order"
      ],
      "hint": "share of buyers with two or more completed orders"
    },
    "revenue_per_customer": {
      "title": "Revenue per customer",
      "needs": [
        "customer",
        "order",
        "amount"
      ]
    },
    "delivery_days": {
      "title": "Days to deliver",
      "needs": [
        "shipment",
        "delivered_at"
      ]
    },
    "return_rate": {
      "title": "Return rate",
      "needs": [
        "order",
        "order_status"
      ],
      "hint": "breakdown of orders by status shows returns"
    },
    "on_time_rate": {
      "title": "Delivered on time",
      "needs": [
        "shipment",
        "delivered_at",
        "promise"
      ]
    },
    "conversion_rate": {
      "title": "Conversion",
      "needs": [
        "session",
        "order"
      ]
    },
    "channel_mix": {
      "title": "Orders by source",
      "needs": [
        "session",
        "traffic_source"
      ],
      "hint": "breakdown of visits or orders by traffic source"
    },
    "cac": {
      "title": "Cost per new customer",
      "needs": [
        "spend",
        "customer"
      ]
    },
    "roas": {
      "title": "Return on ad spend",
      "needs": [
        "spend",
        "traffic_source"
      ]
    },
    "gross_margin": {
      "title": "Gross margin",
      "needs": [
        "order_line",
        "unit_cost"
      ]
    },
    "discount_rate": {
      "title": "Discount share",
      "needs": [
        "order_line",
        "line_discount"
      ]
    }
  },
  "normal": {
    "conversion_rate": {
      "low": 0.01,
      "high": 0.03,
      "unit": "percent",
      "text": "Conversion is typically 1–3%"
    },
    "return_rate": {
      "low": 0.1,
      "high": 0.2,
      "unit": "percent",
      "text": "Returns are typically 10–20% of orders"
    },
    "repeat_rate": {
      "low": 0.2,
      "high": 0.3,
      "unit": "percent",
      "text": "Repeat buyers are typically 20–30% within a year"
    },
    "delivery_days": {
      "low": 3,
      "high": 5,
      "unit": "days",
      "text": "Standard delivery typically takes 3–5 days"
    }
  },
  "checks": [
    {
      "id": "lines_link_orders",
      "kind": "link",
      "from": "order_line",
      "to": "order",
      "good": 0.99,
      "text": "Order lines that link to an order"
    },
    {
      "id": "lines_link_products",
      "kind": "link",
      "from": "order_line",
      "to": "product",
      "good": 0.98,
      "text": "Order lines that link to a product"
    },
    {
      "id": "orders_link_customers",
      "kind": "link",
      "from": "order",
      "to": "customer",
      "good": 0.95,
      "all": true,
      "text": "Orders that link to a customer",
      "fix": "Guest checkouts usually cause this; link them by email."
    },
    {
      "id": "sessions_link_orders",
      "kind": "link",
      "from": "session",
      "to": "order",
      "good": 0.9,
      "text": "Visits that bought, linked to their order"
    },
    {
      "id": "duplicate_customers",
      "kind": "duplicate",
      "of": "customer",
      "by": "customer_email",
      "good": 0.98,
      "text": "Customers with a unique email",
      "fix": "The same person is counted as several customers."
    },
    {
      "id": "status_meaning",
      "kind": "status",
      "of": "order_status",
      "sale": [
        "completed",
        "complete",
        "paid",
        "fulfilled",
        "delivered",
        "shipped",
        "closed"
      ],
      "undo": [
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
  "advice": [
    {
      "id": "connect_spend",
      "kind": "connect",
      "area": "marketing",
      "missing": "spend",
      "text": "Add ad spend by channel to see what each new customer costs.",
      "keywords": [
        "ad spend",
        "marketing spend",
        "spend on",
        "cac",
        "roas",
        "cost per customer",
        "cost per new",
        "acquisition cost",
        "paid+worth"
      ],
      "note": "Ad spend isn't connected, so cost per customer can't be worked out. Connecting spend by channel would answer this."
    },
    {
      "id": "connect_analytics",
      "kind": "connect",
      "area": "marketing",
      "missing": "session",
      "text": "Connect your storefront analytics to measure how many visits turn into orders.",
      "keywords": [
        "conversion",
        "convert",
        "visit",
        "traffic",
        "session",
        "bounce"
      ],
      "note": "Visits aren't connected, so conversion can't be measured. Connecting storefront analytics would answer this."
    },
    {
      "id": "connect_support",
      "kind": "connect",
      "area": "experience",
      "missing": "ticket",
      "text": "Connect your support desk to see which products and deliveries cause complaints.",
      "keywords": [
        "support",
        "ticket",
        "complaint",
        "contact",
        "csat"
      ],
      "note": "Support tickets aren't connected. Connecting your support desk would answer this."
    },
    {
      "id": "utm_tags",
      "kind": "change",
      "area": "marketing",
      "missing": "campaign",
      "text": "Tag campaign links with UTM codes, so orders show the campaign, not just the channel.",
      "keywords": [
        "campaign",
        "utm",
        "newsletter",
        "promotion"
      ],
      "note": "Visits don't record a campaign, so this can be answered by channel at most. Tagging links with UTM codes would answer it by campaign."
    },
    {
      "id": "session_on_order",
      "kind": "change",
      "area": "marketing",
      "low": "sessions_link_orders",
      "text": "Save the session or visitor ID on every order, so visits can be joined to purchases.",
      "keywords": [
        "conversion",
        "convert"
      ],
      "note": "Few orders carry the visit that led to them, so conversion is understated. Saving the session ID on each order would fix it."
    },
    {
      "id": "checkout_events",
      "kind": "change",
      "area": "marketing",
      "missing": "checkout",
      "text": "Track add-to-cart and checkout-started events, to see where buyers drop off.",
      "keywords": [
        "checkout",
        "cart",
        "abandon",
        "drop off",
        "drop-off",
        "funnel"
      ],
      "note": "Cart and checkout steps aren't tracked, so drop-off can't be measured. Tracking checkout events would answer this."
    },
    {
      "id": "refund_reasons",
      "kind": "change",
      "area": "experience",
      "missing": "return_reason",
      "text": "Record a reason on each return in your store, to see which products or carriers disappoint.",
      "keywords": [
        "why+return",
        "why+refund",
        "reason+return",
        "reason+refund"
      ],
      "note": "Returns don't record a reason, so why customers return things can't be answered. Recording a reason on each return would."
    },
    {
      "id": "delivery_promise",
      "kind": "change",
      "area": "experience",
      "missing": "promise",
      "text": "Store a promised delivery date on each order, to see whether deliveries are late, not just slow.",
      "keywords": [
        "on time",
        "late",
        "delay",
        "promise",
        "sla"
      ],
      "note": "Orders don't record a promised date, so lateness can't be measured, only delivery time."
    },
    {
      "id": "product_cost",
      "kind": "change",
      "area": "cost",
      "missing": "unit_cost",
      "text": "Store product cost next to price, to see margin by product and category.",
      "keywords": [
        "margin",
        "profit",
        "cogs",
        "cost+product"
      ],
      "note": "Product cost isn't recorded, so margin can't be worked out. Storing cost next to price would answer this."
    },
    {
      "id": "one_customer_id",
      "kind": "change",
      "area": "customers",
      "low": "duplicate_customers",
      "text": "Use one customer ID or email across your store, email tool and support desk.",
      "keywords": [
        "repeat",
        "loyal",
        "unique customers"
      ],
      "note": "Some customers appear more than once, so repeat customers are understated."
    }
  ],
  "glossary": {
    "AOV": "average order value: net revenue divided by completed orders",
    "basket size": "items per order, not money",
    "GMV": "gross merchandise value: total sales value before returns and fees",
    "repeat rate": "share of buyers with two or more completed orders in the window",
    "conversion rate": "visits that ended in an order divided by all visits",
    "CAC": "marketing spend divided by new customers in the same period",
    "ROAS": "revenue from a source divided by spend on it",
    "return rate": "returned orders divided by completed orders, by count"
  },
  "reading": [
    "Count a sale on its order date when the order is completed; report cancelled and returned orders separately.",
    "Report revenue net of discounts, before tax and shipping, and label anything else.",
    "Compare full periods only; compare with the same period a year earlier when a year of history exists.",
    "Treat business events as context for a change, never proof of its cause."
  ],
  "drivers": [
    {
      "id": "revenue",
      "title": "Net revenue",
      "equals": "orders × average order value − returns and cancellations",
      "splits": [
        "orders",
        "aov",
        "returns"
      ],
      "needs": [
        "order",
        "amount"
      ],
      "look": "Say which side moved: more or fewer orders, or bigger or smaller orders. Revenue alone hides it."
    },
    {
      "id": "orders",
      "title": "Orders",
      "equals": "visits × conversion rate",
      "splits": [
        "visits",
        "conversion"
      ],
      "needs": [
        "order"
      ],
      "look": "A drop in orders is a traffic problem or a conversion problem; check visits first."
    },
    {
      "id": "visits",
      "title": "Visits by source",
      "equals": "organic + paid search + paid social + email + direct + referral + marketplace",
      "splits": [
        "traffic_source"
      ],
      "needs": [
        "session",
        "traffic_source"
      ],
      "look": "Compare each source with the same period before; one source falling is usually the whole story."
    },
    {
      "id": "visitor_type",
      "title": "New and returning visitors",
      "equals": "new visitors + returning visitors",
      "splits": [
        "customer"
      ],
      "needs": [
        "session",
        "customer"
      ],
      "look": "Returning visitors convert several times better than new ones, so a shift in the mix moves conversion without anything else changing."
    },
    {
      "id": "device_region",
      "title": "Visits by device and region",
      "equals": "desktop + mobile + tablet, by region",
      "splits": [
        "device",
        "region"
      ],
      "needs": [
        "session"
      ],
      "look": "Mobile brings most visits but converts lower; a mobile-only drop often means a site or checkout bug."
    },
    {
      "id": "funnel",
      "title": "Conversion funnel",
      "equals": "product views → add to cart → checkout started → order",
      "splits": [
        "checkout"
      ],
      "needs": [
        "session",
        "checkout"
      ],
      "look": "Find the step that lost the most people; that step is where to look."
    },
    {
      "id": "conversion_drivers",
      "title": "What moves conversion",
      "equals": "price and discounts, stock, shipping cost and speed, checkout friction, page speed, trust",
      "splits": [
        "product",
        "line_discount"
      ],
      "needs": [
        "session",
        "order"
      ],
      "look": "Unexpected costs at checkout are the most common reason for abandoned carts; out-of-stock bestsellers and slow pages follow."
    },
    {
      "id": "aov",
      "title": "Average order value",
      "equals": "items per order × price per item",
      "splits": [
        "order_line"
      ],
      "needs": [
        "order_line"
      ],
      "look": "Say whether people bought fewer items or cheaper ones."
    },
    {
      "id": "price_mix",
      "title": "Price per item",
      "equals": "category and product mix × discount depth",
      "splits": [
        "category",
        "line_discount"
      ],
      "needs": [
        "order_line",
        "product"
      ],
      "look": "A shift toward cheaper categories or deeper discounts lowers order value even with steady demand."
    },
    {
      "id": "customers",
      "title": "Buyers",
      "equals": "new buyers + returning buyers",
      "splits": [
        "customer"
      ],
      "needs": [
        "customer",
        "order"
      ],
      "look": "New buyers replacing lost regulars can hide a loyalty problem behind a flat total."
    },
    {
      "id": "repeat",
      "title": "Repeat purchase",
      "equals": "previous buyers × repeat rate",
      "splits": [
        "traffic_source",
        "category",
        "delivered_at"
      ],
      "needs": [
        "customer",
        "order"
      ],
      "look": "First-order experience drives repeat: slow delivery, a return, or a heavy first-order discount all lower it. Compare repeat rate by first-order source and category."
    },
    {
      "id": "returns",
      "title": "Returns and cancellations",
      "equals": "returns by category, size and fit, damage, carrier",
      "splits": [
        "category",
        "shipment",
        "return_reason"
      ],
      "needs": [
        "order",
        "order_status"
      ],
      "look": "Returns concentrate in a few categories (apparel and footwear most); a jump with one carrier points at damage or delay."
    },
    {
      "id": "margin",
      "title": "Gross margin",
      "equals": "net revenue − product cost − discounts − shipping and fulfilment − payment fees",
      "splits": [
        "unit_cost",
        "line_discount"
      ],
      "needs": [
        "order_line",
        "unit_cost"
      ],
      "look": "Growth bought with discounts or free shipping can grow revenue while margin shrinks."
    },
    {
      "id": "efficiency",
      "title": "Marketing efficiency",
      "equals": "spend ÷ new customers (CAC), against first-order margin and repeat value",
      "splits": [
        "spend",
        "traffic_source"
      ],
      "needs": [
        "spend",
        "customer"
      ],
      "look": "A channel pays back when what a customer brings over time exceeds what they cost to win."
    },
    {
      "id": "concentration",
      "title": "Concentration",
      "equals": "share of revenue from the top customers, products and channels",
      "splits": [
        "customer",
        "product",
        "traffic_source"
      ],
      "needs": [
        "order",
        "amount"
      ],
      "look": "Growing dependence on a few buyers, products or one channel is a risk even when totals look healthy."
    }
  ],
  "intelligence": [
    {
      "area": "marketing",
      "text": "Returning visitors usually convert two to three times better than new ones."
    },
    {
      "area": "marketing",
      "text": "Mobile brings most visits for most stores but converts noticeably lower than desktop."
    },
    {
      "area": "marketing",
      "text": "Email and direct traffic tend to convert best; paid social tends to convert lowest and to bring first-time buyers."
    },
    {
      "area": "marketing",
      "text": "Extra costs shown late (shipping, taxes, fees) are the most common reason carts are abandoned."
    },
    {
      "area": "revenue",
      "text": "Most stores peak in November and December, with Black Friday and Cyber Monday the biggest days; compare those weeks with the same weeks last year."
    },
    {
      "area": "experience",
      "text": "Returns rise in January after the holiday peak, and apparel and footwear return far more than other categories, mostly for size and fit."
    },
    {
      "area": "customers",
      "text": "A minority of customers usually brings most revenue; losing a few regulars can matter more than winning many new buyers."
    },
    {
      "area": "customers",
      "text": "Customers won with a heavy first-order discount tend to buy again less often."
    },
    {
      "area": "experience",
      "text": "Late or slow first deliveries lower the chance a customer buys again."
    },
    {
      "area": "revenue",
      "text": "Free-shipping thresholds nudge order value up toward the threshold."
    },
    {
      "area": "cost",
      "text": "Discounts, shipping and returns often erase more margin than product cost changes do."
    }
  ]
}
---

# Online retail

> This playbook is a starting point. The owner can name their industry in their own words in Settings, or tell the Analyst, and add their own targets, normal ranges and facts; those always win over the ranges here.

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

## 4b. How to break down a change

When a number moves, an analyst breaks it into what it is made of until one part explains it. These are the fifteen levels Foundry follows, top down. The Analyst uses them to answer "why" questions, and the insight rules use them to find the part that moved.

| # | Level | Is made of | What to look for |
| --- | --- | --- | --- |
| 1 | Net revenue | orders × average order value − returns and cancellations | Which side moved: more or fewer orders, or bigger or smaller orders |
| 2 | Orders | visits × conversion rate | A traffic problem or a conversion problem; check visits first |
| 3 | Visits by source | organic, paid search, paid social, email, direct, referral, marketplace | One source falling is usually the whole story |
| 4 | New and returning visitors | new + returning | A shift in mix moves conversion with nothing else changing |
| 5 | Device and region | desktop, mobile, tablet; regions | A mobile-only drop often means a site or checkout bug |
| 6 | Conversion funnel | product view → add to cart → checkout → order | The step that lost the most people |
| 7 | What moves conversion | price and discounts, stock, shipping cost and speed, checkout friction, page speed, trust | Late extra costs, out-of-stock bestsellers, slow pages |
| 8 | Average order value | items per order × price per item | Fewer items, or cheaper ones |
| 9 | Price per item | category and product mix × discount depth | A shift to cheaper categories or deeper discounts |
| 10 | Buyers | new + returning buyers | New buyers hiding the loss of regulars |
| 11 | Repeat purchase | previous buyers × repeat rate | First-order delivery, returns and discounts; repeat rate by first source and category |
| 12 | Returns and cancellations | by category, size and fit, damage, carrier | A few categories, or one carrier |
| 13 | Gross margin | net revenue − product cost − discounts − shipping − payment fees | Revenue bought with discounts or free shipping |
| 14 | Marketing efficiency | spend ÷ new customers, against first-order margin and repeat value | Channels that win customers who never come back |
| 15 | Concentration | share of revenue from top customers, products, channels | Dependence on a few, even when totals look fine |

A level the data can't reach is named as such ("there is no checkout data, so the funnel can't be split"), never guessed.

## 4c. What the industry usually sees

Rules of thumb from online retail, used as context for a finding, never as facts about this store. They vary by category, price point and region, and the owner's own knowledge always wins.

- Returning visitors usually convert two to three times better than new ones.
- Mobile brings most visits for most stores but converts noticeably lower than desktop.
- Email and direct traffic tend to convert best; paid social tends to convert lowest and to bring first-time buyers.
- Extra costs shown late (shipping, taxes, fees) are the most common reason carts are abandoned.
- Most stores peak in November and December, with Black Friday and Cyber Monday the biggest days; compare those weeks with the same weeks last year.
- Returns rise in January after the holiday peak, and apparel and footwear return far more than other categories, mostly for size and fit.
- A minority of customers usually brings most revenue; losing a few regulars can matter more than winning many new buyers.
- Customers won with a heavy first-order discount tend to buy again less often.
- Late or slow first deliveries lower the chance a customer buys again.
- Free-shipping thresholds nudge order value up toward the threshold.
- Discounts, shipping and returns often erase more margin than product cost changes do.

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

## 12. Normal ranges

Indicative ranges that help Foundry say whether a number looks usual. They are rules of thumb that vary a lot by category, region, price point and traffic mix. Foundry shows them as "typical", never as facts about this business, and an owner's own target or range always replaces them.

| Metric | Typical | Notes |
| --- | --- | --- |
| Conversion rate | 1–3% | Sessions that end in an order. Higher for repeat visitors and cheaper items; lower on mobile. |
| Cart abandonment | 60–80% | Carts that never become an order. |
| Return rate | 10–20% | By order count. Apparel and footwear often 20–30%; electronics and home goods lower. |
| Repeat-customer rate | 20–30% | Buyers who order again within a year. |
| Days to deliver | 3–5 | Standard domestic shipping. |

How they are used:

- A finding can say "conversion is [X]%, below the typical 1–3% for online retail" only when no target is set and the metric is computed from complete periods.
- On Home, a metric can carry a small grey "typical 1–3%" under its value; nothing is coloured red or green on a normal range alone.
- When the owner adds their own knowledge, for example "our target return rate is 8%" or "Black Friday week doubles sales", it is stored with source *You* and used first.
