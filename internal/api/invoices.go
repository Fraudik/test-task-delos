package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"test-task-dellos/internal/api/apidb"
	"test-task-dellos/internal/ledger"
	"test-task-dellos/internal/money"
)

const IdempotencyKeyHeader = "Idempotency-Key"

type createInvoiceRequest struct {
	Amount string `json:"amount"`
}

type invoiceResponse struct {
	ID        int64             `json:"id"`
	Address   string            `json:"address"`
	Amount    string            `json:"amount"`
	Received  string            `json:"received"`
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	Deposits  []depositResponse `json:"deposits"`
}

type depositResponse struct {
	TxHash      string    `json:"tx_hash"`
	LogIndex    int32     `json:"log_index"`
	BlockNumber int64     `json:"block_number"`
	BlockHash   string    `json:"block_hash"`
	Amount      string    `json:"amount"`
	Status      string    `json:"status"`
	DetectedAt  time.Time `json:"detected_at"`
}

type balanceResponse struct {
	Currency  string `json:"currency"`
	Available string `json:"available"`
	OnHold    string `json:"on_hold"`
}

type invoiceRow struct {
	ID        int64
	Address   string
	Amount    money.Amount
	Status    apidb.InvoiceStatus
	CreatedAt pgtype.Timestamptz
}

func (s *Server) createInvoice(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(IdempotencyKeyHeader)
	if key == "" || len(key) > 255 {
		writeError(w, http.StatusBadRequest, IdempotencyKeyHeader+" header is required (1..255 characters)")
		return
	}
	var req createInvoiceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be {\"amount\": \"<USDT>\"}: "+err.Error())
		return
	}
	amount, err := money.Parse(req.Amount)
	if err != nil || amount <= 0 {
		writeError(w, http.StatusBadRequest, "amount must be a positive USDT amount with up to 6 decimals")
		return
	}

	p := partnerFrom(r.Context())
	q := apidb.New(s.db)
	created, err := q.CreateInvoice(r.Context(), apidb.CreateInvoiceParams{
		PartnerID:      p.ID,
		Address:        newAddress(),
		IdempotencyKey: key,
		Amount:         amount,
	})
	if err == nil {
		s.log.Info("invoice created", "partner_id", p.ID, "invoice_id", created.ID, "address", created.Address, "amount", created.Amount.String())
		s.writeInvoice(w, r, http.StatusCreated, invoiceRow(created))
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.internalError(w, "create invoice", err)
		return
	}

	existing, err := q.InvoiceByIdempotencyKey(r.Context(), apidb.InvoiceByIdempotencyKeyParams{PartnerID: p.ID, IdempotencyKey: key})
	if err != nil {
		s.internalError(w, "load idempotent invoice", err)
		return
	}
	if existing.Amount != amount {
		writeError(w, http.StatusConflict, IdempotencyKeyHeader+" was already used for an invoice of "+existing.Amount.String()+" USDT")
		return
	}
	s.writeInvoice(w, r, http.StatusOK, invoiceRow(existing))
}

func (s *Server) getInvoice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "invoice not found")
		return
	}
	row, err := apidb.New(s.db).GetInvoice(r.Context(), apidb.GetInvoiceParams{ID: id, PartnerID: partnerFrom(r.Context()).ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "invoice not found")
		return
	}
	if err != nil {
		s.internalError(w, "get invoice", err)
		return
	}
	s.writeInvoice(w, r, http.StatusOK, invoiceRow(row))
}

func (s *Server) writeInvoice(w http.ResponseWriter, r *http.Request, status int, inv invoiceRow) {
	rows, err := apidb.New(s.db).InvoiceDeposits(r.Context(), inv.ID)
	if err != nil {
		s.internalError(w, "load invoice deposits", err)
		return
	}
	resp := invoiceResponse{
		ID:        inv.ID,
		Address:   inv.Address,
		Amount:    inv.Amount.String(),
		CreatedAt: inv.CreatedAt.Time.UTC(),
		Deposits:  []depositResponse{},
	}
	received := money.Amount(0)
	for _, d := range rows {
		if countsTowardInvoice(d) {
			received += d.Amount
		}
		resp.Deposits = append(resp.Deposits, depositResponse{
			TxHash:      d.TxHash,
			LogIndex:    d.LogIndex,
			BlockNumber: d.BlockNumber,
			BlockHash:   d.BlockHash,
			Amount:      d.Amount.String(),
			Status:      string(d.Status),
			DetectedAt:  d.CreatedAt.Time.UTC(),
		})
	}
	resp.Received = received.String()
	resp.Status = invoiceStatus(inv, rows, received)
	writeJSON(w, status, resp)
}

func countsTowardInvoice(d apidb.InvoiceDepositsRow) bool {
	return d.Status == apidb.DepositStatusCredited || (d.Status == apidb.DepositStatusConfirmed && d.AmlRisk == "low")
}

func invoiceStatus(inv invoiceRow, deposits []apidb.InvoiceDepositsRow, received money.Amount) string {
	if inv.Status == apidb.InvoiceStatusPaid {
		return "paid"
	}
	inFlight := false
	for _, d := range deposits {
		switch {
		case d.Status == apidb.DepositStatusNeedsReview:
			return "needs_review"
		case d.Status == apidb.DepositStatusPending, d.Status == apidb.DepositStatusConfirmed && d.AmlRisk == "":
			inFlight = true
		}
	}
	switch {
	case inFlight || received >= inv.Amount:
		return "processing"
	case received > 0:
		return "partially_paid"
	default:
		return "awaiting_payment"
	}
}

func (s *Server) getBalance(w http.ResponseWriter, r *http.Request) {
	p := partnerFrom(r.Context())
	available, err := ledger.Balance(r.Context(), s.db, ledger.PartnerAvailable(p.ID))
	if err != nil {
		s.internalError(w, "balance", err)
		return
	}
	hold, err := ledger.Balance(r.Context(), s.db, ledger.PartnerHold(p.ID))
	if err != nil {
		s.internalError(w, "hold balance", err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResponse{Currency: "USDT", Available: available.String(), OnHold: hold.String()})
}

func newAddress() string {
	b := make([]byte, 20)
	rand.Read(b)
	return "0x" + hex.EncodeToString(b)
}
