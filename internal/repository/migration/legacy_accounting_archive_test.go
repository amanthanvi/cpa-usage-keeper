package migration

import (
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func legacyArchiveTestDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "archive.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, table := range legacyOverviewTables {
		if err := db.Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY, requests, tokens, model COLLATE NOCASE, payload, optional)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE INDEX " + table + "_model ON " + table + " (model)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO "+table+" (id,requests,tokens,model,payload,optional) VALUES (?,?,?,?,?,?)",
			int64(9007199254740993), int64(7), "00123", "legacy\x00MODEL", []byte{0, 255, 1}, nil).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO " + table + " (id,requests,tokens,model,payload,optional) VALUES (5,1.5,-2,'A',X'', '')").Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestLegacyOverviewArchivePreservesTypedRowsAndSchema(t *testing.T) {
	db := legacyArchiveTestDatabase(t)
	for attempt := 0; attempt < 2; attempt++ {
		if err := db.Transaction(PreserveLegacyOverviewArchives); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range legacyOverviewTables {
		var record legacyAccountingArchive
		if err := db.Table(legacyAccountingArchiveManifest).Where("source_table = ?", source).Take(&record).Error; err != nil {
			t.Fatal(err)
		}
		if record.RowCount != 2 || record.RowsSHA256 == "" || record.SchemaJSON == "" {
			t.Fatalf("missing archive evidence: %+v", record)
		}
		if err := db.Exec("DELETE FROM " + source).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyLegacyOverviewArchives(db); err != nil {
		t.Fatalf("retained aggregate-only rows failed verification after live clear: %v", err)
	}
	var row struct {
		ID          int64
		Tokens      string
		TokenType   string
		PayloadType string
		NullType    string
		ModelBytes  int
	}
	if err := db.Raw(`SELECT id, tokens, typeof(tokens) AS token_type, typeof(payload) AS payload_type,
		typeof(optional) AS null_type, length(CAST(model AS BLOB)) AS model_bytes
		FROM keeper_legacy_usage_overview_hourly_stats WHERE id = ?`, int64(9007199254740993)).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.ID != 9007199254740993 || row.Tokens != "00123" || row.TokenType != "text" || row.PayloadType != "blob" || row.NullType != "null" || row.ModelBytes != 12 {
		t.Fatalf("SQLite types or values changed: %+v", row)
	}
	if err := db.Transaction(PreserveLegacyOverviewArchives); err == nil {
		t.Fatal("changed live rows must not overwrite or claim the old archive")
	}
}

func TestLegacyOverviewArchiveFailureRollsBackAndCanRetry(t *testing.T) {
	db := legacyArchiveTestDatabase(t)
	forced := errors.New("forced failure after archive before live clear")
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := PreserveLegacyOverviewArchives(tx); err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM usage_overview_hourly_stats").Error; err != nil {
			return err
		}
		return forced
	}); !errors.Is(err, forced) {
		t.Fatalf("expected forced transaction failure, got %v", err)
	}
	if db.Migrator().HasTable(legacyAccountingArchiveManifest) || db.Migrator().HasTable("keeper_legacy_usage_overview_hourly_stats") {
		t.Fatal("failed transaction retained a partial archive")
	}
	var count int64
	if err := db.Table("usage_overview_hourly_stats").Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("failed transaction lost original rows: %d %v", count, err)
	}
	if err := db.Transaction(PreserveLegacyOverviewArchives); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyOverviewArchiveRefusesConflictsAndTampering(t *testing.T) {
	for _, kind := range []string{"orphan", "row", "schema", "source"} {
		t.Run(kind, func(t *testing.T) {
			db := legacyArchiveTestDatabase(t)
			if kind == "orphan" {
				if err := db.Exec("CREATE TABLE keeper_legacy_usage_overview_daily_stats (unrelated)").Error; err != nil {
					t.Fatal(err)
				}
			} else if err := db.Transaction(PreserveLegacyOverviewArchives); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "row":
				if err := db.Exec("UPDATE keeper_legacy_usage_overview_daily_stats SET tokens = 'tampered' WHERE id = 5").Error; err != nil {
					t.Fatal(err)
				}
				if err := VerifyLegacyOverviewArchives(db); err == nil {
					t.Fatal("restart verification accepted changed archive content")
				}
			case "schema":
				if err := db.Exec("ALTER TABLE usage_overview_daily_stats ADD COLUMN unexpected").Error; err != nil {
					t.Fatal(err)
				}
			case "source":
				if err := db.Exec("UPDATE usage_overview_daily_stats SET model = 'a' WHERE id = 5").Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Transaction(PreserveLegacyOverviewArchives); err == nil {
				t.Fatal("archive conflict must refuse without overwriting")
			}
		})
	}
}

func TestRankingRetirementPreservesOriginalState(t *testing.T) {
	db := legacyArchiveTestDatabase(t)
	for _, statement := range []string{
		"CREATE TABLE local_ranking_period_stats (period_key, api_key_id, request_count)",
		"INSERT INTO local_ranking_period_stats VALUES ('2026-10', 11, 23)",
		"CREATE TABLE cpa_api_keys (id INTEGER PRIMARY KEY, local_ranking_avatar_id)",
		"INSERT INTO cpa_api_keys VALUES (11, 7)",
		"CREATE TABLE app_settings (setting_key, value)",
		"INSERT INTO app_settings VALUES ('ranking.identity','synthetic-private-state'),('ranking.local.period_state','synthetic-boundary'),('other','unchanged')",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Transaction(removeRankingMigration); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("local_ranking_period_stats") {
		t.Fatal("ranking table removed")
	}
	var count int64
	if err := db.Table("app_settings").Count(&count).Error; err != nil || count != 3 {
		t.Fatalf("ranking settings lost: %d %v", count, err)
	}
	var avatar int64
	if err := db.Table("cpa_api_keys").Select("local_ranking_avatar_id").Scan(&avatar).Error; err != nil || avatar != 7 {
		t.Fatalf("ranking avatar changed: %d %v", avatar, err)
	}
}
