package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oolio-group/kart-challenge/backend-challenge/internal/catalog"
	"github.com/oolio-group/kart-challenge/backend-challenge/internal/coupon"
	"github.com/oolio-group/kart-challenge/backend-challenge/internal/httpapi"
	"github.com/oolio-group/kart-challenge/backend-challenge/internal/order"
)

// Config defines runtime app configuration.
type Config struct {
	Address                   string
	ProductsFile              string
	Environment               string
	EnableSwagger             bool
	OpenAPIFile               string
	MySQLDSN                  string
	MySQLMaxOpenConns         int
	MySQLMaxIdleConns         int
	MySQLConnMaxLifetime      time.Duration
	MySQLConnMaxIdleTime      time.Duration
	APIKey                    string
	DeviceIDHeader            string
	RateLimitRPS              float64
	RateLimitBurst            int
	RateLimitUserHeader       string
	RateLimitEntryTTL         time.Duration
	RateLimitCleanupInterval  time.Duration
	CouponFiles               []string
	CouponIndex               string
	CouponIndexReloadInterval time.Duration
}

// Runtime contains initialized app resources.
type Runtime struct {
	Handler http.Handler
	Close   func() error
}

// ConfigFromEnv builds config from environment variables.
func ConfigFromEnv() Config {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	address := port
	if !strings.Contains(address, ":") {
		address = ":" + address
	}

	productsFile := strings.TrimSpace(os.Getenv("PRODUCTS_FILE"))
	if productsFile == "" {
		productsFile = "data/products.json"
	}
	environment := strings.TrimSpace(os.Getenv("APP_ENV"))
	if environment == "" {
		environment = "development"
	}
	isProd := strings.EqualFold(environment, "prod") || strings.EqualFold(environment, "production")
	enableSwagger := parseBoolOrDefault(os.Getenv("ENABLE_SWAGGER"), !isProd)
	openAPIFile := strings.TrimSpace(os.Getenv("OPENAPI_FILE"))
	if openAPIFile == "" {
		openAPIFile = filepath.Join("data", "openapi.yaml")
	}

	apiKey := strings.TrimSpace(os.Getenv("API_KEY"))
	if apiKey == "" {
		apiKey = "apitest"
	}
	deviceIDHeader := strings.TrimSpace(os.Getenv("DEVICE_ID_HEADER"))
	if deviceIDHeader == "" {
		deviceIDHeader = "X-Device-ID"
	}

	rateLimitUserHeader := strings.TrimSpace(os.Getenv("RATE_LIMIT_USER_HEADER"))
	if rateLimitUserHeader == "" {
		rateLimitUserHeader = "X-User-ID"
	}

	mysqlDSN := strings.TrimSpace(os.Getenv("MYSQL_DSN"))
	if mysqlDSN == "" {
		mysqlDSN = "root:root@tcp(127.0.0.1:3306)/orderfood?parseTime=true&charset=utf8mb4,utf8"
	}

	return Config{
		Address:                   address,
		ProductsFile:              productsFile,
		Environment:               environment,
		EnableSwagger:             enableSwagger,
		OpenAPIFile:               openAPIFile,
		MySQLDSN:                  mysqlDSN,
		MySQLMaxOpenConns:         parseIntOrDefault(os.Getenv("MYSQL_MAX_OPEN_CONNS"), 100),
		MySQLMaxIdleConns:         parseIntOrDefault(os.Getenv("MYSQL_MAX_IDLE_CONNS"), 25),
		MySQLConnMaxLifetime:      parseDurationOrDefault(os.Getenv("MYSQL_CONN_MAX_LIFETIME"), 5*time.Minute),
		MySQLConnMaxIdleTime:      parseDurationOrDefault(os.Getenv("MYSQL_CONN_MAX_IDLE_TIME"), 2*time.Minute),
		APIKey:                    apiKey,
		DeviceIDHeader:            deviceIDHeader,
		RateLimitRPS:              parseFloatOrDefault(os.Getenv("RATE_LIMIT_RPS"), 20),
		RateLimitBurst:            parseIntOrDefault(os.Getenv("RATE_LIMIT_BURST"), 40),
		RateLimitUserHeader:       rateLimitUserHeader,
		RateLimitEntryTTL:         parseDurationOrDefault(os.Getenv("RATE_LIMIT_ENTRY_TTL"), 15*time.Minute),
		RateLimitCleanupInterval:  parseDurationOrDefault(os.Getenv("RATE_LIMIT_CLEANUP_INTERVAL"), 1*time.Minute),
		CouponFiles:               parseCSV(os.Getenv("COUPON_FILES")),
		CouponIndex:               strings.TrimSpace(os.Getenv("COUPON_INDEX_FILE")),
		CouponIndexReloadInterval: parseDurationOrDefault(os.Getenv("COUPON_INDEX_RELOAD_INTERVAL"), 30*time.Second),
	}
}

// BuildRuntime wires dependencies and returns app resources.
func BuildRuntime(cfg Config) (*Runtime, error) {
	cat, err := catalog.LoadFromJSON(cfg.ProductsFile)
	if err != nil {
		return nil, err
	}

	validator, closer, err := buildCouponValidator(cfg)
	if err != nil {
		return nil, err
	}

	orderStore, err := order.NewMySQLStore(cfg.MySQLDSN, order.MySQLStoreOptions{
		MaxOpenConns:    cfg.MySQLMaxOpenConns,
		MaxIdleConns:    cfg.MySQLMaxIdleConns,
		ConnMaxLifetime: cfg.MySQLConnMaxLifetime,
		ConnMaxIdleTime: cfg.MySQLConnMaxIdleTime,
	})
	if err != nil {
		_ = closer.Close()
		return nil, fmt.Errorf("initialize mysql order store: %w", err)
	}
	orderCloser := io.Closer(orderStore)

	var openAPISpec []byte
	if cfg.EnableSwagger {
		openAPISpec, err = os.ReadFile(cfg.OpenAPIFile)
		if err != nil {
			_ = closer.Close()
			_ = orderCloser.Close()
			return nil, fmt.Errorf("read openapi file: %w", err)
		}
	}

	srv := httpapi.New(httpapi.Config{
		Catalog:         cat,
		CouponValidator: validator,
		OrderStore:      orderStore,
		APIKey:          cfg.APIKey,
		DeviceHeader:    cfg.DeviceIDHeader,
		Environment:     cfg.Environment,
		EnableSwagger:   cfg.EnableSwagger,
		OpenAPISpec:     openAPISpec,
		RateLimit: httpapi.RateLimitConfig{
			RequestsPerSecond: cfg.RateLimitRPS,
			Burst:             cfg.RateLimitBurst,
			UserHeader:        cfg.RateLimitUserHeader,
			EntryTTL:          cfg.RateLimitEntryTTL,
			CleanupInterval:   cfg.RateLimitCleanupInterval,
		},
	})

	return &Runtime{
		Handler: srv.Handler(),
		Close: func() error {
			var closeErr error
			if closer != nil {
				if err := closer.Close(); err != nil {
					closeErr = err
				}
			}
			if orderCloser != nil {
				if err := orderCloser.Close(); err != nil && closeErr == nil {
					closeErr = err
				}
			}
			return closeErr
		},
	}, nil
}

// BuildHandler wires dependencies and returns the API HTTP handler.
func BuildHandler(cfg Config) (http.Handler, error) {
	runtime, err := BuildRuntime(cfg)
	if err != nil {
		return nil, err
	}
	return runtime.Handler, nil
}

func buildCouponValidator(cfg Config) (coupon.Validator, io.Closer, error) {
	if cfg.CouponIndex != "" {
		validator, err := coupon.NewReloadingIndexedValidator(cfg.CouponIndex, coupon.ReloaderOptions{
			Interval: cfg.CouponIndexReloadInterval,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("load coupon index: %w", err)
		}
		return validator, validator, nil
	}

	couponFiles, err := resolveCouponFiles(cfg.CouponFiles)
	if err != nil {
		return nil, nil, err
	}
	return coupon.NewFileValidator(couponFiles), noopCloser{}, nil
}

func resolveCouponFiles(paths []string) ([]string, error) {
	if len(paths) == 0 {
		out := make([]string, 0, len(defaultCouponFiles))
		for _, path := range defaultCouponFiles {
			if fileExists(path) {
				out = append(out, path)
			}
		}
		return out, nil
	}

	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !fileExists(path) {
			return nil, fmt.Errorf("coupon file not found: %s", path)
		}
		out = append(out, path)
	}

	if len(out) == 0 {
		return nil, errors.New("no usable coupon files configured")
	}
	return out, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func parseCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDurationOrDefault(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}

	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return fallback
	}

	return duration
}

func parseIntOrDefault(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}

	return value
}

func parseFloatOrDefault(raw string, fallback float64) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		return fallback
	}

	return value
}

func parseBoolOrDefault(raw string, fallback bool) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}

	return value
}

type noopCloser struct{}

func (noopCloser) Close() error {
	return nil
}

var defaultCouponFiles = []string{
	"couponbase1.gz",
	"couponbase2.gz",
	"couponbase3.gz",
	filepath.Join("testdata", "coupons", "couponbase1.gz"),
	filepath.Join("testdata", "coupons", "couponbase2.gz"),
	filepath.Join("testdata", "coupons", "couponbase3.gz"),
	filepath.Join("testdata", "coupons", "couponbase1.txt"),
	filepath.Join("testdata", "coupons", "couponbase2.txt"),
	filepath.Join("testdata", "coupons", "couponbase3.txt"),
}
