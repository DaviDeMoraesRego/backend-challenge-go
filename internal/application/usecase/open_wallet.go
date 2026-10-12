package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/event"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/ledger"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var _ ports.OpenWallet = (*OpenWalletService)(nil)

// OpenWalletService implementa a abertura de carteira. As dependências são interfaces
// (portas outbound): não sabe se o armazenamento é Postgres ou um fake de teste.
type OpenWalletService struct {
	uow   ports.UnitOfWork
	clock ports.Clock
	ids   ports.IDGenerator
}

func NewOpenWalletService(uow ports.UnitOfWork, clock ports.Clock, ids ports.IDGenerator) *OpenWalletService {
	return &OpenWalletService{uow: uow, clock: clock, ids: ids}
}

// Execute abre a carteira: 1) autorização (antes de qualquer I/O, §2/§13); 2)
// validação da entrada (money.Parse rejeita escala inválida, notação científica e
// negativos); 3) geração de ID fora da transação; 4) persistência atômica.
func (s *OpenWalletService) Execute(ctx context.Context, cmd ports.OpenWalletCommand) (ports.OpenWalletResult, error) {
	// 1) Autorização. Interpretação registrada no ARCHITECTURE.md: "operações de
	// carteira são restritas ao serviço interno" (§2) — abertura, leitura e
	// reconciliação exigem identidade internal; provedores usam /wagering/transactions.
	if !cmd.Principal.IsInternal() {
		return ports.OpenWalletResult{}, fmt.Errorf(
			"%w: opening a wallet requires the internal service scope, got %q",
			ports.ErrForbidden, cmd.Principal.Scope)
	}

	// 2) Validação da entrada (único ponto de conversão texto -> domínio).
	playerID, err := wallet.NewPlayerID(cmd.PlayerID)
	if err != nil {
		return ports.OpenWalletResult{}, fmt.Errorf("%w: playerId: %v", ports.ErrValidation, err)
	}
	currency, err := money.NewCurrency(cmd.InitialCurrency)
	if err != nil {
		return ports.OpenWalletResult{}, fmt.Errorf("%w: initialBalance.currency: %v", ports.ErrValidation, err)
	}
	initialBalance, err := money.Parse(cmd.InitialAmount, currency)
	if err != nil {
		return ports.OpenWalletResult{}, fmt.Errorf("%w: initialBalance.amount: %v", ports.ErrValidation, err)
	}

	// Correlação: geramos um identificador quando o adapter não informou (§11).
	correlationID := cmd.CorrelationID
	if correlationID == "" {
		correlationID, err = s.ids.NewID(ctx)
		if err != nil {
			return ports.OpenWalletResult{}, fmt.Errorf("%w: correlation id", ports.ErrUnavailable)
		}
	}

	// 3) Identidades, fora da transação.
	walletIDRaw, err := s.ids.NewID(ctx)
	if err != nil {
		return ports.OpenWalletResult{}, fmt.Errorf("%w: wallet id", ports.ErrUnavailable)
	}
	walletID, err := wallet.NewID(walletIDRaw)
	if err != nil {
		return ports.OpenWalletResult{}, fmt.Errorf("%w: walletId", ports.ErrValidation)
	}

	now := s.clock.Now()

	w, err := wallet.New(walletID, playerID, initialBalance, now)
	if err != nil {
		return ports.OpenWalletResult{}, fmt.Errorf("%w: %v", ports.ErrValidation, err)
	}

	// Declarado fora do closure: é assim que o resultado atravessa o Execute.
	var result ports.OpenWalletResult

	// 4) Persistência atômica.
	err = s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		// A carteira entra primeiro: se (playerId, currency) já existir, o banco devolve
		// violação de unicidade -> ErrAlreadyExists. Sem TOCTOU: não há "consultar e
		// depois inserir" fora da transação.
		if err := scope.Wallets().Insert(txCtx, w); err != nil {
			if errors.Is(err, ports.ErrAlreadyExists) {
				return fmt.Errorf("%w: wallet already exists for player %s and currency %s",
					ports.ErrConflict, playerID, currency)
			}
			return err
		}

		result = ports.OpenWalletResult{
			WalletID: w.ID(),
			PlayerID: w.PlayerID(),
			Balance:  w.Balance(),
			Version:  w.Version(),
		}

		// Saldo inicial ZERO não cria OPENING, ledger nem eventos (§9): a carteira
		// existe com saldo 0.00 e versão 1, e a resposta sai daqui.
		if !initialBalance.IsPositive() {
			return nil
		}

		// Saldo inicial POSITIVO: os quatro registros entram na MESMA transação.
		openingIDRaw, err := s.ids.NewID(txCtx)
		if err != nil {
			return fmt.Errorf("%w: opening transaction id", ports.ErrUnavailable)
		}
		openingID, err := transaction.NewID(openingIDRaw)
		if err != nil {
			return fmt.Errorf("%w: opening transaction id", ports.ErrValidation)
		}

		// OPENING nasce em PROCESSED: a abertura é atômica com a criação da carteira (§9).
		opening, err := transaction.NewOpening(transaction.NewOpeningParams{
			ID:       openingID,
			PlayerID: playerID,
			WalletID: walletID,
			Amount:   initialBalance,
			Now:      now,
		})
		if err != nil {
			return fmt.Errorf("%w: %v", ports.ErrValidation, err)
		}

		// A movimentação de abertura é a fonte única para o ledger e o evento:
		// ambos descrevem o mesmo crédito 0.00 -> saldo inicial, na versão 1.
		movement, err := wallet.NewOpeningMovement(initialBalance)
		if err != nil {
			return fmt.Errorf("%w: %v", ports.ErrValidation, err)
		}

		entry, err := ledger.NewEntryFromMovement(walletID, openingID, movement, now)
		if err != nil {
			return fmt.Errorf("%w: opening ledger entry: %v", ports.ErrValidation, err)
		}

		// Evento 1: a operação concluiu (§11 — OPENING é uma operação interna).
		processedEventID, err := s.ids.NewID(txCtx)
		if err != nil {
			return fmt.Errorf("%w: event id", ports.ErrUnavailable)
		}
		processed, err := event.NewWagerTransactionProcessed(
			processedEventID, correlationID, "", opening, initialBalance, now)
		if err != nil {
			return fmt.Errorf("%w: processed event: %v", ports.ErrValidation, err)
		}

		// Evento 2: o saldo mudou. O aggregateId deste evento é a CARTEIRA.
		balanceEventID, err := s.ids.NewID(txCtx)
		if err != nil {
			return fmt.Errorf("%w: event id", ports.ErrUnavailable)
		}
		balanceChanged, err := event.NewWalletBalanceChanged(
			balanceEventID, correlationID, processed.Envelope.EventID, w.ID(), openingID, movement, now)
		if err != nil {
			return fmt.Errorf("%w: balance event: %v", ports.ErrValidation, err)
		}

		if err := scope.Transactions().Insert(txCtx, opening); err != nil {
			return err
		}
		if err := scope.Ledger().Append(txCtx, entry); err != nil {
			return err
		}

		processedRecord, err := transactionEventRecord(processed.Envelope, processed)
		if err != nil {
			return err
		}
		balanceRecord, err := walletEventRecord(balanceChanged.Envelope, balanceChanged)
		if err != nil {
			return err
		}
		return scope.Outbox().Append(txCtx, processedRecord, balanceRecord)
	})
	if err != nil {
		return ports.OpenWalletResult{}, err
	}

	return result, nil
}
