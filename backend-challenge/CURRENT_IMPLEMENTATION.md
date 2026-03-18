# Current Implementation Documentation

Last updated: 2026-03-19

## 1. What Is Implemented

This service implements the OpenAPI-backed food ordering API in Go with:

- Product catalog read APIs
- Order placement API
- Global API key auth middleware
- Per-user rate limiting middleware
- Request logging middleware
- Device ID and client IP capture on every API request
- Coupon validation logic with large-file support
- MySQL persistence for orders
- Idempotency support for safe retries
- Coupon index hot-reload without API restart

The API layer is stateless and suitable for multi-replica deployment behind a load balancer.

## 2. Project Structure

- `cmd/api/main.go`: API process entrypoint (HTTP server + graceful shutdown)
- `cmd/couponindex/main.go`: CLI to build coupon index from huge source files
- `internal/app/app.go`: environment config + dependency wiring
- `internal/httpapi/server.go`: HTTP routing + request validation + response shaping
- `internal/httpapi/security.go`: auth + rate-limit middleware
- `internal/httpapi/ratelimit.go`: per-user token-bucket limiter
- `internal/httpapi/logging.go`: request logging middleware
- `internal/catalog/catalog.go`: product catalog loader + in-memory lookup
- `internal/coupon/validator.go`: file-scanning coupon validator (fallback mode)
- `internal/coupon/tokenize.go`: streaming tokenizer for coupon candidates
- `internal/coupon/index.go`: coupon index build + binary search validator
- `internal/coupon/reload.go`: background coupon index hot-reloader
- `internal/order/store.go`: store interfaces + idempotency error contract
- `internal/order/mysql_store.go`: MySQL persistence implementation + schema migration

## 3. Runtime Configuration

All config is loaded in `internal/app/app.go` via `ConfigFromEnv()`.

- `PORT` (default `8080`)
- `PRODUCTS_FILE` (default `data/products.json`)
- `API_KEY` (default `apitest`)
- `DEVICE_ID_HEADER` (default `X-Device-ID`)
- `RATE_LIMIT_RPS` (default `20`)
- `RATE_LIMIT_BURST` (default `40`)
- `RATE_LIMIT_USER_HEADER` (default `X-User-ID`)
- `RATE_LIMIT_ENTRY_TTL` (default `15m`)
- `RATE_LIMIT_CLEANUP_INTERVAL` (default `1m`)
- `MYSQL_DSN` (default `root:root@tcp(127.0.0.1:3306)/orderfood?parseTime=true&charset=utf8mb4,utf8`)
- `MYSQL_MAX_OPEN_CONNS` (default `100`)
- `MYSQL_MAX_IDLE_CONNS` (default `25`)
- `MYSQL_CONN_MAX_LIFETIME` (default `5m`)
- `MYSQL_CONN_MAX_IDLE_TIME` (default `2m`)
- `COUPON_INDEX_FILE` (optional; enables indexed validation)
- `COUPON_INDEX_RELOAD_INTERVAL` (default `30s`)
- `COUPON_FILES` (optional CSV fallback file list when index is not configured)

Coupon mode selection:

- If `COUPON_INDEX_FILE` is set: uses `ReloadingIndexedValidator`
- Else: uses `FileValidator` over configured/default coupon files

## 4. HTTP API Behavior

Implemented in `internal/httpapi/server.go`.

### 4.1 CORS

Global middleware sets:

- `Access-Control-Allow-Origin: *`
- `Access-Control-Allow-Headers: Content-Type, api_key, Idempotency-Key, X-User-ID, X-Device-ID, X-Forwarded-For, X-Real-IP`
- `Access-Control-Allow-Methods: GET,POST,OPTIONS`

`OPTIONS` responds with `204 No Content`.

### 4.2 Global Auth and Rate Limiting

Auth middleware:

- Applied to all API endpoints before business handlers
- Reads `api_key` header
- Missing `api_key` -> `401 Unauthorized`
- Wrong `api_key` -> `403 Forbidden`
- Requires `X-Device-ID` header on all API calls
- Missing device ID -> `400 Bad Request`

Rate limiting middleware:

- Token-bucket limiter with per-user buckets
- User key combines user (`X-User-ID`), device (`X-Device-ID`), and client IP
- Client IP is resolved from `X-Forwarded-For`, then `X-Real-IP`, then remote socket address
- Limit exceeded -> `429 Too Many Requests` + `Retry-After: 1`
- Response includes `X-RateLimit-Limit` and `X-RateLimit-Burst` when limiter is enabled

Request logging:

- Logs every API request with method, path, status, bytes, duration, `ip`, `device_id`, and `user_id`

### 4.3 GET /product

- Returns full product list from in-memory catalog
- Response: `200 OK` JSON
- Adds cache header:
  - `Cache-Control: public, max-age=30, s-maxage=300, stale-while-revalidate=120`

### 4.4 GET /product/{productId}

- Validates path param as positive integer
- Invalid ID format/value -> `400 Bad Request`
- Missing product -> `404 Not Found`
- Success -> `200 OK` JSON product
- Adds same cache header as list endpoint

### 4.5 POST /order

Payload rules:

- JSON decode with `DisallowUnknownFields()`
- Any malformed JSON / unknown fields / extra trailing JSON -> `400 Bad Request`
- `items` must be non-empty
- Each item must have non-empty `productId` and `quantity > 0`
- Each `productId` must exist in catalog
- Business validation failures -> `422 Unprocessable Entity`

Coupon logic:

- If `couponCode` is provided, validator checks it
- Invalid coupon -> `422 Unprocessable Entity`
- Coupon system error -> `500 Internal Server Error`

Persistence:

- Generates order ID (16 random bytes hex)
- Persists order/items via `OrderStore`
- Store failure -> `500 Internal Server Error`

Idempotency:

- Optional request header: `Idempotency-Key`
- If provided and store supports `IdempotencyStore`:
  - Same key + same normalized payload hash -> returns existing order, `200 OK`
  - Same key + different payload hash -> `409 Conflict`

Success response:

- `200 OK`
- Body includes `id`, submitted `items`, and resolved `products`

## 5. Product Catalog Implementation

From `internal/catalog/catalog.go`:

- Products are loaded once from JSON at startup
- Stored as:
  - ordered slice for list endpoint
  - map by ID for O(1) lookup
- Read operations are in-memory and lock-free (immutable after load)

## 6. Coupon Validation Logic

### 6.1 Canonical Rules (shared)

A coupon is considered format-valid only if:

- Length is between 8 and 10
- Characters are uppercase alphanumeric (`A-Z`, `0-9`)

Normalization:

- Trim whitespace
- Convert to uppercase

### 6.2 Fallback Mode: FileValidator

In `internal/coupon/validator.go`:

- Scans each source file on cache miss
- Supports `.gz` and plain text
- Uses streaming tokenizer (`visitNormalizedCodes`) to avoid loading file into memory
- A code is valid only if found in at least 2 distinct files
- Results cached in-memory map (`code -> bool`) for repeated lookups

This mode is correct but expensive for very large files at high request rates.

### 6.3 Indexed Mode: Build Once, Query Fast

In `internal/coupon/index.go` + `cmd/couponindex/main.go`:

Build pipeline:

1. Stream and tokenize each source file
2. Create sorted unique chunk files per source (`chunk-size` configurable)
3. Multi-pass merge chunks into one sorted unique file per source
4. Intersect sources by sorted walk and keep codes present in >=2 files
5. Write compact binary index to `output.tmp`, then atomic rename to final path

Index file format:

- Header magic: `CPNIDX1\n`
- 8-byte big-endian record count
- Fixed-size records (11 bytes each):
  - 1 byte length
  - up to 10 bytes code bytes

Lookup runtime:

- `OpenIndexedValidator()` validates header + file size
- `IsValid()` performs binary search (`O(log N)`) using `ReadAt`
- No full-file scans per request

### 6.4 Hot Reload Without Restart

In `internal/coupon/reload.go`:

- `ReloadingIndexedValidator` checks index file every interval (`COUPON_INDEX_RELOAD_INTERVAL`)
- Reload trigger: `(modTime, size)` change
- Opens new validator and swaps pointer under lock
- Closes old validator after successful swap
- API server does not need restart when index is rebuilt at same path

## 7. MySQL Persistence

Implemented in `internal/order/mysql_store.go`.

### 7.1 Connection and Pooling

`NewMySQLStore()`:

- Validates DSN
- Opens DB handle
- Applies pool defaults/overrides
- Pings DB with timeout
- Runs schema migration

Pool controls are deployment configurable via env vars.

### 7.2 Schema (Auto-Migrated)

`orders` table:

- `id VARCHAR(64)` primary key
- `coupon_code VARCHAR(32)` nullable
- `created_at DATETIME(6)`
- `idempotency_key VARCHAR(128)` nullable, unique index
- `request_hash CHAR(64)` nullable

`order_items` table:

- `id BIGINT UNSIGNED` auto-increment primary key
- `order_id VARCHAR(64)` FK -> `orders(id)` with `ON DELETE CASCADE`
- `product_id VARCHAR(64)`
- `quantity INT`
- `position INT` (preserves request item order)
- `created_at DATETIME(6)`
- Indexes on order lookup and `(order_id, position)`

Migration behavior tolerates duplicate-column/duplicate-key errors on ALTER steps for existing databases.

### 7.3 Write Path

Order create is transactional:

1. Insert row into `orders`
2. Insert each `order_items` row with position
3. Commit

Validation inside store protects against empty IDs, empty items, invalid item values, and nil DB.

## 8. Idempotency Contract

Interfaces in `internal/order/store.go`:

- `Store`: basic create
- `IdempotencyStore`: create-or-return-existing semantics

MySQL behavior:

1. Try insert with `idempotency_key` unique constraint
2. On duplicate key:
   - fetch existing order by idempotency key
   - compare stored `request_hash`
   - same hash -> return existing order (`created=false`)
   - different hash -> `ErrIdempotencyConflict`

API maps conflict error to HTTP `409`.

## 9. Process Lifecycle

`cmd/api/main.go`:

- Constructs runtime (`BuildRuntime`)
- Starts HTTP server with timeouts:
  - read header: 5s
  - read: 15s
  - write: 30s
  - idle: 60s
- Handles `SIGINT`/`SIGTERM`
- Graceful shutdown with 10s timeout
- Closes runtime resources (coupon validator + DB) on exit

## 10. Testing Coverage

### 10.1 Unit and Handler Tests

- `internal/httpapi/server_test.go`
  - auth behavior
  - per-user rate-limit behavior
  - request validation
  - coupon behavior
  - order persistence path
  - idempotency replay/conflict
  - cache header checks

- Coupon tests:
  - `internal/coupon/validator_test.go`
  - `internal/coupon/index_test.go`
  - `internal/coupon/reload_test.go`

### 10.2 MySQL Tests

- `internal/order/mysql_store_test.go`
- Integration tests run only when `MYSQL_TEST_DSN` is set
- Covers create persistence and idempotency behavior on real MySQL

Run all tests:

```bash
go test ./...
```

## 11. Operating Guide

### 11.1 Run API

```bash
go run ./cmd/api
```

### 11.2 Build Coupon Index From Huge Files

```bash
go run ./cmd/couponindex \
  -out data/coupons.idx \
  /Users/fuzail/Downloads/couponbase1.txt \
  /Users/fuzail/Downloads/couponbase2.txt \
  /Users/fuzail/Downloads/couponbase3.txt
```

Then run API with indexed validation:

```bash
COUPON_INDEX_FILE=data/coupons.idx go run ./cmd/api
```

If you rebuild `data/coupons.idx` later, running API processes reload automatically; restart is not required.

## 12. Scale Characteristics (Current)

What scales well now:

- Stateless API nodes can be horizontally replicated
- Product reads are in-memory + cacheable at CDN/proxy layer
- Coupon lookup is `O(log N)` in indexed mode
- Idempotency key makes retries safe across replicas
- MySQL pool tuning is configurable per deployment

Current bottlenecks/gaps for ultra-high scale:

- `POST /order` is still synchronous DB write path
- No async queue ingestion path implemented yet
- No external cache (Redis) layer implemented yet
- No partition/shard strategy implemented in code yet

These gaps are extension points, not blockers for moderate production traffic.

## 13. Extensibility Notes

The codebase is already split by domain boundaries (`catalog`, `coupon`, `order`, `httpapi`, `app`) and uses interfaces for storage (`Store`, `IdempotencyStore`).

This makes it straightforward to add:

- queue-backed async order ingestion
- alternate order stores
- richer coupon strategies
- more endpoints from spec without tight coupling
