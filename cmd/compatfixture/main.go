package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/latency"
	"cpa-usage-keeper/internal/repository"
	"gorm.io/gorm"
)

const fixtureMarker = "managed-upgrade-synthetic-fixture"

func main() {
	if len(os.Args) != 3 {
		panic("usage: compatfixture init|verify fixture-directory")
	}
	dir, err := filepath.Abs(os.Args[2])
	must(err)
	path := filepath.Join(dir, "synthetic.db")
	if os.Args[1] == "init" {
		_, err := os.Stat(path)
		if !os.IsNotExist(err) {
			panic("init requires an absent synthetic.db")
		}
		must(os.MkdirAll(dir, 0700))
	} else if os.Args[1] == "verify" {
		// This marker prevents the harness from opening a private production DB.
		marker, err := os.ReadFile(filepath.Join(dir, "SYNTHETIC_ONLY"))
		must(err)
		if string(marker) != fixtureMarker {
			panic("synthetic fixture marker mismatch")
		}
	} else {
		panic("unsupported mode")
	}
	db, err := repository.OpenDatabase(config.Config{SQLitePath: path, BackupDir: filepath.Join(dir, "backups")})
	must(err)
	sqlDB, err := db.DB()
	must(err)
	defer sqlDB.Close()
	if os.Args[1] == "init" {
		seed(db)
		must(os.WriteFile(filepath.Join(dir, "SYNTHETIC_ONLY"), []byte(fixtureMarker), 0600))
	}
	verify(db)
}

func seed(db *gorm.DB) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, key := range []entities.CPAAPIKey{
		{ID: 1, APIKey: "synthetic-retired-key", DisplayKey: "synthetic***retired", KeyAlias: "Team Client", IsDeleted: true},
		{ID: 2, APIKey: "synthetic-active-key", DisplayKey: "synthetic***active", KeyAlias: "Team Client"},
	} {
		must(db.Create(&key).Error)
	}
	parent, alias, client, agent := "parent-fixture", "Identity Alias", "192.0.2.1", "synthetic-agent"
	for _, event := range []entities.UsageEvent{
		{ID: 1, EventKey: "synthetic-hot-retired", APIGroupKey: "synthetic-retired-key", Model: "synthetic-model", AuthIndex: "fixture-identity", RequestID: "fixture-request-1", SessionID: "fixture-session", ParentSessionID: &parent, ClientIP: &client, UserAgent: &agent, Timestamp: now, TotalTokens: 37},
		{ID: 2, EventKey: "synthetic-hot-active", APIGroupKey: "synthetic-active-key", Model: "synthetic-model", AuthIndex: "fixture-identity", RequestID: "fixture-request-2", Timestamp: now, TotalTokens: 53},
	} {
		must(db.Create(&event).Error)
	}
	must(db.Create(&entities.UsageEventArchive{ID: 3, EventKey: "synthetic-cold-retired", APIGroupKey: "synthetic-retired-key", Model: "synthetic-model", AuthIndex: "fixture-identity", SessionID: "fixture-old-session", ParentSessionID: &parent, Timestamp: now.AddDate(-3, 0, 0), TotalTokens: 71}).Error)
	must(db.Create(&entities.UsageIdentity{ID: 1, Name: "Synthetic Identity", Alias: &alias, AuthType: entities.UsageIdentityAuthTypeAuthFile, Identity: "fixture-identity", TotalRequests: 3, TotalTokens: 161}).Error)
	must(db.Create(&entities.AuthSession{TokenHash: "synthetic-session-hash-not-an-auth-token", Role: "admin", Source: "standard", Alias: "Synthetic Admin", LoginIP: client, LastSeenIP: client, UserAgent: agent, LastSeenAt: &now, ExpiresAt: now.AddDate(1, 0, 0)}).Error)
	value := fixtureMarker
	must(db.Create(&entities.AppSetting{SettingKey: fixtureMarker, Value: &value, ValueType: entities.AppSettingValueTypeJSON}).Error)
	samples, ttft, duration := latency.NewSampleSet(), latency.NewSketch(), latency.NewSketch()
	for id := int64(1); id <= 2500; id++ {
		must(samples.Add(id, 100, 1000))
		must(ttft.Add(100))
		must(duration.Add(1000))
	}
	if len(samples.Points()) != 2500 {
		panic("init must run against the old 2500-point source")
	}
	points, err := samples.MarshalBinary()
	must(err)
	first, err := ttft.MarshalBinary()
	must(err)
	second, err := duration.MarshalBinary()
	must(err)
	must(db.Create(&entities.UsageLatencyStat{ID: 1, BucketType: entities.UsageLatencyBucketDay, BucketStart: now, APIGroupKey: "synthetic-retired-key", SampleCount: 2500, MaxTTFTMS: 100, MaxLatencyMS: 1000, FormatVersion: latency.FormatVersion, TTFTSketch: first, LatencySketch: second, SamplePoints: points, CreatedAt: now, UpdatedAt: now}).Error)
}

func verify(db *gorm.DB) {
	var marker entities.AppSetting
	must(db.Where("setting_key = ?", fixtureMarker).First(&marker).Error)
	if marker.Value == nil || *marker.Value != fixtureMarker {
		panic("database is not the synthetic fixture")
	}
	var integrity string
	must(db.Raw("PRAGMA integrity_check").Scan(&integrity).Error)
	if integrity != "ok" {
		panic("integrity check failed")
	}
	// Hash old persistent columns only, so additive metadata cannot hide a lost
	// key, event, alias, session field, or attribution field during upgrade.
	queries := []string{
		"SELECT id, api_key, display_key, key_alias, is_deleted FROM cpa_api_keys ORDER BY id",
		"SELECT id, event_key, api_group_key, model, auth_index, request_id, session_id, parent_session_id, client_ip, user_agent, timestamp, total_tokens FROM usage_events ORDER BY id",
		"SELECT id, event_key, api_group_key, model, auth_index, session_id, parent_session_id, timestamp, total_tokens FROM usage_events_archive ORDER BY id",
		"SELECT id, name, alias, identity, total_requests, total_tokens FROM usage_identities ORDER BY id",
		"SELECT token_hash, role, source, alias, cpa_api_key_id, login_ip, last_seen_ip, user_agent, last_seen_at, expires_at, created_at, updated_at FROM auth_sessions ORDER BY token_hash",
		"SELECT setting_key, value, value_type, created_at, updated_at FROM app_settings ORDER BY setting_key",
		"SELECT id, bucket_type, bucket_start, api_group_key, sample_count, max_ttft_ms, max_latency_ms, format_version, ttft_sketch, latency_sketch, created_at, updated_at FROM usage_latency_stats ORDER BY id",
	}
	h := sha256.New()
	for _, query := range queries {
		rows, err := db.Raw(query).Rows()
		must(err)
		cols, err := rows.Columns()
		must(err)
		for rows.Next() {
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range values {
				pointers[i] = &values[i]
			}
			must(rows.Scan(pointers...))
			encoded, err := json.Marshal(values)
			must(err)
			h.Write(encoded)
		}
		must(rows.Err())
		must(rows.Close())
	}
	active, err := repository.ListActiveCPAAPIKeys(db)
	must(err)
	if len(active) != 1 || active[0].ID != 2 {
		panic("active key visibility changed")
	}
	if _, err := repository.FindActiveCPAAPIKeyByValue(db, "synthetic-retired-key"); err != gorm.ErrRecordNotFound {
		panic("retired key authentication changed")
	}
	var row entities.UsageLatencyStat
	must(db.First(&row, 1).Error)
	samples, err := latency.UnmarshalSampleSet(row.SamplePoints)
	must(err)
	pointCount := len(samples.Points())
	if (pointCount != 1000 && pointCount != 2500) || row.SampleCount != 2500 {
		panic("unexpected latency sample/stat count")
	}
	fmt.Printf("synthetic integrity=ok active_keys=1 retired_auth=rejected persistence_sha256=%x sample_points=%d sample_blob_sha256=%x\n", h.Sum(nil), pointCount, sha256.Sum256(row.SamplePoints))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
