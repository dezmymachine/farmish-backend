-- Refunds (Phase 17a). A row is created inside the transition's own
-- transaction (the side effect of a cancellation, or the checkout expiry
-- recovery path); the orders.refund job makes the external Paystack call and
-- reacts to its result. The partial unique index stops two full refunds on
-- the same order; a dispute's partial refund (Phase 17b) is exempt, because
-- several partial refunds against one order are legal.
CREATE TABLE refunds (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id           uuid NOT NULL REFERENCES orders(id),
  amount_pesewas     bigint NOT NULL CHECK (amount_pesewas > 0),
  reason             text NOT NULL CHECK (reason IN ('seller_rejected','buyer_cancelled','seller_timeout',
                                                     'stock_unavailable','dispute_refund','dispute_partial')),
  status             text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','pending','processed','failed')),
  paystack_refund_id text UNIQUE,
  failure_reason     text CHECK (char_length(failure_reason) <= 500),
  -- When the Paystack call was last attempted. An ambiguous failure (timeout,
  -- 5xx) keeps the refund pending: before any retry the job reconciles against
  -- Paystack's refund list, and only retries once this is 15 minutes old with
  -- no matching refund at Paystack (ADR-0027).
  attempted_at       timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX refunds_one_full_per_order ON refunds (order_id)
  WHERE reason IN ('seller_rejected','buyer_cancelled','seller_timeout','stock_unavailable','dispute_refund');
CREATE INDEX refunds_order_idx ON refunds (order_id);
CREATE TRIGGER refunds_set_updated_at BEFORE UPDATE ON refunds
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Defence in depth for "refunds never exceed the order base" (the service
-- also checks, under the order lock, when creating and when settling).
ALTER TABLE orders ADD CONSTRAINT orders_refunded_within_base CHECK (refunded_pesewas <= base_pesewas);
