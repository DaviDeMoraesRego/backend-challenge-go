// Testes das consultas do §9: extrato com paginação por cursor, reconciliação e
// isolamento entre provedores (§2/§13).
package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

func TestGetWallet(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "100.00")

	view, err := h.getWallet.Execute(context.Background(), ports.GetWalletQuery{
		Principal: internalPrincipal(),
		WalletID:  seeded.WalletID.String(),
	})
	if err != nil {
		t.Fatalf("getWallet.Execute unexpected error: %v", err)
	}

	if view.WalletID != seeded.WalletID {
		t.Errorf("WalletID = %q, want %q", view.WalletID, seeded.WalletID)
	}
	if got := view.Balance.String(); got != "100.00" {
		t.Errorf("Balance = %q, want %q", got, "100.00")
	}
	if view.Version != 1 {
		t.Errorf("Version = %d, want 1", view.Version)
	}
}

// TestGetWallet_ForbiddenForProvider: operações de carteira são restritas ao serviço
// interno (§2).
func TestGetWallet_ForbiddenForProvider(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "100.00")

	_, err := h.getWallet.Execute(context.Background(), ports.GetWalletQuery{
		Principal: providerPrincipal(testProviderA),
		WalletID:  seeded.WalletID.String(),
	})
	if !errors.Is(err, ports.ErrForbidden) {
		t.Errorf("error = %v, want ErrForbidden", err)
	}
}

func TestGetWallet_NotFound(t *testing.T) {
	h := newHarness(t)

	_, err := h.getWallet.Execute(context.Background(), ports.GetWalletQuery{
		Principal: internalPrincipal(),
		WalletID:  "id-inexistente",
	})
	if !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestGetLedger_CursorPagination: cursor = id do último lançamento entregue; "limit+1"
// sinaliza a próxima página sem COUNT(*).
func TestGetLedger_CursorPagination(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "100.00")
	h.bet(t, seeded, "10.00", "bet-1")
	h.bet(t, seeded, "20.00", "bet-2")
	// Total de lançamentos: abertura + 2 apostas = 3.

	first, err := h.getLedger.Execute(context.Background(), ports.GetLedgerQuery{
		Principal: internalPrincipal(),
		WalletID:  seeded.WalletID.String(),
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("getLedger.Execute unexpected error: %v", err)
	}

	if len(first.Entries) != 2 {
		t.Fatalf("primeira pagina = %d entradas, want 2", len(first.Entries))
	}
	if first.NextCursor == 0 {
		t.Fatalf("NextCursor = 0, want o id do ultimo lancamento (ha proxima pagina)")
	}
	if first.NextCursor != first.Entries[1].ID {
		t.Errorf("NextCursor = %d, want %d (id do ultimo entregue)", first.NextCursor, first.Entries[1].ID)
	}

	second, err := h.getLedger.Execute(context.Background(), ports.GetLedgerQuery{
		Principal: internalPrincipal(),
		WalletID:  seeded.WalletID.String(),
		Cursor:    first.NextCursor,
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("getLedger.Execute unexpected error: %v", err)
	}

	if len(second.Entries) != 1 {
		t.Fatalf("segunda pagina = %d entradas, want 1", len(second.Entries))
	}
	// Última página: não há mais cursor.
	if second.NextCursor != 0 {
		t.Errorf("NextCursor = %d, want 0 (ultima pagina)", second.NextCursor)
	}
	// Nenhum lançamento repetido entre as páginas: é isto que o cursor por id
	// garante e o OFFSET não garantiria.
	if second.Entries[0].ID <= first.Entries[1].ID {
		t.Errorf("segunda pagina sobrepos a primeira: %d <= %d", second.Entries[0].ID, first.Entries[1].ID)
	}
}

func TestGetLedger_AppliesLimitCeiling(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "100.00")

	// Pedir um limite absurdo não pode varrer a tabela inteira: a aplicação
	// aplica um teto.
	page, err := h.getLedger.Execute(context.Background(), ports.GetLedgerQuery{
		Principal: internalPrincipal(),
		WalletID:  seeded.WalletID.String(),
		Limit:     1_000_000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Entries) > maxLedgerLimit {
		t.Errorf("entradas = %d, want no maximo %d", len(page.Entries), maxLedgerLimit)
	}
}

func TestGetLedger_WalletNotFound(t *testing.T) {
	h := newHarness(t)

	_, err := h.getLedger.Execute(context.Background(), ports.GetLedgerQuery{
		Principal: internalPrincipal(),
		WalletID:  "id-inexistente",
	})
	if !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestGetTransaction_ProviderIsolation(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "100.00")
	result := h.placeOK(t, h.operation(t, seeded.WalletID, "BET", "25.00", "tx-1"))

	t.Run("proprio_provedor_ve", func(t *testing.T) {
		view, err := h.getTransaction.Execute(context.Background(), ports.GetTransactionQuery{
			Principal:     providerPrincipal(testProviderA),
			TransactionID: result.TransactionID.String(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if view.ProviderID.String() != testProviderA {
			t.Errorf("ProviderID = %q, want %q", view.ProviderID, testProviderA)
		}
	})

	t.Run("outro_provedor_nao_ve", func(t *testing.T) {
		_, err := h.getTransaction.Execute(context.Background(), ports.GetTransactionQuery{
			Principal:     providerPrincipal(testProviderB),
			TransactionID: result.TransactionID.String(),
		})
		// 403, sem expor nenhum dado da transação de outro provedor.
		if !errors.Is(err, ports.ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
	})

	t.Run("servico_interno_ve_qualquer", func(t *testing.T) {
		if _, err := h.getTransaction.Execute(context.Background(), ports.GetTransactionQuery{
			Principal:     internalPrincipal(),
			TransactionID: result.TransactionID.String(),
		}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestGetTransactionByExternalID(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "100.00")
	h.placeOK(t, h.operation(t, seeded.WalletID, "BET", "25.00", "tx-1"))

	t.Run("proprio_provedor", func(t *testing.T) {
		view, err := h.getByExternal.Execute(context.Background(), ports.GetTransactionByExternalIDQuery{
			Principal:             providerPrincipal(testProviderA),
			ProviderID:            testProviderA,
			ExternalTransactionID: "tx-1",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if view.ExternalTransactionID.String() != "tx-1" {
			t.Errorf("ExternalTransactionID = %q, want %q", view.ExternalTransactionID, "tx-1")
		}
	})

	t.Run("provider_da_rota_diferente_do_token", func(t *testing.T) {
		// O token é do provider-a, mas a ROTA pede o provider-b: 403.
		_, err := h.getByExternal.Execute(context.Background(), ports.GetTransactionByExternalIDQuery{
			Principal:             providerPrincipal(testProviderA),
			ProviderID:            testProviderB,
			ExternalTransactionID: "tx-1",
		})
		if !errors.Is(err, ports.ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
	})
}

func TestReconcileWallet_Consistent(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "1000.00")
	h.bet(t, seeded, "25.00", "bet-1")

	result, err := h.reconcile.Execute(context.Background(), ports.ReconcileWalletQuery{
		Principal: internalPrincipal(),
		WalletID:  seeded.WalletID.String(),
	})
	if err != nil {
		t.Fatalf("reconcile.Execute unexpected error: %v", err)
	}

	// Cenário do exemplo do §9: abertura de 1000.00 e aposta de 25.00.
	if got := result.StoredBalance.String(); got != "975.00" {
		t.Errorf("StoredBalance = %q, want %q", got, "975.00")
	}
	if got := result.CalculatedBalance.String(); got != "975.00" {
		t.Errorf("CalculatedBalance = %q, want %q", got, "975.00")
	}
	if got := result.Difference.String(); got != "0.00" {
		t.Errorf("Difference = %q, want %q (armazenado - reconstruido)", got, "0.00")
	}
	if !result.Consistent {
		t.Errorf("Consistent = false, want true")
	}
	// `checkedEntries` inclui a abertura (§9).
	if result.CheckedEntries != 2 {
		t.Errorf("CheckedEntries = %d, want 2", result.CheckedEntries)
	}
}

// TestReconcileWallet_DetectsDivergence: a reconciliação reporta a divergência, não a corrige.
func TestReconcileWallet_DetectsDivergence(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "1000.00")

	// Saldo armazenado divergente do ledger, simulando corrupção de dados.
	h.tamperWalletBalance(t, seeded.WalletID, "1234.56")

	result, err := h.reconcile.Execute(context.Background(), ports.ReconcileWalletQuery{
		Principal: internalPrincipal(),
		WalletID:  seeded.WalletID.String(),
	})
	if err != nil {
		t.Fatalf("reconcile.Execute unexpected error: %v", err)
	}

	if result.Consistent {
		t.Fatalf("Consistent = true, want false")
	}
	if got := result.StoredBalance.String(); got != "1234.56" {
		t.Errorf("StoredBalance = %q, want %q", got, "1234.56")
	}
	if got := result.CalculatedBalance.String(); got != "1000.00" {
		t.Errorf("CalculatedBalance = %q, want %q", got, "1000.00")
	}
	// difference = armazenado - reconstruido = 1234.56 - 1000.00 = 234.56
	if got := result.Difference.String(); got != "234.56" {
		t.Errorf("Difference = %q, want %q", got, "234.56")
	}
	// E a reconciliação NÃO altera nada (§9).
	if got := h.balanceOf(t, seeded.WalletID); got != "1234.56" {
		t.Errorf("saldo apos reconciliar = %q: a reconciliacao NAO pode alterar o saldo", got)
	}
}

func TestReconcileWallet_ForbiddenForProvider(t *testing.T) {
	h := newHarness(t)
	seeded := h.seedWallet(t, "100.00")

	_, err := h.reconcile.Execute(context.Background(), ports.ReconcileWalletQuery{
		Principal: providerPrincipal(testProviderA),
		WalletID:  seeded.WalletID.String(),
	})
	if !errors.Is(err, ports.ErrForbidden) {
		t.Errorf("error = %v, want ErrForbidden", err)
	}
}

// tamperWalletBalance substitui a carteira por uma versão cujo saldo não corresponde
// ao ledger, simulando corrupção de dados.
func (h *harness) tamperWalletBalance(t *testing.T, id wallet.ID, amount string) {
	t.Helper()

	original, err := (&fakeWalletRepository{store: h.store}).FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID unexpected error: %v", err)
	}
	divergentBalance, err := money.Parse(amount, money.BRL)
	if err != nil {
		t.Fatalf("money.Parse unexpected error: %v", err)
	}

	divergent, err := wallet.Rehydrate(
		id, original.PlayerID(), divergentBalance, original.Version(), original.CreatedAt(), original.UpdatedAt())
	if err != nil {
		t.Fatalf("Rehydrate unexpected error: %v", err)
	}

	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.wallets[id] = divergent
}
