package test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpa-usage-keeper/internal/api"
	"cpa-usage-keeper/internal/auth"
	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/helper"
	"cpa-usage-keeper/internal/repository"
	"cpa-usage-keeper/internal/service"
	servicedto "cpa-usage-keeper/internal/service/dto"
	"gorm.io/gorm"
)

func TestRetiredCPAAPIKeyDatabaseHistoryAndActiveBoundaries(t *testing.T) {
	db := openAPITestDatabase(t)
	ctx := context.Background()
	day := time.Now().In(time.Local).AddDate(0, 0, -1)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local)
	at := day.Add(12 * time.Hour)
	hour := time.Now().In(time.Local).Truncate(time.Hour).Add(-5 * time.Hour)
	oldKey, newKey := "sk-history-old123456", "sk-history-new654321"
	if err := repository.SyncCPAAPIKeys(db, []string{oldKey}, at); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateCPAAPIKeyAlias(db, 1, "Team Client"); err != nil {
		t.Fatal(err)
	}
	if err := repository.SyncCPAAPIKeys(db, []string{newKey}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateCPAAPIKeyAlias(db, 2, "Team Client"); err != nil {
		t.Fatal(err)
	}
	pluginA := helper.PluginAPIKeyGroupKey("plugin:history:a", "same-principal")
	pluginB := helper.PluginAPIKeyGroupKey("plugin:history:b", "same-principal")
	rows := []entities.CPAAPIKey{
		{ID: 3, APIKey: pluginA, SourceType: entities.CPAAPIKeySourcePlugin, AccessProvider: "plugin:history:a", IsDeleted: true},
		{ID: 4, APIKey: pluginB, SourceType: entities.CPAAPIKeySourcePlugin, AccessProvider: "plugin:history:b"},
		{ID: 5, APIKey: "sk-unused-retired", IsDeleted: true},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	groups := []string{oldKey, newKey, pluginA, pluginB}
	available, unavailable := true, int64(0)
	for i, group := range groups {
		count := int64(i + 1)
		cost := float64(count) / 4
		if err := db.Create(&entities.UsageEvent{EventKey: fmt.Sprintf("history-%d", i), Timestamp: at.Add(time.Duration(i) * time.Minute), APIGroupKey: group, Model: "history-model", TotalTokens: count * 10, CostUSD: &cost, CostAvailable: &available}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&entities.UsageOverviewHourlyStat{BucketStart: hour, APIGroupKey: group, Model: "history-model", RequestCount: count, TotalTokens: count * 10, CostUSD: &cost, UnavailableCostCount: &unavailable, SpeedTPSSum: float64(count) * 5, SpeedSampleCount: count}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&entities.UsageOverviewDailyStat{BucketStart: day, APIGroupKey: group, Model: "history-model", RequestCount: count, TotalTokens: count * 10, CostUSD: &cost, UnavailableCostCount: &unavailable, SpeedTPSSum: float64(count) * 5, SpeedSampleCount: count}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var before []entities.UsageEvent
	if err := db.Order("id").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	keys := service.NewCPAAPIKeyService(db)
	usage := service.NewUsageService(db, emptyPricingCatalogForTest())
	router := api.NewRouter(nil, nil, usage, nil, api.AuthConfig{}, nil, "", api.OptionalProviders{CPAAPIKeys: keys})
	date := day.Format("2006-01-02")
	query := "range=custom&unit=day&start=" + date + "&end=" + date
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := serveAPIGet(router, path)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		for _, sensitive := range append(groups, "same-principal") {
			if strings.Contains(response.Body.String(), sensitive) {
				t.Fatalf("%s leaked internal key", path)
			}
		}
		return response
	}
	for _, filter := range []string{"", "&api_key_id=1", "&api_key_id=3"} {
		response := get("/api/v1/usage/events?" + query + filter)
		var payload struct {
			Events []struct {
				APIKey string `json:"api_key"`
			} `json:"events"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		want := 4
		if filter != "" {
			want = 1
		}
		if len(payload.Events) != want {
			t.Fatalf("event filter %s returned %d", filter, len(payload.Events))
		}
		if filter == "&api_key_id=1" && payload.Events[0].APIKey != "Team Client" {
			t.Fatal(payload.Events)
		}
		if filter == "&api_key_id=3" && payload.Events[0].APIKey != "Third-party Identity #3" {
			t.Fatal(payload.Events)
		}
		for _, format := range []string{"csv", "json"} {
			response = get("/api/v1/usage/events/export?" + query + filter + "&format=" + format)
			if format == "csv" {
				records, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
				if err != nil || len(records) != want+1 {
					t.Fatalf("CSV count=%d err=%v", len(records), err)
				}
				column := -1
				for i, name := range records[0] {
					if name == "cpa_api_key_id" {
						column = i
					}
				}
				if column < 0 {
					t.Fatal("missing export ID column")
				}
				ids := map[string]bool{}
				for _, row := range records[1:] {
					ids[row[column]] = true
				}
				if filter == "" && !reflect.DeepEqual(ids, map[string]bool{"1": true, "2": true, "3": true, "4": true}) {
					t.Fatal(ids)
				}
				if filter != "" && !ids[strings.TrimPrefix(filter, "&api_key_id=")] {
					t.Fatal(ids)
				}
			} else if !strings.Contains(response.Body.String(), `"cpa_api_key_id":"`+map[string]string{"": "1", "&api_key_id=1": "1", "&api_key_id=3": "3"}[filter]+`"`) {
				t.Fatal("JSON export lost original ID")
			}
		}
	}
	for _, historyQuery := range []string{query, "range=custom&unit=hour&start=" + url.QueryEscape(hour.Format(time.RFC3339)) + "&end=" + url.QueryEscape(hour.Add(5*time.Hour).Format(time.RFC3339))} {
		for _, filter := range []string{"", "&api_key_id=1", "&api_key_id=3"} {
			response := get("/api/v1/usage/analysis?" + historyQuery + filter)
			var payload struct {
				APIKeyComposition []struct {
					Key         string  `json:"key"`
					Label       string  `json:"label"`
					Requests    int64   `json:"requests"`
					TotalTokens int64   `json:"total_tokens"`
					CostUSD     float64 `json:"cost_usd"`
				} `json:"api_key_composition"`
				CostSummary struct {
					TotalCostUSD  float64 `json:"total_cost_usd"`
					CostAvailable bool    `json:"cost_available"`
				} `json:"cost_summary"`
				ModelComposition []struct {
					Requests    int64 `json:"requests"`
					TotalTokens int64 `json:"total_tokens"`
				} `json:"model_composition"`
				Heatmap struct {
					APIKeys []string `json:"api_keys"`
				} `json:"heatmap"`
				ModelSpeed struct {
					Series []struct {
						SpeedSampleCount int64 `json:"speed_sample_count"`
						TotalTokens      int64 `json:"total_tokens"`
					} `json:"series"`
				} `json:"model_speed"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			var requests, tokens int64
			ids := map[string]bool{}
			for _, item := range payload.APIKeyComposition {
				ids[item.Key] = true
				requests += item.Requests
				tokens += item.TotalTokens
				if (item.Key == "1" || item.Key == "2") && item.Label != "Team Client" {
					t.Fatal(item)
				}
			}
			wantRequests, wantCost, wantRows := int64(10), 2.5, 4
			if filter == "&api_key_id=1" {
				wantRequests, wantCost, wantRows = 1, .25, 1
			}
			if filter == "&api_key_id=3" {
				wantRequests, wantCost, wantRows = 3, .75, 1
			}
			if requests != wantRequests || tokens != wantRequests*10 || len(ids) != wantRows || payload.CostSummary.TotalCostUSD != wantCost || !payload.CostSummary.CostAvailable {
				t.Fatalf("analysis %s %s: %+v", historyQuery, filter, payload)
			}
			if len(payload.ModelComposition) != 1 || payload.ModelComposition[0].Requests != wantRequests || payload.ModelComposition[0].TotalTokens != wantRequests*10 || len(payload.Heatmap.APIKeys) != wantRows || len(payload.ModelSpeed.Series) != 1 || payload.ModelSpeed.Series[0].SpeedSampleCount != wantRequests || payload.ModelSpeed.Series[0].TotalTokens != wantRequests*10 {
				t.Fatalf("retired history lost from model/heatmap/speed: %+v", payload)
			}
		}
	}
	for _, item := range []struct {
		id    string
		count int64
		cost  float64
	}{{"1", 1, .25}, {"3", 3, .75}} {
		response := get("/api/v1/usage/overview?" + query + "&api_key_id=" + item.id)
		var payload struct {
			Usage struct {
				TotalRequests int64 `json:"total_requests"`
				TotalTokens   int64 `json:"total_tokens"`
			} `json:"usage"`
			Summary struct {
				TotalCost     float64 `json:"total_cost"`
				CostAvailable bool    `json:"cost_available"`
			} `json:"summary"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Usage.TotalRequests != item.count || payload.Usage.TotalTokens != item.count*10 || payload.Summary.TotalCost != item.cost || !payload.Summary.CostAvailable {
			t.Fatalf("retired overview totals: %+v", payload)
		}
	}
	options := get("/api/v1/usage/api-keys/options").Body.String()
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		if !strings.Contains(options, `"id":"`+id+`"`) {
			t.Fatal(options)
		}
	}
	for _, scope := range []string{"overview", "analysis", "request-events"} {
		options = get("/api/v1/usage/api-keys/options?scope=" + scope + "&" + query).Body.String()
		if strings.Count(options, `"id"`) != 4 || strings.Contains(options, `"id":"5"`) {
			t.Fatal(options)
		}
		for _, id := range []string{"1", "2", "3", "4"} {
			if !strings.Contains(options, `"id":"`+id+`"`) {
				t.Fatal(options)
			}
		}
	}
	for _, path := range []string{"/api/v1/usage/api-keys", "/api/v1/usage/api-keys/settings"} {
		response := serveAPIGet(router, path)
		if response.Code != 200 || strings.Contains(response.Body.String(), `"id":"1"`) || strings.Contains(response.Body.String(), `"id":"3"`) || strings.Contains(response.Body.String(), oldKey) {
			t.Fatalf("active administration changed: %s", response.Body.String())
		}
	}
	for _, id := range []int64{1, 3} {
		if _, err := keys.FindActiveCPAAPIKeyByID(ctx, id); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("retired id %d authenticated: %v", id, err)
		}
	}
	for _, key := range []string{oldKey, pluginA, pluginB} {
		if _, err := keys.FindActiveCPAAPIKeyByValue(ctx, key); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("history key authenticated: %v", err)
		}
	}
	if _, err := keys.UpdateCPAAPIKeyAlias(ctx, 1, "changed"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("retired alias editable: %v", err)
	}
	for _, id := range []string{"0", "invalid", "99999"} {
		if response := serveAPIGet(router, "/api/v1/usage/events?"+query+"&api_key_id="+id); response.Code == 200 {
			t.Fatalf("invalid/missing id %s accepted", id)
		}
	}
	sessions := auth.NewSessionManager(time.Hour)
	authConfig := api.AuthConfig{Enabled: true, LoginPassword: "history-test", SessionTTL: time.Hour}
	authed := api.NewRouter(nil, nil, usage, nil, authConfig, api.NewAuthHandler(authConfig, sessions), "", api.OptionalProviders{CPAAPIKeys: keys})
	for _, key := range []string{oldKey, pluginA, pluginB} {
		body, err := json.Marshal(map[string]string{"apiKey": key})
		if err != nil {
			t.Fatal(err)
		}
		response := serveCredentialMutation(authed, http.MethodPost, "/api/v1/auth/api-key-login", string(body))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("retired/plugin login status=%d", response.Code)
		}
	}
	for _, id := range []int64{1, 3, 4} {
		token, _, err := sessions.CreateAPIKeyViewerWithSource(id, auth.SessionSourceStandard)
		if err != nil {
			t.Fatal(err)
		}
		response := serveAPIGet(authed, "/api/v1/key-analysis?"+query, &http.Cookie{Name: standardSessionCookieName, Value: token})
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("retired/plugin viewer %d status=%d", id, response.Code)
		}
	}
	token, _, err := sessions.CreateAPIKeyViewerWithSource(2, auth.SessionSourceStandard)
	if err != nil {
		t.Fatal(err)
	}
	response := serveAPIGet(authed, "/api/v1/key-analysis?"+query+"&api_key_id=1", &http.Cookie{Name: standardSessionCookieName, Value: token})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"key":"2"`) || strings.Contains(response.Body.String(), `"key":"1"`) {
		t.Fatalf("viewer scope widened: %s", response.Body.String())
	}
	if err := repository.SyncCPAAPIKeys(db, []string{oldKey, newKey}, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	revived, err := keys.FindActiveCPAAPIKeyByValue(ctx, oldKey)
	if err != nil || revived.ID != 1 || revived.KeyAlias != "Team Client" {
		t.Fatalf("revival lost identity: %+v err=%v", revived, err)
	}
	var plugin entities.CPAAPIKey
	if err := db.First(&plugin, 4).Error; err != nil || plugin.SourceType != entities.CPAAPIKeySourcePlugin || plugin.IsDeleted {
		t.Fatalf("native sync changed plugin: %+v err=%v", plugin, err)
	}
	var after []entities.UsageEvent
	if err := db.Order("id").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("history reads or native sync rewrote raw usage events")
	}
	// The service resolves historical IDs independently of active login authority.
	if _, err := usage.ListUsageEvents(ctx, servicedto.UsageFilter{APIKeyID: "3", StartTime: &day, EndTime: &at}); err != nil {
		t.Fatal(err)
	}
}

func TestRetiredCPAAPIKeyLiveEventsKeepAliasAfterRotation(t *testing.T) {
	db := openAPITestDatabase(t)
	now := time.Now()
	oldKey := "sk-live-history-old123456"
	if err := repository.SyncCPAAPIKeys(db, []string{oldKey}, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateCPAAPIKeyAlias(db, 1, "Retired Live Alias"); err != nil {
		t.Fatal(err)
	}
	liveTestInsert(t, db, []entities.UsageEvent{{EventKey: "history-live-initial", Timestamp: now.Add(-time.Minute), APIGroupKey: oldKey, Model: "history-live"}})
	notifier := service.NewUsageEventsNotifier()
	server := httptest.NewServer(api.NewRouter(nil, nil, service.NewUsageService(db, emptyPricingCatalogForTest()), nil, api.AuthConfig{}, nil, "", api.OptionalProviders{CPAAPIKeys: service.NewCPAAPIKeyService(db), UsageEventsLive: notifier, CostReadGate: service.NewCostReadGate()}))
	defer server.Close()
	response, reader := liveTestConnect(t, &http.Client{Timeout: 10 * time.Second}, server.URL+"/api/v1/usage/events/live?range=24h&api_key_id=1", nil)
	defer response.Body.Close()
	check := func(frame liveTestFrame) {
		t.Helper()
		rows := frame.data["events"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["api_key"] != "Retired Live Alias" {
			t.Fatalf("lost historical live attribution: %+v", frame.data)
		}
	}
	check(liveTestReadKind(t, reader, "snapshot"))
	if err := repository.SyncCPAAPIKeys(db, []string{"sk-live-history-new654321"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	late := liveTestInsert(t, db, []entities.UsageEvent{{EventKey: "history-live-retired", Timestamp: now, APIGroupKey: oldKey, Model: "history-live"}})
	notifier.NotifyUsageEventsCommitted(late)
	check(liveTestReadKind(t, reader, "append"))
	response.Body.Close()
}
