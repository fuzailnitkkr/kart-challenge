package order

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/oolio-group/kart-challenge/backend-challenge/internal/catalog"
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

// UpsertProducts persists product catalog rows into MySQL.
func (s *MySQLStore) UpsertProducts(ctx context.Context, products []catalog.Product) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("mysql store is not initialized")
	}
	if len(products) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin product tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO products (
			id, name, price, category, image_thumbnail, image_mobile, image_tablet, image_desktop
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			name = VALUES(name),
			price = VALUES(price),
			category = VALUES(category),
			image_thumbnail = VALUES(image_thumbnail),
			image_mobile = VALUES(image_mobile),
			image_tablet = VALUES(image_tablet),
			image_desktop = VALUES(image_desktop),
			updated_at = CURRENT_TIMESTAMP(6)
	`)
	if err != nil {
		return fmt.Errorf("prepare product upsert: %w", err)
	}
	defer stmt.Close()

	for _, p := range products {
		id := strings.TrimSpace(p.ID)
		name := strings.TrimSpace(p.Name)
		category := strings.TrimSpace(p.Category)
		if id == "" || name == "" || category == "" {
			return fmt.Errorf("invalid product row")
		}

		var thumbnail, mobile, tablet, desktop string
		if p.Image != nil {
			thumbnail = strings.TrimSpace(p.Image.Thumbnail)
			mobile = strings.TrimSpace(p.Image.Mobile)
			tablet = strings.TrimSpace(p.Image.Tablet)
			desktop = strings.TrimSpace(p.Image.Desktop)
		}

		if _, err := stmt.ExecContext(
			ctx,
			id,
			name,
			p.Price,
			category,
			nullIfEmpty(thumbnail),
			nullIfEmpty(mobile),
			nullIfEmpty(tablet),
			nullIfEmpty(desktop),
		); err != nil {
			return fmt.Errorf("upsert product row: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit product tx: %w", err)
	}

	return nil
}

// ListProducts returns all products from MySQL.
func (s *MySQLStore) ListProducts(ctx context.Context) ([]catalog.Product, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("mysql store is not initialized")
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, name, price, category, image_thumbnail, image_mobile, image_tablet, image_desktop
		 FROM products
		 ORDER BY CAST(id AS UNSIGNED), id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query products: %w", err)
	}
	defer rows.Close()

	products := make([]catalog.Product, 0, 32)
	for rows.Next() {
		product, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		products = append(products, product)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate products: %w", err)
	}

	return products, nil
}

// GetProductByID returns one product from MySQL.
func (s *MySQLStore) GetProductByID(ctx context.Context, id string) (catalog.Product, bool, error) {
	if s == nil || s.db == nil {
		return catalog.Product{}, false, fmt.Errorf("mysql store is not initialized")
	}

	id = strings.TrimSpace(id)
	if id == "" {
		return catalog.Product{}, false, nil
	}

	row := s.db.QueryRowContext(
		ctx,
		`SELECT id, name, price, category, image_thumbnail, image_mobile, image_tablet, image_desktop
		 FROM products
		 WHERE id = ?
		 LIMIT 1`,
		id,
	)

	product, err := scanProduct(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return catalog.Product{}, false, nil
		}
		return catalog.Product{}, false, err
	}
	return product, true, nil
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
		`CREATE TABLE IF NOT EXISTS products (
			id VARCHAR(64) NOT NULL,
			name VARCHAR(255) NOT NULL,
			price DECIMAL(10,2) NOT NULL,
			category VARCHAR(128) NOT NULL,
			image_thumbnail TEXT NULL,
			image_mobile TEXT NULL,
			image_tablet TEXT NULL,
			image_desktop TEXT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`,
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

type sqlProductScanner interface {
	Scan(dest ...any) error
}

func scanProduct(scanner sqlProductScanner) (catalog.Product, error) {
	var (
		p         catalog.Product
		thumbnail sql.NullString
		mobile    sql.NullString
		tablet    sql.NullString
		desktop   sql.NullString
	)

	if err := scanner.Scan(
		&p.ID,
		&p.Name,
		&p.Price,
		&p.Category,
		&thumbnail,
		&mobile,
		&tablet,
		&desktop,
	); err != nil {
		return catalog.Product{}, err
	}

	if thumbnail.Valid || mobile.Valid || tablet.Valid || desktop.Valid {
		p.Image = &catalog.Image{
			Thumbnail: thumbnail.String,
			Mobile:    mobile.String,
			Tablet:    tablet.String,
			Desktop:   desktop.String,
		}
	}

	return p, nil
}
