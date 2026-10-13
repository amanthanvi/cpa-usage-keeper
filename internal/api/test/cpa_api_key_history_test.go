package test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	. "cpa-usage-keeper/internal/api"
	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository"
	"cpa-usage-keeper/internal/service"
	servicedto "cpa-usage-keeper/internal/service/dto"
	"gorm.io/gorm"
)

func TestRetiredCPAAPIKeyHistoryKeepsOriginalIDsAndAliases(t *testing.T) {
	db := openAPITestDatabase(t)
	oldKey, newKey := "sk-history-old123456", "sk-history-new654321"
	firstSync := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	if err := repository.SyncCPAAPIKeys(db, []string{oldKey}, firstSync); err != nil {
		t.Fatalf("sync old key: %v", err)
	}
	if err := repository.UpdateCPAAPIKeyAlias(db, 1, "Team Client"); err != nil {
		t.Fatalf("alias old key: %v", err)
	}
	if err := repository.SyncCPAAPIKeys(db, []string{newKey}, firstSync.Add(time.Hour)); err != nil {
		t.Fatalf("replace old key: %v", err)
	}
	if err := repository.UpdateCPAAPIKeyAlias(db, 2, "Team Client"); err != nil {
		t.Fatalf("alias new key: %v", err)
	}
	keys := service.NewCPAAPIKeyService(db)
	active, err := keys.ListCPAAPIKeys(context.Background())
	if err != nil || len(active) != 1 || active[0].ID != 2 {
		t.Fatalf("active key listing changed: ids=%v err=%v", keyIDs(active), err)
	}
	if _, err := keys.FindActiveCPAAPIKeyByValue(context.Background(), oldKey); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("retired key must not authenticate: %v", err)
	}
	if _, err := keys.FindActiveCPAAPIKeyByID(context.Background(), 1); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("retired key id must not authenticate: %v", err)
	}

	events := &usageEventsStub{events: []servicedto.UsageEventRecord{
		{ID: 11, Timestamp: firstSync, APIGroupKey: oldKey, Model: "model-a"},
		{ID: 12, Timestamp: firstSync.Add(time.Hour), APIGroupKey: newKey, Model: "model-a"},
	}}
	eventRouter := NewRouter(nil, nil, events, nil, AuthConfig{}, nil, "", OptionalProviders{CPAAPIKeys: keys})
	response := serveAPIGet(eventRouter, "/api/v1/usage/events?range=24h")
	if response.Code != http.StatusOK {
		t.Fatalf("events status=%d", response.Code)
	}
	var eventPayload struct {
		Events []struct {
			APIKey string `json:"api_key"`
		} `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &eventPayload); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(eventPayload.Events) != 2 || eventPayload.Events[0].APIKey != "Team Client" || eventPayload.Events[1].APIKey != "Team Client" {
		t.Fatalf("historical event aliases changed: %+v", eventPayload.Events)
	}
	eventBody := response.Body.String()

	export := serveAPIGet(eventRouter, "/api/v1/usage/events/export?range=24h&format=csv")
	if export.Code != http.StatusOK {
		t.Fatalf("export status=%d", export.Code)
	}
	records, err := csv.NewReader(strings.NewReader(export.Body.String())).ReadAll()
	if err != nil || len(records) != 3 {
		t.Fatalf("decode export: records=%d err=%v", len(records), err)
	}
	idColumn, labelColumn := -1, -1
	for index, name := range records[0] {
		switch name {
		case "cpa_api_key_id":
			idColumn = index
		case "api_key":
			labelColumn = index
		}
	}
	if idColumn < 0 || labelColumn < 0 || records[1][idColumn] != "1" || records[2][idColumn] != "2" || records[1][labelColumn] != "Team Client" || records[2][labelColumn] != "Team Client" {
		t.Fatalf("historical export identity changed: ids=%d labels=%d", idColumn, labelColumn)
	}

	analysis := &analysisSplitStub{analysis: &servicedto.AnalysisSnapshot{
		Granularity: servicedto.AnalysisGranularityHourly,
		APIKeyComposition: []servicedto.AnalysisCompositionItem{
			{Key: oldKey, Requests: 3},
			{Key: newKey, Requests: 5},
		},
	}}
	analysisRouter := NewRouter(nil, nil, analysis, nil, AuthConfig{}, nil, "", OptionalProviders{CPAAPIKeys: keys})
	response = serveAPIGet(analysisRouter, "/api/v1/usage/analysis?range=24h")
	if response.Code != http.StatusOK {
		t.Fatalf("analysis status=%d", response.Code)
	}
	var analysisPayload struct {
		APIKeyComposition []struct {
			Key      string `json:"key"`
			Label    string `json:"label"`
			Requests int64  `json:"requests"`
		} `json:"api_key_composition"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &analysisPayload); err != nil {
		t.Fatalf("decode analysis: %v", err)
	}
	if len(analysisPayload.APIKeyComposition) != 2 || analysisPayload.APIKeyComposition[0].Key != "1" || analysisPayload.APIKeyComposition[1].Key != "2" || analysisPayload.APIKeyComposition[0].Label != "Team Client" || analysisPayload.APIKeyComposition[1].Label != "Team Client" || analysisPayload.APIKeyComposition[0].Requests != 3 || analysisPayload.APIKeyComposition[1].Requests != 5 {
		t.Fatalf("historical analysis identity or counts changed: %+v", analysisPayload.APIKeyComposition)
	}
	for _, body := range []string{eventBody, response.Body.String(), export.Body.String()} {
		if strings.Contains(body, oldKey) || strings.Contains(body, newKey) {
			t.Fatal("raw API key leaked in usage response")
		}
	}
}

func keyIDs(rows []entities.CPAAPIKey) []int64 {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}
