package sqlite

import (
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

var eventStart = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func requestEvent(scope string, at time.Time) billing.RequestEvent {
	return billing.RequestEvent{
		At: at, Scope: scope, AuthIndex: "auth-codex", ExecutorType: "CodexExecutor",
		ReasoningEffort: "high", ServiceTier: "auto",
		UpstreamModel: "gpt-5.5", BillingModel: "gpt-5.5",
		LatencyMS: 1500, TTFTMS: 250,
		AccountingQuality: billing.TokenAccountingComplete, PriceSource: billing.PriceSourceCustom,
		Cost: billing.Cost{
			TotalUSD: 0.5, UncachedInputUSD: 0.2, OutputUSD: 0.3,
			UncachedInputTokens: 500, BilledOutputTokens: 500,
		},
	}
}

// requestEventDatabase holds four normal events for Alice and two failed events
// for Bob.
func requestEventDatabase(t *testing.T) (*DB, *billing.State) {
	t.Helper()
	database := openTestDB(t)
	state := billing.NewState()
	state.Keys["scope-a"] = &billing.KeyState{Preview: "sk-tes…0001", Label: "Alice"}
	state.Keys["scope-b"] = &billing.KeyState{Preview: "sk-tes…0002", Label: "Bob"}
	state.Credentials["auth-codex"] = billing.Credential{Provider: "codex", Account: "ops@example.com"}

	events := make([]billing.RequestEvent, 0, 4)
	for i := range 4 {
		events = append(events, requestEvent("scope-a", eventStart.Add(time.Duration(i)*time.Minute)))
	}
	errors := []billing.RequestErrorEvent{
		{Event: requestEvent("scope-b", eventStart.Add(4*time.Minute))},
		{Event: requestEvent("scope-b", eventStart.Add(5*time.Minute))},
	}
	mustSave(t, database, state, billing.Changes{AllKeys: true, Credentials: true, NormalRequestEvents: events, RequestErrorEvents: errors})
	return database, state
}

func mustQueryRequestEvents(t *testing.T, database *DB, query billing.RequestEventQuery) billing.RequestEventView {
	t.Helper()
	view, err := database.RequestEvents(query, eventStart.Add(-billing.RequestEventRetention))
	if err != nil {
		t.Fatalf("RequestEvents error = %v", err)
	}
	return view
}

func TestRequestEventsPageOverTheWholeMatch(t *testing.T) {
	database, _ := requestEventDatabase(t)

	view := mustQueryRequestEvents(t, database, billing.RequestEventQuery{Limit: 2})
	if len(view.Entries) != 2 || view.Total != 6 || !view.Entries[0].At.Equal(eventStart.Add(5*time.Minute)) {
		t.Fatalf("view = %+v of %d, want the newest 2 of 6", view.Entries, view.Total)
	}
	if view.Statuses != (billing.RequestEventStatusCounts{All: 6, Normal: 4, Failed: 2}) {
		t.Fatalf("statuses = %+v", view.Statuses)
	}
	if view = mustQueryRequestEvents(t, database, billing.RequestEventQuery{Offset: 4, Limit: 2}); len(view.Entries) != 2 ||
		!view.Entries[1].At.Equal(eventStart) {
		t.Fatalf("last page = %+v, want the two oldest entries", view.Entries)
	}
	if view = mustQueryRequestEvents(t, database, billing.RequestEventQuery{Offset: 20, Limit: 2}); len(view.Entries) != 0 || view.Total != 6 {
		t.Fatalf("view = %+v, want an empty page over the counted events", view)
	}
}

func TestRequestEventFailedFilterKeepsOverallCounts(t *testing.T) {
	database, _ := requestEventDatabase(t)
	normal, failed := false, true

	if view := mustQueryRequestEvents(t, database, billing.RequestEventQuery{Failed: &normal}); view.Total != 4 ||
		len(view.Entries) != 4 || view.Statuses.All != 6 {
		t.Fatalf("view = %d entries, statuses %+v", view.Total, view.Statuses)
	}
	if view := mustQueryRequestEvents(t, database, billing.RequestEventQuery{Failed: &failed}); view.Total != 2 ||
		len(view.Entries) != 2 {
		t.Fatalf("view = %+v, want the two failed events", view.Entries)
	}
}

func TestRequestEventFieldAndTimeFilters(t *testing.T) {
	database, state := requestEventDatabase(t)
	state.Credentials["auth-xai"] = billing.Credential{Provider: "xai", Account: "ops-xai@example.com"}
	entry := requestEvent("scope-b", eventStart.Add(6*time.Minute))
	entry.AuthIndex = "auth-xai"
	entry.UpstreamModel = "gpt-5.6-sol"
	entry.BillingModel = "team/gpt-5.6-sol"
	mustSave(t, database, state, billing.Changes{Credentials: true, NormalRequestEvents: []billing.RequestEvent{entry}})

	query := billing.RequestEventQuery{
		KeyScope: "scope-b", Model: "team/gpt-5.6-sol", Source: "xai · ops-xai@example.com",
		From: eventStart.Add(6 * time.Minute), To: eventStart.Add(7 * time.Minute), IncludeFilters: true,
	}
	view := mustQueryRequestEvents(t, database, query)
	if view.Total != 1 || len(view.Entries) != 1 || !view.Entries[0].At.Equal(entry.At) {
		t.Fatalf("view = %+v, want the one exact field and time match", view.Entries)
	}
	if view.Filters == nil || len(view.Filters.Models) != 1 || len(view.Filters.Sources) != 1 {
		t.Fatalf("filter options = %+v", view.Filters)
	}

	if outside := mustQueryRequestEvents(t, database, billing.RequestEventQuery{
		From: entry.At.Add(time.Nanosecond), To: entry.At.Add(time.Minute),
	}); outside.Total != 0 {
		t.Fatalf("outside range total = %d, want 0", outside.Total)
	}
}

func TestRequestEventScopeConstrainsRowsAndCounts(t *testing.T) {
	database, _ := requestEventDatabase(t)

	view := mustQueryRequestEvents(t, database, billing.RequestEventQuery{Scope: "scope-a", Limit: 10})
	if view.Total != 4 || len(view.Entries) != 4 ||
		view.Statuses != (billing.RequestEventStatusCounts{All: 4, Normal: 4}) {
		t.Fatalf("scope-a view = %+v, statuses %+v", view.Entries, view.Statuses)
	}
	for _, entry := range view.Entries {
		if entry.Scope != "scope-a" {
			t.Fatalf("scope-a query returned %+v", entry)
		}
	}

	view = mustQueryRequestEvents(t, database, billing.RequestEventQuery{Scope: "scope-b", Limit: 10})
	if view.Total != 2 || len(view.Entries) != 2 ||
		view.Statuses != (billing.RequestEventStatusCounts{All: 2, Failed: 2}) {
		t.Fatalf("scope-b view = %+v, statuses %+v", view.Entries, view.Statuses)
	}
}

func TestRequestEventRowsFollowTheKeyTheyName(t *testing.T) {
	database, state := requestEventDatabase(t)
	state.Keys["scope-a"].Label = "Alice Cooper"
	mustSave(t, database, state, billing.Changes{Keys: []string{"scope-a"}})

	view := mustQueryRequestEvents(t, database, billing.RequestEventQuery{KeyScope: "scope-a"})
	if view.Total != 4 {
		t.Fatalf("total = %d, want the renamed key's whole history", view.Total)
	}
	if entry := view.Entries[0]; entry.Label != "Alice Cooper" || entry.Preview != "sk-tes…0001" ||
		entry.ExecutorType != "CodexExecutor" ||
		entry.ReasoningEffort != "high" || entry.ServiceTier != "auto" ||
		entry.Source != "codex · ops@example.com" || entry.Provider != "codex" ||
		entry.LatencyMS != 1500 || entry.TTFTMS != 250 {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestRequestEventsKeepTheRetentionWindow(t *testing.T) {
	database, state := requestEventDatabase(t)
	cutoff := eventStart.Add(180*24*time.Hour - billing.RequestEventRetention)
	view, err := database.RequestEvents(billing.RequestEventQuery{}, cutoff)
	if err != nil || view.Total != 6 {
		t.Fatalf("six-month history: %+v, %v", view, err)
	}
	failures, err := database.RequestErrors(billing.RequestErrorQuery{}, cutoff)
	if err != nil || failures.Total != 2 {
		t.Fatalf("six-month error history: %+v, %v", failures, err)
	}
	later := eventStart.Add(366 * 24 * time.Hour)

	mustSave(t, database, state, billing.Changes{
		NormalRequestEvents: []billing.RequestEvent{requestEvent("scope-a", later)},
		RequestEventCutoff:  later.Add(-billing.RequestEventRetention),
	})
	view, err = database.RequestEvents(billing.RequestEventQuery{}, later.Add(-billing.RequestEventRetention))
	if err != nil {
		t.Fatalf("RequestEvents error = %v", err)
	}
	if view.Total != 1 || !view.Entries[0].At.Equal(later) {
		t.Fatalf("view = %+v, want only the entry inside the window", view.Entries)
	}
	var errorCount int
	if err := database.db.QueryRow("SELECT count(*) FROM request_errors").Scan(&errorCount); err != nil || errorCount != 0 {
		t.Fatalf("expired error details: count=%d, err=%v", errorCount, err)
	}
}

func TestLoadDropsEntriesPastRetention(t *testing.T) {
	database, _ := requestEventDatabase(t)

	snapshot, errLoad := database.Load(eventStart.Add(3*time.Minute), time.Time{})
	if errLoad != nil {
		t.Fatalf("Load error = %v", errLoad)
	}
	if snapshot.RequestEventCount != 3 {
		t.Fatalf("RequestEventCount = %d, want three", snapshot.RequestEventCount)
	}
	if view := mustQueryRequestEvents(t, database, billing.RequestEventQuery{}); view.Total != 3 {
		t.Fatalf("total = %d, want the aged entries gone from the database", view.Total)
	}
}

func TestEventKeysFollowTimeRangeAndRetainDeletedIdentities(t *testing.T) {
	database := openTestDB(t)
	state := billing.NewState()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	state.Keys["active"] = &billing.KeyState{Preview: "sk-tes…0001", InConfig: true}
	state.Keys["deleted"] = &billing.KeyState{Preview: "sk-tes…0002", Label: "Deleted", DeletedAt: start}
	state.Keys["idle"] = &billing.KeyState{Preview: "sk-tes…0003"}
	mustSave(t, database, state, billing.Changes{AllKeys: true,
		NormalRequestEvents: []billing.RequestEvent{
			requestEvent("active", start), requestEvent("active", start.Add(time.Minute)),
			requestEvent("outside", start.Add(-time.Nanosecond)),
			requestEvent("end", start.Add(time.Hour)), requestEvent("", start),
		},
		RequestErrorEvents: []billing.RequestErrorEvent{{Event: requestEvent("deleted", start.Add(2*time.Minute)), Error: billing.RequestError{StatusCode: 502}}},
	})
	keys, err := database.EventKeys(start, start.Add(time.Hour), start.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %+v, want distinct normal and error identities only", keys)
	}
	found := map[string]billing.EventKey{}
	for _, key := range keys {
		found[key.Scope] = key
	}
	if found["active"].Preview != "sk-tes…0001" || found["deleted"].Label != "Deleted" || !found["deleted"].DeletedAt.Equal(start) {
		t.Fatalf("lost key identity or status: %+v", keys)
	}
	keys, err = database.EventKeys(start, start.Add(time.Hour), start.Add(2*time.Minute))
	if err != nil || len(keys) != 1 || keys[0].Scope != "deleted" {
		t.Fatalf("retention cutoff ignored: %+v, %v", keys, err)
	}
	keys, err = database.EventKeys(start.Add(3*time.Minute), start.Add(time.Hour), start)
	if err != nil || keys == nil || len(keys) != 0 {
		t.Fatalf("empty range = %+v, %v", keys, err)
	}
}
