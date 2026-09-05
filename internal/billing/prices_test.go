package billing

import (
	"fmt"
	"testing"
)

func TestResolveCustomPrice(t *testing.T) {
	state := NewState()
	state.Prices = map[string]CustomPrice{
		"gpt-5.5":      {ModelID: "gpt-5.5", PriceRates: PriceRates{InputPer1M: 1, OutputPer1M: 2}},
		"team/gpt-5.5": {ModelID: "team/gpt-5.5", PriceRates: PriceRates{InputPer1M: 5}},
	}
	price := state.ResolveCustomPrice("team/gpt-5.5")
	if price.InputPer1M != 5 || price.CacheReadPer1M != 5 || price.CacheWritePer1M != 5 {
		t.Fatalf("billing model price or cache fallback is wrong: %+v", price)
	}
	price = state.ResolveCustomPrice("gpt-5.5")
	if price.InputPer1M != 1 {
		t.Fatalf("model price is wrong: %+v", price)
	}
	price = state.ResolveCustomPrice("unknown")
	if price.Source != PriceSourceNone {
		t.Fatalf("unknown model has a custom price: %+v", price)
	}
}

func TestResolveBillingModel(t *testing.T) {
	state := NewState()
	state.Prices = map[string]CustomPrice{
		"grok-4.5":              {ModelID: "grok-4.5"},
		"claude/deepseek-flash": {ModelID: "claude/deepseek-flash"},
		"configured(low)":       {ModelID: "configured(low)"},
	}
	tests := []struct {
		name     string
		upstream string
		route    string
		want     string
	}{
		{name: "model", upstream: "grok-4.5", route: "grok-4.5", want: "grok-4.5"},
		{name: "thinking", upstream: "grok-4.5", route: "grok-4.5(high)", want: "grok-4.5"},
		{name: "route", upstream: "deepseek-v4-flash", route: "claude/deepseek-flash", want: "claude/deepseek-flash"},
		{name: "route thinking", upstream: "deepseek-v4-flash", route: "claude/deepseek-flash(high)", want: "claude/deepseek-flash"},
		{name: "configured suffix", upstream: "upstream-low", route: "configured(low)", want: "configured(low)"},
		{name: "request suffix", upstream: "upstream-high", route: "configured(high)", want: "configured"},
		{name: "auto", upstream: "gpt-5.5", route: "auto(high)", want: "gpt-5.5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := state.ResolveBillingModel(test.upstream, test.route); got != test.want {
				t.Fatalf("ResolveBillingModel(%q, %q) = %q, want %q", test.upstream, test.route, got, test.want)
			}
		})
	}
}

func TestModelWithoutThinkingSuffixPreservesModelIdentity(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{" codex/gpt-5.6-sol(xhigh) ", "codex/gpt-5.6-sol"},
		{"claude/model(32768)", "claude/model"},
		{"provider/model(custom)(high)", "provider/model(custom)"},
		{"model(high", "model(high"},
		{"model(high)-vision-exp", "model(high)-vision-exp"},
		{" MODEL ", "MODEL"},
		{"", ""},
	} {
		if got := ModelWithoutThinkingSuffix(test.input); got != test.want {
			t.Fatalf("ModelWithoutThinkingSuffix(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func BenchmarkCustomPriceLookup(b *testing.B) {
	for _, count := range []int{10, 1000, 100000} {
		b.Run(fmt.Sprintf("models=%d", count), func(b *testing.B) {
			store := NewStore(nil, nil)
			for i := range count {
				model := fmt.Sprintf("provider/model-%d", i)
				store.state.Prices[model] = CustomPrice{ModelID: model, PriceRates: PriceRates{InputPer1M: 1}}
			}
			requested := fmt.Sprintf("provider/model-%d(high)", count-1)
			b.ReportAllocs()
			for b.Loop() {
				price, _, err := store.ResolveModelPrice("upstream", requested, false)
				if err != nil || price.Source != PriceSourceCustom {
					b.Fatalf("lookup failed: %+v, %v", price, err)
				}
			}
		})
	}
}
