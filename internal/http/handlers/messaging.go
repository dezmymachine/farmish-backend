package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/messaging"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// MessageStore is the part of messaging.Service these handlers use.
type MessageStore interface {
	StartConversation(ctx context.Context, buyerID, listingID uuid.UUID, message string) (messaging.Conversation, messaging.Message, bool, error)
	SendMessage(ctx context.Context, senderID, conversationID uuid.UUID, body string) (messaging.Message, error)
	GetMessages(ctx context.Context, callerID, conversationID uuid.UUID, before string, limit int32) ([]messaging.Message, *string, error)
	MarkRead(ctx context.Context, callerID, conversationID uuid.UUID) error
	ListConversations(ctx context.Context, callerID uuid.UUID, limit, offset int32) ([]messaging.Summary, int64, error)
}

// StartConversation opens or reuses a conversation and appends the message.
func (s Server) StartConversation(ctx context.Context, req api.StartConversationRequestObject) (api.StartConversationResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.StartConversation400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	convo, sent, created, err := s.Messages.StartConversation(ctx, u.ID, req.Body.ListingId, req.Body.Message)
	if classified, handled := s.classifyMessagingError(err); handled {
		switch classified.status {
		case 400:
			return api.StartConversation400JSONResponse{
				BadRequestJSONResponse: api.BadRequestJSONResponse(classified.body),
			}, nil
		case 403:
			return api.StartConversation403JSONResponse{
				ForbiddenJSONResponse: api.ForbiddenJSONResponse(classified.body),
			}, nil
		default:
			return api.StartConversation404JSONResponse{
				NotFoundJSONResponse: api.NotFoundJSONResponse(classified.body),
			}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	out := api.StartConversation201JSONResponse(toConversation(convo, sent))
	if !created {
		return api.StartConversation200JSONResponse(toConversation(convo, sent)), nil
	}
	return out, nil
}

// ListConversations returns the caller's summaries, newest activity first.
func (s Server) ListConversations(ctx context.Context, req api.ListConversationsRequestObject) (api.ListConversationsResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	page, limit := pageParams(req.Params.Page, req.Params.Limit)
	items, total, err := s.Messages.ListConversations(ctx, u.ID, limit, (page-1)*limit)
	if err != nil {
		return nil, err
	}
	summaries := make([]api.ConversationSummary, 0, len(items))
	for _, item := range items {
		summaries = append(summaries, toConversationSummary(item))
	}
	return api.ListConversations200JSONResponse(api.ConversationList{
		Items: summaries, Meta: api.PageMeta{Page: page, Limit: limit, Total: total},
	}), nil
}

// ListMessages returns one history page, newest first.
func (s Server) ListMessages(ctx context.Context, req api.ListMessagesRequestObject) (api.ListMessagesResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	before := ""
	if req.Params.Before != nil {
		before = *req.Params.Before
	}
	limit := int32(20)
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	items, next, err := s.Messages.GetMessages(ctx, u.ID, req.Id, before, limit)
	if classified, handled := s.classifyMessagingError(err); handled {
		switch classified.status {
		case 400:
			return api.ListMessages400JSONResponse{
				BadRequestJSONResponse: api.BadRequestJSONResponse(classified.body),
			}, nil
		case 403:
			return api.ListMessages403JSONResponse{
				ForbiddenJSONResponse: api.ForbiddenJSONResponse(classified.body),
			}, nil
		default:
			return api.ListMessages404JSONResponse{
				NotFoundJSONResponse: api.NotFoundJSONResponse(classified.body),
			}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]api.Message, 0, len(items))
	for _, item := range items {
		out = append(out, toMessage(item))
	}
	return api.ListMessages200JSONResponse(api.MessageList{Items: out, NextCursor: next}), nil
}

// SendMessage appends to a conversation the caller participates in.
func (s Server) SendMessage(ctx context.Context, req api.SendMessageRequestObject) (api.SendMessageResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if req.Body == nil {
		return api.SendMessage400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse(apierror.New(apierror.CodeBadRequest, "Malformed request")),
		}, nil
	}
	sent, err := s.Messages.SendMessage(ctx, u.ID, req.Id, req.Body.Body)
	if classified, handled := s.classifyMessagingError(err); handled {
		switch classified.status {
		case 400:
			return api.SendMessage400JSONResponse{
				BadRequestJSONResponse: api.BadRequestJSONResponse(classified.body),
			}, nil
		case 403:
			return api.SendMessage403JSONResponse{
				ForbiddenJSONResponse: api.ForbiddenJSONResponse(classified.body),
			}, nil
		default:
			return api.SendMessage404JSONResponse{
				NotFoundJSONResponse: api.NotFoundJSONResponse(classified.body),
			}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return api.SendMessage201JSONResponse(toMessage(sent)), nil
}

// MarkConversationRead sets the caller's read marker to now.
func (s Server) MarkConversationRead(ctx context.Context, req api.MarkConversationReadRequestObject) (api.MarkConversationReadResponseObject, error) {
	u, ok := users.FromContext(ctx)
	if !ok {
		return nil, errNoUser
	}
	if err := s.Messages.MarkRead(ctx, u.ID, req.Id); err != nil {
		if classified, handled := s.classifyMessagingError(err); handled {
			switch classified.status {
			case 403:
				return api.MarkConversationRead403JSONResponse{
					ForbiddenJSONResponse: api.ForbiddenJSONResponse(classified.body),
				}, nil
			default:
				return api.MarkConversationRead404JSONResponse{
					NotFoundJSONResponse: api.NotFoundJSONResponse(classified.body),
				}, nil
			}
		}
		return nil, err
	}
	return api.MarkConversationRead204Response{}, nil
}

// classifyMessagingError maps domain errors onto the contract once.
func (s Server) classifyMessagingError(err error) (orderError, bool) {
	var invalid *validation.Error
	switch {
	case err == nil:
		return orderError{}, false
	case errors.As(err, &invalid):
		return orderError{status: 400, body: validationFailed(invalid)}, true
	case errors.Is(err, messaging.ErrNotFound):
		return orderError{status: 404, body: api.Error{Error: api.ErrorBody{
			Code: apierror.CodeNotFound, Message: "Conversation not found",
		}}}, true
	case errors.Is(err, messaging.ErrListingInactive):
		return orderError{status: 404, body: api.Error{Error: api.ErrorBody{
			Code: apierror.CodeNotFound, Message: "Listing is not available for messaging",
		}}}, true
	case errors.Is(err, messaging.ErrForbidden):
		return orderError{status: 403, body: api.Error{Error: api.ErrorBody{
			Code: apierror.CodeForbidden, Message: "You are not a participant of this conversation",
		}}}, true
	case errors.Is(err, messaging.ErrOwnListing):
		return orderError{status: 403, body: api.Error{Error: api.ErrorBody{
			Code: apierror.CodeForbidden, Message: "You cannot message your own listing",
		}}}, true
	case errors.Is(err, messaging.ErrBadCursor):
		return orderError{status: 400, body: api.Error{Error: api.ErrorBody{
			Code: apierror.CodeBadRequest, Message: "Invalid history cursor",
		}}}, true
	}
	return orderError{}, false
}

// toConversation maps a conversation plus its appended message.
func toConversation(convo messaging.Conversation, sent messaging.Message) api.Conversation {
	out := api.Conversation{
		Id: convo.ID, ListingId: convo.ListingID, CreatedAt: convo.CreatedAt,
		LastMessage: &api.Message{},
	}
	if convo.OrderID != nil {
		out.OrderId = convo.OrderID
	}
	last := toMessage(sent)
	out.LastMessage = &last
	return out
}

// toMessage maps one message.
func toMessage(message messaging.Message) api.Message {
	return api.Message{
		Id: message.ID, ConversationId: message.ConversationID, SenderId: message.SenderID,
		Body: message.Body, CreatedAt: message.CreatedAt,
	}
}

// toConversationSummary maps one safe-projected summary.
func toConversationSummary(summary messaging.Summary) api.ConversationSummary {
	out := api.ConversationSummary{
		Id: summary.ConversationID,
		Listing: api.ConversationListingRef{
			Id: summary.Listing.ID, Slug: summary.Listing.Slug, Title: summary.Listing.Title,
			CoverImageUrl: summary.Listing.CoverImageURL,
		},
		Counterpart: api.ConversationCounterpart{
			UserId: summary.Counterpart.UserID, Name: summary.Counterpart.Name,
		},
		UnreadCount: summary.UnreadCount,
	}
	if summary.LastMessage != nil {
		out.LastMessage = &api.ConversationLastMessage{
			Body: summary.LastMessage.Body, CreatedAt: summary.LastMessage.CreatedAt,
			FromMe: summary.LastMessage.FromMe,
		}
	}
	return out
}
