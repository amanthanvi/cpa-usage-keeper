package test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/pricing"
	"cpa-usage-keeper/internal/service"
	servicedto "cpa-usage-keeper/internal/service/dto"
)

func addMissingInput(revision int64, configs ...pricing.ModelPricingConfig) servicedto.AddMissingPricingModelsRequest {
	return servicedto.AddMissingPricingModelsRequest{ExpectedRevision: &revision, Models: configs}
}

func TestPricingAddMissingPreservesExistingAndHistoricalCosts(t *testing.T) {
	db := openUsageServiceTestDatabase(t)
	provider, catalog := newCatalogPricingService(t, db)
	existing := completeServiceConfig("existing", 3)
	existing.ModelMultiplier = 0
	existing.ConditionalMultipliers = []pricing.RuleConfig{{Key: "service_tier", Value: "free", Multiplier: 0}}
	existing.Branches = []pricing.PriceBranch{{ID: "weekday", Name: "Weekday", Days: pricing.DaysWeekday, Context: pricing.ContextCondition{Type: pricing.ContextAll}, Period: pricing.PeriodCondition{Type: pricing.PeriodAll}, Prices: pricing.BasePrices{Input: 4}}}
	if _, err := provider.SavePricingModel(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	var before entities.ModelPriceSetting
	if err := db.Where("model = ?", "existing").Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	var rulesBefore []entities.ModelPriceRule
	if err := db.Order("id").Find(&rulesBefore).Error; err != nil {
		t.Fatal(err)
	}
	cost, available := 3.25, true
	event := entities.UsageEvent{EventKey: "add-only-history", Model: "free", Timestamp: time.Now(), CostUSD: &cost, CostAvailable: &available}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&event, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	archive := entities.UsageEventArchive{ID: 101, EventKey: "add-only-archive", Model: "free", Timestamp: time.Now(), CostUSD: &cost, CostAvailable: &available}
	unavailable := int64(2)
	hourly := entities.UsageOverviewHourlyStat{BucketStart: time.Now().Truncate(time.Hour), Model: "free", RequestCount: 7, CostUSD: &cost, UnavailableCostCount: &unavailable}
	daily := entities.UsageOverviewDailyStat{BucketStart: time.Now().Truncate(24 * time.Hour), Model: "free", RequestCount: 7, CostUSD: &cost, UnavailableCostCount: &unavailable}
	for _, fixture := range []any{&archive, &hourly, &daily} {
		if err := db.Create(fixture).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.First(fixture).Error; err != nil {
			t.Fatal(err)
		}
	}
	free := completeServiceConfig(" free ", 0)
	free.ModelMultiplier = 0
	free.ConditionalMultipliers = []pricing.RuleConfig{{Key: "service_tier", Value: "priority", Multiplier: 0}}
	adder := provider.(service.PricingAddMissingProvider)
	response, err := adder.AddMissingPricingModels(context.Background(), addMissingInput(1, free, completeServiceConfig("paid", 2)))
	if err != nil || response.ConfigRevision != 2 || !reflect.DeepEqual(response.Models, []string{"free", "paid"}) {
		t.Fatalf("batch response=%+v err=%v", response, err)
	}
	var after entities.ModelPriceSetting
	var rulesAfter []entities.ModelPriceRule
	if err := db.First(&after, before.ID).Error; err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("existing price row changed: err=%v", err)
	}
	if err := db.Where("model_price_setting_id = ?", before.ID).Order("id").Find(&rulesAfter).Error; err != nil || !reflect.DeepEqual(rulesBefore, rulesAfter) {
		t.Fatalf("existing rules changed: err=%v", err)
	}
	var stored entities.UsageEvent
	if err := db.First(&stored, event.ID).Error; err != nil || !reflect.DeepEqual(event, stored) {
		t.Fatalf("historical event changed: err=%v", err)
	}
	var storedArchive entities.UsageEventArchive
	var storedHourly entities.UsageOverviewHourlyStat
	var storedDaily entities.UsageOverviewDailyStat
	for _, pair := range []struct{ before, after any }{{archive, &storedArchive}, {hourly, &storedHourly}, {daily, &storedDaily}} {
		if err := db.First(pair.after).Error; err != nil || !reflect.DeepEqual(pair.before, reflect.ValueOf(pair.after).Elem().Interface()) {
			t.Fatalf("archive or aggregate changed: type=%T err=%v", pair.before, err)
		}
	}
	listed, err := provider.ListPricingModels(context.Background())
	if err != nil || len(listed.Models) != 3 || listed.Models[1].Model != "free" || listed.Models[1].ModelMultiplier != 0 || listed.Models[1].BasePrices.Input != 0 || listed.Models[1].ConditionalMultipliers[0].Multiplier != 0 {
		t.Fatalf("explicit zero changed: %+v err=%v", listed, err)
	}
	if fee := completeServiceFee(catalog, "free", 1_000_000); !fee.Available || fee.TotalCostUSD != 0 {
		t.Fatalf("free model not published: %+v", fee)
	}
	beforeSnapshot := catalog.Snapshot()
	for _, tc := range []struct {
		input servicedto.AddMissingPricingModelsRequest
		want  error
	}{
		{addMissingInput(1, completeServiceConfig("stale", 1)), service.ErrPricingRevisionConflict},
		{addMissingInput(2, completeServiceConfig("new", 1), completeServiceConfig("existing", 99)), service.ErrPricingModelExists},
	} {
		if _, err := adder.AddMissingPricingModels(context.Background(), tc.input); !errors.Is(err, tc.want) {
			t.Fatalf("expected %v, got %v", tc.want, err)
		}
	}
	result, err := provider.ListPricingModels(context.Background())
	if err != nil || result.ConfigRevision != 2 || len(result.Models) != 3 || catalog.Snapshot() != beforeSnapshot {
		t.Fatalf("rejected batch changed state: %+v err=%v", result, err)
	}
}

func TestPricingAddMissingRollsBackWholeBatchAndPublication(t *testing.T) {
	for _, failure := range []string{"rule", "commit"} {
		t.Run(failure, func(t *testing.T) {
			db := openUsageServiceTestDatabase(t)
			provider, catalog := newCatalogPricingService(t, db)
			before := catalog.Snapshot()
			if failure == "rule" {
				if err := db.Exec(`CREATE TRIGGER fail_add_rule BEFORE INSERT ON model_price_rules BEGIN SELECT RAISE(ABORT, 'fixture rejected'); END`).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.Exec(`CREATE TABLE add_commit_probe (setting_id INTEGER REFERENCES model_price_settings(id) DEFERRABLE INITIALLY DEFERRED)`).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Exec(`CREATE TRIGGER fail_add_commit AFTER INSERT ON model_price_settings BEGIN INSERT INTO add_commit_probe(setting_id) VALUES (9223372036854775807); END`).Error; err != nil {
					t.Fatal(err)
				}
			}
			second := completeServiceConfig("second", 2)
			second.ConditionalMultipliers = []pricing.RuleConfig{{Key: "service_tier", Value: "priority", Multiplier: 2}}
			if _, err := provider.(service.PricingAddMissingProvider).AddMissingPricingModels(context.Background(), addMissingInput(0, completeServiceConfig("first", 1), second)); err == nil {
				t.Fatal("expected fixture failure")
			}
			var count int64
			if err := db.Model(&entities.ModelPriceSetting{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("partial batch persisted: %d err=%v", count, err)
			}
			listed, err := provider.ListPricingModels(context.Background())
			if err != nil || listed.ConfigRevision != 0 || len(listed.Models) != 0 || catalog.Snapshot() != before {
				t.Fatalf("failed commit advanced state: %+v err=%v", listed, err)
			}
		})
	}
}

func TestPricingAddMissingValidatesBeforeWriting(t *testing.T) {
	db := openUsageServiceTestDatabase(t)
	provider, _ := newCatalogPricingService(t, db)
	badBranch := completeServiceConfig("branch", 1)
	badBranch.Branches = []pricing.PriceBranch{{ID: "all", Name: "All", Context: pricing.ContextCondition{Type: pricing.ContextAll}, Period: pricing.PeriodCondition{Type: pricing.PeriodAll}, Prices: pricing.BasePrices{Input: 2}}}
	invalid := completeServiceConfig("negative", -1)
	for _, input := range []servicedto.AddMissingPricingModelsRequest{
		{}, addMissingInput(-1, completeServiceConfig("negative-revision", 1)), addMissingInput(0),
		addMissingInput(0, completeServiceConfig("same", 1), completeServiceConfig(" same ", 2)),
		addMissingInput(0, completeServiceConfig("valid", 1), invalid), addMissingInput(0, badBranch),
	} {
		if _, err := provider.(service.PricingAddMissingProvider).AddMissingPricingModels(context.Background(), input); !errors.Is(err, service.ErrInvalidPricingInput) {
			t.Fatalf("expected validation error: %v", err)
		}
	}
	listed, err := provider.ListPricingModels(context.Background())
	if err != nil || listed.ConfigRevision != 0 || len(listed.Models) != 0 {
		t.Fatalf("validation wrote state: %+v err=%v", listed, err)
	}
}

func TestPricingAddMissingSerializesRevisionWriters(t *testing.T) {
	provider, _ := newCatalogPricingService(t, openUsageServiceTestDatabase(t))
	adder := provider.(service.PricingAddMissingProvider)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, model := range []string{"a", "b"} {
		wg.Go(func() {
			_, err := adder.AddMissingPricingModels(context.Background(), addMissingInput(0, completeServiceConfig(model, 1)))
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, service.ErrPricingRevisionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	listed, err := provider.ListPricingModels(context.Background())
	if success != 1 || conflict != 1 || err != nil || listed.ConfigRevision != 1 || len(listed.Models) != 1 {
		t.Fatalf("writers not serialized: success=%d conflict=%d result=%+v err=%v", success, conflict, listed, err)
	}
}

func TestPricingAddMissingRefusesDuringRecalculation(t *testing.T) {
	withUsageServiceLocation(t, "UTC")
	db, _ := openResolverServiceTestPools(t)
	now := time.Date(2026, 9, 23, 12, 30, 0, 0, time.UTC)
	cost, available := 1.0, true
	if err := db.Create(&entities.UsageEvent{EventKey: "busy-add-history", Model: "model-a", Timestamp: now.Add(-time.Hour), CostUSD: &cost, CostAvailable: &available}).Error; err != nil {
		t.Fatal(err)
	}
	provider, reads, _, _, stop := newPricingRecalculationTestProvider(t, db, now)
	defer stop()
	release, ok := reads.Acquire()
	if !ok {
		t.Fatal("could not hold read")
	}
	defer release()
	started, err := provider.StartPricingRecalculation(context.Background(), servicedto.StartRecalculationRequest{StartAt: now.Add(-90 * time.Minute), ConfigRevision: 0})
	if err != nil || !started.Started {
		t.Fatalf("start fixture: %+v err=%v", started, err)
	}
	if _, err := provider.(service.PricingAddMissingProvider).AddMissingPricingModels(context.Background(), addMissingInput(0, completeServiceConfig("new", 1))); !errors.Is(err, service.ErrPricingBusy) {
		t.Fatalf("expected busy, got %v", err)
	}
	release()
	provider.WaitPricingRecalculation()
	listed, err := provider.ListPricingModels(context.Background())
	if err != nil || listed.ConfigRevision != 0 || len(listed.Models) != 0 {
		t.Fatalf("busy mutation wrote state: %+v err=%v", listed, err)
	}
}
