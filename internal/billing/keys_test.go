package billing

import (
	"testing"
	"time"
)

func TestKeyViewsSettleExpiredCycleWithoutRestartingIt(t *testing.T) {
	now := time.Date(2026, 8, 8, 7, 0, 0, 0, time.UTC)
	store := newEnforceStore(t, now)
	store.ReplaceAll(func(state *State) {
		state.Plans = []Plan{{ID: "p", AmountUSD: 10, PeriodSeconds: 86400}}
		state.Keys["a"] = &KeyState{PlanID: "p", Cycle: Cycle{
			PlanID: "p", StartAt: now.Add(-48 * time.Hour), EndAt: now.Add(-24 * time.Hour), SpentUSD: 2,
		}}
	})

	views := store.KeyViews()
	if len(views) != 1 || !views[0].CycleEndAt.IsZero() {
		t.Fatalf("views = %+v, want inactive cycle", views)
	}
	store.Read(func(state *State) {
		key := state.Keys["a"]
		if key.Cycle != (Cycle{}) {
			t.Fatalf("key = %+v", key)
		}
	})
}

func TestPlanBindingTransactions(t *testing.T) {
	now := time.Date(2026, 8, 8, 7, 0, 0, 0, time.UTC)
	store := newEnforceStore(t, now)
	store.ReplaceAll(func(state *State) {
		state.Keys["a"] = &KeyState{}
		state.Keys["b"] = &KeyState{}
		state.Keys["owned"] = &KeyState{PlanID: "other"}
		state.Plans = []Plan{{ID: "other", AmountUSD: 1}}
	})

	created, err := store.CreatePlanWithBindings(Plan{ID: "p", AmountUSD: 5, PeriodSeconds: 86400}, []string{"a"})
	if err != nil || created.ID != "p" {
		t.Fatalf("CreatePlanWithBindings = %+v, %v", created, err)
	}
	selected := []string{"b"}
	if _, err = store.UpdatePlanWithBindings(PlanPatch{ID: "p"}, &selected); err != nil {
		t.Fatalf("UpdatePlanWithBindings error = %v", err)
	}
	store.Read(func(state *State) {
		if state.Keys["a"].PlanID != "" || state.Keys["b"].PlanID != "p" || state.Keys["b"].Cycle != (Cycle{}) {
			t.Fatalf("keys = %+v", state.Keys)
		}
	})

	rejected := []string{"b", "owned"}
	if _, err = store.UpdatePlanWithBindings(PlanPatch{ID: "p"}, &rejected); err == nil {
		t.Fatal("stealing a key from another plan was accepted")
	}
	store.Read(func(state *State) {
		if state.Keys["b"].PlanID != "p" || state.Keys["owned"].PlanID != "other" {
			t.Fatalf("rejected update was not atomic: %+v", state.Keys)
		}
	})
}

const (
	keptKeyPlaintext    = "sk-live-0123456789"
	deletedKeyPlaintext = "sk-deleted-0123456789"
)

// newSyncStore returns a store holding one plan and both keys above, already
// synchronized and bound, with a clock the caller can move.
func newSyncStore(t *testing.T, clock *time.Time) *Store {
	t.Helper()
	store := newAccountStore(t, *clock)
	store.now = func() time.Time { return *clock }
	store.ReplaceAll(func(state *State) {
		state.Plans = []Plan{{ID: "p", Name: "Weekly", AmountUSD: 10, PeriodSeconds: 604800}}
	})
	if _, errSync := store.SyncKeys([]string{keptKeyPlaintext, deletedKeyPlaintext}, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
	if errBind := store.BindKey(CallerScope(deletedKeyPlaintext), "p"); errBind != nil {
		t.Fatalf("BindKey error = %v", errBind)
	}
	return store
}

func TestDeletedKeyRetainsUsageAndIdentity(t *testing.T) {
	now := time.Date(2026, 8, 12, 15, 57, 0, 0, time.UTC)
	clock := now
	store := newSyncStore(t, &clock)
	scope := CallerScope(deletedKeyPlaintext)
	event := admittedEvent(store, scope, now)

	clock = now.Add(43 * time.Minute)
	if _, errSync := store.SyncKeys([]string{keptKeyPlaintext}, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
	clock = now.Add(time.Hour)
	store.RecordUsage(event)

	store.Read(func(state *State) {
		key := state.Keys[scope]
		if key.Preview != PreviewKey(deletedKeyPlaintext) {
			t.Fatalf("key = %+v, want the deleted record reused, not a bare scope", key)
		}
		if key.DeletedAt.IsZero() || key.InConfig {
			t.Fatalf("key = %+v, want it to stay deleted", key)
		}
		// The window was live at admission, so the spend belongs to it.
		if key.Cycle.SpentUSD == 0 {
			t.Fatalf("Cycle = %+v, want the admitted window charged", key.Cycle)
		}
	})
	if rows := mustRequestEvents(t, store, RequestEventQuery{}).Entries; len(rows) != 1 || rows[0].Preview != PreviewKey(deletedKeyPlaintext) {
		t.Fatalf("deleted key lost its historical identity: %+v", rows)
	}
	clock = now.Add(RequestEventRetention + time.Hour)
	if _, err := store.SyncKeys([]string{keptKeyPlaintext}, false); err != nil {
		t.Fatal(err)
	}
	if rows := mustRequestEvents(t, store, RequestEventQuery{}).Entries; len(rows) != 0 {
		t.Fatal("expired events remained visible")
	}
	key := store.state.Keys[scope]
	if key == nil || key.PlanID != "p" || key.Preview != PreviewKey(deletedKeyPlaintext) || key.DeletedAt.IsZero() {
		t.Fatalf("history expiry changed the deleted key: %+v", key)
	}
}

func TestSyncKeysRestoresQuotaAndBindings(t *testing.T) {
	for _, period := range []int64{0, 3600} {
		t.Run(time.Duration(period*int64(time.Second)).String(), func(t *testing.T) {
			now := time.Date(2026, 8, 12, 15, 57, 0, 0, time.UTC)
			store := newSyncStore(t, &now)
			scope := CallerScope(deletedKeyPlaintext)
			store.ReplaceAll(func(state *State) { state.Plans[0].PeriodSeconds = period })
			store.RecordUsage(admittedEvent(store, scope, now))
			store.ReplaceAll(func(state *State) { state.Keys[scope].Cycle.SpentUSD = 10 })
			cycle := store.state.Keys[scope].Cycle
			if _, err := store.SyncKeys([]string{keptKeyPlaintext}, false); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			if _, err := store.SyncKeys([]string{keptKeyPlaintext, deletedKeyPlaintext}, false); err != nil {
				t.Fatal(err)
			}
			key := store.state.Keys[scope]
			if !key.InConfig || !key.DeletedAt.IsZero() || key.PlanID != "p" || key.Cycle != cycle {
				t.Fatalf("restored key = %+v, want original binding and cycle %+v", key, cycle)
			}
			if store.Authorize(scope, now).Allowed {
				t.Fatal("restoring a key replenished its quota")
			}
			if len(mustRequestEvents(t, store, RequestEventQuery{}).Entries) != 1 {
				t.Fatal("request history was lost")
			}
			now = now.Add(time.Hour)
			if allowed := store.Authorize(scope, now).Allowed; allowed != (period != 0) {
				t.Fatalf("allowed after an hour = %v for period %d", allowed, period)
			}
		})
	}
}

func TestSyncKeysMarksDeletedOnlyOnceAndSparesTrafficOnlyPrincipals(t *testing.T) {
	now := time.Date(2026, 8, 12, 15, 57, 0, 0, time.UTC)
	clock := now
	store := newSyncStore(t, &clock)
	// A principal that no sync ever listed may belong to another access
	// provider, so a CPA key-list sync has no authority over it.
	store.RecordUsage(subsetEvent("foreign-principal", now))

	if _, errSync := store.SyncKeys([]string{keptKeyPlaintext}, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
	result, errSync := store.SyncKeys([]string{keptKeyPlaintext}, false)
	if errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
	if result.Deleted != 0 {
		t.Fatalf("SyncResult = %+v, want an already deleted key counted once", result)
	}
	store.Read(func(state *State) {
		foreign := state.Keys["foreign-principal"]
		if foreign == nil || !foreign.DeletedAt.IsZero() || foreign.InConfig {
			t.Fatalf("foreign principal = %+v, want it untouched", foreign)
		}
	})
}

func TestPlanEditCanKeepOrUnbindDeletedKeys(t *testing.T) {
	now := time.Date(2026, 8, 12, 15, 57, 0, 0, time.UTC)
	clock := now
	store := newSyncStore(t, &clock)
	scope := CallerScope(deletedKeyPlaintext)
	if _, errSync := store.SyncKeys([]string{keptKeyPlaintext}, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}

	selected := []string{CallerScope(keptKeyPlaintext), scope}
	if _, errUpdate := store.UpdatePlanWithBindings(PlanPatch{ID: "p"}, &selected); errUpdate != nil {
		t.Fatalf("UpdatePlanWithBindings error = %v", errUpdate)
	}
	store.Read(func(state *State) {
		if state.Keys[scope].PlanID != "p" {
			t.Fatalf("deleted key = %+v, want its binding kept", state.Keys[scope])
		}
	})

	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p"}, nil); err != nil {
		t.Fatal(err)
	}
	store.Read(func(state *State) {
		if state.Keys[scope].PlanID != "p" {
			t.Fatal("omitting scopes changed a binding")
		}
	})
	selected = []string{CallerScope(keptKeyPlaintext)}
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p"}, &selected); err != nil {
		t.Fatal(err)
	}
	store.Read(func(state *State) {
		key := state.Keys[scope]
		if key.PlanID != "" || key.Cycle != (Cycle{}) || key.DeletedAt.IsZero() {
			t.Fatalf("deleted key was not unbound: %+v", key)
		}
	})
	selected = append(selected, scope)
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p"}, &selected); err == nil {
		t.Fatal("rebound a deleted key through plan editing")
	}

	if errBind := store.BindKey(scope, "p"); errBind == nil {
		t.Fatal("a deleted key was bindable through the management API")
	}
	if err := store.SetLabel(scope, "Historical key"); err != nil {
		t.Fatal(err)
	}
	views := store.KeyViews()
	if len(views) != 2 {
		t.Fatalf("missing retained identity: %+v", views)
	}
	for _, key := range views {
		if key.Scope == scope && (key.Label != "Historical key" || key.DeletedAt.IsZero()) {
			t.Fatalf("deleted key lost its display identity: %+v", key)
		}
	}
}
