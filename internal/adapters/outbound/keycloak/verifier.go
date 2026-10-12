// Decisões de segurança: RS256 fixado (evita confusão de algoritmo: "alg":"none" ou
// HMAC com a chave pública), issuer validado (amarra o token a este sistema) e JWKS
// resolvido por `kid` com intervalo mínimo entre buscas (kid inexistente não vira DoS
// contra o IdP). O JWKS (n e e em base64url) é montado à mão — ~40 linhas — e o parsing
// do JWT fica com golang-jwt/jwt/v5, a parte criptográfica sensível.
package keycloak

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

var (
	// ErrUnauthenticated: token ausente, malformado, expirado ou com assinatura
	// inválida. O adapter HTTP traduz para 401.
	ErrUnauthenticated = errors.New("keycloak: unauthenticated")

	// ErrUnknownScope: token válido, mas sem a claim que define o escopo.
	ErrUnknownScope = errors.New("keycloak: unknown service scope")
)

// minRefreshInterval: sem ele, tokens com `kid` inexistente fariam o serviço martelar
// o IdP a cada requisição (amplificação que derruba a autenticação).
const minRefreshInterval = 30 * time.Second

type Verifier struct {
	issuer     string
	jwksURL    string
	httpClient *http.Client

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	lastFetch time.Time
}

func NewVerifier(jwksURL, issuer string, httpClient *http.Client) (*Verifier, error) {
	if jwksURL == "" || issuer == "" {
		return nil, fmt.Errorf("keycloak: jwksURL and issuer are required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Verifier{
		issuer:     issuer,
		jwksURL:    jwksURL,
		httpClient: httpClient,
		keys:       make(map[string]*rsa.PublicKey),
	}, nil
}

// Verify valida o token e devolve a identidade autenticada.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (ports.Principal, error) {
	if rawToken == "" {
		return ports.Principal{}, fmt.Errorf("%w: missing token", ErrUnauthenticated)
	}

	parsed, err := jwt.Parse(rawToken, v.keyFor(ctx),
		// Fixa RS256: impede o ataque de confusão de algoritmo.
		jwt.WithValidMethods([]string{"RS256"}),
		// Amarra o token a ESTE emissor.
		jwt.WithIssuer(v.issuer),
		// Exige `exp`: token sem expiração é aceito para sempre se vazar.
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		// Indisponibilidade do IdP (JWKS fora) precisa continuar como ErrUnavailable
		// para virar 503, não 401. Embrulhar com `%v` perderia a cadeia de errors.Is.
		if errors.Is(err, ports.ErrUnavailable) {
			return ports.Principal{}, err
		}
		// A biblioteca distingue expirado de assinatura inválida (útil no log), mas ao
		// cliente devolvemos o mesmo 401, sem pistas do motivo.
		return ports.Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if !parsed.Valid {
		return ports.Principal{}, fmt.Errorf("%w: invalid token", ErrUnauthenticated)
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return ports.Principal{}, fmt.Errorf("%w: unexpected claims type", ErrUnauthenticated)
	}

	// A claim `service_scope` distingue os dois mundos de acesso (§2). Ela vem
	// dos protocol mappers do realm (deploy/keycloak/realm-export.json).
	scope := claimString(claims, "service_scope")
	principal := ports.Principal{
		Scope:      ports.Scope(scope),
		ProviderID: claimString(claims, "provider_id"),
	}

	switch principal.Scope {
	case ports.ScopeInternal:
		return principal, nil

	case ports.ScopeProvider:
		// Um provedor SEM `provider_id` não pode ser autorizado a nada: não
		// saberíamos em nome de quem ele age. Recusar é a única resposta segura.
		if principal.ProviderID == "" {
			return ports.Principal{}, fmt.Errorf("%w: provider identity without provider_id claim", ErrUnauthenticated)
		}
		return principal, nil

	default:
		return ports.Principal{}, fmt.Errorf("%w: %q", ErrUnknownScope, scope)
	}
}

// keyFor resolve a chave pública pelo `kid` do cabeçalho (o identificador que o IdP
// publicou). A assinatura não recebe contexto — daí o contexto de cache interno.
func (v *Verifier) keyFor(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, fmt.Errorf("%w: token without kid", ErrUnauthenticated)
		}

		// Caminho quente: chave já em cache.
		if key := v.cachedKey(kid); key != nil {
			return key, nil
		}

		// `kid` desconhecida: pode ser rotação legítima do IdP. Re-buscamos,
		// respeitando o intervalo mínimo.
		if err := v.refresh(ctx); err != nil {
			return nil, err
		}

		if key := v.cachedKey(kid); key != nil {
			return key, nil
		}
		// `kid` continua desconhecida depois da busca: token não é deste IdP.
		return nil, fmt.Errorf("%w: unknown key id %q", ErrUnauthenticated, kid)
	}
}

func (v *Verifier) cachedKey(kid string) *rsa.PublicKey {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.keys[kid]
}

// refresh busca o JWKS e substitui o cache de chaves.
func (v *Verifier) refresh(ctx context.Context) error {
	v.mu.Lock()
	// Segunda checagem já com o lock: evita N requisições simultâneas disparando N
	// buscas ao IdP.
	if time.Since(v.lastFetch) < minRefreshInterval {
		v.mu.Unlock()
		return nil
	}
	v.mu.Unlock()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("keycloak: cannot build JWKS request: %w", err)
	}

	response, err := v.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: cannot reach JWKS endpoint: %v", ports.ErrUnavailable, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: JWKS endpoint returned %d", ports.ErrUnavailable, response.StatusCode)
	}

	var document jwksDocument
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		return fmt.Errorf("keycloak: invalid JWKS document: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		parsed, err := key.toRSAPublicKey()
		if err != nil {
			// Chave de outro tipo (EC, etc.) é ignorada, não fatal.
			continue
		}
		keys[key.Kid] = parsed
	}

	v.mu.Lock()
	v.keys = keys
	v.lastFetch = time.Now()
	v.mu.Unlock()

	return nil
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

// jwk é uma chave no formato JSON Web Key.
type jwk struct {
	Kty string `json:"kty"` // "RSA"
	Kid string `json:"kid"` // identificador usado no cabeçalho do JWT
	Use string `json:"use"` // "sig"
	N   string `json:"n"`   // módulo, base64url
	E   string `json:"e"`   // expoente público, base64url
}

// toRSAPublicKey converte o JWK em *rsa.PublicKey.
func (k jwk) toRSAPublicKey() (*rsa.PublicKey, error) {
	if k.Kty != "RSA" {
		return nil, fmt.Errorf("keycloak: unsupported key type %q", k.Kty)
	}
	if k.N == "" || k.E == "" {
		return nil, errors.New("keycloak: JWK missing modulus or exponent")
	}

	// JWK usa base64url sem padding.
	modulusBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("keycloak: invalid modulus: %w", err)
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("keycloak: invalid exponent: %w", err)
	}

	// O expoente ocupa poucos bytes (65537 é o habitual); acumulamos num int.
	var exponent int
	for _, b := range exponentBytes {
		exponent = exponent<<8 | int(b)
	}
	if exponent <= 0 {
		return nil, errors.New("keycloak: invalid RSA exponent")
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(modulusBytes),
		E: exponent,
	}, nil
}

// claimString lê uma claim textual, tolerando ausência.
func claimString(claims jwt.MapClaims, name string) string {
	value, ok := claims[name].(string)
	if !ok {
		return ""
	}
	return value
}
