// Synthetic store, with intentional missing links and repeated emails.
export function retailData() {
  const customers = Array.from({ length: 5 }, (_, i) => ({
    customer_id: `c${i}`,
    email: i < 2 ? "same@example.test" : `customer${i}@example.test`,
  }));
  const products = Array.from({ length: 6 }, (_, i) => ({
    product_id: `p${i}`,
    unit_cost: 4 + i,
  }));
  const sessions = Array.from({ length: 10 }, (_, i) => ({
    session_id: `s${i}`,
    started_at: new Date(Date.UTC(2026, 6, 1 + i)).toISOString(),
  }));
  const orders = Array.from({ length: 20 }, (_, i) => ({
    order_id: `o${i}`,
    customer_id: i < 16 ? `c${i % 5}` : null,
    session_id: i < 2 ? `s${i}` : null,
    ordered_at: new Date(Date.UTC(2026, 6, 1 + i * 4)).toISOString(),
    net_amount: 10 + i * 2,
    status: i === 3 ? "returned" : i === 9 ? "cancelled" : "completed",
  }));
  const order_items = Array.from({ length: 30 }, (_, i) => ({
    line_id: `l${i}`,
    order_id: i < 24 ? `o${i % 20}` : "missing",
    product_id: i < 27 ? `p${i % 6}` : null,
  }));
  return { customers, products, sessions, orders, order_items };
}
