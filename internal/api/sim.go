package api

import (
	"net/http"

	"test-task-dellos/internal/api/apidb"
	"test-task-dellos/internal/money"
	"test-task-dellos/internal/processing/deposit"
)

type simTransferRequest struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
}

type simTransferResponse struct {
	TxHash      string `json:"tx_hash"`
	BlockNumber uint64 `json:"block_number"`
	BlockHash   string `json:"block_hash"`
}

type simCountRequest struct {
	Count int `json:"count"`
}

type simReorgRequest struct {
	Depth     int  `json:"depth"`
	Reinclude bool `json:"reinclude"`
}

type simRescanRequest struct {
	FromBlock int64 `json:"from_block"`
}

type simRescanResponse struct {
	Rewound bool `json:"rewound"`
}

type simAMLRequest struct {
	Available bool `json:"available"`
}

type simHeadResponse struct {
	Head uint64 `json:"head"`
}

func (s *Server) saveSim(w http.ResponseWriter) bool {
	if s.saveSimState == nil {
		return true
	}
	if err := s.saveSimState(); err != nil {
		s.internalError(w, "save simulator state", err)
		return false
	}
	return true
}

func (s *Server) simTransfer(w http.ResponseWriter, r *http.Request) {
	var req simTransferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	amount, err := money.Parse(req.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.From == "" || req.To == "" {
		writeError(w, http.StatusBadRequest, "from and to are required")
		return
	}
	tr, err := s.sim.SendTransfer(req.From, req.To, amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.saveSim(w) {
		return
	}
	writeJSON(w, http.StatusCreated, simTransferResponse{TxHash: tr.TxHash, BlockNumber: tr.BlockNumber, BlockHash: tr.BlockHash})
}

func (s *Server) simBlocks(w http.ResponseWriter, r *http.Request) {
	var req simCountRequest
	if err := decodeJSON(r, &req); err != nil || req.Count < 1 || req.Count > 10_000 {
		writeError(w, http.StatusBadRequest, "count must be between 1 and 10000")
		return
	}
	head := s.sim.AddBlocks(req.Count)
	if !s.saveSim(w) {
		return
	}
	writeJSON(w, http.StatusOK, simHeadResponse{Head: head})
}

func (s *Server) simReorg(w http.ResponseWriter, r *http.Request) {
	var req simReorgRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	reorg := s.sim.Reorg
	if req.Reinclude {
		reorg = s.sim.ReorgWithReinclusion
	}
	head, err := reorg(req.Depth)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.saveSim(w) {
		return
	}
	writeJSON(w, http.StatusOK, simHeadResponse{Head: head})
}

func (s *Server) simRescan(w http.ResponseWriter, r *http.Request) {
	var req simRescanRequest
	if err := decodeJSON(r, &req); err != nil || req.FromBlock < 0 {
		writeError(w, http.StatusBadRequest, "from_block must be a non-negative integer")
		return
	}
	n, err := apidb.New(s.db).RewindCursor(r.Context(), apidb.RewindCursorParams{Name: deposit.CursorName, NextBlock: req.FromBlock})
	if err != nil {
		s.internalError(w, "rewind cursor", err)
		return
	}
	s.log.Warn("scanner cursor rewound", "from_block", req.FromBlock, "rewound", n > 0)
	writeJSON(w, http.StatusOK, simRescanResponse{Rewound: n > 0})
}

func (s *Server) simAML(w http.ResponseWriter, r *http.Request) {
	var req simAMLRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.aml.SetAvailability(req.Available)
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) simHead(w http.ResponseWriter, r *http.Request) {
	head, err := s.sim.HeadBlock(r.Context())
	if err != nil {
		s.internalError(w, "sim head", err)
		return
	}
	writeJSON(w, http.StatusOK, simHeadResponse{Head: head})
}
