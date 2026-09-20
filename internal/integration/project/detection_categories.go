package project

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

// DetectionCategoryDirectory reads the project-management-owned controlled
// vocabulary with the same client-credentials trust boundary as activation delivery.
type DetectionCategoryDirectory struct {
	BaseURL     string
	Client      *http.Client
	TokenSource func(context.Context) (string, error)
}

func (d *DetectionCategoryDirectory) List(ctx context.Context) ([]application.DetectionCategory, error) {
	if strings.TrimSpace(d.BaseURL) == "" || d.TokenSource == nil {
		return nil, fmt.Errorf("project detection category integration is not configured")
	}
	token, err := d.TokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch project integration token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.BaseURL, "/")+"/internal/v1/contracts/detection-categories", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("project API returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Data struct {
			Items []application.DetectionCategory `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode project detection categories: %w", err)
	}
	return envelope.Data.Items, nil
}
