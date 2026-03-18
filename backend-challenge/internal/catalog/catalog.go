package catalog

import (
	"encoding/json"
	"fmt"
	"os"
)

// Image contains product image URLs for multiple screen sizes.
type Image struct {
	Thumbnail string `json:"thumbnail,omitempty"`
	Mobile    string `json:"mobile,omitempty"`
	Tablet    string `json:"tablet,omitempty"`
	Desktop   string `json:"desktop,omitempty"`
}

// Product describes an item available for ordering.
type Product struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Price    float64 `json:"price"`
	Category string  `json:"category"`
	Image    *Image  `json:"image,omitempty"`
}

// Catalog provides in-memory product lookups.
type Catalog struct {
	products []Product
	byID     map[string]Product
}

// LoadFromJSON builds a catalog from a JSON array file.
func LoadFromJSON(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read products file: %w", err)
	}

	var products []Product
	if err := json.Unmarshal(raw, &products); err != nil {
		return nil, fmt.Errorf("decode products file: %w", err)
	}

	byID := make(map[string]Product, len(products))
	for _, p := range products {
		byID[p.ID] = p
	}

	return &Catalog{
		products: products,
		byID:     byID,
	}, nil
}

// List returns all products in catalog order.
func (c *Catalog) List() []Product {
	out := make([]Product, len(c.products))
	copy(out, c.products)
	return out
}

// GetByID returns a product for a given id.
func (c *Catalog) GetByID(id string) (Product, bool) {
	p, ok := c.byID[id]
	return p, ok
}
