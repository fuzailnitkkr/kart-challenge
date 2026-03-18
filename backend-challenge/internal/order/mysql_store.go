package order

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

const (
	defaultMaxOpenConns    = 100
	defaultMaxIdleConns    = 25
	defaultConnMaxLifetime = 5 * time.Minute
	defaultConnMaxIdleTime = 2 * time.Minute
)

// MySQLStoreOptions configures DB pooling and connection behavior.
type MySQLStoreOptions struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// MySQLStore stores orders in MySQL.
type MySQLStore struct {
	db *sql.DB
}

// NewMySQLStore opens a MySQL database and ensures schema.
func NewMySQLStore(dsn string, opts MySQLStoreOptions) (*MySQLStore, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("mysql dsn is required")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	applyPoolDefaults(db, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}

	if err := migrateMySQL(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &MySQLStore{db: db}, nil
}

// Close closes the underlying database handle.
func (s *MySQLStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Create persists an order and all of its items atomically.
func (s *MySQLStore) Create(ctx context.Context, record Record) error {
	return s.create(ctx, record)
}

// CreateOrGetByIdempotency stores a new order or returns the existing one
// for the same idempotency key.
func (s *MySQLStore) CreateOrGetByIdempotency(ctx context.Context, record Record) (Record, bool, error) {
	record.IdempotencyKey = strings.TrimSpace(record.IdempotencyKey)
	record.RequestHash = strings.TrimSpace(record.RequestHash)

	if record.IdempotencyKey == "" {
		return Record{}, false, fmt.Errorf("idempotency key is required")
	}
	if record.RequestHash == "" {
		return Record{}, false, fmt.Errorf("request hash is required")
	}

	if err := s.create(ctx, record); err != nil {
		if !isDuplicateEntry(err) {
			return Record{}, false, err
		}

		existing, found, getErr := s.GetByIdempotencyKey(ctx, record.IdempotencyKey)
		if getErr != nil {
			return Record{}, false, getErr
		}
		if !found {
			return Record{}, false, fmt.Errorf("idempotency duplicate detected but existing order not found")
		}
		if existing.RequestHash != record.RequestHash {
			return existing, false, ErrIdempotencyConflict
		}
		return existing, false, nil
	}

	return record, true, nil
}

// GetByIdempotencyKey returns an order previously written with the given key.
func (s *MySQLStore) GetByIdempotencyKey(ctx context.Context, key string) (Record, bool, error) {
	if s == nil || s.db == nil {
		return Record{}, false, fmt.Errorf("mysql store is not initialized")
	}

	key = strings.TrimSpace(key)
	if key == "" {
		return Record{}, false, fmt.Errorf("idempotency key is required")
	}

	var record Record
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT id, COALESCE(coupon_code, ''), created_at, COALESCE(idempotency_key, ''), COALESCE(request_hash, '')
		 FROM orders
		 WHERE idempotency_key = ?
		 LIMIT 1`,
		key,
	).Scan(&record.ID, &record.CouponCode, &record.CreatedAt, &record.IdempotencyKey, &record.RequestHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("query order by idempotency key: %w", err)
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT product_id, quantity
		 FROM order_items
		 WHERE order_id = ?
		 ORDER BY position ASC, id ASC`,
		record.ID,
	)
	if err != nil {
		return Record{}, false, fmt.Errorf("query order items by idempotency key: %w", err)
	}
	defer rows.Close()

	items := make([]Item, 0, 4)
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ProductID, &item.Quantity); err != nil {
			return Record{}, false, fmt.Errorf("scan order item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Record{}, false, fmt.Errorf("iterate order items: %w", err)
	}

	record.Items = items
	return record, true, nil
}

func (s *MySQLStore) create(ctx context.Context, record Record) error {
	if strings.TrimSpace(record.ID) == "" {
		return fmt.Errorf("order id is required")
	}
	if len(record.Items) == 0 {
		return fmt.Errorf("at least one item is required")
	}
	if s == nil || s.db == nil {
		return fmt.Errorf("mysql store is not initialized")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO orders (id, coupon_code, created_at, idempotency_key, request_hash) VALUES (?, ?, ?, ?, ?)`,
		record.ID,
		strings.TrimSpace(record.CouponCode),
		record.CreatedAt.UTC(),
		nullIfEmpty(record.IdempotencyKey),
		nullIfEmpty(record.RequestHash),
	); err != nil {
		return fmt.Errorf("insert order: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO order_items (order_id, product_id, quantity, position) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare order item insert: %w", err)
	}
	defer stmt.Close()

	for i, item := range record.Items {
		if strings.TrimSpace(item.ProductID) == "" || item.Quantity <= 0 {
			return fmt.Errorf("invalid order item")
		}
		if _, err := stmt.ExecContext(ctx, record.ID, item.ProductID, item.Quantity, i); err != nil {
			return fmt.Errorf("insert order item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit order tx: %w", err)
	}

	return nil
}

func applyPoolDefaults(db *sql.DB, opts MySQLStoreOptions) {
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpenConns
	}
	maxIdle := opts.MaxIdleConns
	if maxIdle < 0 {
		maxIdle = 0
	}
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	if maxIdle == 0 {
		maxIdle = defaultMaxIdleConns
		if maxIdle > maxOpen {
			maxIdle = maxOpen
		}
	}
	maxLifetime := opts.ConnMaxLifetime
	if maxLifetime <= 0 {
		maxLifetime = defaultConnMaxLifetime
	}
	maxIdleTime := opts.ConnMaxIdleTime
	if maxIdleTime <= 0 {
		maxIdleTime = defaultConnMaxIdleTime
	}

	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(maxLifetime)
	db.SetConnMaxIdleTime(maxIdleTime)
}

func migrateMySQL(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS orders (
			id VARCHAR(64) NOT NULL,
			coupon_code VARCHAR(32) NULL,
			created_at DATETIME(6) NOT NULL,
			idempotency_key VARCHAR(128) NULL,
			request_hash CHAR(64) NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`,
		`CREATE TABLE IF NOT EXISTS order_items (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			order_id VARCHAR(64) NOT NULL,
			product_id VARCHAR(64) NOT NULL,
			quantity INT NOT NULL,
			position INT NOT NULL DEFAULT 0,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (id),
			KEY idx_order_items_order_id (order_id),
			CONSTRAINT fk_order_items_order FOREIGN KEY (order_id) REFERENCES orders(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`,
		`ALTER TABLE orders ADD COLUMN idempotency_key VARCHAR(128) NULL`,
		`ALTER TABLE orders ADD COLUMN request_hash CHAR(64) NULL`,
		`ALTER TABLE orders ADD UNIQUE KEY uk_orders_idempotency_key (idempotency_key)`,
		`ALTER TABLE order_items ADD COLUMN position INT NOT NULL DEFAULT 0`,
		`ALTER TABLE order_items ADD KEY idx_order_items_order_pos (order_id, position)`,
	}

	for i, q := range statements {
		if _, err := db.ExecContext(ctx, q); err != nil {
			// Ignore duplicate errors on ALTER statements for existing schemas.
			if i >= 2 && isDuplicateSchemaEntity(err) {
				continue
			}
			return fmt.Errorf("migrate mysql: %w", err)
		}
	}
	return nil
}

func isDuplicateEntry(err error) bool {
	var dbErr *mysql.MySQLError
	if !errors.As(err, &dbErr) {
		return false
	}
	return dbErr.Number == 1062
}

func isDuplicateSchemaEntity(err error) bool {
	var dbErr *mysql.MySQLError
	if !errors.As(err, &dbErr) {
		return false
	}
	// 1060: duplicate column name, 1061: duplicate key name.
	return dbErr.Number == 1060 || dbErr.Number == 1061
}

func nullIfEmpty(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
