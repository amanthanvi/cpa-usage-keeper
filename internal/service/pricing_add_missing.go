package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/pricing"
	"cpa-usage-keeper/internal/repository"
	repodto "cpa-usage-keeper/internal/repository/dto"
	servicedto "cpa-usage-keeper/internal/service/dto"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var ErrPricingRevisionConflict = errors.New("pricing revision conflict")
var ErrPricingModelExists = errors.New("pricing model already exists")

// PricingAddMissingProvider is optional so existing providers keep their contract.
type PricingAddMissingProvider interface {
	AddMissingPricingModels(context.Context, servicedto.AddMissingPricingModelsRequest) (servicedto.AddMissingPricingModelsResponse, error)
}

// AddMissingPricingModels commits one insert-only batch. It never recalculates stored fees.
func (s *pricingService) AddMissingPricingModels(ctx context.Context, input servicedto.AddMissingPricingModelsRequest) (servicedto.AddMissingPricingModelsResponse, error) {
	if input.ExpectedRevision == nil || *input.ExpectedRevision < 0 {
		return servicedto.AddMissingPricingModelsResponse{}, invalidAddMissingField("expected_revision", "required")
	}
	if len(input.Models) == 0 {
		return servicedto.AddMissingPricingModelsResponse{}, invalidAddMissingField("models", "required")
	}
	seen := make(map[string]bool, len(input.Models))
	for index, config := range input.Models {
		model := strings.TrimSpace(config.Model)
		if seen[model] {
			return servicedto.AddMissingPricingModelsResponse{}, invalidAddMissingField(fmt.Sprintf("models[%d].model", index), "duplicate")
		}
		seen[model] = true
		if err := validatePricingDefaultBranches(config); err != nil {
			return servicedto.AddMissingPricingModelsResponse{}, err
		}
	}
	validated, err := pricing.CompilePricingSnapshot(input.Models, time.Local)
	if err != nil {
		return servicedto.AddMissingPricingModelsResponse{}, fmt.Errorf("%w: %w", ErrInvalidPricingInput, err)
	}
	configs := validated.PricingModelConfigs()
	models := make([]string, 0, len(configs))
	for _, config := range configs {
		models = append(models, config.Model)
	}
	_, revision, err := s.mutatePricingRevision(ctx, func(tx *gorm.DB) error {
		// Suppress SQL diagnostics, including fixture failures, for this narrow command.
		tx = tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
		current, err := repository.LoadPricingConfigRevision(tx)
		if err != nil {
			return err
		}
		if current != *input.ExpectedRevision {
			return ErrPricingRevisionConflict
		}
		var count int64
		if err := tx.Model(&entities.ModelPriceSetting{}).Where("model IN ?", models).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrPricingModelExists
		}
		for _, config := range configs {
			branches, err := json.Marshal(config.Branches)
			if err != nil {
				return err
			}
			multiplier := config.ModelMultiplier
			row := entities.ModelPriceSetting{
				Model: config.Model, PricingStyle: config.PricingStyle,
				PromptPricePer1M: config.BasePrices.Input, CompletionPricePer1M: config.BasePrices.Output,
				CacheReadPricePer1M: config.BasePrices.CacheRead, CacheWritePricePer1M: config.BasePrices.CacheWrite,
				PriceMultiplier: &multiplier, BranchesJSON: string(branches),
			}
			// Create, never Save/upsert: even another writer cannot overwrite an existing row.
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			rules := make([]repodto.ModelPriceRuleInput, len(config.ConditionalMultipliers))
			for index, rule := range config.ConditionalMultipliers {
				rules[index] = repodto.ModelPriceRuleInput{Key: rule.Key, Value: rule.Value, Multiplier: rule.Multiplier}
			}
			if _, err := repository.ReplaceModelPriceRules(tx, config.Model, rules); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, repository.ErrInvalidPricingSnapshot) {
			err = fmt.Errorf("%w: %w", ErrInvalidPricingInput, err)
		}
		return servicedto.AddMissingPricingModelsResponse{}, err
	}
	return servicedto.AddMissingPricingModelsResponse{Models: models, ConfigRevision: revision}, nil
}

func invalidAddMissingField(path, code string) error {
	return fmt.Errorf("%w: %w", ErrInvalidPricingInput, &pricing.ValidationError{Path: path, Code: code, Reason: "invalid add-missing request"})
}

func validatePricingDefaultBranches(input pricing.ModelPricingConfig) error {
	for index, branch := range input.Branches {
		if (branch.Days == "" || branch.Days == pricing.DaysAll) && branch.Context.Type == pricing.ContextAll && branch.Period.Type == pricing.PeriodAll {
			return fmt.Errorf("%w: %w", ErrInvalidPricingInput, &pricing.ValidationError{
				Path: fmt.Sprintf("branches[%d].context", index), Code: "default_conflict", Reason: "an unconditional branch replaces the default price",
			})
		}
	}
	return nil
}
