package order

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func TestMySQLStore_CreateValidation(t *testing.T) {
	t.Parallel()

	store := &MySQLStore{}

	if err := store.Create(context.Background(), Record{ID: "", Items: []Item{{ProductID: "1", Quantity: 1}}}); err == nil {
		t.Fatalf("expected error for empty order id")
	}

	if err := store.Create(context.Background(), Record{ID: "order-1"}); err == nil {
		t.Fatalf("expected error for empty items")
	}

	if _, _, err := store.CreateOrGetByIdempotency(context.Background(), Record{
		ID:             "order-2",
		Items:          []Item{{ProductID: "1", Quantity: 1}},
		RequestHash:    "abc",
		IdempotencyKey: "",
	}); err == nil {
		t.Fatalf("expected error for missing idempotency key")
	}

	if _, _, err := store.CreateOrGetByIdempotency(context.Background(), Record{
		ID:             "order-3",
		Items:          []Item{{ProductID: "1", Quantity: 1}},
		IdempotencyKey: "idem-1",
	}); err == nil {
		t.Fatalf("expected error for missing request hash")
	}
}

func TestMySQLStore_CreateIntegration(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()

	store, err := NewMySQLStore(dsn, MySQLStoreOptions{
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewMySQLStore() error = %v", err)
	}
	defer store.Close()

	record := Record{
		ID:         "order-integration-1",
		CouponCode: "HAPPYHRS",
		Items: []Item{
			{ProductID: "1", Quantity: 2},
			{ProductID: "2", Quantity: 1},
		},
		CreatedAt: time.Now().UTC(),
	}

	if err := store.Create(ctx, record); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	rawDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer rawDB.Close()

	var orderCount int
	if err := rawDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE id = ?`, record.ID).Scan(&orderCount); err != nil {
		t.Fatalf("query orders: %v", err)
	}
	if orderCount != 1 {
		t.Fatalf("stored orders = %d, want 1", orderCount)
	}

	var itemCount int
	if err := rawDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM order_items WHERE order_id = ?`, record.ID).Scan(&itemCount); err != nil {
		t.Fatalf("query order_items: %v", err)
	}
	if itemCount != 2 {
		t.Fatalf("stored order_items = %d, want 2", itemCount)
	}

	_, _ = rawDB.ExecContext(ctx, `DELETE FROM order_items WHERE order_id = ?`, record.ID)
	_, _ = rawDB.ExecContext(ctx, `DELETE FROM orders WHERE id = ?`, record.ID)
}

func TestMySQLStore_IdempotencyIntegration(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()

	store, err := NewMySQLStore(dsn, MySQLStoreOptions{
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewMySQLStore() error = %v", err)
	}
	defer store.Close()

	base := Record{
		ID:             "order-idem-1",
		CouponCode:     "HAPPYHRS",
		Items:          []Item{{ProductID: "1", Quantity: 2}},
		CreatedAt:      time.Now().UTC(),
		IdempotencyKey: "idem-integration-1",
		RequestHash:    "hash-a",
	}

	stored, created, err := store.CreateOrGetByIdempotency(ctx, base)
	if err != nil {
		t.Fatalf("CreateOrGetByIdempotency(first) error = %v", err)
	}
	if !created || stored.ID != base.ID {
		t.Fatalf("expected first call to create record")
	}

	stored, created, err = store.CreateOrGetByIdempotency(ctx, Record{
		ID:             "order-idem-2",
		CouponCode:     "HAPPYHRS",
		Items:          []Item{{ProductID: "1", Quantity: 2}},
		CreatedAt:      time.Now().UTC(),
		IdempotencyKey: "idem-integration-1",
		RequestHash:    "hash-a",
	})
	if err != nil {
		t.Fatalf("CreateOrGetByIdempotency(replay) error = %v", err)
	}
	if created {
		t.Fatalf("expected replay call to return existing record")
	}
	if stored.ID != base.ID {
		t.Fatalf("expected replayed order id %s, got %s", base.ID, stored.ID)
	}

	_, _, err = store.CreateOrGetByIdempotency(ctx, Record{
		ID:             "order-idem-3",
		CouponCode:     "HAPPYHRS",
		Items:          []Item{{ProductID: "1", Quantity: 1}},
		CreatedAt:      time.Now().UTC(),
		IdempotencyKey: "idem-integration-1",
		RequestHash:    "hash-b",
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}

	rawDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer rawDB.Close()

	_, _ = rawDB.ExecContext(ctx, `DELETE FROM order_items WHERE order_id IN (?, ?, ?)`, "order-idem-1", "order-idem-2", "order-idem-3")
	_, _ = rawDB.ExecContext(ctx, `DELETE FROM orders WHERE id IN (?, ?, ?)`, "order-idem-1", "order-idem-2", "order-idem-3")
}
