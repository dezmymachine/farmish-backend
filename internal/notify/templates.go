package notify

import (
	"strings"
	"text/template"
)

// Template names, one per transition (DOMAIN §4's notify column).
const (
	TemplateOrderPaidSeller      = "order_paid_seller"
	TemplateOrderAcceptedBuyer   = "order_accepted_buyer"
	TemplateOrderRejectedBuyer   = "order_rejected_buyer"
	TemplateOrderShippedBuyer    = "order_shipped_buyer"
	TemplateOrderDeliveredBuyer  = "order_delivered_buyer"
	TemplateOrderCompletedSeller = "order_completed_seller"
	TemplateOrderCancelledBuyer  = "order_cancelled_buyer"
	TemplateOrderCancelledSeller = "order_cancelled_seller"
	TemplateOrderDisputedSeller  = "order_disputed_seller"
	TemplateOrderRefundedBuyer   = "order_refunded_buyer"
)

// templates renders every message. Values are order facts the recipient
// already knows; no names, phones or addresses are interpolated.
var templates = map[string]string{
	TemplateOrderPaidSeller:      "Farmish: you have a new paid order {{.orderIdShort}}. Accept it within 48 hours or it is cancelled and refunded.",
	TemplateOrderAcceptedBuyer:   "Farmish: order {{.orderIdShort}} was accepted. We will message you when it ships.",
	TemplateOrderRejectedBuyer:   "Farmish: order {{.orderIdShort}} was declined by the seller. Your payment is being refunded.",
	TemplateOrderShippedBuyer:    "Farmish: order {{.orderIdShort}} is on its way.",
	TemplateOrderDeliveredBuyer:  "Farmish: order {{.orderIdShort}} was delivered. Confirm receipt within 3 days or open a dispute.",
	TemplateOrderCompletedSeller: "Farmish: order {{.orderIdShort}} is complete. Your earnings were released to your balance.",
	TemplateOrderCancelledBuyer:  "Farmish: order {{.orderIdShort}} was cancelled. Your payment is being refunded.",
	TemplateOrderCancelledSeller: "Farmish: order {{.orderIdShort}} was cancelled by the buyer.",
	TemplateOrderDisputedSeller:  "Farmish: the buyer opened a dispute on order {{.orderIdShort}}. Our team will review it.",
	TemplateOrderRefundedBuyer:   "Farmish: your refund for order {{.orderIdShort}} has been processed.",
}

// Render renders a template with the given values, truncated to one SMS.
func Render(name string, params map[string]string) (string, error) {
	source, ok := templates[name]
	if !ok {
		return "", ErrUnknownTemplate
	}
	parsed, err := template.New(name).Parse(source)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	if err := parsed.Execute(&out, params); err != nil {
		return "", err
	}
	message := strings.TrimSpace(out.String())
	if len(message) > MaxMessageLen {
		message = message[:MaxMessageLen]
	}
	return message, nil
}
