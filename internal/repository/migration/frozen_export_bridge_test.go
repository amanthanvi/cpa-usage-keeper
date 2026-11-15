package migration

import (
	"os"
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The explicit path produces a retained synthetic cross-language fixture only.
func TestFrozenExportBridgeFixture(t *testing.T) {
	path := os.Getenv("KEEPER_FROZEN_EXPORT_FIXTURE")
	if path == "" {
		t.Skip("retained fixture path is not requested")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("fixture path must be absolute")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal("exclusive synthetic fixture creation failed")
	}
	if err := file.Close(); err != nil {
		t.Fatal("synthetic fixture close failed")
	}
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("synthetic fixture open failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal("synthetic fixture handle failed")
	}
	defer sqlDB.Close()
	for _, table := range legacyOverviewTables {
		if err := db.Exec("CREATE TABLE " + legacyAccountingIdentifier(table) + " (id, quantity, raw, cost_usd)").Error; err != nil {
			t.Fatal("synthetic aggregate schema failed")
		}
		if err := db.Exec("INSERT INTO "+legacyAccountingIdentifier(table)+" VALUES(?, ?, ?, ?)", int64(9007199254740993), "=synthetic-only\x00\u00e9", []byte{0, 255}, nil).Error; err != nil {
			t.Fatal("synthetic aggregate insert failed")
		}
	}
	if err := db.Exec("CREATE TABLE pricing_migration_state (id INTEGER PRIMARY KEY, baseline_json TEXT)").Error; err != nil {
		t.Fatal("synthetic baseline schema failed")
	}
	if err := db.Exec("INSERT INTO pricing_migration_state VALUES(1, ?)", `{"origin":"synthetic-Go-archive-bridge","cost_policy":"not-recomputed"}`).Error; err != nil {
		t.Fatal("synthetic baseline insert failed")
	}
	if err := db.Exec("CREATE TABLE local_ranking_period_stats (position, quantity)").Error; err != nil {
		t.Fatal("synthetic ranking schema failed")
	}
	if err := db.Exec("INSERT INTO local_ranking_period_stats VALUES(1, 9007199254740993)").Error; err != nil {
		t.Fatal("synthetic ranking insert failed")
	}
	if err := db.Transaction(PreserveLegacyOverviewArchives); err != nil {
		t.Fatal("synthetic archive preservation failed")
	}
}
