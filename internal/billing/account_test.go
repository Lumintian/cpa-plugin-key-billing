package billing

import (
	"strings"
	"testing"
	"time"
)

func newAccountStore(t *testing.T, now time.Time) *Store {
	store, _ := newAccountStoreWithRepository(t, now)
	return store
}

func newAccountStoreWithRepository(t *testing.T, now time.Time) (*Store, *memoryRepository) {
	t.Helper()
	store, repo := newStoreWithRepository(t)
	store.now = func() time.Time { return now }
	store.ReplaceAll(func(state *State) {
		state.Prices = map[string]CustomPrice{"gpt-5.5": {
			ModelID: "gpt-5.5", PriceRates: PriceRates{InputPer1M: 1,
				OutputPer1M:     2,
				CacheReadPer1M:  floatPtr(0.1),
				CacheWritePer1M: floatPtr(1.25)},
		}}
	})
	return store, repo
}

func subsetEvent(scope string, at time.Time) UsageEvent {
	return UsageEvent{
		Scope: scope, KeyPreview: "sk-tes…0001", AuthIndex: "auth-codex", ExecutorType: "CodexExecutor",
		ReasoningEffort: "high", ServiceTier: "auto", At: at, RequestedAt: at,
		UpstreamModel: "gpt-5.5", RouteModel: "gpt-5.5",
		Breakdown: completeBreakdown(500, 400, 100, 500, 200),
	}
}

func admittedEvent(store *Store, scope string, at time.Time) UsageEvent {
	store.Authorize(scope, at)
	return subsetEvent(scope, at)
}

const wantSubsetCost = 0.0005 + 0.00004 + 0.000125 + 0.001

func TestRecordUsageSeparatesNormalAndErrorEvents(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store, repo := newAccountStoreWithRepository(t, now)
	if _, err := store.ClearPluginLogs(); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(subsetEvent("scope-a", now))
	store.RecordUsageError(subsetEvent("scope-a", now.Add(time.Hour)), RequestError{StatusCode: 502})

	entries := mustRequestEvents(t, store, RequestEventQuery{}).Entries
	if len(entries) != 2 || len(repo.requestEvents) != 1 || len(repo.requestErrors) != 1 {
		t.Fatalf("request events = %d, normal writes = %d, error writes = %d", len(entries), len(repo.requestEvents), len(repo.requestErrors))
	}
	if !entries[0].Failed || entries[1].Failed {
		t.Fatalf("request events = %+v", entries)
	}
	errors, err := store.RequestErrors(RequestErrorQuery{})
	if err != nil || len(errors.Entries) != 1 || errors.Entries[0].StatusCode != 502 {
		t.Fatalf("request errors = %+v, err = %v", errors.Entries, err)
	}
	if logs := mustPluginLogs(t, store); len(logs) != 0 {
		t.Fatalf("usage events leaked into plugin logs: %+v", logs)
	}
}

func TestRecordUsageGroupsAndPricesByBillingModel(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store := newAccountStore(t, now)
	store.ReplaceAll(func(state *State) {
		state.Prices["claude/gpt-latest"] = CustomPrice{
			ModelID: "claude/gpt-latest", PriceRates: PriceRates{InputPer1M: 3, OutputPer1M: 4},
		}
	})
	event := subsetEvent("scope-a", now)
	event.RouteModel = "claude/gpt-latest"
	store.RecordUsage(event)

	entries := mustRequestEvents(t, store, RequestEventQuery{}).Entries
	if len(entries) != 1 || entries[0].UpstreamModel != "gpt-5.5" || entries[0].BillingModel != "claude/gpt-latest" {
		t.Fatalf("request events = %+v", entries)
	}
	assertClose(t, "CostUSD", entries[0].Cost.TotalUSD, 0.0015+0.0012+0.0003+0.002)
}

func TestRecordUsagePreservesHostModelAndDurationValues(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store := newAccountStore(t, now)
	event := subsetEvent("scope-a", now)
	event.UpstreamModel = "gpt-5.5(high)"
	event.RouteModel = "gpt-5.5(high)"
	event.Latency = 999 * time.Microsecond
	event.TTFT = 1500 * time.Microsecond
	store.RecordUsage(event)

	entries := mustRequestEvents(t, store, RequestEventQuery{}).Entries
	if len(entries) != 1 {
		t.Fatalf("request events = %+v", entries)
	}
	entry := entries[0]
	if entry.UpstreamModel != "gpt-5.5(high)" || entry.BillingModel != "gpt-5.5" ||
		entry.LatencyMS != 0 || entry.TTFTMS != 1 {
		t.Fatalf("request event = %+v", entry)
	}
}

func TestUsageRequestTimesPreserveWindowAttribution(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store := newAccountStore(t, start)
	store.ReplaceAll(func(state *State) {
		state.Plans = []Plan{{ID: "p", Windows: []QuotaWindow{
			{ID: "short", Name: "短时", AmountUSD: 5, PeriodSeconds: 3600},
			{ID: "long", Name: "预算", AmountUSD: 10, PeriodSeconds: 86400},
		}}}
		state.Keys["scope-a"] = &KeyState{PlanID: "p"}
	})
	store.Authorize("scope-a", start)
	next := store.Authorize("scope-a", start.Add(2*time.Hour))
	event := subsetEvent("scope-a", start.Add(150*time.Minute))
	event.RequestedAt = start
	store.RecordUsage(event)
	store.Read(func(state *State) {
		key := state.Keys["scope-a"]
		if len(key.Cycles) != 2 || key.Cycles["short"].SpentUSD != 0 || key.Cycles["long"].SpentUSD != wantSubsetCost {
			t.Fatalf("late usage attribution: %+v; admission=%+v", key.Cycles, next)
		}
	})
	for _, requestedAt := range []time.Time{time.Time{}, start.Add(48 * time.Hour)} {
		event.RequestedAt = requestedAt
		store.RecordUsage(event)
		store.Read(func(state *State) {
			cycles := state.Keys["scope-a"].Cycles
			if len(cycles) != 2 || cycles["short"].SpentUSD != 0 || cycles["long"].SpentUSD != wantSubsetCost ||
				!cycles["short"].StartAt.Equal(next.Windows[0].StartAt) || !cycles["long"].StartAt.Equal(start) {
				t.Fatalf("request time %s changed current cycles: %+v", requestedAt, cycles)
			}
		})
	}
	if rows := mustRequestEvents(t, store, RequestEventQuery{}).Entries; len(rows) != 3 {
		t.Fatal("usage history lost")
	}
	found := false
	for _, entry := range mustPluginLogs(t, store) {
		found = found || strings.Contains(entry.Message, "请求时间")
	}
	if !found {
		t.Fatal("missing time was not diagnosed")
	}
}

func TestCompletionDoesNotOpenCycleAfterAdministrativeChange(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		change func(*Store) error
		planID string
	}{
		{"reset", func(store *Store) error {
			_, err := store.ResetCycles(ResetRequest{Mode: "all", Scopes: []string{"scope-a"}})
			return err
		}, "daily"},
		{"rebind", func(store *Store) error { return store.BindKey("scope-a", "weekly") }, "weekly"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newAccountStore(t, start)
			store.ReplaceAll(func(state *State) {
				state.Plans = []Plan{
					{ID: "daily", Windows: []QuotaWindow{{ID: "default", Name: "额度", AmountUSD: 5, PeriodSeconds: 86400}}},
					{ID: "weekly", Windows: []QuotaWindow{{ID: "default", Name: "额度", AmountUSD: 5, PeriodSeconds: 604800}}},
				}
				state.Keys["scope-a"] = &KeyState{PlanID: "daily"}
			})
			store.Authorize("scope-a", start)
			if errChange := test.change(store); errChange != nil {
				t.Fatal(errChange)
			}

			event := subsetEvent("scope-a", start.Add(time.Hour))
			event.RequestedAt = start
			store.RecordUsage(event)

			store.Read(func(state *State) {
				key := state.Keys["scope-a"]
				if key.PlanID != test.planID || key.Cycles["default"] != (QuotaCycle{}) {
					t.Fatalf("key = %+v, want plan %q with no active cycle", key, test.planID)
				}
			})
			if len(mustRequestEvents(t, store, RequestEventQuery{}).Entries) != 1 {
				t.Fatal("completion request event was not preserved")
			}
		})
	}
}

func TestReferenceUsageLogsAppliedTierRatesAndBillingModel(t *testing.T) {
	store, _ := newReferencePriceStore(t, 0)
	if _, err := store.ClearPluginLogs(); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(UsageEvent{
		UpstreamModel: "gpt-5.6-sol", RouteModel: "codex/gpt-5.6-sol(xhigh)", At: store.Now(),
		Breakdown: completeBreakdown(300000, 0, 0, 100, 0),
	})
	logs := mustPluginLogs(t, store)
	if len(logs) != 1 || logs[0].Level != PluginLogDebug {
		t.Fatalf("reference usage logs = %+v", logs)
	}
	for _, want := range []string{`billing_model="codex/gpt-5.6-sol"`, "输入=$10", "输出=$45", "费用=$3.00450000"} {
		if !strings.Contains(logs[0].Message, want) {
			t.Fatalf("reference usage log missing %q: %s", want, logs[0].Message)
		}
	}
}
