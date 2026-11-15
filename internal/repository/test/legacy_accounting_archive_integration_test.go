package test

import (
	"context"
	"testing"
	"time"

	"cpa-usage-keeper/internal/repository"
	"cpa-usage-keeper/internal/repository/migration"
)

func TestLegacyOverviewRebuildRetainsAggregateOnlyHistoryAndResumeArchive(t *testing.T) {
	fixture := openPublishedPricingOverviewFixture(t)
	ctx := context.Background()
	baseline, err := repository.MigrateLegacyPricingEvents(ctx, fixture.writer, fixture.reader, fixture.backupDir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"usage_overview_hourly_stats", "usage_overview_daily_stats"} {
		if err := fixture.writer.Table(table).Where("1 = 1").Update("request_count", 99).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.writer.Exec(`CREATE TRIGGER fail_legacy_archive_resume BEFORE UPDATE OF cursors_json ON pricing_migration_state
		WHEN OLD.phase = 'overview_rebuilding' AND NEW.cursors_json <> OLD.cursors_json
		BEGIN SELECT RAISE(ABORT, 'synthetic page failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteLegacyPricingData(ctx, fixture.writer, fixture.reader, baseline); err == nil {
		t.Fatal("expected synthetic page failure")
	}
	if err := migration.VerifyLegacyOverviewArchives(fixture.writer); err != nil {
		t.Fatal(err)
	}
	if err := fixture.writer.Exec("DROP TRIGGER fail_legacy_archive_resume").Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteLegacyPricingData(ctx, fixture.writer, fixture.reader, baseline); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"usage_overview_hourly_stats", "usage_overview_daily_stats"} {
		var archived int64
		if err := fixture.writer.Table("keeper_legacy_" + table).Select("SUM(request_count)").Scan(&archived).Error; err != nil || archived != 99 {
			t.Fatalf("aggregate-only legacy contribution lost on resume: %s %d %v", table, archived, err)
		}
	}
	if err := migration.VerifyLegacyOverviewArchives(fixture.writer); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyOverviewFailedClearRollsBackArchiveAndPhase(t *testing.T) {
	fixture := openPublishedPricingOverviewFixture(t)
	ctx := context.Background()
	baseline, err := repository.MigrateLegacyPricingEvents(ctx, fixture.writer, fixture.reader, fixture.backupDir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.writer.Exec(`CREATE TRIGGER fail_legacy_archive_clear BEFORE DELETE ON usage_overview_daily_stats
		BEGIN SELECT RAISE(ABORT, 'synthetic clear failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteLegacyPricingData(ctx, fixture.writer, fixture.reader, baseline); err == nil {
		t.Fatal("expected synthetic clear failure")
	}
	if fixture.writer.Migrator().HasTable("keeper_legacy_accounting_archives") || fixture.writer.Migrator().HasTable("keeper_legacy_usage_overview_hourly_stats") {
		t.Fatal("failed clear committed a partial archive")
	}
	if err := fixture.writer.Exec("DROP TRIGGER fail_legacy_archive_clear").Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteLegacyPricingData(ctx, fixture.writer, fixture.reader, baseline); err != nil {
		t.Fatal(err)
	}
	if err := migration.VerifyLegacyOverviewArchives(fixture.writer); err != nil {
		t.Fatal(err)
	}
}
