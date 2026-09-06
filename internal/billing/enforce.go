package billing

import (
	"math"
	"strings"
	"time"
)

type QuotaWindowView struct {
	QuotaWindow
	SpentUSD     float64   `json:"spent_usd"`
	RemainingUSD float64   `json:"remaining_usd"`
	UsedPercent  float64   `json:"used_percent"`
	Started      bool      `json:"started"`
	Blocked      bool      `json:"blocked"`
	StartAt      time.Time `json:"start_at,omitzero"`
	EndAt        time.Time `json:"end_at,omitzero"`
}

type QuotaView struct {
	Unlimited bool              `json:"unlimited"`
	Blocked   bool              `json:"blocked"`
	RetryAt   time.Time         `json:"retry_at,omitzero"`
	Windows   []QuotaWindowView `json:"windows"`
}

type Decision struct {
	Allowed  bool
	PlanID   string
	PlanName string
	QuotaView
}

func quotaView(key *KeyState, plan Plan) QuotaView {
	view := QuotaView{Windows: []QuotaWindowView{}, Unlimited: plan.ID == ""}
	for _, window := range plan.Windows {
		cycle := key.Cycles[window.ID]
		item := QuotaWindowView{
			QuotaWindow:  window,
			SpentUSD:     cycle.SpentUSD,
			RemainingUSD: math.Max(0, window.AmountUSD-cycle.SpentUSD),
			UsedPercent:  math.Min(cycle.SpentUSD, window.AmountUSD) / window.AmountUSD * 100,
			Started:      !cycle.StartAt.IsZero(),
			StartAt:      cycle.StartAt,
			EndAt:        cycle.EndAt,
			Blocked:      cycle.SpentUSD >= window.AmountUSD,
		}
		if item.Blocked {
			view.Blocked = true
			if item.EndAt.After(view.RetryAt) {
				view.RetryAt = item.EndAt
			}
		}
		view.Windows = append(view.Windows, item)
	}
	return view
}

func (s *Store) Authorize(scope string, at time.Time) Decision {
	allowed := Decision{Allowed: true}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return allowed
	}
	if at.IsZero() {
		at = s.Now()
	}
	decision := updateResult(s, func(state *State) (Decision, Changes) {
		key := state.Keys[scope]
		if key == nil || key.PlanID == "" {
			return allowed, Changes{}
		}
		touched := Changes{Keys: []string{scope}}
		plan, ok := state.FindPlan(key.PlanID)
		if !ok {
			key.PlanID, key.Cycles = "", nil
			return allowed, touched
		}
		var changed Changes
		if settleExpiredCycles(key, at) {
			changed = touched
		}
		view := quotaView(key, plan)
		if !view.Blocked && activateCycles(key, plan, at) {
			changed = touched
			view = quotaView(key, plan)
		}
		return Decision{Allowed: !view.Blocked, PlanID: plan.ID, PlanName: plan.Name, QuotaView: view}, changed
	})
	if decision.Allowed {
		s.blocked.clear(scope)
	}
	return decision
}
