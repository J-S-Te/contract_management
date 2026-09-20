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

const (
	detectionCategoryMaxAttempts = 3
	detectionCategoryRetryDelay  = 200 * time.Millisecond
)

func (d *DetectionCategoryDirectory) List(ctx context.Context) ([]application.DetectionCategory, error) {
	if strings.TrimSpace(d.BaseURL) == "" || d.TokenSource == nil {
		return nil, fmt.Errorf("project detection category integration is not configured")
	}
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	var lastErr error
	for attempt := 1; attempt <= detectionCategoryMaxAttempts; attempt++ {
		items, retryable, err := d.listOnce(ctx, client)
		if err == nil {
			return items, nil
		}
		lastErr = err
		if !retryable || attempt == detectionCategoryMaxAttempts {
			break
		}
		delay := time.Duration(attempt) * detectionCategoryRetryDelay
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func (d *DetectionCategoryDirectory) listOnce(ctx context.Context, client *http.Client) ([]application.DetectionCategory, bool, error) {
	token, err := d.TokenSource(ctx)
	if err != nil {
		return nil, true, fmt.Errorf("fetch project integration token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.BaseURL, "/")+"/internal/v1/contracts/detection-categories", nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, true, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode == http.StatusBadGateway || response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusGatewayTimeout,
			fmt.Errorf("project API returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Data struct {
			Items []application.DetectionCategory `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("decode project detection categories: %w", err)
	}
	return envelope.Data.Items, false, nil
}
