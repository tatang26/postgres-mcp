package database

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sync"
	"time"

	"postgres-mcp/internal/database/config"
)

// connectTimeout bounds how long a single lazy connection attempt (Open +
// Ping) is allowed to take, so a hung/unreachable remote host can't block
// the whole single-threaded MCP request loop indefinitely.
const connectTimeout = 10 * time.Second

// Pool tuning applied to every registered connection. These are
// deliberately conservative: this server serves one client at a time (see
// package mcpserver's doc comment), so it never needs many concurrent
// connections per database - just enough to avoid a single slow query
// blocking a fast one, while keeping this server's footprint on the
// target database's max_connections small.
const (
	maxOpenConns    = 5
	maxIdleConns    = 2
	connMaxLifetime = 30 * time.Minute
	connMaxIdleTime = 5 * time.Minute
)

// Limits bounds resource usage for every query executed through this
// server, applied uniformly across all registered connections.
type Limits struct {
	// QueryTimeout bounds how long any single query (or EXPLAIN ANALYZE)
	// is allowed to run before its context is canceled.
	QueryTimeout time.Duration
	// MaxRows bounds how many rows a single query result is allowed to
	// contain before the result is truncated.
	MaxRows int
	// MaxBytes bounds the approximate serialized size (in bytes) of a
	// single query result before it is truncated. This is a heuristic,
	// not an exact byte count of the final JSON.
	MaxBytes int
}

// DefaultLimits returns the Limits applied when the caller doesn't
// override them (see cmd/postgres-mcp for the corresponding environment
// variables).
func DefaultLimits() Limits {
	return Limits{
		QueryTimeout: 30 * time.Second,
		MaxRows:      10000,
		MaxBytes:     1 << 20, // 1 MiB
	}
}

type Server interface {
	List() []Info
	Get(ctx context.Context, name string) (*Connection, error)
	Close()
}

type manager struct {
	mu          sync.Mutex
	configMap   map[string]config.Config
	connections map[string]*Connection
	// pending holds an in-flight connect attempt per connection name, so
	// concurrent Get calls for the same not-yet-open name share a single
	// Open+Ping instead of racing to open multiple pools.
	pending map[string]*connectResult
	// limits is applied to every connection this manager opens. It is set
	// once in New (via WithLimits) and never mutated afterward, so it's
	// safe to read without holding mu.
	limits Limits
}

// connectResult is a one-shot future for a single connection attempt.
type connectResult struct {
	done chan struct{}
	conn *Connection
	err  error
}

func (m *manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, c := range m.connections {
		c.DB.Close()
	}
	m.connections = make(map[string]*Connection)
}

// Info describes a single registered connection for discovery purposes
// (e.g. a list_connections tool), deliberately excluding any credentials.
type Info struct {
	Name       string
	AccessMode config.AccessMode
}

// List returns Info for every registered connection, sorted by name.
func (m *manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()

	infoSlc := make([]Info, 0, len(m.configMap))
	for k, config := range m.configMap {
		infoSlc = append(infoSlc, Info{Name: k, AccessMode: config.AccessMode})
	}

	slices.SortFunc(infoSlc, func(a, b Info) int { return cmp.Compare(a.Name, b.Name) })

	return infoSlc
}

// Get returns the cached connection for name, opening and caching it on
// first use. The underlying *sql.DB pool is opened at most once per name:
// concurrent callers that miss the cache for the same name share a single
// connect attempt via m.pending rather than each opening (and leaking)
// their own pool.
func (m *manager) Get(ctx context.Context, name string) (*Connection, error) {
	m.mu.Lock()

	if conn, ok := m.connections[name]; ok {
		m.mu.Unlock()
		return conn, nil
	}

	cfg, ok := m.configMap[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown connection %q; call list_connections to see available connections", name)
	}

	if res, inFlight := m.pending[name]; inFlight {
		m.mu.Unlock()
		<-res.done
		return res.conn, res.err
	}

	res := &connectResult{done: make(chan struct{})}
	m.pending[name] = res
	limits := m.limits
	m.mu.Unlock()

	conn, err := connect(ctx, name, cfg, limits)
	res.conn, res.err = conn, err
	close(res.done)

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, name)
	if err == nil {
		m.connections[name] = conn
	}

	return conn, err
}

// connect opens and pings a new connection pool for cfg, outside of the
// manager's lock so a hung/unreachable host only blocks callers waiting on
// this specific name (via connectResult), not the whole manager.
func connect(ctx context.Context, name string, cfg config.Config, limits Limits) (*Connection, error) {
	db, err := sql.Open("postgres", cfg.URI)
	if err != nil {
		return nil, fmt.Errorf("connection %q: failed to open: %w", name, err)
	}

	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connection %q: failed to connect: %w", name, err)
	}

	return &Connection{DB: db, Mode: cfg.AccessMode, Limits: limits, Name: name}, nil
}

type option func(*manager)

func RegisterConnections(connections ...config.Connection) option {
	return func(m *manager) {
		m.mu.Lock()
		defer m.mu.Unlock()

		for _, c := range connections {
			m.configMap[c.Name] = config.Config{
				URI:        c.URI,
				AccessMode: c.AccessMode,
			}
		}
	}
}

// WithLimits overrides the default resource Limits (see DefaultLimits)
// applied to every connection opened by the resulting Server.
func WithLimits(limits Limits) option {
	return func(m *manager) {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.limits = limits
	}
}

func New(options ...option) Server {
	m := &manager{
		configMap:   make(map[string]config.Config),
		connections: make(map[string]*Connection),
		pending:     make(map[string]*connectResult),
		limits:      DefaultLimits(),
	}

	for _, opt := range options {
		opt(m)
	}

	return m
}
