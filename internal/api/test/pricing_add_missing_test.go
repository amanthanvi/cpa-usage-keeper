package test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	keeperapi "cpa-usage-keeper/internal/api"
	"cpa-usage-keeper/internal/pricing"
	"cpa-usage-keeper/internal/service"
	servicedto "cpa-usage-keeper/internal/service/dto"
)

func TestPricingAddMissingHTTPAtomicContract(t *testing.T) {
	db := openAPITestDatabase(t)
	provider := newAPIPricingProvider(t, db, pricing.NewCatalog(pricing.EmptySnapshot()))
	router := keeperapi.NewRouter(nil, nil, nil, provider, keeperapi.AuthConfig{}, nil, "")
	base := `{"expected_revision":0,"models":[` + completePricingHTTPBody + `]}`
	for _, tc := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"models":[` + completePricingHTTPBody + `]}`, 400, "invalid_pricing"},
		{`{"expected_revision":null,"models":[]}`, 400, "invalid_pricing"},
		{`{"expected_revision":0,"models":[]}`, 400, "invalid_pricing"},
		{strings.Replace(base, `"expected_revision":0`, `"expected_revision":-1`, 1), 400, "invalid_pricing"},
		{strings.Replace(base, `"expected_revision":0`, `"expected_revision":0,"unknown":true`, 1), 400, "invalid_request"},
		{base + `{}`, 400, "invalid_request"},
		{`{"expected_revision":0,"models":[` + completePricingHTTPBody + `,` + completePricingHTTPBody + `]}`, 400, "invalid_pricing"},
		{base, 200, ""},
		{base, 409, "revision_conflict"},
		{strings.Replace(base, `"expected_revision":0`, `"expected_revision":1`, 1), 409, "model_exists"},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, newPricingRequest(http.MethodPost, "/api/v1/pricing/models/add-missing", tc.body))
		if response.Code != tc.status {
			t.Fatalf("status %d want %d: %s", response.Code, tc.status, response.Body.String())
		}
		if tc.code != "" {
			var failure servicedto.PricingErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Code != tc.code {
				t.Fatalf("wrong error: %s", response.Body.String())
			}
		} else {
			var success servicedto.AddMissingPricingModelsResponse
			if json.Unmarshal(response.Body.Bytes(), &success) != nil || success.ConfigRevision != 1 || len(success.Models) != 1 || success.Models[0] != "openai/gpt-5.6" {
				t.Fatalf("wrong success: %s", response.Body.String())
			}
		}
	}
	listed, err := provider.ListPricingModels(context.Background())
	if err != nil || listed.ConfigRevision != 1 || len(listed.Models) != 1 || listed.Models[0].ModelMultiplier != 0 || listed.Models[0].BasePrices.Input != 0 || listed.Models[0].ConditionalMultipliers[0].Multiplier != 0 {
		t.Fatalf("contract did not preserve zero: %+v err=%v", listed, err)
	}
}

type addMissingPricingStub struct {
	pricingStub
	addErr error
}

func (s *addMissingPricingStub) AddMissingPricingModels(context.Context, servicedto.AddMissingPricingModelsRequest) (servicedto.AddMissingPricingModelsResponse, error) {
	return servicedto.AddMissingPricingModelsResponse{}, s.addErr
}

func TestPricingAddMissingHTTPSafeErrorsAndOptionalProvider(t *testing.T) {
	for _, tc := range []struct {
		provider service.PricingProvider
		status   int
		code     string
	}{
		{&pricingStub{}, 503, "add_missing_unavailable"},
		{&addMissingPricingStub{addErr: service.ErrPricingBusy}, 409, "pricing_busy"},
		{&addMissingPricingStub{addErr: errors.New("SELECT private FROM secret_table")}, 500, "internal_error"},
	} {
		router := keeperapi.NewRouter(nil, nil, nil, tc.provider, keeperapi.AuthConfig{}, nil, "")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, newPricingRequest(http.MethodPost, "/api/v1/pricing/models/add-missing", `{"expected_revision":0,"models":[`+completePricingHTTPBody+`]}`))
		var failure servicedto.PricingErrorResponse
		if response.Code != tc.status || json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Code != tc.code || strings.Contains(response.Body.String(), "SELECT") || strings.Contains(response.Body.String(), "secret_table") {
			t.Fatalf("unsafe/wrong response: %d %s", response.Code, response.Body.String())
		}
	}
}
