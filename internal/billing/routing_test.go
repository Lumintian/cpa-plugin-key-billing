package billing

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRoutesAndDirectBindingsUnionModelsAndCredentialsIndependently(t *testing.T) {
	store := newStore(t)
	refA, refB, refDirect := CredentialFingerprint("auth-a"), CredentialFingerprint("auth-b"), CredentialFingerprint("auth-direct")
	codex := CredentialProviderSelector{Source: CredentialSourceAuthFiles, Provider: "codex"}
	claude := CredentialProviderSelector{Source: CredentialSourceAuthFiles, Provider: "claude"}
	configCodex := CredentialProviderSelector{Source: CredentialSourceAIProviders, Provider: "codex"}
	store.ReplaceAll(func(state *State) {
		state.Routes = []Route{
			{ID: "gpt", Name: "GPT", Rule: RouteRule{Models: []string{"gpt-5.6-sol"}, CredentialIDs: []string{refA}, CredentialProviders: []CredentialProviderSelector{codex}}},
			{ID: "claude", Name: "Claude", Rule: RouteRule{Models: []string{"claude-sonnet-4-6"}, CredentialIDs: []string{refB}, CredentialProviders: []CredentialProviderSelector{claude}}},
		}
		state.Keys["scope-a"] = &KeyState{RouteBindings: RouteBindings{
			RouteIDs: []string{"gpt", "claude"}, Models: []string{"gpt-5.6-luna", "gpt-5.6-sol"},
			CredentialIDs: []string{refA, refDirect}, CredentialProviders: []CredentialProviderSelector{codex, configCodex},
		}}
	})

	wantModels := []string{"claude-sonnet-4-6", "gpt-5.6-luna", "gpt-5.6-sol"}
	wantIDs := []string{refA, refB, refDirect}
	slices.Sort(wantIDs)
	wantProviders := []CredentialProviderSelector{configCodex, claude, codex}
	for _, test := range []struct {
		model string
		allow bool
	}{
		{model: "gpt-5.6-sol", allow: true},
		{model: "claude-sonnet-4-6", allow: true},
		{model: "gpt-5.6-luna", allow: true},
		{model: "unknown-model", allow: false},
		{model: "", allow: true},
	} {
		t.Run(test.model, func(t *testing.T) {
			decision := store.ResolveRouting("scope-a", test.model, test.model)
			if decision.AllowsModel() != test.allow || !slices.Equal(decision.ModelScope, wantModels) {
				t.Fatalf("model policy=%+v", decision)
			}
			if !slices.Equal(decision.CredentialIDs, wantIDs) || !slices.Equal(decision.CredentialProviders, wantProviders) {
				t.Fatalf("credential union changed with request model: %+v", decision)
			}
		})
	}
}

func TestDirectModelKeepsBoundRouteCredentialRestrictions(t *testing.T) {
	ref := CredentialFingerprint("auth-a")
	provider := CredentialProviderSelector{Source: CredentialSourceAuthFiles, Provider: "codex"}
	for _, rule := range []RouteRule{
		{Models: []string{"gpt-5.6-sol", "gpt-5.6-terra"}, CredentialIDs: []string{ref}},
		{Models: []string{"gpt-5.6-sol", "gpt-5.6-terra"}, CredentialProviders: []CredentialProviderSelector{provider}},
	} {
		store := newStore(t)
		store.ReplaceAll(func(state *State) {
			state.Routes = []Route{{ID: "codex", Name: "Codex", Rule: rule}}
			state.Keys["scope-a"] = &KeyState{RouteBindings: RouteBindings{RouteIDs: []string{"codex"}, Models: []string{"gpt-5.6-luna"}}}
		})
		decision := store.ResolveRouting("scope-a", "gpt-5.6-luna", "gpt-5.6-luna")
		if !decision.AllowsModel() || !decision.RestrictsCredentials() || !slices.Equal(decision.CredentialIDs, rule.CredentialIDs) || !slices.Equal(decision.CredentialProviders, rule.CredentialProviders) {
			t.Fatalf("direct model lost bound route credentials: %+v", decision)
		}
	}
}

func TestRoutingEmptyDimensionsRemainIndependent(t *testing.T) {
	ref := CredentialFingerprint("auth-a")
	for _, test := range []struct {
		name        string
		rules       []RouteRule
		models      bool
		credentials bool
	}{
		{name: "unbound"},
		{name: "empty route", rules: []RouteRule{{}}},
		{name: "models only", rules: []RouteRule{{Models: []string{"allowed-model"}}}, models: true},
		{name: "credentials only", rules: []RouteRule{{CredentialIDs: []string{ref}}}, credentials: true},
		{name: "separate routes", rules: []RouteRule{{Models: []string{"allowed-model"}}, {CredentialIDs: []string{ref}}, {}}, models: true, credentials: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newStore(t)
			store.ReplaceAll(func(state *State) {
				key := &KeyState{}
				for i, rule := range test.rules {
					id := "route-" + strconv.Itoa(i)
					state.Routes = append(state.Routes, Route{ID: id, Name: id, Rule: rule})
					key.RouteBindings.RouteIDs = append(key.RouteBindings.RouteIDs, id)
				}
				state.Keys["scope-a"] = key
			})
			for _, model := range []string{"allowed-model", "other-model"} {
				decision := store.ResolveRouting("scope-a", model, model)
				if decision.RestrictsModels() != test.models || decision.RestrictsCredentials() != test.credentials || decision.AllowsModel() != (!test.models || model == "allowed-model") {
					t.Fatalf("independent dimensions: %+v", decision)
				}
			}
		})
	}
}

func TestDirectModelAndCredentialBindingsComposeAcrossPhases(t *testing.T) {
	store := newStore(t)
	ref := CredentialFingerprint("auth-a")
	store.ReplaceAll(func(state *State) {
		state.Keys["scope-a"] = &KeyState{RouteBindings: RouteBindings{Models: []string{"gpt-5.6-sol"}, CredentialIDs: []string{ref}}}
	})

	allowed := store.ResolveRouting("scope-a", "gpt-5.6-sol", "gpt-5.6-sol")
	if !allowed.AllowsModel() || !allowed.RestrictsModels() || !allowed.RestrictsCredentials() || !slices.Equal(allowed.CredentialIDs, []string{ref}) {
		t.Fatalf("allowed decision=%+v", allowed)
	}
	denied := store.ResolveRouting("scope-a", "claude-sonnet-4-6", "claude-sonnet-4-6")
	if denied.AllowsModel() || !denied.RestrictsCredentials() {
		t.Fatalf("denied decision=%+v", denied)
	}
}

func TestRouteWritesReplaceOnlyThatRoutesKeyBindings(t *testing.T) {
	store := newStore(t)
	store.ReplaceAll(func(state *State) {
		state.Keys["a"] = &KeyState{}
		state.Keys["b"] = &KeyState{RouteBindings: RouteBindings{Models: []string{"gpt-5.5"}}}
	})
	route, err := store.CreateRoute(Route{Name: "Codex", Rule: RouteRule{Models: []string{"gpt-5.6-sol"}}}, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	bindings := func(scope string) RouteBindings {
		for _, key := range store.KeyViews() {
			if key.Scope == scope {
				return key.RouteBindings
			}
		}
		return RouteBindings{}
	}
	if !slices.Contains(bindings("a").RouteIDs, route.ID) || !slices.Contains(bindings("b").RouteIDs, route.ID) {
		t.Fatalf("route was not bound at create")
	}
	selected := []string{"a"}
	name := "Codex updated"
	if _, err = store.UpdateRoute(RoutePatch{ID: route.ID, Name: &name}, &selected); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(bindings("a").RouteIDs, route.ID) {
		t.Fatal("selected key lost route binding")
	}
	bBindings := bindings("b")
	if slices.Contains(bBindings.RouteIDs, route.ID) || !slices.Contains(bBindings.Models, "gpt-5.5") {
		t.Fatalf("unselected key bindings=%+v", bBindings)
	}
}

func TestRoutingUsesTheBillingModelIdentity(t *testing.T) {
	store := newStore(t)
	store.ReplaceAll(func(state *State) {
		state.Prices = map[string]CustomPrice{"chat/fast": {ModelID: "chat/fast"}, "chat/slow": {ModelID: "chat/slow"}}
		state.Routes = []Route{{ID: "fast", Name: "Fast", Rule: RouteRule{Models: []string{"CHAT/Fast"}}}}
		state.Keys["scope-a"] = &KeyState{RouteBindings: RouteBindings{RouteIDs: []string{"fast"}}}
	})
	tests := []struct {
		name, upstream, requested string
		want                      bool
	}{
		{name: "exact", upstream: "chat/fast", requested: "chat/fast", want: true},
		{name: "case folded", upstream: "chat/fast", requested: "Chat/Fast", want: true},
		{name: "thinking suffix", upstream: "chat/fast", requested: "chat/fast(high)", want: true},
		{name: "refused suffix", upstream: "chat/slow", requested: "chat/slow(max)", want: false},
		{name: "alias cannot borrow upstream grant", upstream: "chat/fast", requested: "chat/slow", want: false},
		{name: "unpriced route", upstream: "", requested: "chat/fast", want: true},
		{name: "unnamed model", upstream: "", requested: "", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := store.ResolveRouting("scope-a", test.upstream, test.requested)
			if decision.AllowsModel() != test.want {
				t.Fatalf("decision=%+v", decision)
			}
			if !decision.AllowsModel() && strings.Contains(decision.Model, "(") {
				t.Fatalf("refused model kept thinking suffix: %q", decision.Model)
			}
		})
	}
}

func TestRoutingSeparatesConfiguredSuffixedModels(t *testing.T) {
	store := newStore(t)
	store.ReplaceAll(func(state *State) {
		state.Prices = map[string]CustomPrice{"chat/fast": {ModelID: "chat/fast"}, "chat/fast(high)": {ModelID: "chat/fast(high)"}}
		state.Keys["scope-a"] = &KeyState{RouteBindings: RouteBindings{Models: []string{"chat/fast"}}}
	})
	if decision := store.ResolveRouting("scope-a", "chat/fast", "chat/fast(high)"); decision.AllowsModel() {
		t.Fatalf("configured suffixed model inherited base grant: %+v", decision)
	}
	if err := store.SetKeyRoutes("scope-a", RouteBindings{Models: []string{"chat/fast", "chat/fast(high)"}}); err != nil {
		t.Fatal(err)
	}
	if decision := store.ResolveRouting("scope-a", "chat/fast", "chat/fast(high)"); !decision.AllowsModel() {
		t.Fatalf("explicit suffixed grant was denied: %+v", decision)
	}
}

func TestMissingRouteFailsClosed(t *testing.T) {
	store := newStore(t)
	store.ReplaceAll(func(state *State) {
		state.Keys["scope-a"] = &KeyState{RouteBindings: RouteBindings{RouteIDs: []string{"missing"}}}
	})
	decision := store.ResolveRouting("scope-a", "gpt-5.6", "gpt-5.6")
	if decision.AllowsModel() || decision.ConfigurationError == "" {
		t.Fatalf("decision=%+v", decision)
	}
}

func TestCredentialProviderSourceIsPartOfIdentity(t *testing.T) {
	rule, err := NormalizeRouteRule(RouteRule{CredentialProviders: []CredentialProviderSelector{{Source: "auth-files", Provider: "codex"}, {Source: "ai-providers", Provider: "codex"}}})
	if err != nil || len(rule.CredentialProviders) != 2 {
		t.Fatalf("rule=%+v err=%v", rule, err)
	}
	if _, err := NormalizeRouteRule(RouteRule{CredentialProviders: []CredentialProviderSelector{{Provider: "codex"}}}); KindOf(err) != KindInvalid {
		t.Fatalf("missing source error=%v", err)
	}
}

func TestDirectCredentialProviderBindingRestrictsOneSource(t *testing.T) {
	store := newStore(t)
	provider := CredentialProviderSelector{Source: CredentialSourceAuthFiles, Provider: "Codex"}
	store.ReplaceAll(func(state *State) {
		state.Keys["scope-a"] = &KeyState{}
	})
	if err := store.SetKeyRoutes("scope-a", RouteBindings{CredentialProviders: []CredentialProviderSelector{provider}}); err != nil {
		t.Fatal(err)
	}
	decision := store.ResolveRouting("scope-a", "gpt-5.6-sol", "gpt-5.6-sol")
	if !decision.AllowsModel() || !decision.RestrictsCredentials() || len(decision.CredentialProviders) != 1 {
		t.Fatalf("decision=%+v", decision)
	}
	allowed := decision.CredentialProviders[0]
	if allowed.Source != CredentialSourceAuthFiles || allowed.Provider != "codex" {
		t.Fatalf("provider=%+v", allowed)
	}
}

func TestDeleteRouteCascadesBindingsAndReportsWidening(t *testing.T) {
	store := newStore(t)
	store.ReplaceAll(func(state *State) {
		state.Routes = []Route{{ID: "only", Name: "Only", Rule: RouteRule{Models: []string{"gpt"}, CredentialProviders: []CredentialProviderSelector{{Source: CredentialSourceAIProviders, Provider: "xai"}}}}}
		state.Keys["scope-a"] = &KeyState{RouteBindings: RouteBindings{RouteIDs: []string{"only"}}}
		state.Keys["scope-deleted"] = &KeyState{DeletedAt: time.Now(), RouteBindings: RouteBindings{RouteIDs: []string{"only"}}}
	})
	views := store.RouteViews()
	if len(views) != 1 || views[0].BoundKeyCount != 2 || views[0].DeletedKeyCount != 1 {
		t.Fatalf("views=%+v", views)
	}
	selected := []string{"scope-a", "scope-deleted"}
	name := "Renamed"
	if _, err := store.UpdateRoute(RoutePatch{ID: "only", Name: &name}, &selected); err != nil {
		t.Fatal(err)
	}
	store.Read(func(state *State) {
		if len(state.Keys["scope-deleted"].RouteBindings.RouteIDs) != 1 {
			t.Fatal("editing lost a selected binding")
		}
	})

	result, err := store.DeleteRoute("only")
	if err != nil || result.AffectedKeys != 2 || result.DeletedKeys != 1 || result.FullyUnrestrictedKeys != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	store.Read(func(state *State) {
		if len(state.Keys["scope-deleted"].RouteBindings.RouteIDs) != 0 {
			t.Fatalf("deleted key retained route binding: %+v", state.Keys["scope-deleted"].RouteBindings)
		}
	})
}

func TestRouteEditCanUnbindDeletedKeys(t *testing.T) {
	store := newStore(t)
	store.ReplaceAll(func(state *State) {
		state.Routes = []Route{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}
		state.Keys["deleted"] = &KeyState{Preview: "sk-tes…0001", DeletedAt: time.Now(),
			RouteBindings: RouteBindings{RouteIDs: []string{"a", "b"}, Models: []string{"gpt-5.5"}}}
	})
	name := "Renamed"
	if _, err := store.UpdateRoute(RoutePatch{ID: "a", Name: &name}, nil); err != nil {
		t.Fatal(err)
	}
	selected := []string{"deleted"}
	if _, err := store.UpdateRoute(RoutePatch{ID: "a"}, &selected); err != nil {
		t.Fatal(err)
	}
	selected = []string{}
	if _, err := store.UpdateRoute(RoutePatch{ID: "a"}, &selected); err != nil {
		t.Fatal(err)
	}
	store.Read(func(state *State) {
		key := state.Keys["deleted"]
		if len(key.RouteBindings.RouteIDs) != 1 || key.RouteBindings.RouteIDs[0] != "b" ||
			len(key.RouteBindings.Models) != 1 || key.DeletedAt.IsZero() {
			t.Fatalf("unbinding changed unrelated settings: %+v", key)
		}
	})
	selected = []string{"deleted"}
	if _, err := store.UpdateRoute(RoutePatch{ID: "a"}, &selected); err == nil {
		t.Fatal("rebound a deleted key through route editing")
	}
}
