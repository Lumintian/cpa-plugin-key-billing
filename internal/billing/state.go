package billing

import "time"

// State is the working set the store keeps in memory. It holds everything the
// request path has to consult without touching disk. Request and error events
// grow with traffic and are queried directly from the repository.
type State struct {
	// Prices is keyed by NormalizeModelID; each value retains its stored spelling.
	Prices map[string]CustomPrice
	Plans  []Plan
	Keys   map[string]*KeyState
	Routes []Route
	// Credentials names the upstream credentials seen so far, keyed by the
	// host's runtime auth index. Request events store that index and read the name
	// from here, so a credential renamed upstream renames its history too.
	Credentials map[string]Credential
}

func NewState() *State {
	return &State{
		Prices:      make(map[string]CustomPrice),
		Keys:        make(map[string]*KeyState),
		Credentials: make(map[string]Credential),
	}
}

// LongContextPrice replaces the whole request's rates when total normalized
// input exceeds ThresholdInputTokens. Cache prices use the tier's input price
// when omitted, exactly like the standard price.
type LongContextPrice struct {
	ThresholdInputTokens int64    `json:"threshold_input_tokens"`
	InputPer1M           float64  `json:"input_per_1m"`
	OutputPer1M          float64  `json:"output_per_1m"`
	CacheReadPer1M       *float64 `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M      *float64 `json:"cache_write_per_1m,omitempty"`
}

type Plan struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Windows []QuotaWindow `json:"windows"`
}

type QuotaWindow struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	AmountUSD     float64 `json:"amount_usd"`
	PeriodSeconds int64   `json:"period_seconds"`
}

// KeyState is identified by caller scope; plaintext keys are never stored.
type KeyState struct {
	Preview string `json:"preview,omitempty"`
	Label   string `json:"label,omitempty"`
	// These two tell the three kinds of record apart:
	//
	//	InConfig set     a key CPA currently holds
	//	DeletedAt set    a key CPA held and no longer does
	//	neither set      a principal only ever seen in traffic, which may belong
	//	                 to another access provider and must therefore never be
	//	                 marked deleted by a CPA Key-list sync
	//
	// Key records are permanent: historical events resolve their scope to the
	// preview and label here, even after the key leaves CPA's configuration.
	InConfig         bool                  `json:"in_config,omitempty"`
	DeletedAt        time.Time             `json:"deleted_at,omitzero"`
	PlanID           string                `json:"plan_id,omitempty"`
	ConcurrencyLimit int                   `json:"concurrency_limit,omitempty"`
	RouteBindings    RouteBindings         `json:"route_bindings"`
	Cycles           map[string]QuotaCycle `json:"cycles"`
}

type QuotaCycle struct {
	// PlanID ties persisted consumption to the plan that opened this window.
	PlanID   string    `json:"plan_id,omitempty"`
	StartAt  time.Time `json:"start_at,omitzero"`
	EndAt    time.Time `json:"end_at,omitzero"`
	SpentUSD float64   `json:"spent_usd"`
}
