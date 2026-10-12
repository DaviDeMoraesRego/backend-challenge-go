// O pool mantém conexões vivas e as empresta (MaxConns é a primeira defesa contra
// pico de carga) e é seguro para uso concorrente — diferente de pgx.Conn, que
// representa uma única conexão. A configuração é validada na inicialização (§4).
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig reúne os parâmetros de conexão e de dimensionamento.
type PoolConfig struct {
	// URL é a string de conexão (postgres://user:pass@host:port/db?sslmode=...).
	URL string

	// MaxConns é o teto de conexões simultâneas do pool.
	MaxConns int32
	// MinConns é quantas conexões ficam aquecidas (evita latência na primeira
	// rajada após ociosidade).
	MinConns int32
	// MaxConnLifetime recicla conexões após um tempo — útil quando há
	// balanceador/proxy no meio que pode derrubar conexões silenciosamente.
	MaxConnLifetime time.Duration
	// MaxConnIdleTime fecha conexões ociosas, liberando recursos do banco.
	MaxConnIdleTime time.Duration
	// ConnectTimeout limita quanto tempo esperamos para ESTABELECER conexão.
	ConnectTimeout time.Duration
}

// comDefaults aplica valores seguros quando algum campo não foi informado.
func (c PoolConfig) comDefaults() PoolConfig {
	if c.MaxConns <= 0 {
		c.MaxConns = 20
	}
	if c.MinConns < 0 {
		c.MinConns = 0
	}
	if c.MaxConnLifetime <= 0 {
		c.MaxConnLifetime = 30 * time.Minute
	}
	if c.MaxConnIdleTime <= 0 {
		c.MaxConnIdleTime = 5 * time.Minute
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 5 * time.Second
	}
	return c
}

// NewPool cria o pool e verifica a conectividade com um Ping: falhar na inicialização
// é mais fácil de diagnosticar do que falhar na primeira consulta real.
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("postgres: connection URL is required")
	}
	resolved := cfg.comDefaults()

	// ParseConfig valida a URL e devolve a configuração base do driver.
	poolConfig, err := pgxpool.ParseConfig(resolved.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: invalid connection URL: %w", err)
	}

	poolConfig.MaxConns = resolved.MaxConns
	poolConfig.MinConns = resolved.MinConns
	poolConfig.MaxConnLifetime = resolved.MaxConnLifetime
	poolConfig.MaxConnIdleTime = resolved.MaxConnIdleTime
	poolConfig.ConnConfig.ConnectTimeout = resolved.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot create pool: %w", err)
	}

	// Ping com prazo próprio: banco sem resposta em ConnectTimeout falha a subida.
	pingCtx, cancel := context.WithTimeout(ctx, resolved.ConnectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		// Fecha o pool para não vazar conexões numa inicialização que vai falhar.
		pool.Close()
		return nil, fmt.Errorf("%w: cannot reach PostgreSQL: %v", errConnectivity, err)
	}

	return pool, nil
}
