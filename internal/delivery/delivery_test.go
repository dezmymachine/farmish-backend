package delivery_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/delivery"
)

func TestManual_UnsupportedMethods(t *testing.T) {
	manual := delivery.Manual{}
	if _, err := manual.Quote(context.Background(), delivery.QuoteInput{Method: delivery.MethodCourier}); !errors.Is(err, delivery.ErrNotSupported) {
		t.Errorf("courier quote err = %v, want ErrNotSupported", err)
	}
	if _, err := manual.CreateShipment(context.Background(), uuid.New()); !errors.Is(err, delivery.ErrNotSupported) {
		t.Errorf("create shipment err = %v, want ErrNotSupported", err)
	}
	if _, err := manual.Track(context.Background(), "tracking-1"); !errors.Is(err, delivery.ErrNotSupported) {
		t.Errorf("track err = %v, want ErrNotSupported", err)
	}
}
