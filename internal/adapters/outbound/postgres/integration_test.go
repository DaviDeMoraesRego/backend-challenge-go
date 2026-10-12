//go:build integration

// Integração do adapter Postgres contra um Postgres real (build tag `integration`):
// constraints, locks, atomicidade, imutabilidade do ledger e reserva do outbox.
//
//	go test -tags=integration ./internal/adapters/outbound/postgres/ -v
//
// Usa o Postgres do docker compose (ou TEST_DATABASE_URL). CUIDADO: resetDatabase
// trunca as tabelas do domínio.
package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/outbound/postgres"
	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/ledger"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

const defaultTestDatabaseURL = "postgres://wager:wager@localhost:5432/wager?sslmode=disable"

// pool é compartilhado pela suíte: o isolamento vem do reset, não de um pool por teste.
var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = defaultTestDatabaseURL
	}

	ctx := context.Background()
	created, err := postgres.NewPool(ctx, postgres.PoolConfig{URL: url, MaxConns: 10})
	if err != nil {
		fmt.Fprintf(os.Stderr, "integracao: nao foi possivel conectar em %s: %v\n", url, err)
		fmt.Fprintf(os.Stderr, "dica: suba o ambiente com `docker compose up -d`\n")
		os.Exit(1)
	}
	pool = created

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// resetDatabase limpa o estado antes de cada teste.
func resetDatabase(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	// Os triggers de imutabilidade precisam ser desabilitados para o TRUNCATE do ledger
	// (§5.5). Todas as tabelas vão num único TRUNCATE sem CASCADE (o CASCADE propagaria
	// ao ledger protegido). Statements separados porque o pgx não aceita múltiplos
	// comandos no protocolo estendido.
	statements := []string{
		`ALTER TABLE wallet_ledger_entries DISABLE TRIGGER USER`,
		`TRUNCATE wallet_ledger_entries, outbox_events, inbox_messages, wager_transactions, wallets RESTART IDENTITY`,
		`ALTER TABLE wallet_ledger_entries ENABLE TRIGGER USER`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("reset falhou em %q: %v", statement, err)
		}
	}
}

var testNow = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, money.BRL)
	if err != nil {
		t.Fatalf("money.Parse(%q): %v", amount, err)
	}
	return m
}

func mustWallet(t *testing.T, id string, balance string) *wallet.Wallet {
	t.Helper()

	walletID, err := wallet.NewID(id)
	if err != nil {
		t.Fatalf("wallet.NewID: %v", err)
	}
	playerID, err := wallet.NewPlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	if err != nil {
		t.Fatalf("wallet.NewPlayerID: %v", err)
	}
	w, err := wallet.New(walletID, playerID, mustMoney(t, balance), testNow)
	if err != nil {
		t.Fatalf("wallet.New: %v", err)
	}
	return w
}

func mustTransactionID(t *testing.T, id string) transaction.ID {
	t.Helper()
	txID, err := transaction.NewID(id)
	if err != nil {
		t.Fatalf("transaction.NewID: %v", err)
	}
	return txID
}

// validHash monta um PayloadHash de 64 hexadecimais sem depender de `strings`.
func validHash(t *testing.T, seed byte) transaction.PayloadHash {
	t.Helper()
	raw := make([]byte, 64)
	for i := range raw {
		raw[i] = seed
	}
	hash, err := transaction.NewPayloadHash(string(raw))
	if err != nil {
		t.Fatalf("NewPayloadHash: %v", err)
	}
	return hash
}

// newProcessedBet monta uma BET externa já PROCESSED. A transação precisa existir porque
// wallet_ledger_entries.transaction_id é FK para wager_transactions.id (§6.4).
func newProcessedBet(t *testing.T, w *wallet.Wallet, txID transaction.ID, amount string, resultBalance string) *transaction.Transaction {
	t.Helper()

	bet, err := transaction.NewExternal(transaction.NewExternalParams{
		ID:                    txID,
		Kind:                  transaction.KindBet,
		PlayerID:              w.PlayerID(),
		WalletID:              w.ID(),
		Amount:                mustMoney(t, amount),
		ProviderID:            "provider-a",
		ExternalTransactionID: transaction.ExternalTransactionID(txID.String()),
		IdempotencyKey:        transaction.IdempotencyKey("provider-a:" + txID.String()),
		PayloadHash:           validHash(t, 'b'),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		CorrelationID:         "corr-1",
		Now:                   testNow,
	})
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	if err := bet.MarkProcessed(mustMoney(t, resultBalance), testNow); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	return bet
}

func TestWallet_InsertFindAndLock(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	created := mustWallet(t, "0192f291-27dd-7d3f-8071-5f8685deef37", "100.00")

	err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Wallets().Insert(txCtx, created)
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Leitura simples e leitura com LOCK devem devolver o MESMO estado.
	err = uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		found, err := scope.Wallets().FindByID(txCtx, created.ID())
		if err != nil {
			return err
		}
		if found.Balance().String() != "100.00" {
			t.Errorf("FindByID balance = %q, want %q", found.Balance().String(), "100.00")
		}

		locked, err := scope.Wallets().LockByID(txCtx, created.ID())
		if err != nil {
			return err
		}
		if locked.Version() != 1 {
			t.Errorf("LockByID version = %d, want 1", locked.Version())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("leitura com lock: %v", err)
	}
}

func TestWallet_DuplicatePlayerCurrency(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	first := mustWallet(t, "0192f291-27dd-7d3f-8071-5f8685deef37", "100.00")
	// MESMO jogador e MESMA moeda, identidade diferente: o UNIQUE (player_id,
	// currency) do schema precisa recusar (§6.2/§9).
	second := mustWallet(t, "0192f291-0000-7000-8000-000000000002", "50.00")

	err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		if err := scope.Wallets().Insert(txCtx, first); err != nil {
			return err
		}
		return scope.Wallets().Insert(txCtx, second)
	})
	if !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists", err)
	}
}

func TestWallet_UpdateRejectsStaleVersion(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	created := mustWallet(t, "0192f291-27dd-7d3f-8071-5f8685deef37", "100.00")
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Wallets().Insert(txCtx, created)
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Lê, debita e persiste (versão 1 -> 2).
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		w, err := scope.Wallets().LockByID(txCtx, created.ID())
		if err != nil {
			return err
		}
		if _, err := w.Debit(mustMoney(t, "25.00"), testNow); err != nil {
			return err
		}
		return scope.Wallets().Update(txCtx, w)
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Agora tentamos gravar um agregado OBSOLETO (versão 1, saldo 100.00).
	// A cláusula `AND version < $3` do UPDATE impede o lost update (§5.7):
	// a linha não é encontrada e a operação vira ErrNotFound.
	stale := mustWallet(t, "0192f291-27dd-7d3f-8071-5f8685deef37", "100.00")

	err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Wallets().Update(txCtx, stale)
	})
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound (lost update bloqueado)", err)
	}

	// E o saldo real permanece o da escrita válida.
	err = uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		found, err := scope.Wallets().FindByID(txCtx, created.ID())
		if err != nil {
			return err
		}
		if got := found.Balance().String(); got != "75.00" {
			t.Errorf("saldo = %q, want %q (o UPDATE obsoleto NAO pode ter passado)", got, "75.00")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verificacao: %v", err)
	}
}

// -----------------------------------------------------------------------------
// WagerTransaction
// -----------------------------------------------------------------------------

func TestTransaction_RoundTripWithNullableFields(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	created := mustWallet(t, "0192f291-27dd-7d3f-8071-5f8685deef37", "1000.00")
	openingID := mustTransactionID(t, "0192f298-345e-7e38-af88-e43f851a819d")

	opening, err := transaction.NewOpening(transaction.NewOpeningParams{
		ID:       openingID,
		PlayerID: created.PlayerID(),
		WalletID: created.ID(),
		Amount:   mustMoney(t, "1000.00"),
		Now:      testNow,
	})
	if err != nil {
		t.Fatalf("NewOpening: %v", err)
	}

	// Grava carteira + OPENING (todos os metadados externos são NULL aqui).
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		if err := scope.Wallets().Insert(txCtx, created); err != nil {
			return err
		}
		return scope.Transactions().Insert(txCtx, opening)
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Relê e confere que os campos NULÁVEIS voltaram corretamente vazios.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		loaded, err := scope.Transactions().FindByID(txCtx, openingID)
		if err != nil {
			return err
		}
		if loaded.State() != transaction.StateProcessed {
			t.Errorf("State = %q, want %q", loaded.State(), transaction.StateProcessed)
		}
		if loaded.ProviderID() != "" {
			t.Errorf("ProviderID = %q, want vazio (OPENING e interno)", loaded.ProviderID())
		}
		if loaded.IdempotencyKey() != "" {
			t.Errorf("IdempotencyKey = %q, want vazio", loaded.IdempotencyKey())
		}
		if result := loaded.ResultBalance(); !result.Valid || result.Value.String() != "1000.00" {
			t.Errorf("ResultBalance = %+v, want 1000.00", result)
		}
		if loaded.ProcessedAt().IsZero() {
			t.Errorf("ProcessedAt = zero, want preenchido")
		}
		return nil
	}); err != nil {
		t.Fatalf("relitura: %v", err)
	}

	// E a busca por (provider, externalId) de uma operação interna não a encontra,
	// comprovando o filtro `origin = 'EXTERNAL'`.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		_, err := scope.Transactions().FindByProviderAndExternalID(txCtx, "provider-a", "qualquer")
		if !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("busca externa: %v", err)
	}
}

func TestTransaction_UniqueConstraints(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	created := mustWallet(t, "0192f291-27dd-7d3f-8071-5f8685deef37", "1000.00")
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Wallets().Insert(txCtx, created)
	}); err != nil {
		t.Fatalf("Insert carteira: %v", err)
	}

	newExternal := func(t *testing.T, id, externalID, idempotencyKey string) *transaction.Transaction {
		t.Helper()
		hash, err := transaction.NewPayloadHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		if err != nil {
			t.Fatalf("NewPayloadHash: %v", err)
		}
		tx, err := transaction.NewExternal(transaction.NewExternalParams{
			ID:                    mustTransactionID(t, id),
			Kind:                  transaction.KindBet,
			PlayerID:              created.PlayerID(),
			WalletID:              created.ID(),
			Amount:                mustMoney(t, "25.00"),
			ProviderID:            "provider-a",
			ExternalTransactionID: transaction.ExternalTransactionID(externalID),
			IdempotencyKey:        transaction.IdempotencyKey(idempotencyKey),
			PayloadHash:           hash,
			RoundID:               "round-987",
			GameID:                "fortune-chimp",
			CorrelationID:         "corr-1",
			Now:                   testNow,
		})
		if err != nil {
			t.Fatalf("NewExternal: %v", err)
		}
		if err := tx.MarkProcessed(mustMoney(t, "75.00"), testNow); err != nil {
			t.Fatalf("MarkProcessed: %v", err)
		}
		return tx
	}

	first := newExternal(t, "0192f298-0000-7000-8000-000000000001", "tx-1", "provider-a:tx-1")
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Transactions().Insert(txCtx, first)
	}); err != nil {
		t.Fatalf("primeiro Insert: %v", err)
	}

	t.Run("chave_de_idempotencia_repetida", func(t *testing.T) {
		// Mesma chave, identidade e externalId diferentes.
		duplicate := newExternal(t, "0192f298-0000-7000-8000-000000000002", "tx-2", "provider-a:tx-1")
		err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
			return scope.Transactions().Insert(txCtx, duplicate)
		})
		if !errors.Is(err, ports.ErrAlreadyExists) {
			t.Errorf("error = %v, want ErrAlreadyExists", err)
		}
	})

	t.Run("par_externo_repetido", func(t *testing.T) {
		// Mesmo (providerId, externalId), com OUTRA chave (§9).
		duplicate := newExternal(t, "0192f298-0000-7000-8000-000000000003", "tx-1", "provider-a:outra-chave")
		err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
			return scope.Transactions().Insert(txCtx, duplicate)
		})
		if !errors.Is(err, ports.ErrAlreadyExists) {
			t.Errorf("error = %v, want ErrAlreadyExists", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Ledger
// -----------------------------------------------------------------------------

// seedWalletWithLedger cria carteira + lançamento de abertura, devolvendo a wallet.
func seedWalletWithLedger(t *testing.T, uow ports.UnitOfWork, id string, amount string) *wallet.Wallet {
	t.Helper()
	ctx := context.Background()

	w := mustWallet(t, id, amount)
	openingID := mustTransactionID(t, "0192f291-0000-7000-8000-000000000001")

	movement, err := wallet.NewOpeningMovement(w.Balance())
	if err != nil {
		t.Fatalf("NewOpeningMovement: %v", err)
	}
	opening, err := transaction.NewOpening(transaction.NewOpeningParams{
		ID:       openingID,
		PlayerID: w.PlayerID(),
		WalletID: w.ID(),
		Amount:   w.Balance(),
		Now:      testNow,
	})
	if err != nil {
		t.Fatalf("NewOpening: %v", err)
	}
	entry, err := ledger.NewEntryFromMovement(w.ID(), openingID, movement, testNow)
	if err != nil {
		t.Fatalf("NewEntryFromMovement: %v", err)
	}

	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		if err := scope.Wallets().Insert(txCtx, w); err != nil {
			return err
		}
		if err := scope.Transactions().Insert(txCtx, opening); err != nil {
			return err
		}
		return scope.Ledger().Append(txCtx, entry)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return w
}

func TestLedger_AppendAndCursorPagination(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	w := seedWalletWithLedger(t, uow, "0192f291-27dd-7d3f-8071-5f8685deef37", "1000.00")

	// Dois débitos, cada um com sua transação (UNIQUE por (wallet, transaction)).
	for i := 1; i <= 2; i++ {
		index := i
		if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
			locked, err := scope.Wallets().LockByID(txCtx, w.ID())
			if err != nil {
				return err
			}
			movement, err := locked.Debit(mustMoney(t, "25.00"), testNow)
			if err != nil {
				return err
			}
			txID := mustTransactionID(t, fmt.Sprintf("0192f291-0000-7000-8000-00000000000%d", index+1))
			// A transação precisa existir ANTES do lançamento (FK do §6.4).
			bet := newProcessedBet(t, w, txID, "25.00", movement.BalanceAfter().String())

			entry, err := ledger.NewEntryFromMovement(w.ID(), txID, movement, testNow)
			if err != nil {
				return err
			}
			if err := scope.Wallets().Update(txCtx, locked); err != nil {
				return err
			}
			if err := scope.Transactions().Insert(txCtx, bet); err != nil {
				return err
			}
			return scope.Ledger().Append(txCtx, entry)
		}); err != nil {
			t.Fatalf("debito %d: %v", i, err)
		}
	}

	// Página de 2: deve devolver os dois PRIMEIROS (abertura + primeiro débito).
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		page, err := scope.Ledger().ListByWallet(txCtx, w.ID(), 0, 2)
		if err != nil {
			return err
		}
		if len(page) != 2 {
			t.Fatalf("primeira pagina = %d, want 2", len(page))
		}
		if page[0].Direction() != wallet.Credit {
			t.Errorf("primeiro lancamento = %q, want %q", page[0].Direction(), wallet.Credit)
		}

		// Página seguinte a partir do cursor (id do último entregue).
		page2, err := scope.Ledger().ListByWallet(txCtx, w.ID(), page[1].ID(), 2)
		if err != nil {
			return err
		}
		if len(page2) != 1 {
			t.Fatalf("segunda pagina = %d, want 1", len(page2))
		}
		// Cursor por id NÃO repete nem pula lançamentos.
		if page2[0].ID() <= page[1].ID() {
			t.Errorf("segunda pagina sobrepos a primeira: %d <= %d", page2[0].ID(), page[1].ID())
		}
		// O saldo posterior encadeia com o anterior (§6.4).
		if !page2[0].BalanceBefore().Equal(page[1].BalanceAfter()) {
			t.Errorf("cadeia de saldo quebrada entre %s e %s",
				page[1].BalanceAfter(), page2[0].BalanceBefore())
		}
		return nil
	}); err != nil {
		t.Fatalf("paginacao: %v", err)
	}
}

// TestLedger_AppendOnly prova no BANCO REAL a garantia do §5.5. Foi este teste
// que revelou que o trigger também bloqueia o TRUNCATE usado na limpeza.
func TestLedger_AppendOnly(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	w := seedWalletWithLedger(t, uow, "0192f291-27dd-7d3f-8071-5f8685deef37", "1000.00")

	t.Run("update_recusado", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE wallet_id = $1::uuid`, w.ID().String())
		if err == nil {
			t.Fatalf("UPDATE foi aceito — o ledger NAO seria append-only")
		}
	})

	t.Run("delete_recusado", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM wallet_ledger_entries WHERE wallet_id = $1::uuid`, w.ID().String())
		if err == nil {
			t.Fatalf("DELETE foi aceito — o ledger NAO seria append-only")
		}
	})

	t.Run("truncate_recusado", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE wallet_ledger_entries`)
		if err == nil {
			t.Fatalf("TRUNCATE foi aceito — o ledger NAO seria append-only")
		}
	})
}

func TestLedger_UniquePerTransaction(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	w := seedWalletWithLedger(t, uow, "0192f291-27dd-7d3f-8071-5f8685deef37", "1000.00")
	openingTxID := mustTransactionID(t, "0192f291-0000-7000-8000-000000000001")

	// A MESMA transação não pode lançar duas vezes na mesma carteira (§6.4) —
	// é o backstop contra movimentação duplicada.
	movement, err := wallet.NewOpeningMovement(mustMoney(t, "10.00"))
	if err != nil {
		t.Fatalf("NewOpeningMovement: %v", err)
	}
	duplicate, err := ledger.NewEntry(ledger.NewEntryParams{
		WalletID:      w.ID(),
		TransactionID: openingTxID,
		Direction:     wallet.Credit,
		Amount:        mustMoney(t, "10.00"),
		BalanceBefore: mustMoney(t, "1000.00"),
		BalanceAfter:  mustMoney(t, "1010.00"),
		OccurredAt:    testNow,
	})
	if err != nil {
		t.Fatalf("NewEntry: %v", err)
	}
	_ = movement

	err = uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Ledger().Append(txCtx, duplicate)
	})
	if !errors.Is(err, ports.ErrAlreadyExists) {
		t.Errorf("error = %v, want ErrAlreadyExists", err)
	}
}

func TestLedger_SumSignedAmounts(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	w := seedWalletWithLedger(t, uow, "0192f291-27dd-7d3f-8071-5f8685deef37", "1000.00")

	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		locked, err := scope.Wallets().LockByID(txCtx, w.ID())
		if err != nil {
			return err
		}
		movement, err := locked.Debit(mustMoney(t, "25.00"), testNow)
		if err != nil {
			return err
		}
		txID := mustTransactionID(t, "0192f291-0000-7000-8000-000000000002")
		bet := newProcessedBet(t, w, txID, "25.00", movement.BalanceAfter().String())

		entry, err := ledger.NewEntryFromMovement(w.ID(), txID, movement, testNow)
		if err != nil {
			return err
		}
		if err := scope.Wallets().Update(txCtx, locked); err != nil {
			return err
		}
		if err := scope.Transactions().Insert(txCtx, bet); err != nil {
			return err
		}
		return scope.Ledger().Append(txCtx, entry)
	}); err != nil {
		t.Fatalf("debito: %v", err)
	}

	// 1000.00 (crédito) - 25.00 (débito) = 975.00 — o mesmo cálculo do §9.
	err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		total, err := scope.Ledger().SumSignedAmounts(txCtx, w.ID())
		if err != nil {
			return err
		}
		if got := total.String(); got != "975.00" {
			t.Errorf("SumSignedAmounts = %q, want %q", got, "975.00")
		}
		count, err := scope.Ledger().CountByWallet(txCtx, w.ID())
		if err != nil {
			return err
		}
		if count != 2 {
			t.Errorf("CountByWallet = %d, want 2", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("soma: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Inbox e Outbox
// -----------------------------------------------------------------------------

func TestInbox_RegisterAndRedelivery(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Inbox().Register(txCtx, "worker-1", "msg-1", "hash-original")
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Reentrega da MESMA mensagem: ErrAlreadyExists (o caso de uso compara hash).
	err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Inbox().Register(txCtx, "worker-1", "msg-1", "hash-original")
	})
	if !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists", err)
	}

	// Outro CONSUMIDOR pode registrar o mesmo messageId: a PK é composta.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Inbox().Register(txCtx, "worker-2", "msg-1", "hash-original")
	}); err != nil {
		t.Errorf("outro consumidor deveria poder registrar: %v", err)
	}

	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		record, err := scope.Inbox().Find(txCtx, "worker-1", "msg-1")
		if err != nil {
			return err
		}
		if record.MessageHash != "hash-original" {
			t.Errorf("MessageHash = %q", record.MessageHash)
		}
		// Ainda não concluída.
		if !record.ProcessedAt.IsZero() {
			t.Errorf("ProcessedAt = %v, want zero antes de MarkProcessed", record.ProcessedAt)
		}
		if err := scope.Inbox().MarkProcessed(txCtx, "worker-1", "msg-1"); err != nil {
			return err
		}
		updated, err := scope.Inbox().Find(txCtx, "worker-1", "msg-1")
		if err != nil {
			return err
		}
		if updated.ProcessedAt.IsZero() {
			t.Errorf("ProcessedAt = zero apos MarkProcessed")
		}
		return nil
	}); err != nil {
		t.Fatalf("Find/MarkProcessed: %v", err)
	}
}

func TestOutbox_AppendClaimPublishLifecycle(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	eventID := "0192f2a0-0000-7000-8000-000000000001"
	aggregateID := "0192f291-27dd-7d3f-8071-5f8685deef37"

	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Outbox().Append(txCtx, ports.OutboxRecord{
			EventID:       eventID,
			AggregateType: "Wallet",
			AggregateID:   aggregateID,
			EventType:     "WalletBalanceChanged",
			EventVersion:  1,
			Payload:       []byte(`{"eventId":"` + eventID + `"}`),
			CorrelationID: "corr-1",
			OccurredAt:    testNow,
			NextAttemptAt: testNow,
		})
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Reserva com lease.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		claimed, err := scope.Outbox().ClaimPending(txCtx, testNow, testNow.Add(30*time.Second), 10)
		if err != nil {
			return err
		}
		if len(claimed) != 1 {
			t.Fatalf("claimed = %d, want 1", len(claimed))
		}
		record := claimed[0]
		if record.EventID != eventID {
			t.Errorf("EventID = %q, want %q", record.EventID, eventID)
		}
		if record.AggregateID != aggregateID {
			t.Errorf("AggregateID = %q, want %q", record.AggregateID, aggregateID)
		}
		if record.CorrelationID != "corr-1" {
			t.Errorf("CorrelationID = %q", record.CorrelationID)
		}
		return scope.Outbox().MarkPublished(txCtx, eventID, testNow)
	}); err != nil {
		t.Fatalf("ClaimPending/MarkPublished: %v", err)
	}

	// Segunda rodada: nada pendente.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		claimed, err := scope.Outbox().ClaimPending(txCtx, testNow.Add(time.Minute), testNow.Add(2*time.Minute), 10)
		if err != nil {
			return err
		}
		if len(claimed) != 0 {
			t.Errorf("claimed = %d, want 0 (evento ja publicado)", len(claimed))
		}
		return nil
	}); err != nil {
		t.Fatalf("segunda rodada: %v", err)
	}
}

// TestOutbox_LeasePreventsDoubleClaim prova que o LEASE deixa o evento
// indisponível para uma segunda reserva antes de vencer (§11) — sem manter
// nenhuma transação aberta.
func TestOutbox_LeasePreventsDoubleClaim(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		return scope.Outbox().Append(txCtx, ports.OutboxRecord{
			EventID:       "0192f2a0-0000-7000-8000-000000000002",
			AggregateType: "Wallet",
			AggregateID:   "0192f291-27dd-7d3f-8071-5f8685deef37",
			EventType:     "WalletBalanceChanged",
			EventVersion:  1,
			Payload:       []byte(`{}`),
			OccurredAt:    testNow,
			NextAttemptAt: testNow,
		})
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	leaseUntil := testNow.Add(30 * time.Second)

	// Primeira reserva: pega o evento.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		claimed, err := scope.Outbox().ClaimPending(txCtx, testNow, leaseUntil, 10)
		if err != nil {
			return err
		}
		if len(claimed) != 1 {
			t.Fatalf("primeira reserva = %d, want 1", len(claimed))
		}
		return nil
	}); err != nil {
		t.Fatalf("primeira reserva: %v", err)
	}

	// Segunda reserva DENTRO do lease: não pode pegar o mesmo evento.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		claimed, err := scope.Outbox().ClaimPending(txCtx, testNow.Add(time.Second), leaseUntil, 10)
		if err != nil {
			return err
		}
		if len(claimed) != 0 {
			t.Errorf("segunda reserva = %d, want 0 (lease ainda valido)", len(claimed))
		}
		return nil
	}); err != nil {
		t.Fatalf("segunda reserva: %v", err)
	}

	// Terceira reserva APÓS o lease: o trabalho abandonado é recuperado (§11).
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		claimed, err := scope.Outbox().ClaimPending(txCtx, leaseUntil.Add(time.Second), leaseUntil.Add(time.Minute), 10)
		if err != nil {
			return err
		}
		if len(claimed) != 1 {
			t.Errorf("reserva apos o lease = %d, want 1 (trabalho abandonado recuperado)", len(claimed))
		}
		return nil
	}); err != nil {
		t.Fatalf("terceira reserva: %v", err)
	}
}

// -----------------------------------------------------------------------------
// UnitOfWork
// -----------------------------------------------------------------------------

func TestUnitOfWork_RollbackOnError(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	w := mustWallet(t, "0192f291-27dd-7d3f-8071-5f8685deef37", "100.00")

	sentinel := errors.New("falha de negocio no meio do bloco")
	err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		if err := scope.Wallets().Insert(txCtx, w); err != nil {
			return err
		}
		return sentinel
	})
	// O erro ORIGINAL precisa chegar ao chamador (não o do rollback).
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want o sentinel do bloco", err)
	}

	// E nada foi persistido: a carteira não existe.
	err = uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		if _, err := scope.Wallets().FindByID(txCtx, w.ID()); !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("carteira encontrada apos rollback")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verificacao: %v", err)
	}
}

func TestUnitOfWork_RollbackOnPanic(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	w := mustWallet(t, "0192f291-27dd-7d3f-8071-5f8685deef37", "100.00")

	func() {
		defer func() {
			// O panic DEVE ser repropagado: engoli-lo esconderia um bug.
			if recover() == nil {
				t.Fatalf("o panic nao foi repropagado")
			}
		}()
		_ = uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
			if err := scope.Wallets().Insert(txCtx, w); err != nil {
				return err
			}
			panic("bug inesperado no meio da transacao")
		})
	}()

	// Rollback aconteceu: a carteira não foi criada.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		if _, err := scope.Wallets().FindByID(txCtx, w.ID()); !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("carteira encontrada apos panic (rollback nao aconteceu)")
		}
		return nil
	}); err != nil {
		t.Fatalf("verificacao: %v", err)
	}

	// E o pool continua saudável: a conexão voltou, sem transação pendurada.
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pool ficou inutilizavel apos o panic: %v", err)
	}
}

// TestLedger_CurrencyPreserved: a leitura do ledger traz a moeda da carteira (via JOIN).
func TestLedger_CurrencyPreserved(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	uow := postgres.NewUnitOfWork(pool)

	playerID, err := wallet.NewPlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4c1")
	if err != nil {
		t.Fatalf("NewPlayerID: %v", err)
	}
	walletID, err := wallet.NewID("0192f291-27dd-7d3f-8071-5f8685deef99")
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	balance, err := money.Parse("1000.00", money.USD)
	if err != nil {
		t.Fatalf("money.Parse: %v", err)
	}
	w, err := wallet.New(walletID, playerID, balance, testNow)
	if err != nil {
		t.Fatalf("wallet.New: %v", err)
	}

	openingID := mustTransactionID(t, "0192f291-0000-7000-8000-0000000000a1")
	movement, err := wallet.NewOpeningMovement(balance)
	if err != nil {
		t.Fatalf("NewOpeningMovement: %v", err)
	}
	opening, err := transaction.NewOpening(transaction.NewOpeningParams{
		ID:       openingID,
		PlayerID: playerID,
		WalletID: walletID,
		Amount:   balance,
		Now:      testNow,
	})
	if err != nil {
		t.Fatalf("NewOpening: %v", err)
	}
	entry, err := ledger.NewEntryFromMovement(walletID, openingID, movement, testNow)
	if err != nil {
		t.Fatalf("NewEntryFromMovement: %v", err)
	}

	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		if err := scope.Wallets().Insert(txCtx, w); err != nil {
			return err
		}
		if err := scope.Transactions().Insert(txCtx, opening); err != nil {
			return err
		}
		return scope.Ledger().Append(txCtx, entry)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		page, err := scope.Ledger().ListByWallet(txCtx, walletID, 0, 10)
		if err != nil {
			return err
		}
		if len(page) != 1 {
			t.Fatalf("lancamentos = %d, want 1", len(page))
		}
		if got := page[0].Amount().Currency(); got != money.USD {
			t.Errorf("moeda do valor = %q, want %q", got, money.USD)
		}
		if got := page[0].BalanceAfter().Currency(); got != money.USD {
			t.Errorf("moeda do saldo posterior = %q, want %q", got, money.USD)
		}
		return nil
	}); err != nil {
		t.Fatalf("ListByWallet: %v", err)
	}

	// A reconciliação também deriva a moeda da carteira.
	if err := uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		total, err := scope.Ledger().SumSignedAmounts(txCtx, walletID)
		if err != nil {
			return err
		}
		if total.Currency() != money.USD || total.String() != "1000.00" {
			t.Errorf("soma = %s %s, want 1000.00 USD", total.String(), total.Currency())
		}
		return nil
	}); err != nil {
		t.Fatalf("SumSignedAmounts: %v", err)
	}
}
