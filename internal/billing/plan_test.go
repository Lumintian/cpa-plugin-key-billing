package billing

import (
	"math"
	"testing"
)

func TestQuotaWindowsValidationAndIdentity(t *testing.T) {
	input := []QuotaWindow{{Name: " Long ", AmountUSD: 10, PeriodSeconds: 7200}, {Name: "Short", AmountUSD: 2, PeriodSeconds: 3600}}
	windows, err := prepareWindows(input, nil)
	if err != nil || windows[0].Name != "Short" || windows[1].Name != "Long" || windows[0].ID == windows[1].ID {
		t.Fatalf("windows = %+v, %v", windows, err)
	}
	valid := Plan{ID: "p", Windows: windows}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	edited, err := prepareWindows(windows, windows)
	if err != nil || edited[0].ID != windows[0].ID {
		t.Fatal("editing changed window identity")
	}
	if _, err := prepareWindows(windows, nil); err == nil {
		t.Fatal("unknown IDs accepted")
	}
	for _, change := range []func(*Plan){
		func(p *Plan) { p.Windows = nil },
		func(p *Plan) { p.Windows[0].Name = " long " },
		func(p *Plan) { p.Windows[0].PeriodSeconds = 7200 },
		func(p *Plan) { p.Windows[0].PeriodSeconds = 0 },
		func(p *Plan) { p.Windows[0].PeriodSeconds = -1 },
		func(p *Plan) { p.Windows[0].PeriodSeconds = maxPeriodSeconds + 1 },
		func(p *Plan) { p.Windows[0].AmountUSD = math.NaN() },
		func(p *Plan) { p.Windows[0].AmountUSD = math.Inf(1) },
		func(p *Plan) { p.Windows[0].AmountUSD = 0 },
	} {
		invalid := clonePlan(valid)
		change(&invalid)
		if invalid.Validate() == nil {
			t.Fatalf("invalid plan accepted: %+v", invalid)
		}
	}
}
