package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"regexp"
	"sort"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/delivery"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// MaxCartLines bounds one quote request. Fifty lines is enough for a real
// basket and small enough to keep validation errors readable.
const MaxCartLines = 50

// ghanaPhone mirrors orders.recipient_phone: exactly +233 followed by nine
// digits. Checkout does not normalize the number; the buyer must send it
// exactly as the courier will dial it.
var ghanaPhone = regexp.MustCompile(`^\+233[0-9]{9}$`)

// PriceCart prices a cart without touching a database. All listings come from
// the caller-supplied snapshot map, all prices come from those snapshots, and
// delivery comes from the provider. Orders are sorted by seller and lines by
// listing, so the same cart always produces the same quote.
func PriceCart(ctx context.Context, listings map[uuid.UUID]ListingSnapshot, cart Cart, rates CommissionRates, feeBps int, deliv delivery.Provider) (CheckoutQuote, error) {
	var invalid validation.Error
	validateBuyer(cart, &invalid)
	linesBySeller := validateLines(cart, listings, &invalid)
	validateDeliveryChoices(cart, linesBySeller, &invalid)
	validateFeeBps(feeBps, &invalid)
	if err := invalid.OrNil(); err != nil {
		return CheckoutQuote{}, err
	}
	if deliv == nil {
		return CheckoutQuote{}, errors.New("checkout requires a delivery provider")
	}
	return priceValidatedCart(ctx, cart, deliv, linesBySeller, rates, feeBps)
}

func validateBuyer(cart Cart, invalid *validation.Error) {
	if cart.BuyerID == uuid.Nil {
		invalid.Add("buyerId", "is required")
	}
}

type indexedLine struct {
	index    int
	line     CartLine
	snapshot ListingSnapshot
}

func validateLines(cart Cart, listings map[uuid.UUID]ListingSnapshot, invalid *validation.Error) map[uuid.UUID][]indexedLine {
	if len(cart.Lines) == 0 {
		invalid.Add("lines", "must contain at least one line")
		return nil
	}
	if len(cart.Lines) > MaxCartLines {
		invalid.Add("lines", "must contain at most 50 lines")
		return nil
	}

	seen := make(map[uuid.UUID]bool, len(cart.Lines))
	linesBySeller := make(map[uuid.UUID][]indexedLine)
	for i, line := range cart.Lines {
		field := lineField(i, "listingId")
		if line.ListingID == uuid.Nil {
			invalid.Add(field, "is required")
			continue
		}
		if seen[line.ListingID] {
			invalid.Add(field, "each listing may appear only once")
			continue
		}
		seen[line.ListingID] = true
		snapshot, ok := listings[line.ListingID]
		if !ok {
			invalid.Add(field, "is unknown")
			continue
		}
		quantityField := lineField(i, "quantity")
		if line.Quantity < int(snapshot.MinOrderQty) {
			invalid.Add(quantityField, "is below the minimum order quantity")
			continue
		}
		if line.Quantity > int(snapshot.QuantityAvailable) {
			invalid.Add(quantityField, "is above the available quantity")
			continue
		}
		if !snapshot.Active {
			invalid.Add(field, "is not active")
			continue
		}
		if snapshot.SellerID == cart.BuyerID {
			invalid.Add(field, "cannot buy your own listing")
			continue
		}
		linesBySeller[snapshot.SellerID] = append(linesBySeller[snapshot.SellerID], indexedLine{
			index: i, line: line, snapshot: snapshot,
		})
	}
	return linesBySeller
}

func validateDeliveryChoices(cart Cart, linesBySeller map[uuid.UUID][]indexedLine, invalid *validation.Error) {
	for seller := range linesBySeller {
		choice, ok := cart.Delivery[seller]
		if !ok {
			invalid.Add("delivery."+seller.String(), "is required")
			continue
		}
		prefix := "delivery." + seller.String()
		switch choice.Method {
		case delivery.MethodPickup, delivery.MethodSellerDelivery, delivery.MethodCourier:
		default:
			invalid.Add(prefix+".method", "is not supported")
			continue
		}
		if choice.Method != delivery.MethodSellerDelivery {
			continue
		}
		if len(choice.Address) == 0 || len(choice.Address) > 300 {
			invalid.Add(prefix+".address", "is required for seller delivery")
		}
		if len(choice.RecipientName) == 0 || len(choice.RecipientName) > 120 {
			invalid.Add(prefix+".recipientName", "is required for seller delivery")
		}
		if !ghanaPhone.MatchString(choice.RecipientPhone) {
			invalid.Add(prefix+".recipientPhone", "must be +233 followed by nine digits")
		}
	}
	for seller := range cart.Delivery {
		if _, ok := linesBySeller[seller]; !ok {
			invalid.Add("delivery."+seller.String(), "does not match a seller in the cart")
		}
	}
}

func validateFeeBps(feeBps int, invalid *validation.Error) {
	if feeBps < 0 || feeBps >= money.BpsDenominator {
		invalid.Add("feeBps", "must be between 0 and 9999")
	}
}

func priceValidatedCart(ctx context.Context, cart Cart, deliv delivery.Provider, linesBySeller map[uuid.UUID][]indexedLine, rates CommissionRates, feeBps int) (CheckoutQuote, error) {
	var invalid validation.Error
	sellers := make([]uuid.UUID, 0, len(linesBySeller))
	for seller := range linesBySeller {
		sellers = append(sellers, seller)
	}
	sort.Slice(sellers, func(i, j int) bool { return bytes.Compare(sellers[i][:], sellers[j][:]) < 0 })

	quote := CheckoutQuote{Orders: make([]OrderQuote, 0, len(sellers))}
	for _, seller := range sellers {
		order, err := priceSellerOrder(ctx, deliv, seller, cart.Delivery[seller], linesBySeller[seller], rates)
		if err != nil {
			var fieldError *amountError
			if errors.As(err, &fieldError) {
				invalid.Add(fieldError.field, fieldError.message)
				continue
			}
			return CheckoutQuote{}, err
		}
		quote.Orders = append(quote.Orders, order)
		if quote.BasePesewas, err = addAmount(quote.BasePesewas, order.BasePesewas); err != nil {
			invalid.Add("base", "is too large")
		}
	}
	if err := invalid.OrNil(); err != nil {
		return CheckoutQuote{}, err
	}
	charge, fee, err := money.GrossUp(quote.BasePesewas, feeBps)
	if err != nil {
		invalid.Add("charge", "is too large")
		return CheckoutQuote{}, invalid.OrNil()
	}
	quote.ProcessingFeePesewas = fee
	quote.ChargePesewas = charge
	return quote, nil
}

func priceSellerOrder(ctx context.Context, deliv delivery.Provider, seller uuid.UUID, choice DeliveryChoice, lines []indexedLine, rates CommissionRates) (OrderQuote, error) {
	sort.Slice(lines, func(i, j int) bool {
		return bytes.Compare(lines[i].line.ListingID[:], lines[j].line.ListingID[:]) < 0
	})
	order := OrderQuote{SellerID: seller, SellerName: lines[0].snapshot.SellerName, Items: make([]ItemQuote, 0, len(lines))}
	providerItems := make([]delivery.QuoteItem, 0, len(lines))
	for _, indexed := range lines {
		lineTotal, err := multiplyAmount(indexed.snapshot.UnitPricePesewas, indexed.line.Quantity)
		if err != nil {
			return OrderQuote{}, &amountError{field: lineField(indexed.index, "quantity"), message: "is too large"}
		}
		order.Items = append(order.Items, ItemQuote{
			ListingID: indexed.line.ListingID, Title: indexed.snapshot.Title, Unit: indexed.snapshot.Unit,
			UnitPricePesewas: indexed.snapshot.UnitPricePesewas, Quantity: indexed.line.Quantity,
			LineTotalPesewas: lineTotal,
		})
		var subtotal int64
		subtotal, err = addAmount(order.SubtotalPesewas, lineTotal)
		if err != nil {
			return OrderQuote{}, &amountError{field: lineField(indexed.index, "quantity"), message: "is too large"}
		}
		order.SubtotalPesewas = subtotal
		providerItems = append(providerItems, delivery.QuoteItem{
			ListingID: indexed.line.ListingID, OffersSellerDelivery: indexed.snapshot.OffersSellerDelivery,
			SellerDeliveryFee: indexed.snapshot.SellerDeliveryFee,
		})
	}

	rate, err := resolveOrderRate(lines, rates)
	if err != nil {
		return OrderQuote{}, err
	}
	shipping, err := quoteOrderDelivery(ctx, deliv, seller, choice, providerItems)
	if err != nil {
		return OrderQuote{}, err
	}
	order.DeliveryFeePesewas = shipping
	base, err := addAmount(order.SubtotalPesewas, order.DeliveryFeePesewas)
	if err != nil {
		return OrderQuote{}, &amountError{field: "base", message: "is too large"}
	}
	order.BasePesewas = base
	commission, err := money.Commission(order.SubtotalPesewas, rate)
	if err != nil {
		return OrderQuote{}, &amountError{field: "commission", message: "is too large"}
	}
	order.CommissionRateBps = rate
	order.CommissionPesewas = commission
	return order, nil
}

// resolveOrderRate implements DOMAIN §2.1 for one seller-order. Each line
// first uses its own category override, then its parent, then the default.
// Because one order carries only one commission snapshot, lines with different
// applicable rates use the highest of those rates.
func resolveOrderRate(lines []indexedLine, rates CommissionRates) (int, error) {
	best := -1
	for _, indexed := range lines {
		rate, ok := rates.Overrides[indexed.snapshot.CategoryID]
		if !ok && indexed.snapshot.ParentCategoryID != nil {
			rate, ok = rates.Overrides[*indexed.snapshot.ParentCategoryID]
		}
		if !ok {
			rate = rates.Default
		}
		if rate > best {
			best = rate
		}
	}
	if best < 0 || best > money.BpsDenominator {
		return 0, &amountError{field: "commission", message: "has an invalid rate"}
	}
	return best, nil
}

func quoteOrderDelivery(ctx context.Context, deliv delivery.Provider, seller uuid.UUID, choice DeliveryChoice, items []delivery.QuoteItem) (int64, error) {
	field := "delivery." + seller.String() + ".method"
	quote, err := deliv.Quote(ctx, delivery.QuoteInput{Method: choice.Method, Items: items})
	if errors.Is(err, delivery.ErrDeliveryNotOffered) {
		return 0, &amountError{field: field, message: "is not offered by every item"}
	}
	if errors.Is(err, delivery.ErrNotSupported) {
		return 0, &amountError{field: field, message: "is not supported"}
	}
	if err != nil {
		return 0, fmt.Errorf("quote delivery: %w", err)
	}
	if quote.FeePesewas < 0 {
		return 0, &amountError{field: field, message: "returned a negative fee"}
	}
	return quote.FeePesewas, nil
}

type amountError struct {
	field   string
	message string
}

func (e *amountError) Error() string { return e.field + ": " + e.message }

func lineField(index int, name string) string {
	return fmt.Sprintf("lines[%d].%s", index, name)
}

func multiplyAmount(unitPrice int64, quantity int) (int64, error) {
	if unitPrice < 0 || quantity < 0 {
		return 0, errors.New("negative amount")
	}
	// check() has already rejected negatives and anything above MaxAmount, so
	// these conversions cannot wrap.
	hi, lo := bits.Mul64(uint64(unitPrice), uint64(quantity)) //nolint:gosec // G115: validated non-negative
	if hi != 0 || lo > uint64(math.MaxInt64) {
		return 0, errors.New("amount is too large")
	}
	return int64(lo), nil
}

func addAmount(first, second int64) (int64, error) {
	if first < 0 || second < 0 {
		return 0, errors.New("negative amount")
	}
	sum, carry := bits.Add64(uint64(first), uint64(second), 0) //nolint:gosec // G115: validated non-negative
	if carry != 0 || sum > uint64(math.MaxInt64) {
		return 0, errors.New("amount is too large")
	}
	return int64(sum), nil
}
