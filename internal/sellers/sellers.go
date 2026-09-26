// Package sellers owns seller profiles and their verification: users become
// sellers by creating a profile, and admins verify submitted IDs. ID numbers
// are encrypted at rest and never leave the service except as last4.
package sellers

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Verification statuses stored in seller_profiles.verification_status.
const (
	StatusUnverified = "unverified"
	StatusPending    = "pending"
	StatusVerified   = "verified"
	StatusRejected   = "rejected"
)

// IDTypes stored in seller_profiles.id_type (DOMAIN §11).
const (
	IDGhanaCard      = "ghana_card"
	IDPassport       = "passport"
	IDVotersID       = "voters_id"
	IDDriversLicense = "drivers_license"
)

// Verification decisions accepted by the admin endpoint.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

var (
	// ErrNotFound means no seller profile exists for the user.
	ErrNotFound = errors.New("seller profile not found")
	// ErrInvalidTransition means an admin decision was attempted from a
	// status other than pending.
	ErrInvalidTransition = errors.New("invalid verification transition")
)

// Profile is a seller profile row: the owner's view. IDNumber is never
// populated; IDNumberLast4 is the masked form for owner and admins.
type Profile struct {
	UserID             uuid.UUID
	BusinessName       string
	Region             string
	District           string
	Bio                *string
	ShowPhone          bool
	ShowWhatsapp       bool
	Whatsapp           *string
	VerificationStatus string
	IDType             *string
	IDNumberLast4      *string
	SubmittedAt        *time.Time
	ReviewedAt         *time.Time
	ReviewedBy         *uuid.UUID
	RejectionReason    *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// PublicProfile is the safe projection served to anonymous callers.
type PublicProfile struct {
	UserID       uuid.UUID
	BusinessName string
	Region       string
	District     string
	Bio          *string
	Verified     bool
	MemberSince  time.Time
}

// AdminProfile is the review-queue view: the profile plus its owner's
// contact fields.
type AdminProfile struct {
	Profile
	DisplayName *string
	Email       *string
	Phone       *string
}

// ProfileInput is the owner's write model. Whatsapp is raw user input,
// normalised server-side. IDType and IDNumber must be given together.
type ProfileInput struct {
	BusinessName string
	Region       string
	District     string
	Bio          *string
	ShowPhone    bool
	ShowWhatsapp bool
	Whatsapp     *string
	IDType       *string
	IDNumber     *string
}
