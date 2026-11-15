package dto

import "cpa-usage-keeper/internal/pricing"

type AddMissingPricingModelsRequest struct {
	ExpectedRevision *int64                       `json:"expected_revision"`
	Models           []pricing.ModelPricingConfig `json:"models"`
}

type AddMissingPricingModelsResponse struct {
	Models         []string `json:"models"`
	ConfigRevision int64    `json:"config_revision"`
}
