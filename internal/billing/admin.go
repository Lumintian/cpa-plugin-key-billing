package billing

import (
	"fmt"
	"slices"
	"strings"
)

func (s *Store) Plans() []Plan {
	plans := []Plan{}
	s.read(func(state *State) { plans = append(plans, state.Plans...) })
	return plans
}

// CreatePlanWithBindings creates a plan and binds the selected currently
// unbound keys in the same state transaction.
func (s *Store) CreatePlanWithBindings(plan Plan, scopes []string) (Plan, error) {
	plan.ID = strings.TrimSpace(plan.ID)
	plan.Name = strings.TrimSpace(plan.Name)
	scopes = normalizeScopes(scopes)
	var errApply error
	stored := updateResult(s, func(state *State) (Plan, Changes) {
		if plan.ID == "" {
			plan.ID = state.freePlanID(plan.Name)
		}
		if errValidate := plan.Validate(); errValidate != nil {
			errApply = errValidate
			return Plan{}, Changes{}
		}
		if _, exists := state.FindPlan(plan.ID); exists {
			errApply = conflictf("订阅计划 %q 已存在", plan.ID)
			return Plan{}, Changes{}
		}
		if plan.Name == "" {
			plan.Name = plan.ID
		}
		for _, scope := range scopes {
			key := state.liveKey(scope)
			if key == nil {
				errApply = notFoundf("API Key %q 不存在", scope)
				return Plan{}, Changes{}
			}
			if key.PlanID != "" {
				errApply = conflictf("API Key %q 已绑定其他订阅计划", scope)
				return Plan{}, Changes{}
			}
		}
		state.Plans = append(state.Plans, plan)
		for _, scope := range scopes {
			state.Keys[scope].PlanID = plan.ID
			state.Keys[scope].Cycle = Cycle{}
		}
		return plan, Changes{Plans: true, Keys: scopes}
	})
	return stored, errApply
}

type PlanPatch struct {
	ID            string   `json:"id"`
	Name          *string  `json:"name,omitempty"`
	AmountUSD     *float64 `json:"amount_usd,omitempty"`
	PeriodSeconds *int64   `json:"period_seconds,omitempty"`
}

// UpdatePlanWithBindings applies a plan edit and, when scopes is non-nil,
// replaces the plan's complete key set. Selected keys may be unbound or already
// on this plan; keys owned by another plan are rejected atomically.
func (s *Store) UpdatePlanWithBindings(patch PlanPatch, scopes *[]string) (Plan, error) {
	patch.ID = strings.TrimSpace(patch.ID)
	if patch.ID == "" {
		return Plan{}, invalidf("订阅计划 ID 不能为空")
	}

	var errApply error
	stored := updateResult(s, func(state *State) (Plan, Changes) {
		for i := range state.Plans {
			if state.Plans[i].ID != patch.ID {
				continue
			}
			updated := state.Plans[i]
			if patch.Name != nil {
				updated.Name = strings.TrimSpace(*patch.Name)
			}
			if patch.AmountUSD != nil {
				updated.AmountUSD = *patch.AmountUSD
			}
			if patch.PeriodSeconds != nil {
				updated.PeriodSeconds = *patch.PeriodSeconds
			}
			if errValidate := updated.Validate(); errValidate != nil {
				errApply = errValidate
				return Plan{}, Changes{}
			}
			var selected map[string]struct{}
			if scopes != nil {
				normalized := normalizeScopes(*scopes)
				selected = make(map[string]struct{}, len(normalized))
				for _, scope := range normalized {
					key := state.liveKey(scope)
					if key == nil {
						errApply = notFoundf("API Key %q 不存在", scope)
						return Plan{}, Changes{}
					}
					if key.PlanID != "" && key.PlanID != patch.ID {
						errApply = conflictf("API Key %q 已绑定其他订阅计划", scope)
						return Plan{}, Changes{}
					}
					selected[scope] = struct{}{}
				}
			}

			periodChanged := updated.PeriodSeconds != state.Plans[i].PeriodSeconds
			if periodChanged || scopes != nil {
				for scope, key := range state.Keys {
					// A deleted key is absent from the editor, so its absence
					// from the selection says nothing. Unbinding it here would
					// throw away the binding kept for a later re-add.
					if key == nil || !key.DeletedAt.IsZero() {
						continue
					}
					_, shouldBind := selected[scope]
					if key.PlanID == patch.ID && (periodChanged || !shouldBind) {
						key.Cycle = Cycle{}
					}
					if scopes != nil && key.PlanID == patch.ID && !shouldBind {
						key.PlanID = ""
					}
				}
			}
			if scopes != nil {
				for scope := range selected {
					key := state.Keys[scope]
					if key.PlanID == "" {
						key.PlanID = patch.ID
						key.Cycle = Cycle{}
					}
				}
			}
			state.Plans[i] = updated
			return updated, Changes{Plans: true, AllKeys: true}
		}
		errApply = notFoundf("订阅计划 %q 不存在", patch.ID)
		return Plan{}, Changes{}
	})
	return stored, errApply
}

func (s *Store) DeletePlan(id string) (int, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, invalidf("订阅计划 ID 不能为空")
	}

	var errApply error
	unbound := updateResult(s, func(state *State) (int, Changes) {
		index := slices.IndexFunc(state.Plans, func(plan Plan) bool { return plan.ID == id })
		if index < 0 {
			errApply = notFoundf("订阅计划 %q 不存在", id)
			return 0, Changes{}
		}
		state.Plans = slices.Delete(state.Plans, index, index+1)

		released := 0
		for _, key := range state.Keys {
			if key == nil || key.PlanID != id {
				continue
			}
			key.PlanID = ""
			key.Cycle = Cycle{}
			released++
		}
		return released, Changes{Plans: true, AllKeys: true}
	})
	return unbound, errApply
}

func (s *State) freePlanID(name string) string {
	return freeID(name, "plan", func(id string) bool {
		_, exists := s.FindPlan(id)
		return exists
	})
}

// freeID turns a display name into an identifier nothing else answers to.
func freeID(name, prefix string, taken func(string) bool) string {
	// A slug with no letters is not a readable identifier: "日额度 0.003" would
	// otherwise become "0-003". The counter below reads better.
	if base := slugify(name); strings.ContainsAny(base, "abcdefghijklmnopqrstuvwxyz") {
		if !taken(base) {
			return base
		}
		for i := 2; i < 1000; i++ {
			if candidate := fmt.Sprintf("%s-%d", base, i); !taken(candidate) {
				return candidate
			}
		}
	}
	for i := 1; ; i++ {
		if candidate := fmt.Sprintf("%s-%d", prefix, i); !taken(candidate) {
			return candidate
		}
	}
}

func slugify(value string) string {
	var builder strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && builder.Len() > 0 {
				builder.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	return slug
}
