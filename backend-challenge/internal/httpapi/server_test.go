package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oolio-group/kart-challenge/backend-challenge/internal/catalog"
	"github.com/oolio-group/kart-challenge/backend-challenge/internal/order"
)

type fakeCouponValidator struct {
	valid map[string]bool
	err   error
}

func (f fakeCouponValidator) IsValid(_ context.Context, code string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.valid[code], nil
}

type fakeOrderStore struct {
	records []order.Record
	err     error
	byKey   map[string]order.Record
}

type fakeProductReader struct {
	list    []catalog.Product
	getByID map[string]catalog.Product
	listErr error
	getErr  error
}

func (f fakeProductReader) ListProducts(_ context.Context) ([]catalog.Product, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	out := make([]catalog.Product, len(f.list))
	copy(out, f.list)
	return out, nil
}

func (f fakeProductReader) GetProductByID(_ context.Context, id string) (catalog.Product, bool, error) {
	if f.getErr != nil {
		return catalog.Product{}, false, f.getErr
	}
	p, ok := f.getByID[id]
	return p, ok, nil
}

func (f *fakeOrderStore) Create(_ context.Context, record order.Record) error {
	if f.err != nil {
		return f.err
	}
	f.records = append(f.records, record)
	return nil
}

func (f *fakeOrderStore) CreateOrGetByIdempotency(_ context.Context, record order.Record) (order.Record, bool, error) {
	if f.err != nil {
		return order.Record{}, false, f.err
	}
	if f.byKey == nil {
		f.byKey = make(map[string]order.Record)
	}

	existing, ok := f.byKey[record.IdempotencyKey]
	if ok {
		if existing.RequestHash != record.RequestHash {
			return existing, false, order.ErrIdempotencyConflict
		}
		return existing, false, nil
	}

	f.byKey[record.IdempotencyKey] = record
	f.records = append(f.records, record)
	return record, true, nil
}

func TestServer_ListProducts(t *testing.T) {
	t.Parallel()

	h, _ := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{})
	req := httptest.NewRequest(http.MethodGet, "/product", nil)
	setRequiredHeaders(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if cacheControl := rec.Header().Get("Cache-Control"); cacheControl == "" {
		t.Fatalf("expected Cache-Control header")
	}

	var products []catalog.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &products); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(products) != 2 {
		t.Fatalf("len(products) = %d, want 2", len(products))
	}
}

func TestServer_ListProducts_FromProductReader(t *testing.T) {
	t.Parallel()

	cat := testCatalog(t)
	reader := fakeProductReader{
		list: []catalog.Product{
			{ID: "9", Name: "Donut", Category: "Dessert", Price: 5.25},
		},
		getByID: map[string]catalog.Product{
			"9": {ID: "9", Name: "Donut", Category: "Dessert", Price: 5.25},
		},
	}

	server := New(Config{
		Catalog:         cat,
		ProductReader:   reader,
		CouponValidator: fakeCouponValidator{},
		OrderStore:      &fakeOrderStore{},
		APIKey:          "apitest",
	})

	req := httptest.NewRequest(http.MethodGet, "/product", nil)
	setRequiredHeaders(req)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var products []catalog.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &products); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(products) != 1 {
		t.Fatalf("len(products) = %d, want 1", len(products))
	}
	if products[0].ID != "9" {
		t.Fatalf("product id = %q, want 9", products[0].ID)
	}
}

func TestServer_ListProducts_ProductReaderError(t *testing.T) {
	t.Parallel()

	cat := testCatalog(t)
	server := New(Config{
		Catalog:         cat,
		ProductReader:   fakeProductReader{listErr: errors.New("db down")},
		CouponValidator: fakeCouponValidator{},
		OrderStore:      &fakeOrderStore{},
		APIKey:          "apitest",
	})

	req := httptest.NewRequest(http.MethodGet, "/product", nil)
	setRequiredHeaders(req)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestServer_HealthEndpoint(t *testing.T) {
	t.Parallel()

	h, _ := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp healthResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("health status = %q, want ok", resp.Status)
	}
}

func TestServer_SwaggerDisabledByDefault(t *testing.T) {
	t.Parallel()

	h, _ := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_SwaggerEnabledNonProd(t *testing.T) {
	t.Parallel()

	cat := testCatalog(t)
	server := New(Config{
		Catalog:         cat,
		CouponValidator: fakeCouponValidator{},
		OrderStore:      &fakeOrderStore{},
		APIKey:          "apitest",
		DeviceHeader:    "X-Device-ID",
		Environment:     "development",
		EnableSwagger:   true,
		OpenAPISpec:     []byte("openapi: 3.1.0\ninfo:\n  title: Test\n  version: 1.0.0\n"),
	})
	h := server.Handler()

	reqSpec := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	recSpec := httptest.NewRecorder()
	h.ServeHTTP(recSpec, reqSpec)
	if recSpec.Code != http.StatusOK {
		t.Fatalf("openapi status = %d, want %d", recSpec.Code, http.StatusOK)
	}
	if got := recSpec.Body.String(); got == "" || !bytes.Contains([]byte(got), []byte("openapi:")) {
		t.Fatalf("expected openapi payload, got %q", got)
	}

	reqUI := httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
	recUI := httptest.NewRecorder()
	h.ServeHTTP(recUI, reqUI)
	if recUI.Code != http.StatusOK {
		t.Fatalf("swagger status = %d, want %d", recUI.Code, http.StatusOK)
	}
	if got := recUI.Body.String(); !bytes.Contains([]byte(got), []byte("SwaggerUIBundle")) {
		t.Fatalf("expected swagger html bundle, got %q", got)
	}
}

func TestServer_GetProduct(t *testing.T) {
	t.Parallel()

	h, _ := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{})

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/product/1", nil)
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if cacheControl := rec.Header().Get("Cache-Control"); cacheControl == "" {
			t.Fatalf("expected Cache-Control header")
		}
	})

	t.Run("invalid_id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/product/abc", nil)
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/product/9", nil)
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
}

func TestServer_GetProduct_FromProductReader(t *testing.T) {
	t.Parallel()

	cat := testCatalog(t)
	reader := fakeProductReader{
		list: []catalog.Product{
			{ID: "9", Name: "Donut", Category: "Dessert", Price: 5.25},
		},
		getByID: map[string]catalog.Product{
			"9": {ID: "9", Name: "Donut", Category: "Dessert", Price: 5.25},
		},
	}

	server := New(Config{
		Catalog:         cat,
		ProductReader:   reader,
		CouponValidator: fakeCouponValidator{},
		OrderStore:      &fakeOrderStore{},
		APIKey:          "apitest",
	})

	req := httptest.NewRequest(http.MethodGet, "/product/9", nil)
	setRequiredHeaders(req)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var out catalog.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.ID != "9" {
		t.Fatalf("id = %q, want 9", out.ID)
	}
}

func TestServer_AuthAppliesToProductEndpoints(t *testing.T) {
	t.Parallel()

	h, _ := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{})
	req := httptest.NewRequest(http.MethodGet, "/product", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestServer_DeviceIDRequired(t *testing.T) {
	t.Parallel()

	h, _ := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{})
	req := httptest.NewRequest(http.MethodGet, "/product", nil)
	req.Header.Set("api_key", "apitest")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestServer_PlaceOrder(t *testing.T) {
	t.Parallel()

	orderStore := &fakeOrderStore{}
	handler, sharedStore := testHandler(t, fakeCouponValidator{
		valid: map[string]bool{
			"HAPPYHRS": true,
		},
	}, orderStore)

	t.Run("missing_api_key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[{"productId":"1","quantity":1}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("wrong_api_key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[{"productId":"1","quantity":1}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("api_key", "wrong")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("validation_error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[{"productId":"999","quantity":1}]}`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
		}
	})

	t.Run("invalid_coupon", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"couponCode":"NOPECODE","items":[{"productId":"1","quantity":1}]}`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
		}

		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		message, _ := out["error"].(string)
		if strings.TrimSpace(message) == "" {
			t.Fatalf("expected non-empty error message, got %q", rec.Body.String())
		}
	})

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"couponCode":"HAPPYHRS","items":[{"productId":"1","quantity":2}]}`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var out orderResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if out.ID == "" {
			t.Fatalf("expected non-empty order id")
		}
		if len(out.Items) != 1 || len(out.Products) != 1 {
			t.Fatalf("unexpected response payload: %+v", out)
		}

		if len(sharedStore.records) == 0 {
			t.Fatalf("expected order to be persisted")
		}
		saved := sharedStore.records[len(sharedStore.records)-1]
		if saved.ID != out.ID {
			t.Fatalf("saved order id = %s, response id = %s", saved.ID, out.ID)
		}
		if len(saved.Items) != 1 || saved.Items[0].ProductID != "1" || saved.Items[0].Quantity != 2 {
			t.Fatalf("unexpected saved order items: %+v", saved.Items)
		}
		if saved.CouponCode != "HAPPYHRS" {
			t.Fatalf("saved coupon = %q, want HAPPYHRS", saved.CouponCode)
		}
	})

	t.Run("db_write_error", func(t *testing.T) {
		handler, _ := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{
			err: errors.New("db down"),
		})

		req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[{"productId":"1","quantity":2}]}`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("idempotency_replay_returns_same_order", func(t *testing.T) {
		handler, store := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{})

		req1 := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[{"productId":"1","quantity":2}]}`))
		req1.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req1)
		req1.Header.Set("Idempotency-Key", "idem-1")
		rec1 := httptest.NewRecorder()
		handler.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("first status = %d, want %d", rec1.Code, http.StatusOK)
		}

		req2 := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[{"productId":"1","quantity":2}]}`))
		req2.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req2)
		req2.Header.Set("Idempotency-Key", "idem-1")
		rec2 := httptest.NewRecorder()
		handler.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusOK {
			t.Fatalf("second status = %d, want %d", rec2.Code, http.StatusOK)
		}

		var out1, out2 orderResp
		if err := json.Unmarshal(rec1.Body.Bytes(), &out1); err != nil {
			t.Fatalf("decode first response: %v", err)
		}
		if err := json.Unmarshal(rec2.Body.Bytes(), &out2); err != nil {
			t.Fatalf("decode second response: %v", err)
		}

		if out1.ID == "" || out2.ID == "" {
			t.Fatalf("expected non-empty order ids")
		}
		if out1.ID != out2.ID {
			t.Fatalf("expected replayed order id to match: %s vs %s", out1.ID, out2.ID)
		}
		if len(store.records) != 1 {
			t.Fatalf("expected single persisted order for replay, got %d", len(store.records))
		}
	})

	t.Run("idempotency_conflict", func(t *testing.T) {
		handler, _ := testHandler(t, fakeCouponValidator{}, &fakeOrderStore{})

		req1 := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[{"productId":"1","quantity":2}]}`))
		req1.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req1)
		req1.Header.Set("Idempotency-Key", "idem-2")
		rec1 := httptest.NewRecorder()
		handler.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("first status = %d, want %d", rec1.Code, http.StatusOK)
		}

		req2 := httptest.NewRequest(http.MethodPost, "/order", bytes.NewBufferString(`{"items":[{"productId":"1","quantity":1}]}`))
		req2.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req2)
		req2.Header.Set("Idempotency-Key", "idem-2")
		rec2 := httptest.NewRecorder()
		handler.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusConflict {
			t.Fatalf("second status = %d, want %d", rec2.Code, http.StatusConflict)
		}
	})
}

func TestServer_ValidateCoupon(t *testing.T) {
	t.Parallel()

	handler, _ := testHandler(t, fakeCouponValidator{
		valid: map[string]bool{
			"HAPPYHRS": true,
		},
	}, &fakeOrderStore{})

	t.Run("missing_api_key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/coupon/validate", bytes.NewBufferString(`{"couponCode":"HAPPYHRS"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("missing_device_id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/coupon/validate", bytes.NewBufferString(`{"couponCode":"HAPPYHRS"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("api_key", "apitest")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/coupon/validate", bytes.NewBufferString(`{"couponCode":`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("empty_coupon", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/coupon/validate", bytes.NewBufferString(`{"couponCode":"   "}`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid_coupon", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/coupon/validate", bytes.NewBufferString(`{"couponCode":"badcode"}`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
		}

		var out couponValidateResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if out.Valid {
			t.Fatalf("valid = %v, want false", out.Valid)
		}
		if out.CouponCode != "BADCODE" {
			t.Fatalf("couponCode = %q, want BADCODE", out.CouponCode)
		}
		if strings.TrimSpace(out.Message) == "" {
			t.Fatalf("expected non-empty message")
		}
	})

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/coupon/validate", bytes.NewBufferString(`{"couponCode":"happyhrs"}`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var out couponValidateResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !out.Valid {
			t.Fatalf("valid = %v, want true", out.Valid)
		}
		if out.CouponCode != "HAPPYHRS" {
			t.Fatalf("couponCode = %q, want HAPPYHRS", out.CouponCode)
		}
		if strings.TrimSpace(out.Message) == "" {
			t.Fatalf("expected non-empty message")
		}
	})

	t.Run("validator_error", func(t *testing.T) {
		handler, _ := testHandler(t, fakeCouponValidator{
			err: errors.New("validator down"),
		}, &fakeOrderStore{})

		req := httptest.NewRequest(http.MethodPost, "/coupon/validate", bytes.NewBufferString(`{"couponCode":"HAPPYHRS"}`))
		req.Header.Set("Content-Type", "application/json")
		setRequiredHeaders(req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

func TestServer_RateLimitPerUser(t *testing.T) {
	t.Parallel()

	handler, _ := testHandlerWithRateLimit(t, fakeCouponValidator{}, &fakeOrderStore{}, RateLimitConfig{
		RequestsPerSecond: 1,
		Burst:             1,
		UserHeader:        "X-User-ID",
		EntryTTL:          time.Minute,
		CleanupInterval:   10 * time.Millisecond,
	})

	req1 := httptest.NewRequest(http.MethodGet, "/product", nil)
	setRequiredHeaders(req1)
	req1.Header.Set("X-User-ID", "user-a")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", rec1.Code, http.StatusOK)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/product", nil)
	setRequiredHeaders(req2)
	req2.Header.Set("X-User-ID", "user-a")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", rec2.Code, http.StatusTooManyRequests)
	}

	req3 := httptest.NewRequest(http.MethodGet, "/product", nil)
	setRequiredHeaders(req3)
	req3.Header.Set("X-User-ID", "user-b")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("third status = %d, want %d", rec3.Code, http.StatusOK)
	}
}

func testHandler(t *testing.T, validator fakeCouponValidator, orderStore *fakeOrderStore) (http.Handler, *fakeOrderStore) {
	return testHandlerWithRateLimit(t, validator, orderStore, RateLimitConfig{})
}

func testHandlerWithRateLimit(t *testing.T, validator fakeCouponValidator, orderStore *fakeOrderStore, rateLimit RateLimitConfig) (http.Handler, *fakeOrderStore) {
	t.Helper()

	cat := testCatalog(t)
	server := New(Config{
		Catalog:         cat,
		CouponValidator: validator,
		OrderStore:      orderStore,
		APIKey:          "apitest",
		RateLimit:       rateLimit,
	})
	return server.Handler(), orderStore
}

func testCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "products.json")
	contents := `[
{"id":"1","name":"Waffle","category":"Dessert","price":6.5},
{"id":"2","name":"Cake","category":"Dessert","price":7.0}
]`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write products: %v", err)
	}

	cat, err := catalog.LoadFromJSON(path)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return cat
}

func setRequiredHeaders(r *http.Request) {
	r.Header.Set("api_key", "apitest")
	r.Header.Set("X-Device-ID", "device-1")
}
