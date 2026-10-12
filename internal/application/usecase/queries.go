package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/ledger"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// Limites da paginação do extrato: o §9 exige cursor opaco; o teto evita que um
// cliente peça "todo o histórico" e derrube a API.
const (
	defaultLedgerLimit = 50
	maxLedgerLimit     = 200
)

// requireInternal centraliza a autorização de acesso a dados de carteira (§2).
func requireInternal(principal ports.Principal, operation string) error {
	if !principal.IsInternal() {
		return fmt.Errorf("%w: %s requires the internal service scope", ports.ErrForbidden, operation)
	}
	return nil
}

// normalizeLimit aplica o default e o teto da paginação.
func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultLedgerLimit
	}
	if limit > maxLedgerLimit {
		return maxLedgerLimit
	}
	return limit
}

var _ ports.GetWallet = (*GetWalletService)(nil)

// GetWalletService implementa GET /wallets/:walletId (§9).
type GetWalletService struct {
	uow ports.UnitOfWork
}

func NewGetWalletService(uow ports.UnitOfWork) *GetWalletService {
	return &GetWalletService{uow: uow}
}

// Execute devolve a projeção da carteira. Passa por `uow.Execute` (transação de
// leitura) para uma visão consistente: fora dela, poderia ler o saldo antes de um
// commit e a versão depois — estado que nunca existiu no banco.
func (s *GetWalletService) Execute(ctx context.Context, query ports.GetWalletQuery) (ports.WalletView, error) {
	if err := requireInternal(query.Principal, "wallet lookup"); err != nil {
		return ports.WalletView{}, err
	}
	walletID, err := walletIDFromString(query.WalletID)
	if err != nil {
		return ports.WalletView{}, err
	}

	var view ports.WalletView
	err = s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		w, err := scope.Wallets().FindByID(txCtx, walletID)
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				return fmt.Errorf("%w: wallet %s", ports.ErrNotFound, walletID)
			}
			return err
		}
		view = ports.WalletView{
			WalletID: w.ID(),
			PlayerID: w.PlayerID(),
			Balance:  w.Balance(),
			Version:  w.Version(),
		}
		return nil
	})
	if err != nil {
		return ports.WalletView{}, err
	}
	return view, nil
}

var _ ports.GetWalletLedger = (*GetWalletLedgerService)(nil)

// GetWalletLedgerService implementa GET /wallets/:walletId/ledger (§9).
type GetWalletLedgerService struct {
	uow ports.UnitOfWork
}

func NewGetWalletLedgerService(uow ports.UnitOfWork) *GetWalletLedgerService {
	return &GetWalletLedgerService{uow: uow}
}

// Execute devolve uma página do extrato, ordenada por `id` crescente.
//
// Cursor em vez de OFFSET: em ledger append-only, OFFSET repete/pula registros quando
// entram lançamentos durante a navegação e fica mais caro quanto maior o offset. O
// cursor por `id` é marcador estável no tempo e usa o índice (wallet_id, id) em range
// scan. Técnica do "limit+1": uma linha a mais indica próxima página sem COUNT(*).
func (s *GetWalletLedgerService) Execute(ctx context.Context, query ports.GetLedgerQuery) (ports.LedgerPage, error) {
	if err := requireInternal(query.Principal, "ledger lookup"); err != nil {
		return ports.LedgerPage{}, err
	}
	walletID, err := walletIDFromString(query.WalletID)
	if err != nil {
		return ports.LedgerPage{}, err
	}
	limit := normalizeLimit(query.Limit)

	var page ports.LedgerPage
	err = s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		// A carteira precisa existir: sem esta checagem, uma carteira inexistente
		// devolveria 200 com lista vazia — indistinguível de "sem lançamentos".
		if _, err := scope.Wallets().FindByID(txCtx, walletID); err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				return fmt.Errorf("%w: wallet %s", ports.ErrNotFound, walletID)
			}
			return err
		}

		entries, err := scope.Ledger().ListByWallet(txCtx, walletID, query.Cursor, limit+1)
		if err != nil {
			return err
		}

		hasMore := len(entries) > limit
		if hasMore {
			entries = entries[:limit] // descarta o "espiador" da próxima página
		}

		views := make([]ports.LedgerEntryView, 0, len(entries))
		for _, entry := range entries {
			views = append(views, ledgerEntryView(entry))
		}
		page.Entries = views
		if hasMore && len(views) > 0 {
			// O cursor é o ID do ÚLTIMO lançamento entregue: opaco para o cliente
			// (ele não deve interpretar o valor, apenas devolvê-lo).
			page.NextCursor = views[len(views)-1].ID
		}
		return nil
	})
	if err != nil {
		return ports.LedgerPage{}, err
	}
	return page, nil
}

// ledgerEntryView projeta um lançamento do domínio para a resposta.
func ledgerEntryView(entry *ledger.Entry) ports.LedgerEntryView {
	return ports.LedgerEntryView{
		ID:            entry.ID(),
		TransactionID: entry.TransactionID(),
		Direction:     entry.Direction(),
		Amount:        entry.Amount(),
		BalanceBefore: entry.BalanceBefore(),
		BalanceAfter:  entry.BalanceAfter(),
		CreatedAt:     entry.CreatedAt(),
	}
}

var _ ports.GetWagerTransaction = (*GetWagerTransactionService)(nil)

// GetWagerTransactionService implementa GET /wagering/transactions/:id (§9).
type GetWagerTransactionService struct {
	uow ports.UnitOfWork
}

func NewGetWagerTransactionService(uow ports.UnitOfWork) *GetWagerTransactionService {
	return &GetWagerTransactionService{uow: uow}
}

// Execute busca a transação pelo id interno. Autorização (§2): o serviço interno vê
// qualquer transação; o provedor só vê as próprias. A checagem vem depois da leitura
// (é o dado lido que determina a posse); acesso indevido é 403 sem dado do outro.
func (s *GetWagerTransactionService) Execute(ctx context.Context, query ports.GetTransactionQuery) (ports.TransactionView, error) {
	transactionID, err := transactionIDFromString(query.TransactionID)
	if err != nil {
		return ports.TransactionView{}, err
	}

	var view ports.TransactionView
	err = s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		tx, err := scope.Transactions().FindByID(txCtx, transactionID)
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				return fmt.Errorf("%w: transaction %s", ports.ErrNotFound, transactionID)
			}
			return err
		}
		if err := authorizeTransactionAccess(query.Principal, tx.ProviderID()); err != nil {
			return err
		}
		view = transactionView(tx)
		return nil
	})
	if err != nil {
		return ports.TransactionView{}, err
	}
	return view, nil
}

var _ ports.GetWagerTransactionByExternalID = (*GetWagerTransactionByExternalIDService)(nil)

// GetWagerTransactionByExternalIDService implementa
// GET /providers/:providerId/wagering/transactions/:externalTransactionId (§9).
type GetWagerTransactionByExternalIDService struct {
	uow ports.UnitOfWork
}

func NewGetWagerTransactionByExternalIDService(uow ports.UnitOfWork) *GetWagerTransactionByExternalIDService {
	return &GetWagerTransactionByExternalIDService{uow: uow}
}

// Execute busca a transação pelo par (providerId, externalTransactionId). O providerId
// vem da rota, mas para um provedor é validado contra a identidade do token (§2: a URL
// nunca é fonte de autoridade); trocar o id na URL resulta em 403.
func (s *GetWagerTransactionByExternalIDService) Execute(ctx context.Context, query ports.GetTransactionByExternalIDQuery) (ports.TransactionView, error) {
	providerID, err := providerIDFromString(query.ProviderID)
	if err != nil {
		return ports.TransactionView{}, err
	}
	externalID, err := externalTransactionIDFromString(query.ExternalTransactionID)
	if err != nil {
		return ports.TransactionView{}, err
	}

	if query.Principal.IsProvider() && query.Principal.ProviderID != providerID.String() {
		return ports.TransactionView{}, fmt.Errorf(
			"%w: identity %q cannot read transactions of provider %q",
			ports.ErrForbidden, query.Principal.ProviderID, providerID)
	}
	if !query.Principal.IsProvider() && !query.Principal.IsInternal() {
		return ports.TransactionView{}, fmt.Errorf("%w: unknown scope %q",
			ports.ErrForbidden, query.Principal.Scope)
	}

	var view ports.TransactionView
	err = s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		tx, err := scope.Transactions().FindByProviderAndExternalID(txCtx, providerID, externalID)
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				return fmt.Errorf("%w: transaction %s/%s", ports.ErrNotFound, providerID, externalID)
			}
			return err
		}
		view = transactionView(tx)
		return nil
	})
	if err != nil {
		return ports.TransactionView{}, err
	}
	return view, nil
}

// authorizeTransactionAccess aplica a regra de isolamento entre provedores (§2).
func authorizeTransactionAccess(principal ports.Principal, owner transaction.ProviderID) error {
	if principal.IsInternal() {
		return nil
	}
	if principal.IsProvider() && principal.ProviderID == owner.String() {
		return nil
	}
	// Mensagem sem detalhes do recurso: não vaza existência nem conteúdo.
	return fmt.Errorf("%w: transaction does not belong to the authenticated identity", ports.ErrForbidden)
}

var _ ports.ReconcileWallet = (*ReconcileWalletService)(nil)

// ReconcileWalletService implementa POST /wallets/:walletId/reconciliation (§9).
type ReconcileWalletService struct {
	uow ports.UnitOfWork
}

func NewReconcileWalletService(uow ports.UnitOfWork) *ReconcileWalletService {
	return &ReconcileWalletService{uow: uow}
}

// Execute confronta o saldo armazenado com o reconstruído do ledger (§9). Saldo, soma
// dos lançamentos e contagem são lidos na MESMA transação: ler em transações distintas
// poderia capturar o saldo após um commit e o ledger antes dele, gerando divergência
// FALSA. A reconciliação nunca altera o saldo — a correção é um NOVO lançamento (§5.5).
func (s *ReconcileWalletService) Execute(ctx context.Context, query ports.ReconcileWalletQuery) (ports.ReconcileWalletResult, error) {
	if err := requireInternal(query.Principal, "wallet reconciliation"); err != nil {
		return ports.ReconcileWalletResult{}, err
	}
	walletID, err := walletIDFromString(query.WalletID)
	if err != nil {
		return ports.ReconcileWalletResult{}, err
	}

	var result ports.ReconcileWalletResult
	err = s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		w, err := scope.Wallets().FindByID(txCtx, walletID)
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				return fmt.Errorf("%w: wallet %s", ports.ErrNotFound, walletID)
			}
			return err
		}

		calculated, err := scope.Ledger().SumSignedAmounts(txCtx, walletID)
		if err != nil {
			return err
		}
		checked, err := scope.Ledger().CountByWallet(txCtx, walletID)
		if err != nil {
			return err
		}

		stored := w.Balance()
		// `difference` = armazenado - reconstruído, exatamente como o §9 define.
		difference, err := stored.Sub(calculated)
		if err != nil {
			return fmt.Errorf("reconciliation: cannot compute difference: %w", err)
		}

		result = ports.ReconcileWalletResult{
			WalletID:          w.ID(),
			StoredBalance:     stored,
			CalculatedBalance: calculated,
			Difference:        difference,
			Consistent:        difference.IsZero(),
			CheckedEntries:    checked,
		}
		return nil
	})
	if err != nil {
		return ports.ReconcileWalletResult{}, err
	}
	return result, nil
}

// transactionView projeta o agregado acessor por acessor (não quebra o encapsulamento).
func transactionView(tx *transaction.Transaction) ports.TransactionView {
	view := ports.TransactionView{
		TransactionID:         tx.ID(),
		ProviderID:            tx.ProviderID(),
		ExternalTransactionID: tx.ExternalTransactionID(),
		PlayerID:              tx.PlayerID(),
		WalletID:              tx.WalletID(),
		RoundID:               tx.RoundID(),
		GameID:                tx.GameID(),
		Kind:                  tx.Kind(),
		State:                 tx.State(),
		Amount:                tx.Amount(),
		ReferenceExternalID:   tx.ReferenceExternalTransactionID(),
		FailureCode:           tx.FailureCode(),
		Attempts:              tx.Attempts(),
		NextRetryAt:           tx.NextRetryAt(),
		CreatedAt:             tx.CreatedAt(),
		UpdatedAt:             tx.UpdatedAt(),
		ProcessedAt:           tx.ProcessedAt(),
	}
	if tx.FailureCode() != "" {
		view.Correctable = tx.FailureCode().IsCorrectable()
	}
	if result := tx.ResultBalance(); result.Valid {
		view.ResultBalance = result.Value
		view.HasResultBalance = true
	}
	return view
}

// Estes helpers garantem que o erro devolvido seja sempre ErrValidation — nunca um erro
// cru de domínio, que o adapter não saberia mapear para 400.

func walletIDFromString(raw string) (wallet.ID, error) {
	id, err := wallet.NewID(raw)
	if err != nil {
		return "", fmt.Errorf("%w: walletId: %v", ports.ErrValidation, err)
	}
	return id, nil
}

func transactionIDFromString(raw string) (transaction.ID, error) {
	id, err := transaction.NewID(raw)
	if err != nil {
		return "", fmt.Errorf("%w: transactionId: %v", ports.ErrValidation, err)
	}
	return id, nil
}

func providerIDFromString(raw string) (transaction.ProviderID, error) {
	id, err := transaction.NewProviderID(raw)
	if err != nil {
		return "", fmt.Errorf("%w: providerId: %v", ports.ErrValidation, err)
	}
	return id, nil
}

func externalTransactionIDFromString(raw string) (transaction.ExternalTransactionID, error) {
	id, err := transaction.NewExternalTransactionID(raw)
	if err != nil {
		return "", fmt.Errorf("%w: externalTransactionId: %v", ports.ErrValidation, err)
	}
	return id, nil
}
