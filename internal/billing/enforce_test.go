package billing

import (
	"strconv"
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
		used, _ := d.Dimensions[0].Used.Float64()
		assertClose(t, "window cost", used, wantSubsetCost)
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
	if !next.Allowed || !next.Windows[0].StartAt.Equal(now.Add(10*time.Hour)) || next.Windows[1].Dimensions[0].Used != "0" {
		t.Fatalf("idle restart: %+v", next)
	}
}

func TestQuotaWindowsWithDifferentDimensions(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	store := newAccountStore(t, now)
	plan := Plan{ID: "p", Windows: []QuotaWindow{
		{ID: "requests", Name: "请求", RequestLimit: 1, PeriodSeconds: 3600},
		{ID: "tokens", Name: "Token", TokenLimit: 3000, PeriodSeconds: 7200},
		{ID: "amount", Name: "金额", AmountUSD: 4 * wantSubsetCost, PeriodSeconds: 86400},
	}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	store.ReplaceAll(func(state *State) {
		state.Plans = []Plan{plan}
		state.Keys["s"] = &KeyState{PlanID: plan.ID}
	})
	for hour := range 4 {
		at := now.Add(time.Duration(hour) * time.Hour)
		store.now = func() time.Time { return at }
		before := store.Authorize("s", at)
		if !before.Allowed || before.Windows[0].Dimensions[0].Used != "0" || before.Windows[1].Dimensions[0].Used.String() != strconv.Itoa((hour%2)*1500) {
			t.Fatalf("hour %d: windows did not reset independently: %+v", hour, before)
		}
		spent, _ := before.Windows[2].Dimensions[0].Used.Float64()
		assertClose(t, "amount carried across shorter windows", spent, float64(hour)*wantSubsetCost)
		store.RecordUsage(subsetEvent("s", at))
		after := store.Authorize("s", at)
		if after.Allowed || !after.Windows[0].Blocked || after.Windows[1].Blocked != (hour%2 == 1) || after.Windows[2].Blocked != (hour == 3) {
			t.Fatalf("hour %d: dimension enforcement interfered: %+v", hour, after)
		}
		reset := at.Add(time.Hour)
		if hour == 3 {
			reset = now.Add(24 * time.Hour)
		}
		if !after.RetryAt.Equal(reset) {
			t.Fatalf("hour %d: retry at %v, want %v", hour, after.RetryAt, reset)
		}
	}
	store.now = func() time.Time { return now.Add(4 * time.Hour) }
	blocked := store.Authorize("s", store.Now())
	if blocked.Allowed || blocked.Windows[0].Started || blocked.Windows[1].Started || !blocked.Windows[2].Blocked {
		t.Fatalf("short window reset bypassed the amount limit: %+v", blocked)
	}
	store.now = func() time.Time { return now.Add(24 * time.Hour) }
	restored := store.Authorize("s", store.Now())
	if !restored.Allowed || restored.Windows[0].Dimensions[0].Used != "0" || restored.Windows[1].Dimensions[0].Used != "0" || restored.Windows[2].Dimensions[0].Used != "0" {
		t.Fatalf("windows did not recover after all exhausted quotas reset: %+v", restored)
	}
}
