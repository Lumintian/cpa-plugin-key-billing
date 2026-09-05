package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestV12PriceMigrationPreservesModelIDsAndHistory(t *testing.T) {
	for _, modelIDs := range [][]string{
		{"model-a", "model-b"},
		{"gpt-*", "model?"},
	} {
		t.Run(modelIDs[0], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v12.db")
			legacy, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(legacyPricingSchemaSQL + "PRAGMA user_version=12;"); err != nil {
				t.Fatal(err)
			}
			for position, modelID := range modelIDs {
				if _, err := legacy.Exec("INSERT INTO prices(position,pattern) VALUES(?,?)", position, modelID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := legacy.Exec("INSERT INTO request_events(at,scope,failed) VALUES(1,'old-key',1),(2,'old-key',0)"); err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			database, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			assertMigratedReferencePriceSchema(t, database)
			state := mustLoad(t, database).State
			if len(state.Prices) != len(modelIDs) {
				t.Fatalf("prices lost: %+v", state.Prices)
			}
			for _, modelID := range modelIDs {
				price := state.Prices[billing.NormalizeModelID(modelID)]
				if price.ModelID != modelID || price.InputPer1M != 0 || price.OutputPer1M != 0 {
					t.Fatalf("price changed: %+v", price)
				}
			}
			var events int
			if err := database.db.QueryRow("SELECT count(*) FROM request_events").Scan(&events); err != nil || events != 2 {
				t.Fatalf("usage history changed: events=%d err=%v", events, err)
			}
		})
	}
}

func TestV10V11PricingMigrationIsAtomicAndPreservesLegacyJSON(t *testing.T) {
	for _, version := range []int{10, 11} {
		for _, conflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/conflict=%t", version, conflict), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "legacy.db")
				raw, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				old := legacyPricingSchemaSQL
				old += `ALTER TABLE plans ADD COLUMN period_kind TEXT NOT NULL DEFAULT 'monthly';
INSERT INTO plans(position,id,name,amount_usd) VALUES(0,'plan','legacy plan',100);
INSERT INTO prices(position,pattern) VALUES(0,'model-a');
INSERT INTO request_events(id,at,scope,failed) VALUES(1,1,'legacy-scope',1),(2,2,'legacy-scope',0);
INSERT INTO request_errors(request_event_id,status_code,reason) VALUES(1,502,'preserved failure');`
				if version == 10 {
					old += `DROP TABLE routes;
ALTER TABLE api_keys DROP COLUMN route_bindings_json;
CREATE TABLE model_groups(position INTEGER,id TEXT,name TEXT);
CREATE TABLE model_group_models(position INTEGER,group_id TEXT,model TEXT);
CREATE TABLE key_model_groups(position INTEGER,scope TEXT,group_id TEXT);
CREATE TABLE key_allowed_models(position INTEGER,scope TEXT,model TEXT);
INSERT INTO api_keys(scope,plan_id,cycle_spent_usd) VALUES('legacy-scope','plan',4.5);
INSERT INTO model_groups VALUES(0,'group','Legacy group');
INSERT INTO model_group_models VALUES(0,'group','model-a');
INSERT INTO key_model_groups VALUES(0,'legacy-scope','group');
INSERT INTO key_allowed_models VALUES(0,'legacy-scope','other-model');`
				} else {
					old += `INSERT INTO api_keys(scope,plan_id,cycle_spent_usd,route_bindings_json)
VALUES('legacy-scope','plan',4.5,'[{"kind":"model","value":"model-a"}]');`
				}
				if conflict {
					// Fail after the earlier migration steps have transformed legacy
					// bindings and periods, proving the entire chain rolls back.
					old += `CREATE TABLE reference_prices_metadata(marker TEXT); INSERT INTO reference_prices_metadata VALUES('keep');`
				}
				old += fmt.Sprintf("PRAGMA user_version=%d;", version)
				if _, err := raw.Exec(old); err != nil {
					t.Fatal(err)
				}
				d, err := Open(path)
				if conflict {
					if err == nil {
						d.Close()
						t.Fatal("incompatible reference price schema accepted")
					}
					var preservedVersion int
					if err := raw.QueryRow("PRAGMA user_version").Scan(&preservedVersion); err != nil || preservedVersion != version {
						t.Fatal("version was not rolled back", preservedVersion, err)
					}
					var id, kind string
					if err := raw.QueryRow("SELECT pattern FROM prices").Scan(&id); err != nil || id != "model-a" {
						t.Fatal("legacy price schema was not restored", id, err)
					}
					if err := raw.QueryRow("SELECT period_kind FROM plans").Scan(&kind); err != nil || kind != "monthly" {
						t.Fatal("legacy period schema was not restored", kind, err)
					}
					if version == 10 {
						if err := raw.QueryRow("SELECT id FROM model_groups").Scan(&id); err != nil || id != "group" {
							t.Fatal("legacy table was lost", id, err)
						}
					} else if err := raw.QueryRow("SELECT route_bindings_json FROM api_keys").Scan(&id); err != nil || !strings.HasPrefix(id, "[") {
						t.Fatal("legacy JSON was not restored", id, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				assertMigratedReferencePriceSchema(t, d)
				snapshot, err := d.Load(time.Time{}, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.RequestEventCount != 2 || len(snapshot.State.Prices) != 1 || snapshot.State.Prices["model-a"].ModelID != "model-a" || snapshot.State.Prices["model-a"].InputPer1M != 0 || snapshot.State.Plans[0].PeriodSeconds != 2592000 {
					t.Fatal("history or prices changed", snapshot)
				}
				key := snapshot.State.Keys["legacy-scope"]
				if key == nil || key.Cycle.SpentUSD != 4.5 || (len(key.RouteBindings.Models) == 0 && len(key.RouteBindings.RouteIDs) == 0) {
					t.Fatal("legacy key state was lost", key)
				}
				var reason string
				if err := raw.QueryRow("SELECT reason FROM request_errors WHERE request_event_id=1").Scan(&reason); err != nil || reason != "preserved failure" {
					t.Fatal("failed event details were lost", reason, err)
				}
			})
		}
	}
}

// Each migration must produce the same reference-price tables and index as a
// fresh database, including the absence of any derived alias table.
func assertMigratedReferencePriceSchema(t *testing.T, migrated *DB) {
	t.Helper()
	fresh := openTestDB(t)
	readSchema := func(database *DB) string {
		rows, err := database.db.Query(`
            SELECT sql FROM sqlite_master
            WHERE name LIKE 'reference_price%' AND sql IS NOT NULL ORDER BY name
        `)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var statements []string
		for rows.Next() {
			var statement string
			if err := rows.Scan(&statement); err != nil {
				t.Fatal(err)
			}
			statements = append(statements, strings.Join(strings.Fields(statement), " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(statements, "\n")
	}
	if got, want := readSchema(migrated), readSchema(fresh); got != want {
		t.Fatalf("migrated reference schema differs from fresh schema:\ngot: %s\nwant: %s", got, want)
	}
}

// Fixed v12 schema: later schema changes must not alter migration input.
const legacyPricingSchemaSQL = `
CREATE TABLE api_keys (
	scope                 TEXT    PRIMARY KEY,
	preview               TEXT    NOT NULL DEFAULT '',
	label                 TEXT    NOT NULL DEFAULT '',
	in_config             INTEGER NOT NULL DEFAULT 0,
	deleted_at            INTEGER NOT NULL DEFAULT 0,
	plan_id               TEXT    NOT NULL DEFAULT '',
	concurrency_limit      INTEGER NOT NULL DEFAULT 0,
	cycle_plan_id         TEXT    NOT NULL DEFAULT '',
	cycle_start_at        INTEGER NOT NULL DEFAULT 0,
	cycle_end_at          INTEGER NOT NULL DEFAULT 0,
	cycle_spent_usd       REAL    NOT NULL DEFAULT 0,
	route_bindings_json   TEXT    NOT NULL DEFAULT '{}'
);

CREATE TABLE routes (
	position INTEGER PRIMARY KEY,
	id       TEXT    NOT NULL UNIQUE,
	name     TEXT    NOT NULL DEFAULT '',
	rule_json TEXT   NOT NULL DEFAULT '{}'
);

CREATE TABLE plans (
	position       INTEGER PRIMARY KEY,
	id             TEXT    NOT NULL UNIQUE,
	name           TEXT    NOT NULL DEFAULT '',
	amount_usd     REAL    NOT NULL DEFAULT 0,
	period_seconds INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE prices (
	position                        INTEGER PRIMARY KEY,
	pattern                         TEXT    NOT NULL,
	input_per_1m                    REAL    NOT NULL DEFAULT 0,
	output_per_1m                   REAL    NOT NULL DEFAULT 0,
	cache_read_per_1m               REAL,
	cache_write_per_1m              REAL,
	long_context_threshold          INTEGER,
	long_context_input_per_1m       REAL,
	long_context_output_per_1m      REAL,
	long_context_cache_read_per_1m  REAL,
	long_context_cache_write_per_1m REAL
);

CREATE TABLE credentials (
	auth_index TEXT PRIMARY KEY,
	provider   TEXT NOT NULL DEFAULT '',
	account    TEXT NOT NULL DEFAULT '',
	name       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE request_events (
	id                          INTEGER PRIMARY KEY AUTOINCREMENT,
	at                          INTEGER NOT NULL,
	scope                       TEXT    NOT NULL,
	auth_index                  TEXT    NOT NULL DEFAULT '',
	provider                    TEXT    NOT NULL DEFAULT '',
	executor_type               TEXT    NOT NULL DEFAULT '',
	reasoning_effort            TEXT    NOT NULL DEFAULT '',
	service_tier                TEXT    NOT NULL DEFAULT '',
	upstream_model              TEXT    NOT NULL DEFAULT '',
	billing_model               TEXT    NOT NULL DEFAULT '',
	failed                      INTEGER NOT NULL DEFAULT 0,
	latency_ms                  INTEGER NOT NULL DEFAULT 0,
	ttft_ms                     INTEGER NOT NULL DEFAULT 0,
	accounting_quality          TEXT    NOT NULL DEFAULT '',
	price_source                TEXT    NOT NULL DEFAULT '',
	reasoning_tokens            INTEGER NOT NULL DEFAULT 0,
	total_usd                   REAL    NOT NULL DEFAULT 0,
	uncached_input_usd          REAL    NOT NULL DEFAULT 0,
	cache_read_usd              REAL    NOT NULL DEFAULT 0,
	cache_write_usd             REAL    NOT NULL DEFAULT 0,
	output_usd                  REAL    NOT NULL DEFAULT 0,
	uncached_input_tokens       INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens           INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens          INTEGER NOT NULL DEFAULT 0,
	billed_output_tokens        INTEGER NOT NULL DEFAULT 0,
	tiered                      INTEGER NOT NULL DEFAULT 0,
	long_context                INTEGER NOT NULL DEFAULT 0,
	threshold_input_tokens      INTEGER NOT NULL DEFAULT 0,
	applied_input_per_1m        REAL    NOT NULL DEFAULT 0,
	applied_output_per_1m       REAL    NOT NULL DEFAULT 0,
	applied_cache_read_per_1m   REAL    NOT NULL DEFAULT 0,
	applied_cache_write_per_1m  REAL    NOT NULL DEFAULT 0
);

CREATE INDEX request_events_at ON request_events(at);
CREATE INDEX request_events_scope_at ON request_events(scope, at);
CREATE INDEX request_events_model_at ON request_events(billing_model, at);
CREATE INDEX request_events_auth_at ON request_events(auth_index, at);

CREATE TABLE request_errors (
	request_event_id INTEGER PRIMARY KEY REFERENCES request_events(id) ON DELETE CASCADE,
	status_code      INTEGER NOT NULL DEFAULT 0,
	error_type       TEXT    NOT NULL DEFAULT '',
	reason           TEXT    NOT NULL DEFAULT '',
	body             TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX request_errors_status ON request_errors(status_code);
CREATE INDEX request_errors_type ON request_errors(error_type);

CREATE TABLE plugin_logs (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	at      INTEGER NOT NULL,
	level   TEXT    NOT NULL DEFAULT '',
	message TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX plugin_logs_at ON plugin_logs(at);
`
