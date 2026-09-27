package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payouts"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// PayoutStore is the part of payouts.Service these handlers use.
type PayoutStore interface {
	ListBanks(ctx context.Context, accountType string) ([]payments.Bank, error)
	Get(ctx context.Context, sellerID uuid.UUID) (payouts.Account, error)
	SetAccount(ctx context.Context, sellerID uuid.UUID, displayName string, in payouts.Input) (payouts.Account, error)
	Approve(ctx context.Context, adminID, sellerID uuid.UUID) (payouts.Account, error)
}

// ListPayoutBanks returns Paystack's banks for one account type, or both
// lists when no type is given.
func (s Server) ListPayoutBanks(ctx context.Context, req api.ListPayoutBanksRequestObject) (api.ListPayoutBanksResponseObject, error) {
	if _, ok := users.FromContext(ctx); !ok {
		return nil, errNoUser
	}
	accountType := ""
	if req.Params.Type != nil {
		accountType = string(*req.Params.Type)
	}
	banks, err := s.Payouts.ListBanks(ctx, accountType)
	var verr *validation.Error
	switch {
	case err == nil:
	case errors.As(err, &verr):
		return api.ListPayoutBanks400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	default:
		return nil, err
	}
	items := make([]api.BankItem, 0, len(banks))
	for _, bank := range banks {
		items = append(items, api.BankItem{Code: bank.Code, Name: bank.Name})
	}
	return api.ListPayoutBanks200JSONResponse{Items: items}, nil
}

// GetMyPayoutAccount returns the caller's masked payout account.
func (s Server) GetMyPayoutAccount(ctx context.Context, _ api.GetMyPayoutAccountRequestObject) (api.GetMyPayoutAccountResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	account, err := s.Payouts.Get(ctx, u.ID)
	switch {
	case errors.Is(err, payouts.ErrSellerProfileRequired):
		return api.GetMyPayoutAccount403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "A seller profile is required")),
		}, nil
	case errors.Is(err, payouts.ErrNotFound):
		return api.GetMyPayoutAccount404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Payout account not found")),
		}, nil
	case err != nil:
		return nil, err
	}
	return api.GetMyPayoutAccount200JSONResponse(toPayoutAccount(account)), nil
}

// SetMyPayoutAccount resolves, name-checks and stores the caller's payout
// account. Step-up freshness is enforced by middleware before reaching here.
func (s Server) SetMyPayoutAccount(ctx context.Context, req api.SetMyPayoutAccountRequestObject) (api.SetMyPayoutAccountResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.SetMyPayoutAccount400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	displayName := ""
	if u.DisplayName != nil {
		displayName = *u.DisplayName
	}
	account, err := s.Payouts.SetAccount(ctx, u.ID, displayName, payouts.Input{
		Type: string(req.Body.Type), BankCode: req.Body.BankCode, AccountNumber: req.Body.AccountNumber,
	})
	var verr *validation.Error
	switch {
	case err == nil:
		return api.SetMyPayoutAccount200JSONResponse(toPayoutAccount(account)), nil
	case errors.As(err, &verr):
		return api.SetMyPayoutAccount400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(validationFailed(verr)),
		}, nil
	case errors.Is(err, payouts.ErrSellerProfileRequired):
		return api.SetMyPayoutAccount403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse(apierror.New(apierror.CodeForbidden, "A seller profile is required")),
		}, nil
	case errors.Is(err, payouts.ErrUnresolvable):
		return api.SetMyPayoutAccount422JSONResponse{
			UnprocessableJSONResponse: api.UnprocessableJSONResponse(
				apierror.New(apierror.CodeAccountUnresolvable, "This account could not be resolved; check the number and bank")),
		}, nil
	case errors.Is(err, payments.ErrProviderUnavailable), errors.Is(err, payments.ErrRejected):
		return api.SetMyPayoutAccount502JSONResponse{
			PaymentProviderErrorJSONResponse: api.PaymentProviderErrorJSONResponse(
				apierror.New(apierror.CodePaymentProvider, "The payment provider is unavailable; try again")),
		}, nil
	default:
		return nil, err
	}
}

// ApprovePayoutAccount moves a needs_review account to verified.
func (s Server) ApprovePayoutAccount(ctx context.Context, req api.ApprovePayoutAccountRequestObject) (api.ApprovePayoutAccountResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	account, err := s.Payouts.Approve(ctx, u.ID, req.SellerId)
	if err == nil {
		return api.ApprovePayoutAccount200JSONResponse(toPayoutAccount(account)), nil
	}
	switch {
	case errors.Is(err, payouts.ErrNotFound):
		return api.ApprovePayoutAccount404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse(apierror.New(apierror.CodeNotFound, "Payout account not found")),
		}, nil
	case errors.Is(err, payouts.ErrAlreadyVerified):
		return api.ApprovePayoutAccount409JSONResponse{
			ConflictJSONResponse: api.ConflictJSONResponse(apierror.New(apierror.CodeConflict, "This payout account needs no approval")),
		}, nil
	default:
		return nil, err
	}
}

// toPayoutAccount maps a masked account onto the contract. The ciphertext
// and the recipient code stay server-side: 18b reads the recipient when it
// executes payouts.
func toPayoutAccount(account payouts.Account) api.PayoutAccount {
	out := api.PayoutAccount{
		Type: api.PayoutAccountType(account.Type), BankCode: account.BankCode, BankName: account.BankName,
		AccountNumberMasked: account.NumberMask, AccountName: account.AccountName,
		Status: api.PayoutAccountStatus(account.Status),
	}
	if account.CooldownUntil != nil {
		out.CooldownUntil = account.CooldownUntil
	}
	return out
}
