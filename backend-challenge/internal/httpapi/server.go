package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oolio-group/kart-challenge/backend-challenge/internal/catalog"
	"github.com/oolio-group/kart-challenge/backend-challenge/internal/coupon"
	"github.com/oolio-group/kart-challenge/backend-challenge/internal/order"
)

// Config configures HTTP API dependencies.
type Config struct {
	Catalog         *catalog.Catalog
	CouponValidator coupon.Validator
	OrderStore      order.Store
	APIKey          string
	DeviceHeader    string
	Environment     string
	EnableSwagger   bool
	OpenAPISpec     []byte
	RateLimit       RateLimitConfig
}

// Server exposes handlers for the OpenAPI contract.
type Server struct {
	catalog         *catalog.Catalog
	couponValidator coupon.Validator
	orderStore      order.Store
	apiKey          string
	rateLimiter     *UserRateLimiter
	rateLimitHeader string
	deviceHeader    string
	environment     string
	enableSwagger   bool
	openAPISpec     []byte
	startedAt       time.Time
}

// New constructs an API server.
func New(cfg Config) *Server {
	rateLimiter := NewUserRateLimiter(cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst, RateLimitOptions{
		EntryTTL:        cfg.RateLimit.EntryTTL,
		CleanupInterval: cfg.RateLimit.CleanupInterval,
	})

	rateLimitHeader := strings.TrimSpace(cfg.RateLimit.UserHeader)
	if rateLimitHeader == "" {
		rateLimitHeader = defaultRateLimitUserHeader
	}
	deviceHeader := strings.TrimSpace(cfg.DeviceHeader)
	if deviceHeader == "" {
		deviceHeader = defaultDeviceIDHeader
	}
	environment := strings.TrimSpace(cfg.Environment)
	if environment == "" {
		environment = "development"
	}
	enableSwagger := cfg.EnableSwagger && len(cfg.OpenAPISpec) > 0

	return &Server{
		catalog:         cfg.Catalog,
		couponValidator: cfg.CouponValidator,
		orderStore:      cfg.OrderStore,
		apiKey:          cfg.APIKey,
		rateLimiter:     rateLimiter,
		rateLimitHeader: rateLimitHeader,
		deviceHeader:    deviceHeader,
		environment:     environment,
		enableSwagger:   enableSwagger,
		openAPISpec:     append([]byte(nil), cfg.OpenAPISpec...),
		startedAt:       time.Now().UTC(),
	}
}

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler {
	secureMux := http.NewServeMux()

	secureMux.HandleFunc("GET /product", s.handleListProducts)
	secureMux.HandleFunc("GET /product/{productId}", s.handleGetProduct)
	secureMux.HandleFunc("POST /order", s.handlePlaceOrder)

	secured := withAuthAndRateLimit(secureMux, AuthAndRateLimitConfig{
		APIKey:        s.apiKey,
		UserHeader:    s.rateLimitHeader,
		DeviceHeader:  s.deviceHeader,
		RequireDevice: true,
		RateLimiter:   s.rateLimiter,
		LimitPerSec:   s.rateLimiter.LimitPerSecond(),
		BurstCapacity: s.rateLimiter.BurstCapacity(),
	})

	rootMux := http.NewServeMux()
	rootMux.HandleFunc("GET /health", s.handleHealth)
	rootMux.Handle("GET /product", secured)
	rootMux.Handle("GET /product/{productId}", secured)
	rootMux.Handle("POST /order", secured)

	if s.enableSwagger {
		rootMux.HandleFunc("GET /openapi.yaml", s.handleOpenAPIYAML)
		rootMux.HandleFunc("GET /swagger", s.handleSwaggerRoot)
		rootMux.HandleFunc("GET /swagger/", s.handleSwaggerUI)
	}

	logged := withRequestLogging(rootMux, RequestLoggingConfig{
		UserHeader:   s.rateLimitHeader,
		DeviceHeader: s.deviceHeader,
	})

	return withCORS(logged, s.deviceHeader)
}

type healthResp struct {
	Status      string `json:"status"`
	Environment string `json:"environment"`
	UptimeSec   int64  `json:"uptimeSec"`
	Time        string `json:"time"`
}

type orderReq struct {
	CouponCode string      `json:"couponCode,omitempty"`
	Items      []orderItem `json:"items"`
}

type orderItem struct {
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
}

type orderResp struct {
	ID       string            `json:"id"`
	Items    []orderItem       `json:"items"`
	Products []catalog.Product `json:"products"`
}

func (s *Server) handleListProducts(w http.ResponseWriter, _ *http.Request) {
	setProductCacheHeaders(w)
	writeJSON(w, http.StatusOK, s.catalog.List())
}

func (s *Server) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	productID := strings.TrimSpace(r.PathValue("productId"))
	idNum, err := strconv.ParseInt(productID, 10, 64)
	if err != nil || idNum <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id := strconv.FormatInt(idNum, 10)
	product, ok := s.catalog.GetByID(id)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	setProductCacheHeaders(w)
	writeJSON(w, http.StatusOK, product)
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req orderReq
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if len(req.Items) == 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	products := make([]catalog.Product, 0, len(req.Items))
	for _, item := range req.Items {
		if strings.TrimSpace(item.ProductID) == "" || item.Quantity <= 0 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}

		product, ok := s.catalog.GetByID(item.ProductID)
		if !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		products = append(products, product)
	}

	if code := strings.TrimSpace(req.CouponCode); code != "" && s.couponValidator != nil {
		valid, err := s.couponValidator.IsValid(r.Context(), code)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !valid {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
	}

	resp := orderResp{ID: randomID(), Items: req.Items, Products: products}

	if s.orderStore != nil {
		items := make([]order.Item, 0, len(req.Items))
		for _, item := range req.Items {
			items = append(items, order.Item{
				ProductID: item.ProductID,
				Quantity:  item.Quantity,
			})
		}

		record := order.Record{
			ID:         resp.ID,
			CouponCode: strings.TrimSpace(req.CouponCode),
			Items:      items,
		}

		idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if idempotencyKey != "" {
			if idemStore, ok := s.orderStore.(order.IdempotencyStore); ok {
				record.IdempotencyKey = idempotencyKey
				record.RequestHash = hashOrderRequest(req)

				stored, created, err := idemStore.CreateOrGetByIdempotency(r.Context(), record)
				if err != nil {
					if errors.Is(err, order.ErrIdempotencyConflict) {
						w.WriteHeader(http.StatusConflict)
						return
					}
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				if !created {
					itemsResp := make([]orderItem, 0, len(stored.Items))
					for _, item := range stored.Items {
						itemsResp = append(itemsResp, orderItem{
							ProductID: item.ProductID,
							Quantity:  item.Quantity,
						})
					}

					storedProducts, ok := s.productsForOrderItems(stored.Items)
					if !ok {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}

					resp.ID = stored.ID
					resp.Items = itemsResp
					resp.Products = storedProducts
				}
			} else if err := s.orderStore.Create(r.Context(), record); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		} else if err := s.orderStore.Create(r.Context(), record); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, healthResp{
		Status:      "ok",
		Environment: s.environment,
		UptimeSec:   int64(time.Since(s.startedAt).Seconds()),
		Time:        time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleOpenAPIYAML(w http.ResponseWriter, _ *http.Request) {
	if !s.enableSwagger || len(s.openAPISpec) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.openAPISpec)
}

func (s *Server) handleSwaggerRoot(w http.ResponseWriter, r *http.Request) {
	if !s.enableSwagger {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/swagger/index.html", http.StatusTemporaryRedirect)
}

func (s *Server) handleSwaggerUI(w http.ResponseWriter, r *http.Request) {
	if !s.enableSwagger {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.URL.Path != "/swagger/" && r.URL.Path != "/swagger/index.html" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	const swaggerHTML = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Food Ordering API - Swagger</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
    <script>
      window.onload = function() {
        window.ui = SwaggerUIBundle({
          url: '/openapi.yaml',
          dom_id: '#swagger-ui'
        });
      };
    </script>
  </body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(swaggerHTML))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func withCORS(next http.Handler, deviceHeader string) http.Handler {
	deviceHeader = strings.TrimSpace(deviceHeader)
	if deviceHeader == "" {
		deviceHeader = defaultDeviceIDHeader
	}

	allowedHeaders := "Content-Type, api_key, Idempotency-Key, X-User-ID, X-Device-ID, X-Forwarded-For, X-Real-IP"
	if !strings.EqualFold(deviceHeader, defaultDeviceIDHeader) {
		allowedHeaders += ", " + deviceHeader
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", allowedHeaders)
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func setProductCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=30, s-maxage=300, stale-while-revalidate=120")
}

func hashOrderRequest(req orderReq) string {
	normalized := orderReq{
		CouponCode: strings.TrimSpace(req.CouponCode),
		Items:      make([]orderItem, 0, len(req.Items)),
	}
	for _, item := range req.Items {
		normalized.Items = append(normalized.Items, orderItem{
			ProductID: strings.TrimSpace(item.ProductID),
			Quantity:  item.Quantity,
		})
	}

	raw, _ := json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Server) productsForOrderItems(items []order.Item) ([]catalog.Product, bool) {
	out := make([]catalog.Product, 0, len(items))
	for _, item := range items {
		product, ok := s.catalog.GetByID(item.ProductID)
		if !ok {
			return nil, false
		}
		out = append(out, product)
	}
	return out, true
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}
