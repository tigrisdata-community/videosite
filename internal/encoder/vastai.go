// Package encoder owns the server-side bits of the encoding pipeline:
// the Vast.ai REST client, the Tigris IAM helper, the orchestrator
// goroutines, and the webhook handler.
package encoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// ErrNoOffers means the search returned no usable offers.
var ErrNoOffers = errors.New("encoder: no vast.ai offers matched")

// VastClient is a thin wrapper around the Vast.ai REST API. We don't use the
// vast-python or vastaicli helpers because they shell out; this package is
// self-contained and only needs http + json.
type VastClient struct {
	apiKey string
	base   string
	http   *http.Client
}

func NewVastClient(apiKey, base string, hc *http.Client) *VastClient {
	if base == "" {
		base = "https://console.vast.ai"
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &VastClient{apiKey: apiKey, base: base, http: hc}
}

// Offer is the subset of fields we actually consume from a search result.
type Offer struct {
	AskContractID int     `json:"ask_contract_id"`
	GpuName       string  `json:"gpu_name"`
	NumGpus       int     `json:"num_gpus"`
	GpuRAM        float64 `json:"gpu_ram"`
	DphTotal      float64 `json:"dph_total"`
	Reliability2  float64 `json:"reliability2"`
}

// LaunchConfig is the body shape for PUT /asks/{id}/. ClientID and Runtype
// are fixed to the values vast-python uses and aren't part of the caller's
// vocabulary — the wire form is hand-built in Mint instead.
type LaunchConfig struct {
	Image   string            `json:"image"`
	Env     map[string]string `json:"env"`
	Disk    int               `json:"disk"`
	Onstart string            `json:"onstart"`
	Label   string            `json:"label"`
}

// Instance is the subset of fields we read when polling.
type Instance struct {
	ID             int    `json:"id"`
	ActualStatus   string `json:"actual_status"`
	IntendedStatus string `json:"intended_status"`
	StatusMsg      string `json:"status_msg"`
}

func (c *VastClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encoder/vastai: marshal: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("encoder/vastai: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("encoder/vastai: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("encoder/vastai: read body: %w", err)
	}
	return out, resp.StatusCode, nil
}

// SearchOffers calls POST /api/v0/bundles/. The query is the operator-based
// filter object documented in vast-python (e.g. {"gpu_name": {"in": [...]}}).
func (c *VastClient) SearchOffers(ctx context.Context, query map[string]any) ([]Offer, error) {
	offers, _, err := c.SearchOffersRaw(ctx, query)
	return offers, err
}

// SearchOffersRaw is SearchOffers but also returns the raw response body, so
// the vast-search CLI can show the full server response when diagnosing
// empty results.
func (c *VastClient) SearchOffersRaw(ctx context.Context, query map[string]any) ([]Offer, []byte, error) {
	body, status, err := c.do(ctx, http.MethodPost, "/api/v0/bundles/", query)
	if err != nil {
		return nil, nil, err
	}
	if status >= 400 {
		return nil, body, fmt.Errorf("encoder/vastai: search offers: %d: %s", status, body)
	}
	var resp struct {
		Offers []Offer `json:"offers"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, body, fmt.Errorf("encoder/vastai: search offers: parse: %w", err)
	}
	return resp.Offers, body, nil
}

// Mint accepts an offer and creates an instance. Returns the new instance ID.
func (c *VastClient) Mint(ctx context.Context, askContractID int, cfg LaunchConfig) (int, error) {
	wire := map[string]any{
		"client_id": "me",
		"runtype":   "args",
		"image":     cfg.Image,
		"env":       cfg.Env,
		"disk":      cfg.Disk,
		"onstart":   cfg.Onstart,
		"label":     cfg.Label,
	}
	body, status, err := c.do(ctx, http.MethodPut,
		fmt.Sprintf("/api/v0/asks/%d/", askContractID), wire)
	if err != nil {
		return 0, err
	}
	if status >= 400 {
		return 0, fmt.Errorf("encoder/vastai: mint: %d: %s", status, body)
	}
	var resp struct {
		Success     bool   `json:"success"`
		NewContract int    `json:"new_contract"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("encoder/vastai: mint: parse: %w", err)
	}
	if !resp.Success || resp.NewContract == 0 {
		return 0, fmt.Errorf("encoder/vastai: mint: %s", resp.Error)
	}
	return resp.NewContract, nil
}

// GetInstance fetches an instance by id. Vast wraps the result in
// {"instances": {...}} on this endpoint.
func (c *VastClient) GetInstance(ctx context.Context, id int) (*Instance, error) {
	body, status, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/api/v0/instances/%d/", id), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrInstanceGone
	}
	if status >= 400 {
		return nil, fmt.Errorf("encoder/vastai: get instance %d: %d: %s", id, status, body)
	}
	var resp struct {
		Instances Instance `json:"instances"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("encoder/vastai: get instance %d: parse: %w", id, err)
	}
	return &resp.Instances, nil
}

// ErrInstanceGone is returned when GetInstance / Destroy hit a 404. Callers
// treat this as success for cleanup paths.
var ErrInstanceGone = errors.New("encoder: vast.ai instance gone")

// Destroy slays an instance. 404 is treated as success.
func (c *VastClient) Destroy(ctx context.Context, id int) error {
	body, status, err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/api/v0/instances/%d/", id), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status >= 400 {
		return fmt.Errorf("encoder/vastai: destroy %d: %d: %s", id, status, body)
	}
	return nil
}

// PreferredOfferQuery returns the search filters we use to find boxes.
// Verified-only: unverified hosts are cheaper but flaky enough that the
// retry-on-failure tax wasn't worth the savings in practice. An empty
// countries slice drops the geolocation filter entirely.
func PreferredOfferQuery(gpuNames, countries []string, minReliability float64) map[string]any {
	q := map[string]any{
		"verified":      map[string]any{"eq": true},
		"rentable":      map[string]any{"eq": true},
		"reliability":   map[string]any{"gte": minReliability},
		"num_gpus":      map[string]any{"eq": 1},
		"gpu_name":      map[string]any{"in": gpuNames},
		"cuda_max_good": map[string]any{"gte": 12.0},
		"inet_down":     map[string]any{"gte": 1000},
		"order":         [][]string{{"dph_total", "asc"}},
		"limit":         20,
		"type":          "ondemand",
	}
	if len(countries) > 0 {
		q["geolocation"] = map[string]any{"in": countries}
	}
	return q
}

// PickOffer sorts offers so a name earlier in `prefs` always beats a later
// one (regardless of price), and within a name the cheapest wins.
func PickOffer(offers []Offer, prefs []string) (Offer, error) {
	if len(offers) == 0 {
		return Offer{}, ErrNoOffers
	}
	rank := make(map[string]int, len(prefs))
	for i, n := range prefs {
		rank[n] = i
	}
	sorted := make([]Offer, len(offers))
	copy(sorted, offers)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, oki := rank[sorted[i].GpuName]
		rj, okj := rank[sorted[j].GpuName]
		if oki && okj && ri != rj {
			return ri < rj
		}
		if oki != okj {
			return oki
		}
		return sorted[i].DphTotal < sorted[j].DphTotal
	})
	return sorted[0], nil
}
