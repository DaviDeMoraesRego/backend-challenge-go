// Fakes in-memory das portas outbound para testar as regras aplicacionais sem banco.
// Emulam as constraints de unicidade (-> ErrAlreadyExists) e a ausência de registro
// (-> ErrNotFound), além do "bigserial" do ledger. NÃO emulam transação de verdade (sem
// rollback) nem locks: atomicidade e concorrência reais são provadas contra o Postgres
// nos testes de integração.
package usecase

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/ledger"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// fakeClock devolve sempre o mesmo instante até ser avançado (determinismo para
// backoff, TTL e timestamps).
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// fakeIDs gera identificadores sequenciais e legíveis ("tx-0001"); o domínio exige
// apenas não-vazio.
type fakeIDs struct {
	mu     sync.Mutex
	prefix string
	seq    int
}

func (g *fakeIDs) NewID(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	return fmt.Sprintf("%s-%04d", g.prefix, g.seq), nil
}

type fakeStore struct {
	mu sync.Mutex

	wallets      map[wallet.ID]*wallet.Wallet
	transactions map[transaction.ID]*transaction.Transaction
	ledger       []*ledger.Entry
	ledgerNextID int64

	inbox  map[string]*ports.InboxMessage
	outbox []ports.OutboxRecord

	// deadOutbox emula outbox_events.dead_at: eventos desistidos saem do ClaimPending.
	deadOutbox map[string]struct{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		wallets:      map[wallet.ID]*wallet.Wallet{},
		transactions: map[transaction.ID]*transaction.Transaction{},
		inbox:        map[string]*ports.InboxMessage{},
		deadOutbox:   map[string]struct{}{},
	}
}

// outboxByEventID encontra o índice de um evento na outbox.
func (s *fakeStore) outboxIndex(eventID string) int {
	for i := range s.outbox {
		if s.outbox[i].EventID == eventID {
			return i
		}
	}
	return -1
}

// inboxKey monta a chave composta (consumerName, messageId) — a mesma da PK.
func inboxKey(consumerName, messageID string) string {
	return consumerName + "|" + messageID
}

type fakeTxScope struct{ store *fakeStore }

func (s *fakeTxScope) Wallets() ports.WalletRepository { return &fakeWalletRepository{store: s.store} }
func (s *fakeTxScope) Transactions() ports.WagerTransactionRepository {
	return &fakeTransactionRepository{store: s.store}
}
func (s *fakeTxScope) Ledger() ports.LedgerRepository { return &fakeLedgerRepository{store: s.store} }
func (s *fakeTxScope) Inbox() ports.InboxRepository   { return &fakeInboxRepository{store: s.store} }
func (s *fakeTxScope) Outbox() ports.OutboxRepository { return &fakeOutboxRepository{store: s.store} }

// fakeUnitOfWork executa o bloco e devolve o erro (sem rollback — ver o cabeçalho).
type fakeUnitOfWork struct{ store *fakeStore }

func (u *fakeUnitOfWork) Execute(ctx context.Context, fn func(context.Context, ports.TxScope) error) error {
	return fn(ctx, &fakeTxScope{store: u.store})
}

type fakeWalletRepository struct{ store *fakeStore }

func (r *fakeWalletRepository) Insert(ctx context.Context, w *wallet.Wallet) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	// Emula UNIQUE (player_id, currency).
	for _, existing := range r.store.wallets {
		if existing.PlayerID() == w.PlayerID() && existing.Currency().Equal(w.Currency()) {
			return ports.ErrAlreadyExists
		}
	}
	if _, exists := r.store.wallets[w.ID()]; exists {
		return ports.ErrAlreadyExists
	}
	r.store.wallets[w.ID()] = w
	return nil
}

func (r *fakeWalletRepository) FindByPlayerAndCurrency(ctx context.Context, playerID wallet.PlayerID, currency money.Currency) (*wallet.Wallet, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, w := range r.store.wallets {
		if w.PlayerID() == playerID && w.Currency().Equal(currency) {
			return w, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *fakeWalletRepository) FindByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	w, ok := r.store.wallets[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return w, nil
}

// LockByID: no fake, "travar" é apenas buscar. Ver a limitação documentada.
func (r *fakeWalletRepository) LockByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error) {
	return r.FindByID(ctx, id)
}

func (r *fakeWalletRepository) Update(ctx context.Context, w *wallet.Wallet) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	if _, ok := r.store.wallets[w.ID()]; !ok {
		return ports.ErrNotFound
	}
	r.store.wallets[w.ID()] = w
	return nil
}

type fakeTransactionRepository struct{ store *fakeStore }

func (r *fakeTransactionRepository) Insert(ctx context.Context, tx *transaction.Transaction) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, existing := range r.store.transactions {
		// UNIQUE (idempotency_key).
		if tx.IdempotencyKey() != "" && existing.IdempotencyKey() == tx.IdempotencyKey() {
			return ports.ErrAlreadyExists
		}
		// UNIQUE (provider_id, external_transaction_id) para origem EXTERNAL.
		if tx.Origin() == transaction.OriginExternal && existing.Origin() == transaction.OriginExternal &&
			existing.ProviderID() == tx.ProviderID() &&
			existing.ExternalTransactionID() == tx.ExternalTransactionID() {
			return ports.ErrAlreadyExists
		}
		// UNIQUE (wallet_id) WHERE kind = 'OPENING' — um crédito inicial por carteira.
		if tx.Kind() == transaction.KindOpening && existing.Kind() == transaction.KindOpening &&
			existing.WalletID() == tx.WalletID() {
			return ports.ErrAlreadyExists
		}
	}
	if _, exists := r.store.transactions[tx.ID()]; exists {
		return ports.ErrAlreadyExists
	}
	r.store.transactions[tx.ID()] = tx
	return nil
}

func (r *fakeTransactionRepository) Update(ctx context.Context, tx *transaction.Transaction) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	if _, ok := r.store.transactions[tx.ID()]; !ok {
		return ports.ErrNotFound
	}
	r.store.transactions[tx.ID()] = tx
	return nil
}

func (r *fakeTransactionRepository) FindByIDempotencyKey(ctx context.Context, key transaction.IdempotencyKey) (*transaction.Transaction, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, tx := range r.store.transactions {
		if tx.IdempotencyKey() == key {
			return tx, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *fakeTransactionRepository) FindByProviderAndExternalID(ctx context.Context, providerID transaction.ProviderID, externalID transaction.ExternalTransactionID) (*transaction.Transaction, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, tx := range r.store.transactions {
		if tx.Origin() == transaction.OriginExternal &&
			tx.ProviderID() == providerID &&
			tx.ExternalTransactionID() == externalID {
			return tx, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *fakeTransactionRepository) FindByID(ctx context.Context, id transaction.ID) (*transaction.Transaction, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	tx, ok := r.store.transactions[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return tx, nil
}

// HasSuccessfulReversal emula o índice único parcial wtx_reversal_once_per_reference_uniq.
func (r *fakeTransactionRepository) HasSuccessfulReversal(ctx context.Context, providerID transaction.ProviderID, referenceExternalID transaction.ExternalTransactionID) (bool, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, tx := range r.store.transactions {
		if tx.Kind().IsReversal() &&
			tx.State() == transaction.StateProcessed &&
			tx.ProviderID() == providerID &&
			tx.ReferenceExternalTransactionID() == referenceExternalID {
			return true, nil
		}
	}
	return false, nil
}

// ListDuePendingReferences devolve as pendências vencidas, ordenadas: a iteração de
// mapa em Go é aleatória e sem ordenar o teste ficaria intermitente.
func (r *fakeTransactionRepository) ListDuePendingReferences(ctx context.Context, now time.Time, limit int) ([]*transaction.Transaction, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	var due []*transaction.Transaction
	for _, tx := range r.store.transactions {
		if tx.State() == transaction.StatePendingReference && !tx.NextRetryAt().After(now) {
			due = append(due, tx)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].NextRetryAt().Equal(due[j].NextRetryAt()) {
			return due[i].ID() < due[j].ID()
		}
		return due[i].NextRetryAt().Before(due[j].NextRetryAt())
	})
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

type fakeLedgerRepository struct{ store *fakeStore }

// Append atribui o id sequencial aqui (como o BIGSERIAL), via ledger.Rehydrate.
func (r *fakeLedgerRepository) Append(ctx context.Context, entry *ledger.Entry) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, existing := range r.store.ledger {
		// UNIQUE (wallet_id, transaction_id).
		if existing.WalletID() == entry.WalletID() && existing.TransactionID() == entry.TransactionID() {
			return ports.ErrAlreadyExists
		}
	}

	r.store.ledgerNextID++
	stored, err := ledger.Rehydrate(ledger.RehydrateParams{
		ID:            r.store.ledgerNextID,
		WalletID:      entry.WalletID(),
		TransactionID: entry.TransactionID(),
		Direction:     entry.Direction(),
		Amount:        entry.Amount(),
		BalanceBefore: entry.BalanceBefore(),
		BalanceAfter:  entry.BalanceAfter(),
		CreatedAt:     entry.CreatedAt(),
	})
	if err != nil {
		return err
	}
	r.store.ledger = append(r.store.ledger, stored)
	return nil
}

func (r *fakeLedgerRepository) ListByWallet(ctx context.Context, walletID wallet.ID, afterID int64, limit int) ([]*ledger.Entry, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	var page []*ledger.Entry
	for _, entry := range r.store.ledger {
		if entry.WalletID() == walletID && entry.ID() > afterID {
			page = append(page, entry)
		}
	}
	// O fake mantém a ordem de inserção, que é a mesma de `id` crescente — a
	// mesma garantia que o adapter Postgres dará com ORDER BY id.
	if limit > 0 && len(page) > limit {
		page = page[:limit]
	}
	return page, nil
}

func (r *fakeLedgerRepository) SumSignedAmounts(ctx context.Context, walletID wallet.ID) (money.Money, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	// Precisamos da moeda para começar a soma. Se não houver lançamento, a moeda
	// é desconhecida: devolvemos o zero da moeda da carteira correspondente.
	currency := money.Currency("")
	for _, w := range r.store.wallets {
		if w.ID() == walletID {
			currency = w.Currency()
			break
		}
	}
	if currency == "" {
		return money.Money{}, ports.ErrNotFound
	}

	total, err := money.Zero(currency)
	if err != nil {
		return money.Money{}, err
	}
	for _, entry := range r.store.ledger {
		if entry.WalletID() != walletID {
			continue
		}
		signed, err := entry.SignedAmount()
		if err != nil {
			return money.Money{}, err
		}
		total, err = total.Add(signed)
		if err != nil {
			return money.Money{}, err
		}
	}
	return total, nil
}

func (r *fakeLedgerRepository) CountByWallet(ctx context.Context, walletID wallet.ID) (int, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	count := 0
	for _, entry := range r.store.ledger {
		if entry.WalletID() == walletID {
			count++
		}
	}
	return count, nil
}

type fakeInboxRepository struct{ store *fakeStore }

func (r *fakeInboxRepository) Register(ctx context.Context, consumerName, messageID, messageHash string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	key := inboxKey(consumerName, messageID)
	if _, exists := r.store.inbox[key]; exists {
		return ports.ErrAlreadyExists
	}
	r.store.inbox[key] = &ports.InboxMessage{
		ConsumerName: consumerName,
		MessageID:    messageID,
		MessageHash:  messageHash,
		ReceivedAt:   time.Now().UTC(),
	}
	return nil
}

func (r *fakeInboxRepository) MarkProcessed(ctx context.Context, consumerName, messageID string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.inbox[inboxKey(consumerName, messageID)]
	if !ok {
		return ports.ErrNotFound
	}
	record.ProcessedAt = time.Now().UTC()
	return nil
}

func (r *fakeInboxRepository) Find(ctx context.Context, consumerName, messageID string) (*ports.InboxMessage, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.inbox[inboxKey(consumerName, messageID)]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return record, nil
}

type fakeOutboxRepository struct{ store *fakeStore }

func (r *fakeOutboxRepository) Append(ctx context.Context, records ...ports.OutboxRecord) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	r.store.outbox = append(r.store.outbox, records...)
	return nil
}

func (r *fakeOutboxRepository) ClaimPending(ctx context.Context, now, leaseUntil time.Time, limit int) ([]ports.OutboxRecord, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	var claimed []ports.OutboxRecord
	for i := range r.store.outbox {
		record := &r.store.outbox[i]
		if !record.PublishedAt.IsZero() {
			continue
		}
		if _, dead := r.store.deadOutbox[record.EventID]; dead {
			continue
		}
		if record.NextAttemptAt.After(now) {
			// Reservado por outro publisher (lease ainda válido).
			continue
		}

		// Emula o LEASE do `UPDATE ... SET next_attempt_at = leaseUntil`: a linha
		// fica indisponível até o lease vencer, sem manter lock aberto.
		record.NextAttemptAt = leaseUntil
		claimed = append(claimed, *record)

		if limit > 0 && len(claimed) == limit {
			break
		}
	}
	return claimed, nil
}

func (r *fakeOutboxRepository) MarkPublished(ctx context.Context, eventID string, publishedAt time.Time) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	idx := r.store.outboxIndex(eventID)
	if idx < 0 {
		return ports.ErrNotFound
	}
	r.store.outbox[idx].PublishedAt = publishedAt
	return nil
}

func (r *fakeOutboxRepository) Reschedule(ctx context.Context, eventID string, attempts int, nextAttemptAt time.Time) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	idx := r.store.outboxIndex(eventID)
	if idx < 0 {
		return ports.ErrNotFound
	}
	r.store.outbox[idx].Attempts = attempts
	r.store.outbox[idx].NextAttemptAt = nextAttemptAt
	return nil
}

// MarkDead espelha o adapter: marca como morto e incrementa attempts.
func (r *fakeOutboxRepository) MarkDead(ctx context.Context, eventID string, deadAt time.Time) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	if r.store.deadOutbox == nil {
		r.store.deadOutbox = map[string]struct{}{}
	}
	r.store.deadOutbox[eventID] = struct{}{}
	if idx := r.store.outboxIndex(eventID); idx >= 0 {
		r.store.outbox[idx].Attempts++
	}
	return nil
}

// -----------------------------------------------------------------------------
// Publisher
// -----------------------------------------------------------------------------

// fakeEventPublisher registra as publicações e pode ser instruído a falhar.
type fakeEventPublisher struct {
	mu        sync.Mutex
	published []ports.OutboxRecord
	groupIDs  []string
	failNext  error
}

func (p *fakeEventPublisher) Publish(ctx context.Context, record ports.OutboxRecord, groupID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.failNext != nil {
		err := p.failNext
		p.failNext = nil
		return err
	}
	p.published = append(p.published, record)
	p.groupIDs = append(p.groupIDs, groupID)
	return nil
}

// count publica com sucesso quantos eventos foram entregues ao broker.
func (p *fakeEventPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

// harness reúne as dependências fakes e os casos de uso já montados.
//
// Sem ele, cada teste repetiria dez linhas de construção — e um teste difícil de
// escrever é um teste que ninguém escreve.
type harness struct {
	store     *fakeStore
	clock     *fakeClock
	ids       *fakeIDs
	publisher *fakeEventPublisher

	openWallet *OpenWalletService
	place      *PlaceWagerOperationService
	resolver   *ResolvePendingReferencesService
	relay      *RelayOutboxService

	// Reads
	getWallet      *GetWalletService
	getLedger      *GetWalletLedgerService
	getTransaction *GetWagerTransactionService
	getByExternal  *GetWagerTransactionByExternalIDService
	reconcile      *ReconcileWalletService
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store := newFakeStore()
	clock := &fakeClock{now: time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)}
	ids := &fakeIDs{prefix: "id"}
	uow := &fakeUnitOfWork{store: store}
	publisher := &fakeEventPublisher{}

	return &harness{
		store:      store,
		clock:      clock,
		ids:        ids,
		publisher:  publisher,
		openWallet: NewOpenWalletService(uow, clock, ids),
		place: NewPlaceWagerOperationService(uow, clock, ids, PlaceWagerOperationConfig{
			PendingReferenceBaseBackoff: time.Second,
		}),
		resolver: NewResolvePendingReferencesService(uow, clock, ids, ResolvePendingReferencesConfig{
			BaseBackoff: time.Second,
			MaxAttempts: 3,
		}),
		relay: NewRelayOutboxService(uow, publisher, clock, RelayOutboxConfig{
			BatchSize:   50,
			Lease:       time.Second,
			BaseBackoff: time.Second,
			MaxAttempts: 3,
		}),
		getWallet:      NewGetWalletService(uow),
		getLedger:      NewGetWalletLedgerService(uow),
		getTransaction: NewGetWagerTransactionService(uow),
		getByExternal:  NewGetWagerTransactionByExternalIDService(uow),
		reconcile:      NewReconcileWalletService(uow),
	}
}

// internalPrincipal é a identidade do serviço interno (§2).
func internalPrincipal() ports.Principal {
	return ports.Principal{Scope: ports.ScopeInternal}
}

// providerPrincipal é a identidade de um provedor autenticado (§2).
func providerPrincipal(providerID string) ports.Principal {
	return ports.Principal{Scope: ports.ScopeProvider, ProviderID: providerID}
}

const (
	testProviderA = "provider-a"
	testProviderB = "provider-b"
	testPlayerID  = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
)

// seedWallet abre uma carteira pelo CAMINHO REAL (o caso de uso de abertura),
// e não inserindo direto no mapa: assim os testes cobrem também os eventos e o
// lançamento de abertura.
func (h *harness) seedWallet(t *testing.T, initialAmount string) ports.OpenWalletResult {
	t.Helper()

	result, err := h.openWallet.Execute(context.Background(), ports.OpenWalletCommand{
		Principal:       internalPrincipal(),
		PlayerID:        testPlayerID,
		InitialAmount:   initialAmount,
		InitialCurrency: "BRL",
		CorrelationID:   "corr-seed",
	})
	if err != nil {
		t.Fatalf("seedWallet(%s) unexpected error: %v", initialAmount, err)
	}
	return result
}

// operation monta um comando de operação com valores padrão sensatos.
func (h *harness) operation(t *testing.T, walletID wallet.ID, kind, amount, externalID string) ports.PlaceOperationCommand {
	t.Helper()
	return ports.PlaceOperationCommand{
		Principal:             providerPrincipal(testProviderA),
		ProviderID:            testProviderA,
		ExternalTransactionID: externalID,
		IdempotencyKey:        testProviderA + ":" + externalID,
		PlayerID:              testPlayerID,
		WalletID:              walletID.String(),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Amount:                amount,
		Currency:              "BRL",
		CorrelationID:         "corr-" + externalID,
	}
}

// ledgerCount devolve quantos lançamentos a carteira possui.
func (h *harness) ledgerCount(t *testing.T, walletID wallet.ID) int {
	t.Helper()
	count, err := (&fakeLedgerRepository{store: h.store}).CountByWallet(context.Background(), walletID)
	if err != nil {
		t.Fatalf("CountByWallet unexpected error: %v", err)
	}
	return count
}

// balanceOf lê o saldo direto do fake (para asserções curtas).
func (h *harness) balanceOf(t *testing.T, walletID wallet.ID) string {
	t.Helper()
	w, err := (&fakeWalletRepository{store: h.store}).FindByID(context.Background(), walletID)
	if err != nil {
		t.Fatalf("FindByID unexpected error: %v", err)
	}
	return w.Balance().String()
}

// outboxByType filtra os eventos gravados por tipo (para asserções de evento).
func (h *harness) outboxByType(eventType string) []ports.OutboxRecord {
	var found []ports.OutboxRecord
	for _, record := range h.store.outbox {
		if record.EventType == eventType {
			found = append(found, record)
		}
	}
	return found
}

// payloadContains é um atalho para inspecionar o JSON do evento gravado.
func payloadContains(t *testing.T, record ports.OutboxRecord, fragment string) {
	t.Helper()
	if !strings.Contains(string(record.Payload), fragment) {
		t.Errorf("payload do evento %s nao contem %q:\n%s", record.EventType, fragment, record.Payload)
	}
}
