package keycloak

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

const testIssuer = "https://issuer.test/realms/wager"

// newSigningKey gera um par RSA e o documento JWKS público (com o `kid`).
func newSigningKey(t *testing.T, kid string) (*rsa.PrivateKey, []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	document := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": kid,
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return key, encoded
}

func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// jwksServer serve um corpo fixo com o status informado.
func jwksServer(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestVerify_ValidTokenReturnsPrincipal(t *testing.T) {
	key, jwks := newSigningKey(t, "kid-1")
	server := jwksServer(t, jwks, http.StatusOK)

	verifier, err := NewVerifier(server.URL, testIssuer, server.Client())
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	token := signToken(t, key, "kid-1", jwt.MapClaims{
		"iss":           testIssuer,
		"exp":           time.Now().Add(time.Minute).Unix(),
		"service_scope": "provider",
		"provider_id":   "provider-a",
	})

	principal, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if principal.Scope != ports.ScopeProvider || principal.ProviderID != "provider-a" {
		t.Errorf("principal = %+v, want provider/provider-a", principal)
	}
}

// TestVerify_JWKSUnavailableIsClassified trava o ADR-008: queda do IdP é
// ErrUnavailable (503), nunca 401.
func TestVerify_JWKSUnavailableIsClassified(t *testing.T) {
	// JWKS responde 500: indisponibilidade, não credencial inválida.
	server := jwksServer(t, []byte(`{}`), http.StatusInternalServerError)

	verifier, err := NewVerifier(server.URL, testIssuer, server.Client())
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	key, _ := newSigningKey(t, "kid-1")
	token := signToken(t, key, "kid-1", jwt.MapClaims{
		"iss":           testIssuer,
		"exp":           time.Now().Add(time.Minute).Unix(),
		"service_scope": "internal",
	})

	if _, err := verifier.Verify(context.Background(), token); !errors.Is(err, ports.ErrUnavailable) {
		t.Fatalf("error = %v, want ports.ErrUnavailable (503, nao 401)", err)
	}
}

func TestVerify_WrongIssuerIsUnauthenticated(t *testing.T) {
	key, jwks := newSigningKey(t, "kid-1")
	server := jwksServer(t, jwks, http.StatusOK)

	verifier, err := NewVerifier(server.URL, testIssuer, server.Client())
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// Assinatura válida (kid presente), mas emissor diferente: 401, não 503.
	token := signToken(t, key, "kid-1", jwt.MapClaims{
		"iss":           "https://outro-emissor/realms/wager",
		"exp":           time.Now().Add(time.Minute).Unix(),
		"service_scope": "internal",
	})

	_, err = verifier.Verify(context.Background(), token)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("error = %v, want ErrUnauthenticated", err)
	}
	if errors.Is(err, ports.ErrUnavailable) {
		t.Errorf("emissor invalido NAO pode ser classificado como indisponibilidade")
	}
}

func TestVerify_ProviderWithoutProviderIDIsRejected(t *testing.T) {
	key, jwks := newSigningKey(t, "kid-1")
	server := jwksServer(t, jwks, http.StatusOK)

	verifier, err := NewVerifier(server.URL, testIssuer, server.Client())
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// Provedor sem `provider_id`: não saberíamos em nome de quem ele age -> recusar.
	token := signToken(t, key, "kid-1", jwt.MapClaims{
		"iss":           testIssuer,
		"exp":           time.Now().Add(time.Minute).Unix(),
		"service_scope": "provider",
	})

	if _, err := verifier.Verify(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("error = %v, want ErrUnauthenticated", err)
	}
}

func TestVerify_MissingToken(t *testing.T) {
	verifier, err := NewVerifier("http://unused.invalid/jwks", testIssuer, nil)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("error = %v, want ErrUnauthenticated", err)
	}
}
