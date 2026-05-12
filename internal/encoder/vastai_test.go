package encoder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPickOffer(t *testing.T) {
	tests := []struct {
		name    string
		offers  []Offer
		prefs   []string
		wantID  int
		wantErr error
	}{
		{
			name:    "no offers returns ErrNoOffers",
			offers:  nil,
			prefs:   []string{"RTX_3090"},
			wantErr: ErrNoOffers,
		},
		{
			name: "3090 wins over cheaper 4090",
			offers: []Offer{
				{AskContractID: 1, GpuName: "RTX_4090", DphTotal: 0.20},
				{AskContractID: 2, GpuName: "RTX_3090", DphTotal: 0.40},
			},
			prefs:  []string{"RTX_3090", "RTX_4090"},
			wantID: 2,
		},
		{
			name: "cheapest 3090 within preferred name",
			offers: []Offer{
				{AskContractID: 1, GpuName: "RTX_3090", DphTotal: 0.30},
				{AskContractID: 2, GpuName: "RTX_3090", DphTotal: 0.20},
				{AskContractID: 3, GpuName: "RTX_4090", DphTotal: 0.10},
			},
			prefs:  []string{"RTX_3090", "RTX_4090"},
			wantID: 2,
		},
		{
			name: "fallback to 4090 when no 3090 present",
			offers: []Offer{
				{AskContractID: 1, GpuName: "RTX_4090", DphTotal: 0.50},
				{AskContractID: 2, GpuName: "RTX_4090", DphTotal: 0.30},
			},
			prefs:  []string{"RTX_3090", "RTX_4090"},
			wantID: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PickOffer(tt.offers, tt.prefs)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.AskContractID != tt.wantID {
				t.Errorf("ask id = %d, want %d", got.AskContractID, tt.wantID)
			}
		})
	}
}

func TestPreferredOfferQuery_VerifiedOnly(t *testing.T) {
	q := PreferredOfferQuery([]string{"RTX 3090"}, []string{"US", "CA"}, 0.95)
	got, ok := q["verified"].(map[string]any)
	if !ok {
		t.Fatalf("query missing verified filter; got: %v", q["verified"])
	}
	if got["eq"] != true {
		t.Errorf("verified filter = %v, want {eq: true}", got)
	}
	if _, ok := q["gpu_name"]; !ok {
		t.Errorf("query missing gpu_name filter")
	}
	geo, ok := q["geolocation"].(map[string]any)
	if !ok {
		t.Fatalf("query missing geolocation filter; got: %v", q["geolocation"])
	}
	if _, ok := geo["in"].([]string); !ok {
		t.Errorf("geolocation filter = %v, want {in: []string}", geo)
	}
}

func TestPreferredOfferQuery_EmptyCountriesDropsGeoFilter(t *testing.T) {
	q := PreferredOfferQuery([]string{"RTX 3090"}, nil, 0.95)
	if _, ok := q["geolocation"]; ok {
		t.Errorf("expected geolocation filter to be absent when countries is empty")
	}
}

func TestVastClient_Mint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v0/asks/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q, want Bearer test-key", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body["client_id"] != "me" || body["runtype"] != "args" {
			t.Errorf("expected hard-coded client_id/runtype: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "new_contract": 12345,
		})
	}))
	defer srv.Close()

	c := NewVastClient("test-key", srv.URL, srv.Client())
	id, err := c.Mint(context.Background(), 999, LaunchConfig{
		Image: "img", Disk: 32, Onstart: "/bin/run",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if id != 12345 {
		t.Errorf("id = %d, want 12345", id)
	}
}

func TestVastClient_DestroyTreats404AsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"detail":"not found"}`)
	}))
	defer srv.Close()
	c := NewVastClient("k", srv.URL, srv.Client())
	if err := c.Destroy(context.Background(), 1); err != nil {
		t.Errorf("err = %v, want nil for 404", err)
	}
}
