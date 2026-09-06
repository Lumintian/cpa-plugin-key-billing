package billing

import (
	"testing"
	"time"
)

func TestIndependentQuotaWindows(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store := newAccountStore(t, now)
	store.ReplaceAll(func(state *State) {
		state.Plans = []Plan{{ID: "p", Windows: []QuotaWindow{
			{ID: "short", Name: "短时", AmountUSD: wantSubsetCost, PeriodSeconds: 3600},
			{ID: "long", Name: "预算", AmountUSD: wantSubsetCost, PeriodSeconds: 7200},
		}}}
		state.Keys["s"] = &KeyState{PlanID: "p"}
	})
	for _, scope := range []string{"", "unknown"} {
		if !store.Authorize(scope, now).Allowed {
			t.Fatal("unknown key blocked")
		}
	}
	view, _ := store.KeyViewForScope("s")
	if view.Windows[0].Started || view.Windows[1].Started {
		t.Fatal("reading quota started cycles")
	}
	first := store.Authorize("s", now)
	if !first.Allowed || !first.Windows[0].StartAt.Equal(first.Windows[1].StartAt) {
		t.Fatalf("first admission: %+v", first)
	}
	store.RecordUsage(subsetEvent("s", now))
	blocked := store.Authorize("s", now)
	if blocked.Allowed || !blocked.RetryAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("blocked = %+v", blocked)
	}
	for _, d := range blocked.Windows {
		assertClose(t, "window cost", d.SpentUSD, wantSubsetCost)
	}
	if rows := mustRequestEvents(t, store, RequestEventQuery{}).Entries; len(rows) != 1 || rows[0].Cost.TotalUSD != wantSubsetCost {
		t.Fatalf("duplicated history: %+v", rows)
	}
	store.now = func() time.Time { return now.Add(time.Hour) }
	views := store.KeyViews()
	if len(views) != 1 || views[0].Windows[0].Started || !views[0].Windows[1].Blocked {
		t.Fatalf("reading quota after expiration: %+v", views)
	}
	blocked = store.Authorize("s", now.Add(time.Hour))
	if blocked.Allowed || blocked.Windows[0].Started || !blocked.Windows[1].Blocked {
		t.Fatalf("expired short window restarted while blocked: %+v", blocked)
	}
	next := store.Authorize("s", now.Add(10*time.Hour))
	if !next.Allowed || !next.Windows[0].StartAt.Equal(now.Add(10*time.Hour)) || next.Windows[1].SpentUSD != 0 {
		t.Fatalf("idle restart: %+v", next)
	}
}
