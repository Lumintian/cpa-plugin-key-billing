package billing

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"math"
	"slices"
	"strings"
	"time"
)

const maxPeriodSeconds = int64(math.MaxInt64) / int64(time.Second)

func (p Plan) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return invalidf("订阅计划 ID 不能为空")
	}
	if len(p.Windows) == 0 {
		return invalidf("订阅计划至少需要一个额度窗口")
	}
	ids := make(map[string]bool)
	names := make(map[string]bool)
	periods := make(map[int64]bool)
	for _, window := range p.Windows {
		if window.ID == "" || ids[window.ID] {
			return invalidf("额度窗口 ID 无效或重复")
		}
		name := strings.TrimSpace(window.Name)
		if name == "" || len(name) > maxRouteNameBytes {
			return invalidf("窗口名称不能为空且不能超过 %d 字节", maxRouteNameBytes)
		}
		if names[strings.ToLower(name)] {
			return invalidf("窗口名称 %q 重复", name)
		}
		if !(window.AmountUSD > 0) || math.IsInf(window.AmountUSD, 0) {
			return invalidf("窗口 %q：额度必须为有限正数", name)
		}
		if window.PeriodSeconds <= 0 || window.PeriodSeconds > maxPeriodSeconds {
			return invalidf("窗口 %q：周期必须为 1 到 %d 秒", name, maxPeriodSeconds)
		}
		if periods[window.PeriodSeconds] {
			return invalidf("窗口 %q 的周期与其他窗口重复", name)
		}
		ids[window.ID], names[strings.ToLower(name)], periods[window.PeriodSeconds] = true, true, true
	}
	return nil
}

func prepareWindows(windows, existing []QuotaWindow) ([]QuotaWindow, error) {
	windows = slices.Clone(windows)
	for i := range windows {
		window := &windows[i]
		window.Name = strings.TrimSpace(window.Name)
		if window.ID == "" {
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return nil, err
			}
			window.ID = hex.EncodeToString(id[:])
		} else if !slices.ContainsFunc(existing, func(old QuotaWindow) bool { return old.ID == window.ID }) {
			return nil, invalidf("额度窗口 %q 已不存在，请刷新后重试", window.Name)
		}
	}
	slices.SortFunc(windows, func(a, b QuotaWindow) int {
		return cmp.Compare(a.PeriodSeconds, b.PeriodSeconds)
	})
	return windows, nil
}

func clonePlan(plan Plan) Plan {
	plan.Windows = slices.Clone(plan.Windows)
	return plan
}

func (s *State) FindPlan(id string) (Plan, bool) {
	id = strings.TrimSpace(id)
	for _, plan := range s.Plans {
		if plan.ID == id && id != "" {
			return plan, true
		}
	}
	return Plan{}, false
}

// Expiration never starts another window; only admission can do that.
func settleExpiredCycles(key *KeyState, now time.Time) bool {
	changed := false
	for id, cycle := range key.Cycles {
		if !now.Before(cycle.EndAt) {
			delete(key.Cycles, id)
			changed = true
		}
	}
	return changed
}

func activateCycles(key *KeyState, plan Plan, now time.Time) bool {
	if key.Cycles == nil {
		key.Cycles = make(map[string]QuotaCycle)
	}
	changed := false
	for _, window := range plan.Windows {
		if _, exists := key.Cycles[window.ID]; !exists {
			key.Cycles[window.ID] = QuotaCycle{PlanID: plan.ID, StartAt: now, EndAt: now.Add(time.Duration(window.PeriodSeconds) * time.Second)}
			changed = true
		}
	}
	return changed
}

func (key *KeyState) ValidateCycles(plan Plan) error {
	if key.PlanID != "" && plan.ID != key.PlanID {
		return invalidf("API Key 绑定的订阅计划不存在")
	}
	for id, cycle := range key.Cycles {
		index := slices.IndexFunc(plan.Windows, func(window QuotaWindow) bool { return window.ID == id })
		if index < 0 || cycle.PlanID != key.PlanID || cycle.StartAt.IsZero() || cycle.EndAt.IsZero() ||
			!cycle.EndAt.Equal(cycle.StartAt.Add(time.Duration(plan.Windows[index].PeriodSeconds)*time.Second)) ||
			cycle.SpentUSD < 0 || math.IsNaN(cycle.SpentUSD) || math.IsInf(cycle.SpentUSD, 0) {
			return invalidf("API Key 的额度周期数据无效")
		}
	}
	return nil
}
