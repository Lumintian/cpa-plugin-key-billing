package billing

// Builtin prices are shipped with the plugin and are intentionally not persisted.
var builtinPrices = map[string]CustomPrice{
	NormalizeModelID("codex-auto-review"): {
		ModelID: "codex-auto-review",
		// Matches models.dev openai/gpt-5.4.
		PriceRates: PriceRates{
			InputPer1M:     2.5,
			OutputPer1M:    15,
			CacheReadPer1M: float64Ptr(0.25),
			LongContext: &LongContextPrice{
				ThresholdInputTokens: 272000,
				InputPer1M:           5,
				OutputPer1M:          22.5,
				CacheReadPer1M:       float64Ptr(0.5),
			},
		},
	},
	NormalizeModelID("gpt-image-1.5"): {
		ModelID: "gpt-image-1.5",
		PriceRates: PriceRates{
			InputPer1M:     5,
			OutputPer1M:    32,
			CacheReadPer1M: float64Ptr(1.25),
		},
	},
}

func float64Ptr(value float64) *float64 {
	return &value
}

func resolveBuiltinRates(modelID string) (PriceRates, bool) {
	price, found := builtinPrices[NormalizeModelID(modelID)]
	return price.PriceRates, found
}

func ResolveBuiltinPrice(modelID string) Price {
	if rates, found := resolveBuiltinRates(modelID); found {
		return rates.resolve(PriceSourceBuiltin)
	}
	return Price{Source: PriceSourceNone}
}
