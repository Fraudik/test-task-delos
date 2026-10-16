package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/aml/mock"
	"test-task-dellos/internal/api/apidb"
	"test-task-dellos/internal/chain/simulator"
)

const APIKeyHeader = "X-API-Key"

type Server struct {
	db           *pgxpool.Pool
	sim          *simulator.Simulator
	saveSimState func() error
	aml          *mock.Mock
	log          *slog.Logger
}

type Option func(*Server)

func WithSimulator(sim *simulator.Simulator, aml *mock.Mock, saveState func() error) Option {
	return func(s *Server) {
		s.sim = sim
		s.aml = aml
		s.saveSimState = saveState
	}
}

func New(db *pgxpool.Pool, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{db: db, log: log.With("component", "api")}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/invoices", s.authenticated(s.createInvoice))
	mux.Handle("GET /v1/invoices/{id}", s.authenticated(s.getInvoice))
	mux.Handle("GET /v1/balance", s.authenticated(s.getBalance))
	mux.HandleFunc("GET /openapi.yaml", serveOpenAPI)
	mux.HandleFunc("GET /docs", serveDocs)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	if s.sim != nil {
		mux.HandleFunc("POST /sim/transfers", s.simTransfer)
		mux.HandleFunc("POST /sim/blocks", s.simBlocks)
		mux.HandleFunc("POST /sim/reorg", s.simReorg)
		mux.HandleFunc("POST /sim/rescan", s.simRescan)
		mux.HandleFunc("PUT /sim/aml", s.simAML)
		mux.HandleFunc("GET /sim/head", s.simHead)
	}
	return mux
}

func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type partnerKey struct{}

type partner struct {
	ID   int64
	Name string
}

func partnerFrom(ctx context.Context) partner {
	return ctx.Value(partnerKey{}).(partner)
}

func (s *Server) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(APIKeyHeader)
		if key == "" {
			writeError(w, http.StatusUnauthorized, "missing "+APIKeyHeader+" header")
			return
		}
		row, err := apidb.New(s.db).PartnerByAPIKeyHash(r.Context(), HashAPIKey(key))
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		if err != nil {
			s.internalError(w, "authenticate", err)
			return
		}
		ctx := context.WithValue(r.Context(), partnerKey{}, partner{ID: row.ID, Name: row.Name})
		next(w, r.WithContext(ctx))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func (s *Server) internalError(w http.ResponseWriter, op string, err error) {
	s.log.Error("request failed", "op", op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
