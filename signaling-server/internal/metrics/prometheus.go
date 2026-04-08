package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PrometheusClient queries Prometheus HTTP API for metrics
type PrometheusClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewPrometheusClient creates a new Prometheus client
func NewPrometheusClient(baseURL string) *PrometheusClient {
	return &PrometheusClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// prometheusResponse represents Prometheus API response
type prometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// QueryAccountBytes queries Prometheus for the total TURN traffic bytes for an account.
// Uses turn_traffic_rcvb which measures bytes received by TURN from the client (agent uploads).
func (c *PrometheusClient) QueryAccountBytes(ctx context.Context, accountID string) (int64, error) {
	query := fmt.Sprintf(`sum(turn_traffic_rcvb{user=~"[0-9]+:%s"})`, accountID)

	u, err := url.Parse(c.baseURL + "/api/v1/query")
	if err != nil {
		return 0, fmt.Errorf("failed to parse URL: %w", err)
	}

	q := u.Query()
	q.Set("query", query)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus returned status %d", resp.StatusCode)
	}

	var promResp prometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return 0, fmt.Errorf("failed to decode response: %w", err)
	}

	if promResp.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed: %s", promResp.Status)
	}

	if len(promResp.Data.Result) == 0 {
		return 0, nil
	}

	// Extract value from the first result
	if len(promResp.Data.Result[0].Value) < 2 {
		return 0, fmt.Errorf("invalid value format in prometheus response")
	}

	// The value is the second element in the value array (first is timestamp)
	valueStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected value type in prometheus response")
	}

	bytes, err := strconv.ParseInt(valueStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse bytes value: %w", err)
	}

	return bytes, nil
}
