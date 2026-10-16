package simulator

import (
	"errors"
	"sync"
	"testing"

	"test-task-dellos/internal/chain"
	"test-task-dellos/internal/money"
)

// Проверка соответствия интерфейсу
var _ chain.Client = (*Simulator)(nil)

func mustHead(t *testing.T, s *Simulator) uint64 {
	t.Helper()
	head, err := s.HeadBlock(t.Context())
	if err != nil {
		t.Fatalf("HeadBlock: %v", err)
	}
	return head
}

func mustHash(t *testing.T, s *Simulator, n uint64) string {
	t.Helper()
	h, err := s.BlockHash(t.Context(), n)
	if err != nil {
		t.Fatalf("BlockHash(%d): %v", n, err)
	}
	return h
}

func mustTransfers(t *testing.T, s *Simulator, from, to uint64) []chain.Transfer {
	t.Helper()
	trs, err := s.GetTransfers(t.Context(), from, to)
	if err != nil {
		t.Fatalf("GetTransfers(%d, %d): %v", from, to, err)
	}
	return trs
}

func mustSend(t *testing.T, s *Simulator, from, to string, value money.Amount) chain.Transfer {
	t.Helper()
	tr, err := s.SendTransfer(from, to, value)
	if err != nil {
		t.Fatalf("SendTransfer: %v", err)
	}
	return tr
}

func txHashes(trs []chain.Transfer) []string {
	out := make([]string, len(trs))
	for i, tr := range trs {
		out[i] = tr.TxHash
	}
	return out
}

func TestNew(t *testing.T) {
	s := New()

	if head := mustHead(t, s); head != 0 {
		t.Errorf("genesis number = %d, want 0", head)
	}
	if h := mustHash(t, s, 0); h == "" || h == zeroHash {
		t.Errorf("genesis hash = %q, want non-zero hash", h)
	}
	if p := s.blocks[0].parent; p != zeroHash {
		t.Errorf("genesis parent = %s, want zero hash", p)
	}
	if trs := mustTransfers(t, s, 0, 0); len(trs) != 0 {
		t.Errorf("genesis has %d transfers, want 0", len(trs))
	}
}

func TestHeadBlock(t *testing.T) {
	s := New()
	s.AddBlocks(3)
	if head := mustHead(t, s); head != 3 {
		t.Errorf("head = %d, want 3", head)
	}
	mustSend(t, s, "a", "b", 1)
	if head := mustHead(t, s); head != 4 {
		t.Errorf("head after transfer = %d, want 4", head)
	}
}

func TestBlockHash(t *testing.T) {
	s := New()
	s.AddBlocks(5)

	t.Run("unique hashes and linked chain", func(t *testing.T) {
		seen := make(map[string]uint64)
		for n := uint64(0); n <= 5; n++ {
			h := mustHash(t, s, n)
			if prev, ok := seen[h]; ok {
				t.Errorf("blocks %d and %d share hash %s", prev, n, h)
			}
			seen[h] = n
			if n > 0 && s.blocks[n].parent != mustHash(t, s, n-1) {
				t.Errorf("block %d parent does not match hash of block %d", n, n-1)
			}
		}
	})

	t.Run("stable between calls", func(t *testing.T) {
		if mustHash(t, s, 3) != mustHash(t, s, 3) {
			t.Error("BlockHash(3) changed between calls")
		}
	})

	t.Run("not found", func(t *testing.T) {
		for _, n := range []uint64{6, 100} {
			if _, err := s.BlockHash(t.Context(), n); !errors.Is(err, chain.ErrBlockNotFound) {
				t.Errorf("BlockHash(%d) error = %v, want ErrBlockNotFound", n, err)
			}
		}
	})
}

func TestBlockHashes(t *testing.T) {
	s := New()
	s.AddBlocks(4)

	hashes, err := s.BlockHashes(t.Context(), 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 3 {
		t.Fatalf("got %d hashes, want 3", len(hashes))
	}
	for i, h := range hashes {
		if want := mustHash(t, s, uint64(i+1)); h != want {
			t.Errorf("hash of block %d = %s, want %s", i+1, h, want)
		}
	}
	if _, err := s.BlockHashes(t.Context(), 3, 5); !errors.Is(err, chain.ErrBlockNotFound) {
		t.Errorf("range above head: err = %v, want ErrBlockNotFound", err)
	}
	if _, err := s.BlockHashes(t.Context(), 3, 2); err == nil {
		t.Error("inverted range: expected error")
	}
}

func TestGetTransfers(t *testing.T) {
	s := New()
	a := mustSend(t, s, "a", "b", 1) // block 1
	s.AddBlocks(1)                   // block 2, empty
	b := mustSend(t, s, "c", "d", 2) // block 3
	c := mustSend(t, s, "e", "f", 3) // block 4

	tests := []struct {
		name     string
		from, to uint64
		want     []string
	}{
		{"whole chain", 0, 4, []string{a.TxHash, b.TxHash, c.TxHash}},
		{"single block", 3, 3, []string{b.TxHash}},
		{"empty block", 2, 2, nil},
		{"range with empty block", 1, 3, []string{a.TxHash, b.TxHash}},
		{"to above head is clamped", 3, 100, []string{b.TxHash, c.TxHash}},
		{"from above head", 5, 10, nil},
		{"from greater than to", 4, 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := txHashes(mustTransfers(t, s, tt.from, tt.to))
			if len(got) != len(tt.want) {
				t.Fatalf("GetTransfers(%d, %d) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("GetTransfers(%d, %d)[%d] = %s, want %s", tt.from, tt.to, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSendTransfer(t *testing.T) {
	t.Run("mines transfer into new block", func(t *testing.T) {
		s := New()
		tr := mustSend(t, s, "0xfrom", "0xto", money.OneUSDT)

		if tr.From != "0xfrom" || tr.To != "0xto" {
			t.Errorf("from/to = %s/%s, want 0xfrom/0xto", tr.From, tr.To)
		}
		if tr.Value != money.OneUSDT {
			t.Errorf("value = %d, want %d", tr.Value, money.OneUSDT)
		}
		if tr.TxHash == "" {
			t.Error("empty tx hash")
		}
		if tr.BlockNumber != 1 || tr.LogIndex != 0 {
			t.Errorf("block/log = %d/%d, want 1/0", tr.BlockNumber, tr.LogIndex)
		}
		if h := mustHash(t, s, tr.BlockNumber); tr.BlockHash != h {
			t.Errorf("transfer block hash = %s, want %s", tr.BlockHash, h)
		}

		trs := mustTransfers(t, s, tr.BlockNumber, tr.BlockNumber)
		if len(trs) != 1 || trs[0] != tr {
			t.Fatalf("block transfers = %+v, want only %+v", trs, tr)
		}
	})

	t.Run("unique tx hashes", func(t *testing.T) {
		s := New()
		seen := make(map[string]bool)
		for range 10 {
			tr := mustSend(t, s, "a", "b", 1)
			if seen[tr.TxHash] {
				t.Fatalf("duplicate tx hash %s", tr.TxHash)
			}
			seen[tr.TxHash] = true
		}
	})

	t.Run("rejects non-positive value", func(t *testing.T) {
		for name, v := range map[string]money.Amount{
			"zero":     0,
			"negative": -1,
		} {
			s := New()
			if _, err := s.SendTransfer("a", "b", v); err == nil {
				t.Errorf("%s: expected error", name)
			}
			if head := mustHead(t, s); head != 0 {
				t.Errorf("%s: head = %d after rejected transfer, want 0", name, head)
			}
		}
	})
}

func TestAddBlocks(t *testing.T) {
	s := New()

	if got := s.AddBlocks(0); got != 0 {
		t.Errorf("AddBlocks(0) = %d, want 0", got)
	}
	if got := s.AddBlocks(4); got != 4 {
		t.Errorf("AddBlocks(4) = %d, want 4", got)
	}
	if got := s.AddBlocks(2); got != 6 {
		t.Errorf("AddBlocks(2) = %d, want 6", got)
	}
	if head := mustHead(t, s); head != 6 {
		t.Errorf("HeadBlock = %d, want 6", head)
	}
	if trs := mustTransfers(t, s, 0, 6); len(trs) != 0 {
		t.Errorf("empty blocks contain %d transfers", len(trs))
	}
}

func TestReorg(t *testing.T) {
	t.Run("replaces last blocks and drops their transfers", func(t *testing.T) {
		s := New()
		s.AddBlocks(2)
		tr := mustSend(t, s, "a", "b", 10) // block 3
		s.AddBlocks(1)                     // head 4

		before := make([]string, 5)
		for n := range before {
			before[n] = mustHash(t, s, uint64(n))
		}

		head, err := s.Reorg(2)
		if err != nil {
			t.Fatalf("Reorg: %v", err)
		}
		if head != 4 {
			t.Errorf("head after Reorg(2) = %d, want 4", head)
		}
		if got := mustHead(t, s); got != head {
			t.Errorf("HeadBlock = %d, want %d", got, head)
		}

		// Blocks below the fork point are untouched.
		for n := range 3 {
			if h := mustHash(t, s, uint64(n)); h != before[n] {
				t.Errorf("block %d changed: %s -> %s", n, before[n], h)
			}
		}

		// Reorged heights get new hashes and the new branch links to the fork point.
		for n := uint64(3); n <= 4; n++ {
			h := mustHash(t, s, n)
			if h == before[n] {
				t.Errorf("block %d hash unchanged after reorg", n)
			}
			if s.blocks[n].parent != mustHash(t, s, n-1) {
				t.Errorf("block %d is not linked to block %d", n, n-1)
			}
		}

		if h := mustHash(t, s, tr.BlockNumber); h == tr.BlockHash {
			t.Error("transfer block survived reorg")
		}
		if trs := mustTransfers(t, s, 0, head); len(trs) != 0 {
			t.Errorf("transfers after reorg = %v, want none", txHashes(trs))
		}
	})

	t.Run("keeps transfers below fork point", func(t *testing.T) {
		s := New()
		tr := mustSend(t, s, "a", "b", 5) // block 1
		s.AddBlocks(3)                    // head 4

		if _, err := s.Reorg(2); err != nil {
			t.Fatalf("Reorg: %v", err)
		}
		trs := mustTransfers(t, s, 0, mustHead(t, s))
		if len(trs) != 1 || trs[0].TxHash != tr.TxHash || trs[0].BlockHash != tr.BlockHash {
			t.Errorf("transfers after reorg = %+v, want only %s", trs, tr.TxHash)
		}
	})

	t.Run("invalid depth", func(t *testing.T) {
		s := New()
		s.AddBlocks(3) // 4 blocks including genesis

		for _, depth := range []int{-1, 0, 4, 10} {
			if _, err := s.Reorg(depth); err == nil {
				t.Errorf("Reorg(%d): expected error", depth)
			}
		}
		if head := mustHead(t, s); head != 3 {
			t.Errorf("head = %d after rejected reorgs, want 3", head)
		}
	})

	t.Run("genesis only", func(t *testing.T) {
		if _, err := New().Reorg(1); err == nil {
			t.Error("Reorg(1) on genesis-only chain: expected error")
		}
	})
}

func TestReturnedDataIsCopy(t *testing.T) {
	s := New()
	sent := mustSend(t, s, "a", "b", 100)

	// The returned slice must not share its backing array with the simulator.
	got := mustTransfers(t, s, sent.BlockNumber, sent.BlockNumber)
	got[0].Value = 2
	got[0].To = "mallory"
	got[0] = chain.Transfer{}

	trs := mustTransfers(t, s, sent.BlockNumber, sent.BlockNumber)
	if len(trs) != 1 || trs[0] != sent {
		t.Errorf("stored transfers = %+v, want only %+v", trs, sent)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New()
	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			for range 50 {
				if _, err := s.SendTransfer("a", "b", 1); err != nil {
					t.Error(err)
					return
				}
				s.AddBlocks(1)
			}
		})
		wg.Go(func() {
			for range 50 {
				head, err := s.HeadBlock(t.Context())
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := s.BlockHash(t.Context(), head); err != nil {
					t.Error(err)
					return
				}
				if _, err := s.GetTransfers(t.Context(), 0, head); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()

	if head := mustHead(t, s); head != 400 {
		t.Errorf("head = %d, want 400", head)
	}
	if trs := mustTransfers(t, s, 0, 400); len(trs) != 200 {
		t.Errorf("got %d transfers, want 200", len(trs))
	}
}

func TestReorgWithReinclusion(t *testing.T) {
	s := New()
	tr := mustSend(t, s, "a", "b", 7)
	s.AddBlocks(1)

	head, err := s.ReorgWithReinclusion(2)
	if err != nil {
		t.Fatal(err)
	}
	if head != 2 {
		t.Errorf("head = %d, want 2", head)
	}
	trs := mustTransfers(t, s, 0, head)
	if len(trs) != 1 {
		t.Fatalf("got %d transfers after reorg, want 1", len(trs))
	}
	got := trs[0]
	if got.TxHash != tr.TxHash || got.Value != tr.Value || got.From != tr.From || got.To != tr.To || got.LogIndex != 0 {
		t.Errorf("re-included transfer = %+v, want same tx as %+v", got, tr)
	}
	if got.BlockHash == tr.BlockHash || got.BlockHash != mustHash(t, s, got.BlockNumber) {
		t.Errorf("re-included transfer block hash = %s, want the new canonical block", got.BlockHash)
	}
}

func TestSaveAndLoadFile(t *testing.T) {
	path := t.TempDir() + "/chain.json"
	s := New()
	tr := mustSend(t, s, "a", "b", 42)
	s.AddBlocks(3)
	if err := s.SaveFile(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if mustHead(t, loaded) != mustHead(t, s) {
		t.Errorf("head after load = %d, want %d", mustHead(t, loaded), mustHead(t, s))
	}
	for n := uint64(0); n <= mustHead(t, s); n++ {
		if mustHash(t, loaded, n) != mustHash(t, s, n) {
			t.Errorf("block %d hash differs after load", n)
		}
	}
	trs := mustTransfers(t, loaded, 0, mustHead(t, loaded))
	if len(trs) != 1 || trs[0] != tr {
		t.Errorf("transfers after load = %+v, want %+v", trs, tr)
	}
	next := mustSend(t, loaded, "a", "b", 1)
	if next.TxHash == tr.TxHash {
		t.Error("loaded simulator reuses hashes")
	}

	fresh, err := LoadFile(t.TempDir() + "/missing.json")
	if err != nil || mustHead(t, fresh) != 0 {
		t.Errorf("LoadFile(missing) = head %d, %v; want a new chain", mustHead(t, fresh), err)
	}
}
