package order

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrIdempotencyConflict is returned when the same idempotency key
	// is reused with a different request payload.
	ErrIdempotencyConflict = errors.New("idempotency key conflict")
)

// Item is a persisted order line item.
type Item struct {
	ProductID string
	Quantity  int
}

// Record is the persisted order payload.
type Record struct {
	ID         string
	CouponCode string
	Items      []Item
	CreatedAt  time.Time

	// Optional fields used for idempotent order creation.
	IdempotencyKey string
	RequestHash    string
}

// Store persists orders.
type Store interface {
	Create(ctx context.Context, record Record) error
}

// IdempotencyStore supports "create-or-return-existing" behavior
// for idempotent writes.
type IdempotencyStore interface {
	Store
	CreateOrGetByIdempotency(ctx context.Context, record Record) (stored Record, created bool, err error)
}
