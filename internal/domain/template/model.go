package template

import "time"

const DefaultNumberFormat = "HT-{YYYYMMDD}-{ID8}"

type Field struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Default string `json:"default,omitempty"`
	Locked  bool   `json:"locked,omitempty"`
}

type Template struct {
	ID               string    `json:"id"`
	TenantID         string    `json:"tenant_id"`
	Name             string    `json:"name"`
	OriginalFilename string    `json:"original_filename"`
	NumberFormat     string    `json:"number_format"`
	Fields           []Field   `json:"fields"`
	Content          []byte    `json:"-"`
	CreatedAt        time.Time `json:"created_at"`
	CreatedBy        string    `json:"created_by"`
}
