package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/event"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/ledger"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var _ ports.PlaceWagerOperation = (*PlaceWagerOperationService)(nil)

// errConcurrentOperationInsert: corrida perdida no índice único da chave de
// idempotência (outra requisição inseriu a mesma operação). Não é conflito para o
// cliente: repetimos o bloco numa nova transação, que encontrará o vencedor e
// devolverá o replay. Por isso não é exposto nos adapters.
var errConcurrentOperationInsert = errors.New("usecase: concurrent insert of the same operation")

// PlaceWagerOperationConfig reúne os parâmetros de política do caso de uso.
type PlaceWagerOperationConfig struct {
	// PendingReferenceBaseBackoff é a base do backoff exponencial em PENDING_REFERENCE (§7).
	PendingReferenceBaseBackoff time.Duration
}

// comDefaults concentra os defaults num só lugar, tornando a política auditável.
func (c PlaceWagerOperationConfig) comDefaults() PlaceWagerOperationConfig {
	if c.PendingReferenceBaseBackoff <= 0 {
		c.PendingReferenceBaseBackoff = 2 * time.Second
	}
	return c
}

type PlaceWagerOperationService struct {
	uow     ports.UnitOfWork
	clock   ports.Clock
	ids     ports.IDGenerator
	backoff time.Duration
}

// NewPlaceWagerOperationService monta o caso de uso com a política de backoff.
func NewPlaceWagerOperationService(
	uow ports.UnitOfWork,
	clock ports.Clock,
	ids ports.IDGenerator,
	cfg PlaceWagerOperationConfig,
) *PlaceWagerOperationService {
	resolved := cfg.comDefaults()
	return &PlaceWagerOperationService{
		uow:     uow,
		clock:   clock,
		ids:     ids,
		backoff: resolved.PendingReferenceBaseBackoff,
	}
}

// parsedOperation é a operação já convertida para tipos de domínio: o parsing
// acontece uma vez, antes da transação, e os métodos internos recebem dados validados.
type parsedOperation struct {
	kind transaction.Kind

	providerID     transaction.ProviderID
	externalID     transaction.ExternalTransactionID
	idempotencyKey transaction.IdempotencyKey
	payloadHash    transaction.PayloadHash

	playerID wallet.PlayerID
	walletID wallet.ID
	roundID  string
	gameID   string

	amount money.Money

	referenceExternalID transaction.ExternalTransactionID

	correlationID string
}

// Execute processa a operação.
func (s *PlaceWagerOperationService) Execute(ctx context.Context, cmd ports.PlaceOperationCommand) (ports.PlaceOperationResult, error) {
	// 1) Autorização (§2): provedor só opera em nome de si mesmo (compara a claim do
	// token com o providerId do payload); identidade interna é permitida (worker SQS).
	switch {
	case cmd.Principal.IsProvider():
		if cmd.Principal.ProviderID != cmd.ProviderID {
			return ports.PlaceOperationResult{}, fmt.Errorf(
				"%w: identity %q cannot operate on behalf of provider %q",
				ports.ErrForbidden, cmd.Principal.ProviderID, cmd.ProviderID)
		}
	case cmd.Principal.IsInternal():
		// Autorizado: o serviço interno responde pelo sistema como um todo.
	default:
		return ports.PlaceOperationResult{}, fmt.Errorf("%w: unknown scope %q",
			ports.ErrForbidden, cmd.Principal.Scope)
	}

	// 2) Parsing estrito: único ponto de conversão texto -> domínio.
	parsed, err := parseOperation(cmd)
	if err != nil {
		return ports.PlaceOperationResult{}, err
	}

	// Correlação de rastreio (§12): se o adapter não injetou, geramos.
	correlationID := cmd.CorrelationID
	if correlationID == "" {
		correlationID, err = s.ids.NewID(ctx)
		if err != nil {
			return ports.PlaceOperationResult{}, fmt.Errorf("%w: correlation id", ports.ErrUnavailable)
		}
	}

	// Capturado uma vez e reutilizado em carteira, ledger, transação e eventos.
	now := s.clock.Now()

	// A correlação acompanha a transação persistida (permite retomar PENDING_REFERENCE
	// com o mesmo rastro, §12).
	parsed.correlationID = correlationID

	// Declarado fora do closure: é a comunicação entre o bloco transacional e o chamador.
	var result ports.PlaceOperationResult

	run := func(txCtx context.Context, scope ports.TxScope) error {
		return s.executeInScope(txCtx, scope, cmd, parsed, correlationID, now, &result)
	}

	// Repetição limitada para as corridas residuais. A re-checagem dentro do lock
	// resolve o caso comum (mesma carteira serializada); esta repetição cobre os
	// caminhos sem lock para serializar (ex.: carteira inexistente, em que duas
	// requisições com a mesma chave tentariam inserir a rejeição ao mesmo tempo).
	// Limite baixo de propósito: se 3 tentativas falharem, propagamos o erro (503).
	const maxInsertAttempts = 3

	// `err` já existe (vem do parseOperation); atribuímos com `=` — redeclarar com
	// `:=` criaria outra variável e descartaria o resultado.
	for attempt := 0; attempt < maxInsertAttempts; attempt++ {
		err = s.uow.Execute(ctx, run)
		if !errors.Is(err, errConcurrentOperationInsert) {
			break
		}
	}
	if err != nil {
		return ports.PlaceOperationResult{}, err
	}
	return result, nil
}

// parseOperation converte a entrada textual em tipos de domínio. Vive aqui (não nos
// adapters) para HTTP e SQS aplicarem as mesmas regras (§10), sem dois pontos de
// verdade que divergem.
func parseOperation(cmd ports.PlaceOperationCommand) (parsedOperation, error) {
	var parsed parsedOperation

	kind, err := transaction.ParseKind(cmd.Kind)
	if err != nil {
		return parsed, fmt.Errorf("%w: kind: %v", ports.ErrValidation, err)
	}
	// §6.3: OPENING é interno e deve ser rejeitado por HTTP/SQS. Erro de validação (e
	// não REJECTED) porque o schema também não aceita esse tipo em operações externas.
	if kind.IsInternal() {
		return parsed, fmt.Errorf("%w: kind OPENING is reserved for internal wallet opening", ports.ErrValidation)
	}

	providerID, err := transaction.NewProviderID(cmd.ProviderID)
	if err != nil {
		return parsed, fmt.Errorf("%w: providerId: %v", ports.ErrValidation, err)
	}
	externalID, err := transaction.NewExternalTransactionID(cmd.ExternalTransactionID)
	if err != nil {
		return parsed, fmt.Errorf("%w: externalTransactionId: %v", ports.ErrValidation, err)
	}
	idempotencyKey, err := transaction.NewIdempotencyKey(cmd.IdempotencyKey)
	if err != nil {
		// O header `Idempotency-Key` é obrigatório (§9).
		return parsed, fmt.Errorf("%w: idempotencyKey is required: %v", ports.ErrValidation, err)
	}
	playerID, err := wallet.NewPlayerID(cmd.PlayerID)
	if err != nil {
		return parsed, fmt.Errorf("%w: playerId: %v", ports.ErrValidation, err)
	}
	walletID, err := wallet.NewID(cmd.WalletID)
	if err != nil {
		return parsed, fmt.Errorf("%w: walletId: %v", ports.ErrValidation, err)
	}

	currency, err := money.NewCurrency(cmd.Currency)
	if err != nil {
		return parsed, fmt.Errorf("%w: money.currency: %v", ports.ErrValidation, err)
	}
	amount, err := money.Parse(cmd.Amount, currency)
	if err != nil {
		return parsed, fmt.Errorf("%w: money.amount: %v", ports.ErrValidation, err)
	}
	if err := validateAmountForKind(kind, amount); err != nil {
		return parsed, err
	}

	// Referência: obrigatória para reversões, proibida nos demais tipos (§7).
	var referenceExternalID transaction.ExternalTransactionID
	if kind.IsReversal() {
		referenceExternalID, err = transaction.NewExternalTransactionID(cmd.ReferenceExternalTransactionID)
		if err != nil {
			return parsed, fmt.Errorf("%w: referenceExternalTransactionId is required for %s: %v",
				ports.ErrValidation, kind, err)
		}
	} else if cmd.ReferenceExternalTransactionID != "" {
		return parsed, fmt.Errorf("%w: %s must not carry a reference", ports.ErrValidation, kind)
	}

	// Hash canônico (§9): dos campos já normalizados, excluindo a chave de idempotência
	// e metadados de transporte — HTTP e SQS produzem o mesmo hash para a operação.
	payloadHash, err := transaction.ComputePayloadHash(transaction.CanonicalPayloadFields{
		ProviderID:                     providerID,
		ExternalTransactionID:          externalID,
		PlayerID:                       playerID,
		WalletID:                       walletID,
		RoundID:                        cmd.RoundID,
		GameID:                         cmd.GameID,
		Kind:                           kind,
		Amount:                         amount,
		ReferenceExternalTransactionID: referenceExternalID,
	})
	if err != nil {
		return parsed, fmt.Errorf("%w: payload hash: %v", ports.ErrValidation, err)
	}

	parsed = parsedOperation{
		kind:                kind,
		providerID:          providerID,
		externalID:          externalID,
		idempotencyKey:      idempotencyKey,
		payloadHash:         payloadHash,
		playerID:            playerID,
		walletID:            walletID,
		roundID:             cmd.RoundID,
		gameID:              cmd.GameID,
		amount:              amount,
		referenceExternalID: referenceExternalID,
	}
	return parsed, nil
}

// validateAmountForKind aplica a política de VALOR ZERO por tipo (§7).
func validateAmountForKind(kind transaction.Kind, amount money.Money) error {
	if kind.AllowsZeroAmount() {
		// LOSS exige exatamente "0.00" — valor positivo aqui é erro de contrato.
		if kind == transaction.KindLoss && !amount.IsZero() {
			return fmt.Errorf("%w: LOSS requires amount 0.00, got %s", ports.ErrValidation, amount)
		}
		return nil
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: %s requires a positive amount, got %s", ports.ErrValidation, kind, amount)
	}
	return nil
}

// executeInScope roda o fluxo completo DENTRO de uma transação.
func (s *PlaceWagerOperationService) executeInScope(
	ctx context.Context,
	scope ports.TxScope,
	cmd ports.PlaceOperationCommand,
	parsed parsedOperation,
	correlationID string,
	now time.Time,
	result *ports.PlaceOperationResult,
) error {
	// 4.1) Inbox (§6.5) — somente no caminho SQS.
	if cmd.InboxConsumer != "" {
		if err := s.ensureInbox(ctx, scope, cmd); err != nil {
			return err
		}
	}

	// 4.2) Idempotência (§9): primeira consulta pela chave de idempotência.
	existing, err := scope.Transactions().FindByIDempotencyKey(ctx, parsed.idempotencyKey)
	switch {
	case err == nil:
		// A chave já existe. Duas possibilidades:
		if existing.PayloadHash() != parsed.payloadHash {
			// Conteúdo DIFERENTE com a mesma chave -> 409 (§9).
			return fmt.Errorf("%w: idempotency key %q reused with different payload",
				ports.ErrConflict, parsed.idempotencyKey)
		}
		// Conteúdo IGUAL -> REPLAY: devolvemos o resultado PERSISTIDO, sem
		// reaplicar nada (§9), inclusive `idempotentReplay: true`.
		*result = replayOf(existing)
		return nil

	case !errors.Is(err, ports.ErrNotFound):
		// Falha de leitura (banco fora, por exemplo): transitória. NÃO é replay.
		return err
	}

	// Chave nova: verificamos o PAR EXTERNO. §9: uma operação identificada por
	// (providerId, externalTransactionId) não pode ser reaplicada com outra chave.
	duplicated, err := scope.Transactions().FindByProviderAndExternalID(ctx, parsed.providerID, parsed.externalID)
	switch {
	case err == nil:
		// Encontrar o par externo NÃO é conflito automático: a requisição vencedora
		// pode ter comitado entre as duas consultas, tornando isto um replay. A
		// distinção correta é comparar a CHAVE da transação encontrada (§9: par
		// externo com outra chave é conflito; com a mesma chave é reenvio legítimo).
		if duplicated.IdempotencyKey() != parsed.idempotencyKey {
			return fmt.Errorf("%w: operation %s/%s already exists under idempotency key %q",
				ports.ErrConflict, parsed.providerID, parsed.externalID, duplicated.IdempotencyKey())
		}

		// Mesma chave: resta conferir o CONTEÚDO. Mesma chave e conteúdo diferente
		// continua sendo 409.
		if duplicated.PayloadHash() != parsed.payloadHash {
			return fmt.Errorf("%w: idempotency key %q reused with different payload",
				ports.ErrConflict, parsed.idempotencyKey)
		}

		*result = replayOf(duplicated)
		return nil

	case !errors.Is(err, ports.ErrNotFound):
		return err
	}

	// 4.3) Aplicação das regras (§7). A carteira é travada com SELECT ... FOR UPDATE
	// antes de qualquer decisão: leitura, decisão e escrita ficam serializadas por
	// carteira entre processos (§8), sem lock global (§5.6).
	w, err := scope.Wallets().LockByID(ctx, parsed.walletID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			// Carteira inexistente é 404, não rejeição persistida: wallet_id é NOT NULL
			// com FK, então não há transação a gravar. WALLET_NOT_FOUND fica para o caso
			// de posse (carteira de outro jogador), em que o FK é satisfeito.
			return fmt.Errorf("%w: wallet %s", ports.ErrNotFound, parsed.walletID)
		}
		return err
	}

	// Re-checagem da idempotência já com o lock da carteira. Sem isto, sob concorrência
	// as requisições que passaram juntas pela verificação inicial (sem lock) poderiam
	// perder a corrida do índice único antes do vencedor comitar (invisível no READ
	// COMMITTED), gerando 500. Com o lock, quando a segunda obtém a linha o vencedor JÁ
	// comitou e a leitura devolve replay. A verificação inicial permanece para evitar
	// pegar o lock quando o replay já pode ser respondido sem tocar na carteira.
	if existing, err := scope.Transactions().FindByIDempotencyKey(ctx, parsed.idempotencyKey); err == nil {
		if existing.PayloadHash() != parsed.payloadHash {
			return fmt.Errorf("%w: idempotency key %q reused with different payload",
				ports.ErrConflict, parsed.idempotencyKey)
		}
		*result = replayOf(existing)
		return nil
	} else if !errors.Is(err, ports.ErrNotFound) {
		return err
	}

	// Coerência carteira x operação: o jogador precisa ser o dono da carteira. Usamos
	// o MESMO código de carteira inexistente para não revelar a existência de carteira
	// de outro jogador (mesma lógica de isolamento entre provedores, §2).
	if w.PlayerID() != parsed.playerID {
		return s.reject(ctx, scope, cmd, parsed, correlationID, now, transaction.FailureWalletNotFound, result)
	}
	if !w.Currency().Equal(parsed.amount.Currency()) {
		return s.reject(ctx, scope, cmd, parsed, correlationID, now, transaction.FailureCurrencyMismatch, result)
	}

	switch parsed.kind {
	case transaction.KindLoss:
		// §7: LOSS não movimenta a carteira (sem ledger, saldo ou versão), mas conclui
		// com sucesso e produz WagerTransactionProcessed (§11).
		return s.completeWithoutMovement(ctx, scope, cmd, parsed, correlationID, now, w, result)

	case transaction.KindBet:
		movement, err := w.Debit(parsed.amount, now)
		if err != nil {
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				return s.reject(ctx, scope, cmd, parsed, correlationID, now, transaction.FailureBetInsufficientFunds, result)
			}
			return err
		}
		// Não há referência em BET: passamos um OptionalID vazio.
		return s.completeWithMovement(ctx, scope, cmd, parsed, correlationID, now, w, movement, transaction.NoID(), result)

	case transaction.KindWin:
		movement, err := w.Credit(parsed.amount, now)
		if err != nil {
			return err
		}
		return s.completeWithMovement(ctx, scope, cmd, parsed, correlationID, now, w, movement, transaction.NoID(), result)

	case transaction.KindRefund, transaction.KindRollback:
		return s.applyReversal(ctx, scope, cmd, parsed, correlationID, now, w, result)

	default:
		// Inalcançável (ParseKind já validou); mantido para exaustividade.
		return fmt.Errorf("%w: unsupported kind %s", ports.ErrValidation, parsed.kind)
	}
}

// ensureInbox registra a mensagem na inbox e valida reentregas (§6.5/§10).
func (s *PlaceWagerOperationService) ensureInbox(ctx context.Context, scope ports.TxScope, cmd ports.PlaceOperationCommand) error {
	err := scope.Inbox().Register(ctx, cmd.InboxConsumer, cmd.InboxMessageID, cmd.InboxMessageHash)
	if err == nil {
		// Primeira vez que vemos esta mensagem.
		return nil
	}
	if !errors.Is(err, ports.ErrAlreadyExists) {
		return err
	}

	// REENTREGA: §10 exige verificar o hash. Mesmo messageId com corpo DIFERENTE é
	// mensagem inválida (rejeição permanente, DLQ) — nunca aplicação a mais.
	existing, err := scope.Inbox().Find(ctx, cmd.InboxConsumer, cmd.InboxMessageID)
	if err != nil {
		return err
	}
	if existing.MessageHash != cmd.InboxMessageHash {
		return fmt.Errorf("%w: inbox message %q redelivered with different content",
			ports.ErrValidation, cmd.InboxMessageID)
	}

	// Mesma mensagem, mesmo conteúdo: se o tratamento anterior concluiu, a consulta à
	// chave (4.2) devolve replay; se morreu no meio, o processamento segue e a
	// idempotência da transação garante que nada seja aplicado duas vezes.
	return nil
}

// markInboxCompleted marca a conclusão durável do tratamento (§6.5). Chamado nos
// estados terminais e também em PENDING_REFERENCE: o §6.5 permite concluir a mensagem
// quando a pendência já está persistida, pois o worker de referências assume a
// continuidade. Não marcar aqui faria a mensagem voltar para sempre.
func (s *PlaceWagerOperationService) markInboxCompleted(ctx context.Context, scope ports.TxScope, cmd ports.PlaceOperationCommand) error {
	if cmd.InboxConsumer == "" {
		return nil
	}
	return scope.Inbox().MarkProcessed(ctx, cmd.InboxConsumer, cmd.InboxMessageID)
}

// completeWithoutMovement conclui uma operação que NÃO movimenta a carteira (LOSS).
func (s *PlaceWagerOperationService) completeWithoutMovement(
	ctx context.Context,
	scope ports.TxScope,
	cmd ports.PlaceOperationCommand,
	parsed parsedOperation,
	correlationID string,
	now time.Time,
	w *wallet.Wallet,
	result *ports.PlaceOperationResult,
) error {
	tx, err := s.newTransactionEntity(ctx, parsed, now)
	if err != nil {
		return err
	}

	// O saldo observado é o próprio saldo da carteira (inalterado). Isso importa
	// porque o evento `WagerTransactionProcessed` é emitido para LOSS (§11).
	if err := tx.MarkProcessed(w.Balance(), now); err != nil {
		return err
	}

	if err := scope.Transactions().Insert(ctx, tx); err != nil {
		if errors.Is(err, ports.ErrAlreadyExists) {
			return errConcurrentOperationInsert
		}
		return err
	}

	processedEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	processed, err := event.NewWagerTransactionProcessed(processedEventID, correlationID, "", tx, w.Balance(), now)
	if err != nil {
		return fmt.Errorf("%w: processed event: %v", ports.ErrValidation, err)
	}
	record, err := transactionEventRecord(processed.Envelope, processed)
	if err != nil {
		return err
	}
	if err := scope.Outbox().Append(ctx, record); err != nil {
		return err
	}
	if err := s.markInboxCompleted(ctx, scope, cmd); err != nil {
		return err
	}

	*result = ports.PlaceOperationResult{
		TransactionID:    tx.ID(),
		State:            tx.State(),
		Balance:          w.Balance(),
		BalanceKnown:     true,
		IdempotentReplay: false,
	}
	return nil
}

// completeWithMovement conclui uma operação que moveu a carteira (BET, WIN,
// REFUND, ROLLBACK), persistindo saldo, ledger, transação e DOIS eventos.
func (s *PlaceWagerOperationService) completeWithMovement(
	ctx context.Context,
	scope ports.TxScope,
	cmd ports.PlaceOperationCommand,
	parsed parsedOperation,
	correlationID string,
	now time.Time,
	w *wallet.Wallet,
	movement wallet.Movement,
	resolvedReference transaction.OptionalID,
	result *ports.PlaceOperationResult,
) error {
	tx, err := s.newTransactionEntity(ctx, parsed, now)
	if err != nil {
		return err
	}
	// Referência interna resolvida (§6.3): preenchida APENAS em reversões, para
	// auditoria e para a regra de reversão única por referência.
	if resolvedReference.Valid {
		if err := tx.AttachResolvedReference(resolvedReference.Value, now); err != nil {
			return err
		}
	}
	if err := tx.MarkProcessed(movement.BalanceAfter(), now); err != nil {
		return err
	}

	// Ledger: construído A PARTIR DO MOVIMENTO — logo, descreve exatamente o que
	// a carteira fez, sem chance de divergência.
	entry, err := ledger.NewEntryFromMovement(w.ID(), tx.ID(), movement, now)
	if err != nil {
		return fmt.Errorf("%w: ledger entry: %v", ports.ErrValidation, err)
	}

	// Evento 1: operação concluída (§11) — agregado é a TRANSAÇÃO.
	processedEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	processed, err := event.NewWagerTransactionProcessed(processedEventID, correlationID, "", tx, movement.BalanceAfter(), now)
	if err != nil {
		return fmt.Errorf("%w: processed event: %v", ports.ErrValidation, err)
	}

	// Evento 2: saldo alterado (§11) — agregado é a CARTEIRA.
	balanceEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	balanceChanged, err := event.NewWalletBalanceChanged(
		balanceEventID, correlationID, processed.Envelope.EventID, w.ID(), tx.ID(), movement, now)
	if err != nil {
		return fmt.Errorf("%w: balance event: %v", ports.ErrValidation, err)
	}

	// A ordem importa: wallet_ledger_entries.transaction_id é FK para
	// wager_transactions.id, então a transação precisa existir ANTES do lançamento.
	// Sequência: saldo -> transação -> ledger -> outbox, tudo na mesma transação SQL.
	if err := scope.Wallets().Update(ctx, w); err != nil {
		return err
	}
	if err := scope.Transactions().Insert(ctx, tx); err != nil {
		if errors.Is(err, ports.ErrAlreadyExists) {
			return errConcurrentOperationInsert
		}
		return err
	}
	if err := scope.Ledger().Append(ctx, entry); err != nil {
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
	if err := scope.Outbox().Append(ctx, processedRecord, balanceRecord); err != nil {
		return err
	}
	if err := s.markInboxCompleted(ctx, scope, cmd); err != nil {
		return err
	}

	*result = ports.PlaceOperationResult{
		TransactionID:    tx.ID(),
		State:            tx.State(),
		Balance:          movement.BalanceAfter(),
		BalanceKnown:     true,
		IdempotentReplay: false,
	}
	return nil
}

// reject persiste uma rejeição de negócio (§7) e emite o evento. Rejeição é um
// resultado, não um erro: a transação fica auditável com failureCode estável e o
// worker SQS pode remover a mensagem (§10).
func (s *PlaceWagerOperationService) reject(
	ctx context.Context,
	scope ports.TxScope,
	cmd ports.PlaceOperationCommand,
	parsed parsedOperation,
	correlationID string,
	now time.Time,
	code transaction.FailureCode,
	result *ports.PlaceOperationResult,
) error {
	tx, err := s.newTransactionEntity(ctx, parsed, now)
	if err != nil {
		return err
	}
	if err := tx.MarkRejected(code, now); err != nil {
		return err
	}

	if err := scope.Transactions().Insert(ctx, tx); err != nil {
		if errors.Is(err, ports.ErrAlreadyExists) {
			return errConcurrentOperationInsert
		}
		return err
	}

	rejectedEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	rejected, err := event.NewWagerTransactionRejected(rejectedEventID, correlationID, "", tx, code, now)
	if err != nil {
		return fmt.Errorf("%w: rejected event: %v", ports.ErrValidation, err)
	}
	record, err := transactionEventRecord(rejected.Envelope, rejected)
	if err != nil {
		return err
	}
	if err := scope.Outbox().Append(ctx, record); err != nil {
		return err
	}
	if err := s.markInboxCompleted(ctx, scope, cmd); err != nil {
		return err
	}

	// Rejeição não observa resultado financeiro: sem saldo (o replay devolveria um
	// número que não corresponde a nenhuma movimentação).
	*result = ports.PlaceOperationResult{
		TransactionID:    tx.ID(),
		State:            tx.State(),
		FailureCode:      code,
		BalanceKnown:     false,
		IdempotentReplay: false,
	}
	return nil
}

// newTransactionEntity constrói a transação em PENDING. Os efeitos (movimento, ledger,
// eventos) vêm depois e a entidade é persistida já em sua forma final — sem commit
// intermediário de aceite, como o §6.3 permite para operações sem dependências.
func (s *PlaceWagerOperationService) newTransactionEntity(
	ctx context.Context,
	parsed parsedOperation,
	now time.Time,
) (*transaction.Transaction, error) {
	// O contexto vem de fora (o da transação em curso): usar context.Background()
	// deixaria a geração do ID sem cancelamento/deadline do pedido original.
	idRaw, err := s.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: transaction id", ports.ErrUnavailable)
	}
	id, err := transaction.NewID(idRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: transaction id: %v", ports.ErrValidation, err)
	}

	tx, err := transaction.NewExternal(transaction.NewExternalParams{
		ID:                             id,
		Kind:                           parsed.kind,
		PlayerID:                       parsed.playerID,
		WalletID:                       parsed.walletID,
		Amount:                         parsed.amount,
		ProviderID:                     parsed.providerID,
		ExternalTransactionID:          parsed.externalID,
		IdempotencyKey:                 parsed.idempotencyKey,
		PayloadHash:                    parsed.payloadHash,
		RoundID:                        parsed.roundID,
		GameID:                         parsed.gameID,
		CorrelationID:                  parsed.correlationID,
		ReferenceExternalTransactionID: parsed.referenceExternalID,
		Now:                            now,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: transaction: %v", ports.ErrValidation, err)
	}
	return tx, nil
}

// replayOf converte uma transação persistida no resultado de replay (§9).
func replayOf(tx *transaction.Transaction) ports.PlaceOperationResult {
	replay := ports.PlaceOperationResult{
		TransactionID:    tx.ID(),
		State:            tx.State(),
		FailureCode:      tx.FailureCode(),
		IdempotentReplay: true,
	}
	// §9: em replay de operação concluída, o saldo é o observado no processamento
	// original — vem do REGISTRO, não de uma nova consulta à carteira.
	if result := tx.ResultBalance(); result.Valid {
		replay.Balance = result.Value
		replay.BalanceKnown = true
	}
	return replay
}
