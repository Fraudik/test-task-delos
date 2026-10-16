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
)

const IdempotencyKeyHeader = "Idempotency-Key"

type invoiceResponse struct {
	ID        int64             `json:"id"`
	Address   string            `json:"address"`
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

func (s *Server) createInvoice(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(IdempotencyKeyHeader)
	if key == "" || len(key) > 255 {
		writeError(w, http.StatusBadRequest, IdempotencyKeyHeader+" header is required (1..255 characters)")
		return
	}
	p := partnerFrom(r.Context())
	q := apidb.New(s.db)

	row, err := q.CreateInvoice(r.Context(), apidb.CreateInvoiceParams{PartnerID: p.ID, Address: newAddress(), IdempotencyKey: key})
	status := http.StatusCreated
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := q.InvoiceByIdempotencyKey(r.Context(), apidb.InvoiceByIdempotencyKeyParams{PartnerID: p.ID, IdempotencyKey: key})
		if err != nil {
			s.internalError(w, "load idempotent invoice", err)
			return
		}
		row = apidb.CreateInvoiceRow(existing)
		status = http.StatusOK
	} else if err != nil {
		s.internalError(w, "create invoice", err)
		return
	} else {
		s.log.Info("invoice created", "partner_id", p.ID, "invoice_id", row.ID, "address", row.Address)
	}
	s.writeInvoice(w, r, status, row.ID, row.Address, row.CreatedAt)
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
	s.writeInvoice(w, r, http.StatusOK, row.ID, row.Address, row.CreatedAt)
}

func (s *Server) writeInvoice(w http.ResponseWriter, r *http.Request, status int, id int64, address string, createdAt pgtype.Timestamptz) {
	rows, err := apidb.New(s.db).InvoiceDeposits(r.Context(), id)
	if err != nil {
		s.internalError(w, "load invoice deposits", err)
		return
	}
	resp := invoiceResponse{ID: id, Address: address, CreatedAt: createdAt.Time.UTC(), Deposits: []depositResponse{}}
	statuses := make([]apidb.DepositStatus, len(rows))
	for i, d := range rows {
		statuses[i] = d.Status
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
	resp.Status = invoiceStatus(statuses)
	writeJSON(w, status, resp)
}

func invoiceStatus(deposits []apidb.DepositStatus) string {
	seen := make(map[apidb.DepositStatus]bool, len(deposits))
	for _, d := range deposits {
		seen[d] = true
	}
	switch {
	case seen[apidb.DepositStatusNeedsReview]:
		return "needs_review"
	case seen[apidb.DepositStatusPending] || seen[apidb.DepositStatusConfirmed]:
		return "processing"
	case seen[apidb.DepositStatusCredited]:
		return "paid"
	case seen[apidb.DepositStatusRefunded]:
		return "refunded"
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
