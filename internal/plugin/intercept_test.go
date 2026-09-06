package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

const testAPIKey = "sk-test-key-0001"

func exhaustedApp(t *testing.T, resetAfter time.Duration) *App {
	t.Helper()
	app := newAppWithPrice(t, true)
	if _, errSync := app.store.SyncKeys([]string{testAPIKey}, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
	if _, errCreate := app.store.CreatePlanWithBindings(billing.Plan{
		ID: "plan-5", Name: "Plan 5", Windows: []billing.QuotaWindow{{Name: "额度", AmountUSD: 5, PeriodSeconds: int64(resetAfter / time.Second)}},
	}, []string{billing.CallerScope(testAPIKey)}); errCreate != nil {
		t.Fatalf("CreatePlanWithBindings error = %v", errCreate)
	}
	admit(t, app, "openai", "/v1/chat/completions")
	billUsage(t, app, 0, 0, 0, 2_500_000, 0)
	return app
}

func callIntercept(t *testing.T, app *App, sourceFormat string) RequestInterceptResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		SourceFormat: sourceFormat,
		Model:        "gpt-5.5",
		Metadata:     map[string]any{MetadataCallerScope: billing.CallerScope(testAPIKey)},
	}))
	if err != nil {
		t.Fatalf("request.intercept_before error = %v", err)
	}
	var resp RequestInterceptResponse
	decodeResult(t, raw, &resp)
	return resp
}

func TestInterceptTerminatesAnExhaustedKey(t *testing.T) {
	app := exhaustedApp(t, 30*time.Minute)
	resp := callIntercept(t, app, "openai")
	if !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("response = %+v, want quota rejection", resp)
	}
	retryAfter, err := strconv.Atoi(resp.ResponseHeaders.Get("Retry-After"))
	if err != nil || retryAfter != 1800 {
		t.Fatalf("Retry-After = %q, want 1800", resp.ResponseHeaders.Get("Retry-After"))
	}
}

// The type and code are the ones CLIProxyAPI derives from the status, so a
// client branching on them cannot tell whether the proxy or the plugin refused.
// Anthropic routes get the proxy's own envelope; every other format, Gemini
// included, gets its OpenAI-shaped one.
func TestInterceptUsesTheClientErrorShape(t *testing.T) {
	app := exhaustedApp(t, 12*time.Hour)
	for _, test := range []struct {
		format string
		want   map[string]string
	}{
		{"openai", map[string]string{"type": "rate_limit_error", "code": "rate_limit_exceeded"}},
		{"claude", map[string]string{"type": "rate_limit_error"}},
		{"gemini", map[string]string{"type": "rate_limit_error", "code": "rate_limit_exceeded"}},
	} {
		t.Run(test.format, func(t *testing.T) {
			resp := callIntercept(t, app, test.format)
			var payload struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(resp.ResponseBody, &payload); err != nil {
				t.Fatalf("invalid JSON error body: %v (raw=%s)", err, resp.ResponseBody)
			}
			for field, want := range test.want {
				if got, _ := payload.Error[field].(string); got != want {
					t.Fatalf("error.%s = %q, want %q (body=%s)", field, got, want, resp.ResponseBody)
				}
			}
			if message, _ := payload.Error["message"].(string); !strings.Contains(message, "quota exhausted") {
				t.Fatalf("message = %q, want it to state the exhausted quota", message)
			}
		})
	}
}

func TestCompletionDuringReferencePriceRefresh(t *testing.T) {
	for _, test := range []struct {
		name     string
		complete bool
		generate bool
		fail     bool
	}{
		{name: "completed generation", complete: true, generate: true},
		{name: "completed non-generation", complete: true},
		{name: "completed failed refresh", complete: true, generate: true, fail: true},
		{name: "active generation", generate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prices, err := os.ReadFile("../billing/testdata/models_dev_prices.json")
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseDownload := func() { releaseOnce.Do(func() { close(release) }) }
			app := newApp(billing.NewStore(openRepository, func(ctx context.Context) ([]byte, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if test.fail {
					return nil, fmt.Errorf("download failed")
				}
				return prices, nil
			}))
			t.Cleanup(app.Shutdown)
			defer releaseDownload()
			cfg := billing.DefaultConfig()
			cfg.Enabled = true
			cfg.StateFile = filepath.Join(t.TempDir(), "state.db")
			// Leave startup reference loading pending to exercise the admission fetch.
			if err := app.store.Configure(cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := app.store.SyncKeys([]string{testAPIKey}, false); err != nil {
				t.Fatal(err)
			}
			if err := app.store.SetConcurrencyLimit(flowScope(), 1); err != nil {
				t.Fatal(err)
			}
			if _, err := app.store.CreatePlanWithBindings(billing.Plan{
				ID: "cancel-test", Name: "Cancel test", Windows: []billing.QuotaWindow{{Name: "额度", AmountUSD: 10, PeriodSeconds: 3600}},
			}, []string{flowScope()}); err != nil {
				t.Fatal(err)
			}
			request := mustMarshal(t, RequestInterceptRequest{
				RequestID: "refresh-request", SourceFormat: "openai", Model: "gpt-4o", RequestedModel: "gpt-4o",
				Metadata: map[string]any{MetadataCallerScope: flowScope(), MetadataGenerate: test.generate},
			})
			type result struct {
				raw []byte
				err error
			}
			done := make(chan result, 1)
			go func() {
				raw, err := app.HandleMethod(MethodRequestInterceptBefore, request)
				done <- result{raw, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("admission did not reach reference download")
			}
			if test.complete {
				completeRequest(t, app, "refresh-request")
				completeRequest(t, app, "refresh-request")
			}
			releaseDownload()
			var response RequestInterceptResponse
			select {
			case result := <-done:
				if result.err != nil {
					t.Fatal(result.err)
				}
				decodeResult(t, result.raw, &response)
			case <-time.After(5 * time.Second):
				t.Fatal("admission did not finish after reference download")
			}
			if response.Terminate != test.complete {
				t.Fatalf("admission = %+v", response)
			}
			view, _ := app.store.KeyViewForScope(flowScope())
			if test.complete {
				if view.CurrentConcurrency != 0 || !view.Windows[0].EndAt.IsZero() {
					t.Fatalf("completed request changed admission state: %+v", view)
				}
			} else if view.CurrentConcurrency != 1 || view.Windows[0].EndAt.IsZero() {
				t.Fatalf("active request was not admitted: %+v", view)
			}
			if len(app.admissions) != 0 {
				t.Fatalf("admission markers retained: %d", len(app.admissions))
			}
			completeRequest(t, app, "refresh-request")
			view, _ = app.store.KeyViewForScope(flowScope())
			if view.CurrentConcurrency != 0 {
				t.Fatalf("completion leaked slot: %+v", view)
			}
		})
	}
}
