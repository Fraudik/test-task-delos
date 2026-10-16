package processing_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/aml/mock"
	"test-task-dellos/internal/chain"
	"test-task-dellos/internal/chain/simulator"
	"test-task-dellos/internal/ledger"
	"test-task-dellos/internal/money"
	"test-task-dellos/internal/processing"
	"test-task-dellos/internal/testdb"
)

const (
	payer       = "0x1111111111111111111111111111111111111111"
	blacklisted = "0xbad0000000000000000000000000000000000bad"
)

type countingChain struct {
	chain.Client
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingChain) inc(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = make(map[string]int)
	}
	c.calls[method]++
}

func (c *countingChain) take() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	calls := c.calls
	c.calls = nil
	return calls
}

func (c *countingChain) HeadBlock(ctx context.Context) (uint64, error) {
	c.inc("HeadBlock")
	return c.Client.HeadBlock(ctx)
}

func (c *countingChain) BlockHash(ctx context.Context, number uint64) (string, error) {
	c.inc("BlockHash")
	return c.Client.BlockHash(ctx, number)
}

func (c *countingChain) GetTransfers(ctx context.Context, from, to uint64) ([]chain.Transfer, error) {
	c.inc("GetTransfers")
	return c.Client.GetTransfers(ctx, from, to)
}

type env struct {
	t     *testing.T
	db    *pgxpool.Pool
	sim   *simulator.Simulator
	chain *countingChain
	aml   *mock.Mock
	cfg   processing.Config
	proc  *processing.Processor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := processing.DefaultConfig()
	cfg.AMLBackoffBase = time.Hour
	cfg.AMLBackoffMax = time.Hour
	sim := simulator.New()
	e := &env{t: t, db: testdb.New(t), sim: sim, chain: &countingChain{Client: sim}, aml: mock.New([]string{blacklisted}), cfg: cfg}
	e.proc = e.newProcessor()
	return e
}

func (e *env) newProcessor() *processing.Processor {
	e.t.Helper()
	p, err := processing.New(e.db, e.chain, e.aml, e.cfg, slog.New(slog.NewTextHandler(e.t.Output(), nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) partner(feeBps int) int64 {
	e.t.Helper()
	return e.partnerWithFee(feeBps, 0)
}

func (e *env) partnerWithFee(feeBps int, feeFixed money.Amount) int64 {
	e.t.Helper()
	var id int64
	err := e.db.QueryRow(e.t.Context(),
		`INSERT INTO partners (name, fee_bps, fee_fixed) VALUES ('partner-' || gen_random_uuid(), $1, $2) RETURNING id`, feeBps, int64(feeFixed),
	).Scan(&id)
	if err != nil {
		e.t.Fatalf("create partner: %v", err)
	}
	return id
}

func (e *env) invoice(partnerID int64) string {
	e.t.Helper()
	b := make([]byte, 20)
	rand.Read(b)
	address := "0x" + hex.EncodeToString(b)
	if _, err := e.db.Exec(e.t.Context(), `INSERT INTO invoices (partner_id, address) VALUES ($1, $2)`, partnerID, address); err != nil {
		e.t.Fatalf("create invoice: %v", err)
	}
	return address
}

func (e *env) send(from, to string, amount money.Amount) chain.Transfer {
	e.t.Helper()
	tr, err := e.sim.SendTransfer(from, to, amount)
	if err != nil {
		e.t.Fatalf("send transfer: %v", err)
	}
	return tr
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.proc.Tick(e.t.Context()); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

func (e *env) step(name string, run func(context.Context) (int, error)) {
	e.t.Helper()
	if _, err := run(e.t.Context()); err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
}

func (e *env) status(tr chain.Transfer) string {
	e.t.Helper()
	var s string
	err := e.db.QueryRow(e.t.Context(), `SELECT status::text FROM deposits WHERE tx_hash = $1`, tr.TxHash).Scan(&s)
	if err != nil {
		e.t.Fatalf("deposit status %s: %v", tr.TxHash, err)
	}
	return s
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.t.Context(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e *env) assertStatus(tr chain.Transfer, want string) {
	e.t.Helper()
	if got := e.status(tr); got != want {
		e.t.Errorf("deposit status = %s, want %s", got, want)
	}
}

func (e *env) assertRisk(tr chain.Transfer, want string) {
	e.t.Helper()
	var risk string
	if err := e.db.QueryRow(e.t.Context(), `SELECT a.risk FROM deposit_aml_checks a JOIN deposits d ON d.id = a.deposit_id WHERE d.tx_hash = $1`, tr.TxHash).Scan(&risk); err != nil {
		e.t.Fatalf("deposit AML risk %s: %v", tr.TxHash, err)
	}
	if risk != want {
		e.t.Errorf("deposit AML risk = %q, want %q", risk, want)
	}
}

const resetForReprocessing = `WITH d AS (UPDATE deposits SET status = 'confirmed' RETURNING id) UPDATE deposit_aml_checks a SET risk = '', reason = '', checked_at = NULL, next_attempt_at = now() FROM d WHERE a.deposit_id = d.id`

func (e *env) assertBalances(want map[ledger.Account]money.Amount) {
	e.t.Helper()
	for a, w := range want {
		got, err := ledger.Balance(e.t.Context(), e.db, a)
		if err != nil {
			e.t.Fatalf("balance %s: %v", a.ID, err)
		}
		if got != w {
			e.t.Errorf("balance %s = %d, want %d", a.ID, got, w)
		}
	}
	if err := ledger.CheckIntegrity(e.t.Context(), e.db); err != nil {
		e.t.Error(err)
	}
}

func TestLowRiskDepositIsCreditedAfterConfirmations(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(150)
	address := e.invoice(pid)
	hold, available := ledger.PartnerHold(pid), ledger.PartnerAvailable(pid)

	tr := e.send(payer, "0x"+strings.ToUpper(address[2:]), 100*money.OneUSDT)
	e.tick()
	e.assertStatus(tr, "pending")
	e.assertRisk(tr, "low")
	e.assertBalances(map[ledger.Account]money.Amount{ledger.Custody: 0, hold: 0, available: 0})

	e.sim.AddBlocks(3)
	e.tick()
	e.assertStatus(tr, "pending")
	e.assertBalances(map[ledger.Account]money.Amount{hold: 0, available: 0})

	e.sim.AddBlocks(1)
	e.step("scan", e.proc.ScanBlocks)
	e.step("confirmations", e.proc.ProcessConfirmations)
	e.assertStatus(tr, "confirmed")
	e.assertBalances(map[ledger.Account]money.Amount{hold: 100 * money.OneUSDT, available: 0})

	e.step("settlement", e.proc.ProcessSettlement)
	e.assertStatus(tr, "credited")
	e.assertBalances(map[ledger.Account]money.Amount{
		ledger.Custody: 100 * money.OneUSDT,
		hold:           0,
		available:      98_500_000,
		ledger.Fees:    1_500_000,
	})
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 2 {
		t.Errorf("%d ledger transactions, want 2", n)
	}
}

func TestHighRiskCreatesSingleRefund(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(150)
	address := e.invoice(pid)
	hold, available := ledger.PartnerHold(pid), ledger.PartnerAvailable(pid)

	tr := e.send(strings.ToUpper(blacklisted), address, 40*money.OneUSDT)
	e.tick()
	e.assertStatus(tr, "pending")
	e.assertRisk(tr, "high")
	if n := e.count(`SELECT count(*) FROM refunds`); n != 0 {
		t.Errorf("%d refunds before confirmations, want 0", n)
	}

	e.sim.AddBlocks(4)
	e.tick()
	e.assertStatus(tr, "refunded")

	assertRefund := func() {
		t.Helper()
		var to, status string
		var amount int64
		err := e.db.QueryRow(t.Context(), `SELECT to_address, amount, status::text FROM refunds`).Scan(&to, &amount, &status)
		if err != nil {
			t.Fatalf("load refund: %v", err)
		}
		if to != blacklisted || money.Amount(amount) != 40*money.OneUSDT || status != "ready_to_send" {
			t.Errorf("refund = %s %d %s, want %s %d ready_to_send", to, amount, status, blacklisted, 40*money.OneUSDT)
		}
		if n := e.count(`SELECT count(*) FROM refunds`); n != 1 {
			t.Errorf("%d refunds, want 1", n)
		}
		e.assertBalances(map[ledger.Account]money.Amount{hold: 0, available: 0, ledger.Fees: 0, ledger.RefundPending: 40 * money.OneUSDT})
	}
	assertRefund()

	if _, err := e.db.Exec(t.Context(), resetForReprocessing); err != nil {
		t.Fatal(err)
	}
	restarted := e.newProcessor()
	for range 3 {
		if err := restarted.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	e.assertStatus(tr, "refunded")
	assertRefund()
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 2 {
		t.Errorf("%d ledger transactions, want 2", n)
	}
}

func TestAMLUnavailableKeepsFundsOnHold(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(0)
	address := e.invoice(pid)
	hold, available := ledger.PartnerHold(pid), ledger.PartnerAvailable(pid)

	e.aml.SetAvailability(false)
	tr := e.send(payer, address, 25*money.OneUSDT)
	e.tick()
	e.assertStatus(tr, "pending")
	e.assertRisk(tr, "")

	e.sim.AddBlocks(4)
	e.tick()
	e.assertStatus(tr, "confirmed")
	e.assertBalances(map[ledger.Account]money.Amount{hold: 25 * money.OneUSDT, available: 0})

	attemptsAndBackoff := func() (int, bool) {
		t.Helper()
		var attempts int
		var waiting bool
		err := e.db.QueryRow(t.Context(),
			`SELECT a.attempts, a.next_attempt_at > now() + interval '59 minutes' FROM deposit_aml_checks a JOIN deposits d ON d.id = a.deposit_id WHERE d.tx_hash = $1`, tr.TxHash,
		).Scan(&attempts, &waiting)
		if err != nil {
			t.Fatal(err)
		}
		return attempts, waiting
	}
	if attempts, waiting := attemptsAndBackoff(); attempts != 1 || !waiting {
		t.Errorf("attempts = %d, backing off = %v; want 1, true", attempts, waiting)
	}

	e.aml.SetAvailability(true)
	e.tick()
	if attempts, _ := attemptsAndBackoff(); attempts != 1 {
		t.Errorf("AML retried during backoff: attempts = %d", attempts)
	}
	e.assertStatus(tr, "confirmed")
	e.assertBalances(map[ledger.Account]money.Amount{hold: 25 * money.OneUSDT, available: 0})

	if _, err := e.db.Exec(t.Context(), `UPDATE deposit_aml_checks SET next_attempt_at = now() WHERE risk = ''`); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.assertStatus(tr, "credited")
	e.assertBalances(map[ledger.Account]money.Amount{hold: 0, available: 25 * money.OneUSDT})
}

func TestReorgBeforeConfirmationsLeavesNoCredit(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(100)
	address := e.invoice(pid)

	tr := e.send(payer, address, 10*money.OneUSDT)
	e.sim.AddBlocks(2)
	e.tick()
	e.assertStatus(tr, "pending")

	if _, err := e.sim.Reorg(3); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.assertStatus(tr, "orphaned")

	e.sim.AddBlocks(10)
	for range 3 {
		e.tick()
	}
	e.assertStatus(tr, "orphaned")
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 0 {
		t.Errorf("%d ledger transactions after reorg, want 0", n)
	}
	e.assertBalances(map[ledger.Account]money.Amount{
		ledger.Custody:               0,
		ledger.PartnerHold(pid):      0,
		ledger.PartnerAvailable(pid): 0,
	})
}

func TestReprocessingAndRescanDoNotDuplicate(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(200)
	address := e.invoice(pid)
	want := map[ledger.Account]money.Amount{
		ledger.PartnerHold(pid):      0,
		ledger.PartnerAvailable(pid): 49 * money.OneUSDT,
		ledger.Fees:                  money.OneUSDT,
	}

	tr := e.send(payer, address, 50*money.OneUSDT)
	e.sim.AddBlocks(4)
	e.tick()
	e.assertStatus(tr, "credited")
	e.assertBalances(want)

	restarted := e.newProcessor()
	if _, err := e.db.Exec(t.Context(), `UPDATE chain_cursors SET next_block = 0, last_hash = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(t.Context(), resetForReprocessing); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := restarted.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	e.assertStatus(tr, "credited")
	if n := e.count(`SELECT count(*) FROM deposits`); n != 1 {
		t.Errorf("%d deposits after rescan, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 2 {
		t.Errorf("%d ledger transactions, want 2", n)
	}
	e.assertBalances(want)
}

func TestConcurrentProcessorsCreditOnce(t *testing.T) {
	e := newEnv(t)
	p1, p2 := e.partner(100), e.partner(250)
	var transfers []chain.Transfer
	for i := range 6 {
		pid := p1
		if i%2 == 1 {
			pid = p2
		}
		transfers = append(transfers, e.send(payer, e.invoice(pid), 10*money.OneUSDT))
	}
	e.sim.AddBlocks(5)
	e.step("scan", e.proc.ScanBlocks)

	var wg sync.WaitGroup
	for range 6 {
		p := e.newProcessor()
		wg.Go(func() {
			for range 5 {
				for _, run := range []func(context.Context) (int, error){p.ProcessConfirmations, p.ProcessAML, p.ProcessSettlement} {
					if _, err := run(context.Background()); err != nil {
						t.Error(err)
					}
				}
			}
		})
	}
	wg.Wait()

	for _, tr := range transfers {
		e.assertStatus(tr, "credited")
	}
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 12 {
		t.Errorf("%d ledger transactions, want 12", n)
	}
	e.assertBalances(map[ledger.Account]money.Amount{
		ledger.Custody:              60 * money.OneUSDT,
		ledger.PartnerAvailable(p1): 3 * 9_900_000,
		ledger.PartnerAvailable(p2): 3 * 9_750_000,
		ledger.Fees:                 3*100_000 + 3*250_000,
		ledger.PartnerHold(p1):      0,
		ledger.PartnerHold(p2):      0,
	})
}

func TestIgnoresTransfersToUnknownAddresses(t *testing.T) {
	e := newEnv(t)
	e.invoice(e.partner(100))

	e.send(payer, "0x2222222222222222222222222222222222222222", 5*money.OneUSDT)
	e.sim.AddBlocks(5)
	e.tick()

	if n := e.count(`SELECT count(*) FROM deposits`); n != 0 {
		t.Errorf("%d deposits for unknown address, want 0", n)
	}
	head, _ := e.sim.HeadBlock(t.Context())
	if n := e.count(`SELECT next_block FROM chain_cursors`); n != int(head)+1 {
		t.Errorf("cursor next_block = %d, want %d", n, head+1)
	}
}

func TestRunProcessesDepositInBackground(t *testing.T) {
	e := newEnv(t)
	e.cfg.PollInterval = 10 * time.Millisecond
	proc := e.newProcessor()
	pid := e.partner(0)
	tr := e.send(payer, e.invoice(pid), 7*money.OneUSDT)
	e.sim.AddBlocks(4)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		proc.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM deposits WHERE status = 'credited'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("deposit was not credited by Run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	e.assertStatus(tr, "credited")
	e.assertBalances(map[ledger.Account]money.Amount{ledger.PartnerAvailable(pid): 7 * money.OneUSDT})
}

func TestConfirmationsDoNotCallChain(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(0)
	first := e.send(payer, e.invoice(pid), money.OneUSDT)
	second := e.send(payer, e.invoice(pid), money.OneUSDT)
	e.sim.AddBlocks(2)
	e.step("scan", e.proc.ScanBlocks)
	e.chain.take()

	for range 3 {
		e.step("confirmations", e.proc.ProcessConfirmations)
	}
	e.assertStatus(first, "pending")

	e.sim.AddBlocks(2)
	e.step("scan", e.proc.ScanBlocks)
	e.chain.take()
	e.step("confirmations", e.proc.ProcessConfirmations)
	e.assertStatus(first, "confirmed")
	e.assertStatus(second, "confirmed")

	if calls := e.chain.take(); len(calls) != 0 {
		t.Errorf("chain calls from confirmations = %v, want none", calls)
	}
}

func TestConfirmationsCountScannedBlocksOnly(t *testing.T) {
	e := newEnv(t)
	e.cfg.ScanBatch = 2
	proc := e.newProcessor()
	tr := e.send(payer, e.invoice(e.partner(0)), money.OneUSDT)
	e.sim.AddBlocks(10)

	e.step("scan", proc.ScanBlocks)
	e.step("confirmations", proc.ProcessConfirmations)
	e.assertStatus(tr, "pending")

	e.step("scan", proc.ScanBlocks)
	e.step("confirmations", proc.ProcessConfirmations)
	e.assertStatus(tr, "pending")

	e.step("scan", proc.ScanBlocks)
	e.step("confirmations", proc.ProcessConfirmations)
	e.assertStatus(tr, "confirmed")
}

func TestIdleScanTouchesOnlyHeadAndCursor(t *testing.T) {
	e := newEnv(t)
	e.sim.AddBlocks(3)
	e.step("scan", e.proc.ScanBlocks)
	e.chain.take()

	e.step("scan", e.proc.ScanBlocks)
	if calls := e.chain.take(); calls["HeadBlock"] != 1 || calls["BlockHash"] != 1 || len(calls) != 2 {
		t.Errorf("idle scan chain calls = %v, want 1 HeadBlock and 1 BlockHash", calls)
	}
}

func TestScannerOrphansNotReadyDepositsOnReorg(t *testing.T) {
	e := newEnv(t)
	tr := e.send(payer, e.invoice(e.partner(0)), money.OneUSDT)
	e.sim.AddBlocks(1)
	e.step("scan", e.proc.ScanBlocks)

	if _, err := e.sim.Reorg(2); err != nil {
		t.Fatal(err)
	}
	e.step("confirmations", e.proc.ProcessConfirmations)
	e.assertStatus(tr, "pending")

	e.step("scan", e.proc.ScanBlocks)
	e.assertStatus(tr, "orphaned")
}

func TestSeparateScannerAndWorkers(t *testing.T) {
	e := newEnv(t)
	e.cfg.PollInterval = 10 * time.Millisecond
	pid := e.partner(100)
	tr := e.send(payer, e.invoice(pid), 20*money.OneUSDT)
	e.sim.AddBlocks(4)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { e.newProcessor().RunScanner(ctx) })
	for range 3 {
		w := e.newProcessor()
		wg.Go(func() { w.RunWorkers(ctx) })
	}

	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM deposits WHERE tx_hash = $1 AND status = 'credited'`, tr.TxHash) == 0 {
		if time.Now().After(deadline) {
			cancel()
			wg.Wait()
			t.Fatal("deposit was not credited by separate scanner and workers")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	e.assertBalances(map[ledger.Account]money.Amount{ledger.PartnerAvailable(pid): 19_800_000, ledger.Fees: 200_000})
}

func TestStagesRaceSettleOnce(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(100)
	var transfers []chain.Transfer
	for range 10 {
		transfers = append(transfers, e.send(payer, e.invoice(pid), 10*money.OneUSDT))
	}
	e.sim.AddBlocks(5)
	e.step("scan", e.proc.ScanBlocks)

	start := make(chan struct{})
	var wg sync.WaitGroup
	stages := []func(context.Context) (int, error){
		e.newProcessor().ProcessConfirmations,
		e.newProcessor().ProcessAML,
		e.newProcessor().ProcessSettlement,
		e.newProcessor().ProcessSettlement,
	}
	for _, run := range stages {
		wg.Go(func() {
			<-start
			for range 5 {
				if _, err := run(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	close(start)
	wg.Wait()
	e.step("confirmations", e.proc.ProcessConfirmations)
	e.step("aml", e.proc.ProcessAML)
	e.step("settlement", e.proc.ProcessSettlement)

	for _, tr := range transfers {
		e.assertStatus(tr, "credited")
	}
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 20 {
		t.Errorf("%d ledger transactions, want 20", n)
	}
	e.assertBalances(map[ledger.Account]money.Amount{
		ledger.PartnerHold(pid):      0,
		ledger.PartnerAvailable(pid): 10 * 9_900_000,
		ledger.Fees:                  10 * 100_000,
	})
}

func (e *env) corruptAccount(id string) {
	e.t.Helper()
	if _, err := e.db.Exec(e.t.Context(), `INSERT INTO ledger_accounts (id, kind) VALUES ($1, 'asset')`, id); err != nil {
		e.t.Fatalf("corrupt account %s: %v", id, err)
	}
}

func (e *env) assertNeedsReview(tr chain.Transfer, reason error) {
	e.t.Helper()
	e.assertStatus(tr, "needs_review")
	var got string
	if err := e.db.QueryRow(e.t.Context(), `SELECT review_reason FROM deposits WHERE tx_hash = $1`, tr.TxHash).Scan(&got); err != nil {
		e.t.Fatalf("review reason %s: %v", tr.TxHash, err)
	}
	if !strings.Contains(got, reason.Error()) {
		e.t.Errorf("review reason = %q, want it to contain %q", got, reason)
	}
}

func TestConfirmationFlagsRejectedHoldForReview(t *testing.T) {
	e := newEnv(t)
	good, bad := e.partner(0), e.partner(0)
	okTr := e.send(payer, e.invoice(good), money.OneUSDT)
	badTr := e.send(payer, e.invoice(bad), money.OneUSDT)
	e.sim.AddBlocks(5)
	e.step("scan", e.proc.ScanBlocks)
	e.corruptAccount(ledger.PartnerHold(bad).ID)

	n, err := e.proc.ProcessConfirmations(t.Context())
	if err != nil {
		t.Fatalf("ProcessConfirmations: %v", err)
	}
	if n != 2 {
		t.Errorf("processed %d deposits, want 2", n)
	}
	e.assertStatus(okTr, "confirmed")
	e.assertNeedsReview(badTr, ledger.ErrAccountKindMismatch)

	for range 3 {
		e.tick()
	}
	e.assertStatus(okTr, "credited")
	e.assertNeedsReview(badTr, ledger.ErrAccountKindMismatch)
	e.assertRisk(badTr, "")
	if n := e.count(`SELECT a.attempts FROM deposit_aml_checks a JOIN deposits d ON d.id = a.deposit_id WHERE d.tx_hash = $1`, badTr.TxHash); n != 0 {
		t.Errorf("AML attempted %d times for deposit under review, want 0", n)
	}
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 2 {
		t.Errorf("%d ledger transactions, want 2 for the good deposit only", n)
	}
	e.assertBalances(map[ledger.Account]money.Amount{ledger.PartnerAvailable(good): money.OneUSDT})
}

func TestSettlementFlagsRejectedTransactionForReview(t *testing.T) {
	e := newEnv(t)
	good, bad := e.partner(0), e.partner(0)
	okTr := e.send(payer, e.invoice(good), money.OneUSDT)
	badTr := e.send(payer, e.invoice(bad), money.OneUSDT)
	e.sim.AddBlocks(5)
	e.step("scan", e.proc.ScanBlocks)
	e.step("confirmations", e.proc.ProcessConfirmations)
	e.step("aml", e.proc.ProcessAML)
	e.corruptAccount(ledger.PartnerAvailable(bad).ID)

	n, err := e.proc.ProcessSettlement(t.Context())
	if err != nil {
		t.Fatalf("ProcessSettlement: %v", err)
	}
	if n != 2 {
		t.Errorf("processed %d deposits, want 2", n)
	}
	e.assertStatus(okTr, "credited")
	e.assertNeedsReview(badTr, ledger.ErrAccountKindMismatch)

	for range 3 {
		e.tick()
	}
	e.assertNeedsReview(badTr, ledger.ErrAccountKindMismatch)
	e.assertRisk(badTr, "low")
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 3 {
		t.Errorf("%d ledger transactions, want 3", n)
	}
	e.assertBalances(map[ledger.Account]money.Amount{
		ledger.PartnerAvailable(good): money.OneUSDT,
		ledger.PartnerHold(bad):       money.OneUSDT,
	})
}

func TestSettlementIsWokenWithoutPolling(t *testing.T) {
	e := newEnv(t)
	e.cfg.PollInterval = time.Hour
	proc := e.newProcessor()
	pid := e.partner(0)
	tr := e.send(payer, e.invoice(pid), 3*money.OneUSDT)
	e.sim.AddBlocks(4)
	e.step("scan", e.proc.ScanBlocks)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		proc.RunWorkers(ctx)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM deposits WHERE tx_hash = $1 AND status = 'credited'`, tr.TxHash) == 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("deposit was not credited without polling")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	e.assertBalances(map[ledger.Account]money.Amount{ledger.PartnerAvailable(pid): 3 * money.OneUSDT})
}

func TestConfirmationsRespectBatchSize(t *testing.T) {
	e := newEnv(t)
	e.cfg.BatchSize = 3
	proc := e.newProcessor()
	pid := e.partner(0)
	for range 7 {
		e.send(payer, e.invoice(pid), money.OneUSDT)
	}
	e.sim.AddBlocks(5)
	e.step("scan", proc.ScanBlocks)

	for _, want := range []int{3, 3, 1, 0} {
		n, err := proc.ProcessConfirmations(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("confirmed %d deposits, want %d", n, want)
		}
	}
}

func TestConfirmationsSkipLockedDeposits(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(0)
	locked := e.send(payer, e.invoice(pid), money.OneUSDT)
	free := e.send(payer, e.invoice(pid), money.OneUSDT)
	e.sim.AddBlocks(5)
	e.step("scan", e.proc.ScanBlocks)

	tx, err := e.db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM chain_cursors FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM deposits WHERE tx_hash = $1 FOR UPDATE`, locked.TxHash); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	n, err := e.proc.ProcessConfirmations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("confirmed %d deposits, want 1", n)
	}
	e.assertStatus(free, "confirmed")
	e.assertStatus(locked, "pending")
}

func TestFixedPlusPercentFee(t *testing.T) {
	e := newEnv(t)
	pid := e.partnerWithFee(100, 500_000)
	big := e.send(payer, e.invoice(pid), 100*money.OneUSDT)
	small := e.send(payer, e.invoice(pid), 300_000)
	e.sim.AddBlocks(4)
	e.tick()

	e.assertStatus(big, "credited")
	e.assertStatus(small, "credited")
	e.assertBalances(map[ledger.Account]money.Amount{
		ledger.PartnerHold(pid):      0,
		ledger.PartnerAvailable(pid): 98_500_000,
		ledger.Fees:                  1_500_000 + 300_000,
	})

	if _, err := e.db.Exec(t.Context(), `UPDATE partners SET fee_bps = 0, fee_fixed = 0 WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	var bps, fixed int64
	err := e.db.QueryRow(t.Context(), `SELECT fee_bps, fee_fixed FROM deposits WHERE tx_hash = $1`, big.TxHash).Scan(&bps, &fixed)
	if err != nil {
		t.Fatal(err)
	}
	if bps != 100 || fixed != 500_000 {
		t.Errorf("deposit fee snapshot = %d bps + %d, want 100 bps + 500000", bps, fixed)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := processing.DefaultConfig()
	cfg.Confirmations = 0
	cfg.AMLLease = cfg.AMLTimeout
	if _, err := processing.New(nil, nil, nil, cfg, nil); err == nil {
		t.Error("New accepted invalid config")
	}
}

func TestReincludedTransferIsCreditedOnce(t *testing.T) {
	e := newEnv(t)
	pid := e.partner(0)
	tr := e.send(payer, e.invoice(pid), 5*money.OneUSDT)
	e.sim.AddBlocks(1)
	e.tick()
	e.assertStatus(tr, "pending")

	if _, err := e.sim.ReorgWithReinclusion(2); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.assertStatus(tr, "pending")
	var block int64
	var hash string
	if err := e.db.QueryRow(t.Context(), `SELECT block_number, block_hash FROM deposits WHERE tx_hash = $1`, tr.TxHash).Scan(&block, &hash); err != nil {
		t.Fatal(err)
	}
	if hash == tr.BlockHash {
		t.Error("deposit still points at the orphaned block")
	}

	e.sim.AddBlocks(5)
	e.tick()
	e.tick()
	e.assertStatus(tr, "credited")
	if n := e.count(`SELECT count(*) FROM deposits`); n != 1 {
		t.Errorf("%d deposits, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM ledger_transactions`); n != 2 {
		t.Errorf("%d ledger transactions, want 2", n)
	}
	e.assertBalances(map[ledger.Account]money.Amount{ledger.PartnerAvailable(pid): 5 * money.OneUSDT, ledger.PartnerHold(pid): 0})
}
