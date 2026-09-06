package billing

import (
	"fmt"
	"slices"
	"strings"
)

func (s *Store) Plans() []Plan {
	plans := []Plan{}
	s.read(func(state *State) {
		for _, plan := range state.Plans {
			plans = append(plans, clonePlan(plan))
		}
	})
	return plans
}

// CreatePlanWithBindings creates a plan and binds the selected currently
// unbound keys in the same state transaction.
func (s *Store) CreatePlanWithBindings(plan Plan, scopes []string) (Plan, error) {
	plan.ID = strings.TrimSpace(plan.ID)
	plan.Name = strings.TrimSpace(plan.Name)
	scopes = normalizeScopes(scopes)
	return editConfiguration(s, func(state *State) (Plan, Changes, error) {
		if plan.ID == "" {
			plan.ID = state.freePlanID(plan.Name)
		}
		windows, err := prepareWindows(plan.Windows, nil)
		if err != nil {
			return Plan{}, Changes{}, err
		}
		plan.Windows = windows
		if errValidate := plan.Validate(); errValidate != nil {
			return Plan{}, Changes{}, errValidate
		}
		if _, exists := state.FindPlan(plan.ID); exists {
			return Plan{}, Changes{}, conflictf("订阅计划 %q 已存在", plan.ID)
		}
		if plan.Name == "" {
			plan.Name = plan.ID
		}
		for _, scope := range scopes {
			key := state.liveKey(scope)
			if key == nil {
				return Plan{}, Changes{}, notFoundf("API Key %q 不存在", scope)
			}
			if key.PlanID != "" {
				return Plan{}, Changes{}, conflictf("API Key %q 已绑定其他订阅计划", scope)
			}
		}
		state.Plans = append(state.Plans, plan)
		for _, scope := range scopes {
			state.Keys[scope].PlanID = plan.ID
			state.Keys[scope].Cycles = nil
		}
		return clonePlan(plan), Changes{Plans: true, Keys: scopes}, nil
	})
}

type PlanPatch struct {
	ID      string         `json:"id"`
	Name    *string        `json:"name,omitempty"`
	Windows *[]QuotaWindow `json:"windows,omitempty"`
}

// UpdatePlanWithBindings applies a plan edit and, when scopes is non-nil,
// replaces the plan's complete key set. Selected keys may be unbound or already
// on this plan; keys owned by another plan are rejected atomically.
func (s *Store) UpdatePlanWithBindings(patch PlanPatch, scopes *[]string) (Plan, error) {
	patch.ID = strings.TrimSpace(patch.ID)
	if patch.ID == "" {
		return Plan{}, invalidf("订阅计划 ID 不能为空")
	}

	return editConfiguration(s, func(state *State) (Plan, Changes, error) {
		for i := range state.Plans {
			if state.Plans[i].ID != patch.ID {
				continue
			}
			updated := state.Plans[i]
			if patch.Name != nil {
				updated.Name = strings.TrimSpace(*patch.Name)
			}
			if patch.Windows != nil {
				windows, err := prepareWindows(*patch.Windows, updated.Windows)
				if err != nil {
					return Plan{}, Changes{}, err
				}
				updated.Windows = windows
			}
			if errValidate := updated.Validate(); errValidate != nil {
				return Plan{}, Changes{}, errValidate
			}
			var selected map[string]struct{}
			if scopes != nil {
				normalized := normalizeScopes(*scopes)
				selected = make(map[string]struct{}, len(normalized))
				for _, scope := range normalized {
					key := state.Keys[scope]
					if key == nil || !key.DeletedAt.IsZero() && key.PlanID != patch.ID {
						return Plan{}, Changes{}, notFoundf("API Key %q 不存在", scope)
					}
					if key.PlanID != "" && key.PlanID != patch.ID {
						return Plan{}, Changes{}, conflictf("API Key %q 已绑定其他订阅计划", scope)
					}
					selected[scope] = struct{}{}
				}
			}

			var resetWindows []string
			if patch.Windows != nil {
				for _, old := range state.Plans[i].Windows {
					if !slices.ContainsFunc(updated.Windows, func(window QuotaWindow) bool {
						return window.ID == old.ID && window.PeriodSeconds == old.PeriodSeconds
					}) {
						resetWindows = append(resetWindows, old.ID)
					}
				}
			}

			for scope, key := range state.Keys {
				if key == nil || key.PlanID != patch.ID {
					continue
				}
				_, shouldBind := selected[scope]
				if scopes != nil && !shouldBind {
					key.PlanID, key.Cycles = "", nil
					continue
				}
				for _, id := range resetWindows {
					delete(key.Cycles, id)
				}
			}
			if scopes != nil {
				for scope := range selected {
					key := state.Keys[scope]
					if key.PlanID == "" {
						key.PlanID = patch.ID
						key.Cycles = nil
					}
				}
			}
			state.Plans[i] = updated
			return clonePlan(updated), Changes{Plans: true, AllKeys: true}, nil
		}
		return Plan{}, Changes{}, notFoundf("订阅计划 %q 不存在", patch.ID)
	})
}

func (s *Store) DeletePlan(id string) (int, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, invalidf("订阅计划 ID 不能为空")
	}

	return editConfiguration(s, func(state *State) (int, Changes, error) {
		index := slices.IndexFunc(state.Plans, func(plan Plan) bool { return plan.ID == id })
		if index < 0 {
			return 0, Changes{}, notFoundf("订阅计划 %q 不存在", id)
		}
		state.Plans = slices.Delete(state.Plans, index, index+1)

		released := 0
		for _, key := range state.Keys {
			if key == nil || key.PlanID != id {
				continue
			}
			key.PlanID = ""
			key.Cycles = nil
			released++
		}
		return released, Changes{Plans: true, AllKeys: true}, nil
	})
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
