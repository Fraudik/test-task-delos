package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/app"
	"test-task-dellos/internal/testdb"
)

const (
	acmeKey     = "acme-secret-key"
	globexKey   = "globex-secret-key"
	payer       = "0x1111111111111111111111111111111111111111"
	blacklisted = "0xbad0000000000000000000000000000000000bad"
)

type env struct {
	t   *testing.T
	db  *pgxpool.Pool
	app *app.App
	srv *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testdb.New(t)
	cfg := app.Config{
		Confirmations: 5,
		AMLBlacklist:  []string{blacklisted},
		Partners: []app.PartnerConfig{
			{Name: "acme", APIKey: acmeKey, FeeBps: 150, FeeFixed: "0.5"},
			{Name: "globex", APIKey: globexKey, FeeBps: 100},
		},
	}
	a, err := app.New(t.Context(), db, cfg, slog.New(slog.NewTextHandler(t.Output(), nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	return &env{t: t, db: db, app: a, srv: srv}
}

type response struct {
	status int
	body   map[string]any
}

func (e *env) call(method, path, apiKey string, headers map[string]string, body any) response {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.srv.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := response{status: res.StatusCode}
	data, _ := io.ReadAll(res.Body)
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(data, &out.body); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return out
}

func (e *env) createInvoice(apiKey, idempotencyKey string) response {
	e.t.Helper()
	return e.createInvoiceFor(apiKey, idempotencyKey, "100")
}

func (e *env) createInvoiceFor(apiKey, idempotencyKey, amount string) response {
	e.t.Helper()
	return e.call(http.MethodPost, "/v1/invoices", apiKey, map[string]string{"Idempotency-Key": idempotencyKey}, map[string]string{"amount": amount})
}

func (e *env) invoice(apiKey string, id any) response {
	e.t.Helper()
	return e.call(http.MethodGet, fmt.Sprintf("/v1/invoices/%v", id), apiKey, nil, nil)
}

func (e *env) balance(apiKey string) (string, string) {
	e.t.Helper()
	res := e.call(http.MethodGet, "/v1/balance", apiKey, nil, nil)
	if res.status != http.StatusOK {
		e.t.Fatalf("balance status = %d, body %v", res.status, res.body)
	}
	return res.body["available"].(string), res.body["on_hold"].(string)
}

func (e *env) sim(method, path string, body any) response {
	e.t.Helper()
	res := e.call(method, path, "", nil, body)
	if res.status >= 300 {
		e.t.Fatalf("%s %s = %d %v", method, path, res.status, res.body)
	}
	return res
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.app.Tick(e.t.Context()); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

func (e *env) assertInvoice(apiKey string, id any, wantStatus string, wantDeposits ...string) {
	e.t.Helper()
	res := e.invoice(apiKey, id)
	if res.status != http.StatusOK {
		e.t.Fatalf("invoice status code = %d, body %v", res.status, res.body)
	}
	if res.body["status"] != wantStatus {
		e.t.Errorf("invoice status = %v, want %s", res.body["status"], wantStatus)
	}
	deposits := res.body["deposits"].([]any)
	if len(deposits) != len(wantDeposits) {
		e.t.Fatalf("invoice has %d deposits, want %d: %v", len(deposits), len(wantDeposits), deposits)
	}
	for i, d := range deposits {
		if got := d.(map[string]any)["status"]; got != wantDeposits[i] {
			e.t.Errorf("deposit %d status = %v, want %s", i, got, wantDeposits[i])
		}
	}
}

func (e *env) assertBalance(apiKey, wantAvailable, wantHold string) {
	e.t.Helper()
	available, hold := e.balance(apiKey)
	if available != wantAvailable || hold != wantHold {
		e.t.Errorf("balance = available %s, on hold %s; want %s, %s", available, hold, wantAvailable, wantHold)
	}
}

func (e *env) scalar(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.QueryRow(e.t.Context(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestFullScenarioLowRisk(t *testing.T) {
	e := newEnv(t)
	inv := e.createInvoice(acmeKey, "order-1")
	if inv.status != http.StatusCreated {
		t.Fatalf("create invoice = %d %v", inv.status, inv.body)
	}
	id, address := inv.body["id"], inv.body["address"].(string)
	e.assertInvoice(acmeKey, id, "awaiting_payment")

	e.sim(http.MethodPost, "/sim/transfers", map[string]string{"from": payer, "to": address, "amount": "100"})
	e.tick()
	e.assertInvoice(acmeKey, id, "processing", "pending")
	e.assertBalance(acmeKey, "0.000000", "0.000000")

	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 3})
	e.tick()
	e.assertInvoice(acmeKey, id, "processing", "pending")
	e.assertBalance(acmeKey, "0.000000", "0.000000")

	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 1})
	e.tick()
	e.assertInvoice(acmeKey, id, "paid", "credited")
	e.assertBalance(acmeKey, "98.000000", "0.000000")
	e.assertBalance(globexKey, "0.000000", "0.000000")

	if n := e.scalar(`SELECT count(*) FROM invoices WHERE notified_at IS NOT NULL`); n != 1 {
		t.Errorf("%d invoices notified, want 1", n)
	}
	for range 3 {
		e.tick()
	}
	e.assertBalance(acmeKey, "98.000000", "0.000000")
	if n := e.scalar(`SELECT count(*) FROM ledger_transactions`); n != 2 {
		t.Errorf("%d ledger transactions, want 2", n)
	}
}

func TestFullScenarioHighRiskRefund(t *testing.T) {
	e := newEnv(t)
	inv := e.createInvoice(globexKey, "order-1")
	e.sim(http.MethodPost, "/sim/transfers", map[string]string{"from": blacklisted, "to": inv.body["address"].(string), "amount": "40.25"})
	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 4})
	e.tick()
	e.tick()

	e.assertInvoice(globexKey, inv.body["id"], "awaiting_payment", "refunded")
	e.assertBalance(globexKey, "0.000000", "0.000000")
	var status, to string
	var amount int64
	if err := e.db.QueryRow(t.Context(), `SELECT status::text, to_address, amount FROM refunds`).Scan(&status, &to, &amount); err != nil {
		t.Fatal(err)
	}
	if status != "initiated" || to != blacklisted || amount != 40_250_000 {
		t.Errorf("refund = %s %s %d, want initiated %s 40250000", status, to, amount, blacklisted)
	}
	if n := e.scalar(`SELECT count(*) FROM refunds`); n != 1 {
		t.Errorf("%d refunds, want 1", n)
	}
}

func TestAMLOutageKeepsFundsOnHold(t *testing.T) {
	e := newEnv(t)
	inv := e.createInvoiceFor(acmeKey, "order-1", "10")
	e.sim(http.MethodPut, "/sim/aml", map[string]bool{"available": false})
	e.sim(http.MethodPost, "/sim/transfers", map[string]string{"from": payer, "to": inv.body["address"].(string), "amount": "10"})
	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 4})
	e.tick()

	e.assertInvoice(acmeKey, inv.body["id"], "processing", "confirmed")
	e.assertBalance(acmeKey, "0.000000", "10.000000")

	e.sim(http.MethodPut, "/sim/aml", map[string]bool{"available": true})
	if _, err := e.db.Exec(t.Context(), `UPDATE deposit_aml_checks SET next_attempt_at = now() WHERE risk = ''`); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.assertInvoice(acmeKey, inv.body["id"], "paid", "credited")
	e.assertBalance(acmeKey, "9.350000", "0.000000")
}

func TestReorgBeforeConfirmationsLeavesNothing(t *testing.T) {
	e := newEnv(t)
	inv := e.createInvoiceFor(acmeKey, "order-1", "10")
	e.sim(http.MethodPost, "/sim/transfers", map[string]string{"from": payer, "to": inv.body["address"].(string), "amount": "10"})
	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 2})
	e.tick()
	e.sim(http.MethodPost, "/sim/reorg", map[string]int{"depth": 3})
	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 10})
	e.tick()
	e.tick()

	e.assertInvoice(acmeKey, inv.body["id"], "awaiting_payment", "orphaned")
	e.assertBalance(acmeKey, "0.000000", "0.000000")
	if n := e.scalar(`SELECT count(*) FROM ledger_transactions`); n != 0 {
		t.Errorf("%d ledger transactions after reorg, want 0", n)
	}
}

func TestInvoiceCreationIsIdempotent(t *testing.T) {
	e := newEnv(t)
	first := e.createInvoice(acmeKey, "order-1")
	again := e.createInvoice(acmeKey, "order-1")
	other := e.createInvoice(acmeKey, "order-2")
	foreign := e.createInvoice(globexKey, "order-1")

	if first.status != http.StatusCreated || again.status != http.StatusOK {
		t.Errorf("statuses = %d, %d; want 201, 200", first.status, again.status)
	}
	if first.body["id"] != again.body["id"] || first.body["address"] != again.body["address"] {
		t.Errorf("same key returned different invoices: %v vs %v", first.body, again.body)
	}
	if other.body["id"] == first.body["id"] || foreign.body["id"] == first.body["id"] {
		t.Error("different key or partner reused the invoice")
	}
	for _, res := range []response{first, other, foreign} {
		if id, err := uuid.Parse(fmt.Sprint(res.body["id"])); err != nil || id.Version() != 4 {
			t.Errorf("invoice id %v is not a UUIDv4", res.body["id"])
		}
	}

	if res := e.createInvoice(acmeKey, ""); res.status != http.StatusBadRequest {
		t.Errorf("missing Idempotency-Key = %d, want 400", res.status)
	}

	var wg sync.WaitGroup
	ids := make(chan any, 20)
	for range 20 {
		wg.Go(func() { ids <- e.createInvoice(acmeKey, "order-concurrent").body["id"] })
	}
	wg.Wait()
	close(ids)
	var firstID any
	for id := range ids {
		if firstID == nil {
			firstID = id
		}
		if id != firstID {
			t.Errorf("concurrent requests created different invoices: %v and %v", firstID, id)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM invoices WHERE idempotency_key = 'order-concurrent'`); n != 1 {
		t.Errorf("%d invoices for one key, want 1", n)
	}
}

func TestPartnersAreIsolated(t *testing.T) {
	e := newEnv(t)
	acme := e.createInvoice(acmeKey, "order-1")
	e.sim(http.MethodPost, "/sim/transfers", map[string]string{"from": payer, "to": acme.body["address"].(string), "amount": "100"})
	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 4})
	e.tick()

	if res := e.invoice(globexKey, acme.body["id"]); res.status != http.StatusNotFound {
		t.Errorf("foreign invoice = %d, want 404", res.status)
	}
	if res := e.invoice(acmeKey, acme.body["id"]); res.status != http.StatusOK {
		t.Errorf("own invoice = %d, want 200", res.status)
	}
	e.assertBalance(acmeKey, "98.000000", "0.000000")
	e.assertBalance(globexKey, "0.000000", "0.000000")

	if res := e.call(http.MethodGet, "/v1/balance", "", nil, nil); res.status != http.StatusUnauthorized {
		t.Errorf("no API key = %d, want 401", res.status)
	}
	if res := e.call(http.MethodGet, "/v1/balance", "wrong-key", nil, nil); res.status != http.StatusUnauthorized {
		t.Errorf("wrong API key = %d, want 401", res.status)
	}
	for _, id := range []string{uuid.NewString(), "999999"} {
		if res := e.invoice(acmeKey, id); res.status != http.StatusNotFound {
			t.Errorf("unknown invoice %s = %d, want 404", id, res.status)
		}
	}
}

func TestDocsAreServed(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/docs", "/openapi.yaml"} {
		res, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, res.StatusCode)
		}
	}
}

func TestInvoicePaidByTwoTransfers(t *testing.T) {
	e := newEnv(t)
	inv := e.createInvoiceFor(acmeKey, "order-split", "100")
	if inv.body["amount"] != "100.000000" || inv.body["received"] != "0.000000" {
		t.Errorf("new invoice amount/received = %v/%v, want 100.000000/0.000000", inv.body["amount"], inv.body["received"])
	}
	id, address := inv.body["id"], inv.body["address"].(string)

	e.sim(http.MethodPost, "/sim/transfers", map[string]string{"from": payer, "to": address, "amount": "60"})
	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 4})
	e.tick()
	e.tick()
	e.assertInvoice(acmeKey, id, "partially_paid", "confirmed")
	if res := e.invoice(acmeKey, id); res.body["received"] != "60.000000" {
		t.Errorf("received = %v, want 60.000000", res.body["received"])
	}
	e.assertBalance(acmeKey, "0.000000", "60.000000")

	e.sim(http.MethodPost, "/sim/transfers", map[string]string{"from": payer, "to": address, "amount": "40"})
	e.sim(http.MethodPost, "/sim/blocks", map[string]int{"count": 4})
	e.tick()
	e.tick()
	e.assertInvoice(acmeKey, id, "paid", "credited", "credited")
	e.assertBalance(acmeKey, "98.000000", "0.000000")
	if n := e.scalar(`SELECT count(*) FROM invoices WHERE notified_at IS NOT NULL`); n != 1 {
		t.Errorf("%d invoices notified, want 1", n)
	}
}

func TestInvoiceAmountValidation(t *testing.T) {
	e := newEnv(t)
	for _, amount := range []string{"", "0", "-1", "1.0000001", "abc"} {
		if res := e.createInvoiceFor(acmeKey, "bad-"+amount, amount); res.status != http.StatusBadRequest {
			t.Errorf("amount %q = %d, want 400", amount, res.status)
		}
	}
	if res := e.createInvoiceFor(acmeKey, "order-1", "10"); res.status != http.StatusCreated {
		t.Fatalf("create = %d %v", res.status, res.body)
	}
	if res := e.createInvoiceFor(acmeKey, "order-1", "11"); res.status != http.StatusConflict {
		t.Errorf("same key with another amount = %d, want 409", res.status)
	}
}
