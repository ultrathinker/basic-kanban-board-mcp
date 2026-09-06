// Package store defines persistence contracts and the SQLite implementation.
//
// Contract rules that the implementation must honour (PLAN §7):
//   - Every mutation runs inside a single write transaction opened with
//     BEGIN IMMEDIATE on the one-connection writer pool. All checks (WIP,
//     dependencies, cycles, sequence allocation, versions, claims, idempotency)
//     happen inside that transaction — never as a Go pre-check followed by a
//     separate write, which is a time-of-check/time-of-use race.
//   - Timestamps come from the database clock, not from Go, so leases cannot be
//     skewed by a caller.
//   - Events are appended in the same transaction as the change they describe,
//     and published to subscribers only after the commit succeeds.
package store

import (
	"context"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Tx is the unit of work handed to every repository method. The concrete type
// wraps *sql.Tx; repositories never open their own transactions.
type Tx interface {
	// Now returns the database clock for this transaction. Every timestamp
	// written or compared inside the transaction must come from here.
	Now() time.Time
}

// Store is the top-level handle. Read runs on the concurrent reader pool;
// Write serializes on the single-connection writer pool.
type Store interface {
	Read(ctx context.Context, fn func(Tx) error) error
	Write(ctx context.Context, fn func(Tx) error) error

	Projects() ProjectRepo
	Columns() ColumnRepo
	Tasks() TaskRepo
	Links() LinkRepo
	Notes() NoteRepo
	Events() EventRepo
	Tokens() TokenRepo
	Sessions() SessionRepo
	Idempotency() IdempotencyRepo

	// Backup performs VACUUM INTO to the given path.
	Backup(ctx context.Context, path string) error
	// Checkpoint runs wal_checkpoint(TRUNCATE); a long-lived SSE deployment
	// otherwise grows the WAL without bound.
	Checkpoint(ctx context.Context) error
	Health(ctx context.Context) (HealthInfo, error)
	Close() error
}

// HealthInfo backs `kanban doctor` and /readyz.
type HealthInfo struct {
	Path        string
	JournalMode string
	WALBytes    int64
	PageCount   int64
	Migration   int
	WriteMicros int64
}

// ProjectRepo persists projects. Key lookups are case-insensitive.
type ProjectRepo interface {
	Create(tx Tx, p *domain.Project) error
	Update(tx Tx, p *domain.Project, ifVersion *int) error
	GetByKey(tx Tx, key string) (*domain.Project, error)
	GetByID(tx Tx, id string) (*domain.Project, error)
	List(tx Tx, includeArchived bool) ([]*domain.Project, error)
	// NextTaskSeq allocates the next PROJ-N number atomically inside tx.
	NextTaskSeq(tx Tx, projectID string) (int, error)
	SetFocus(tx Tx, projectID string, taskID *string) error
	Archive(tx Tx, projectID string, archived bool) error
	Delete(tx Tx, projectID string) error // admin CLI only, never over MCP
}

// ColumnRepo persists columns. Names are unique per project.
type ColumnRepo interface {
	ListByProject(tx Tx, projectID string) ([]*domain.Column, error)
	GetByName(tx Tx, projectID, name string) (*domain.Column, error)
	GetByID(tx Tx, id string) (*domain.Column, error)
	Create(tx Tx, c *domain.Column) error
	Update(tx Tx, c *domain.Column) error
	Delete(tx Tx, id string) error
	// CountTasks returns unarchived tasks in the column, used for WIP checks
	// inside the same transaction as the move.
	CountTasks(tx Tx, columnID string, excludeTaskID string) (int, error)
}

// TaskFilter mirrors board_get.filter (PLAN §6.1). Zero value means "no filter".
type TaskFilter struct {
	ProjectIDs   []string
	ColumnIDs    []string // OR
	Types        []domain.Type
	PriorityMin  *domain.Priority
	Tags         []string // OR
	Assignee     *string
	Claimed      ClaimedFilter
	Blocked      *bool
	Query        string // substring of title or body, case-insensitive
	UpdatedSince *time.Time
	IncludeDone  bool
	DoneLimit    int
	IncludeArchived bool
	ParentID     *string
	Limit        int
	Offset       int
}

type ClaimedFilter string

const (
	ClaimedAny       ClaimedFilter = ""
	ClaimedMine      ClaimedFilter = "mine"
	ClaimedUnclaimed ClaimedFilter = "unclaimed"
	ClaimedOther     ClaimedFilter = "other"
)

// TaskRepo persists tasks.
type TaskRepo interface {
	Create(tx Tx, t *domain.Task) error
	// Update writes content changes. It must bump Version, and when ifVersion
	// is non-nil and does not match the stored version it must return a
	// domain.Error with CodeConflict carrying the CURRENT task as Current.
	Update(tx Tx, t *domain.Task, ifVersion *int) error
	GetByKey(tx Tx, key string) (*domain.Task, error)
	GetByID(tx Tx, id string) (*domain.Task, error)
	GetManyByKeys(tx Tx, keys []string) (map[string]*domain.Task, error)
	List(tx Tx, f TaskFilter) ([]*domain.Task, error)
	Children(tx Tx, parentID string) ([]*domain.Task, error)
	Archive(tx Tx, id string, archived bool, actor string) error
	Delete(tx Tx, id string) error // admin CLI only

	// Claim performs the compare-and-set lease from PLAN §6.7 as ONE statement:
	// UPDATE ... WHERE id = ? AND (claimed_by IS NULL OR claim_expires_at < now
	// OR claimed_by = actor). It must NOT bump Version. Returns false when the
	// row was not taken because someone else holds a live lease.
	Claim(tx Tx, id, actor string, ttl time.Duration) (ok bool, task *domain.Task, err error)
	Release(tx Tx, id, actor string, force bool) (ok bool, err error)

	// Move sets the column, resets ColumnEnteredAt and maintains StartedAt /
	// DoneAt. Callers must have validated the move with domain.CheckMove inside
	// the same transaction.
	Move(tx Tx, id, columnID string, rank int64, actor string) error

	// NeighbourRanks returns the ranks bracketing a requested position so the
	// service can pick a sparse rank; when the gap is exhausted the
	// implementation renumbers the column inside the transaction.
	NeighbourRanks(tx Tx, columnID string, position RankPosition) (before, after int64, err error)
	RenumberColumn(tx Tx, columnID string) error
}

// RankPosition selects where a task lands within a column.
type RankPosition string

const (
	RankTop    RankPosition = "top"
	RankBottom RankPosition = "bottom"
)

// LinkRepo persists typed dependency edges.
type LinkRepo interface {
	Add(tx Tx, l *domain.Link) error
	Remove(tx Tx, blockerID, blockedID string, t domain.LinkType) error
	// OpenBlockers returns the keys of blockers that are not in a done column
	// and not archived, sorted — exactly what CheckMove and the compact
	// rendering need.
	OpenBlockers(tx Tx, taskID string) ([]string, error)
	Blocks(tx Tx, taskID string) ([]string, error)
	// WouldCycle reports whether adding blocker->blocked closes a cycle. It must
	// consider parent chains too: a task may not block anything in its own
	// parent/child chain.
	WouldCycle(tx Tx, blockerID, blockedID string) (bool, []string, error)
	ListForTasks(tx Tx, taskIDs []string) (map[string][]domain.Link, error)
}

// NoteRepo appends agent working-log entries.
type NoteRepo interface {
	Add(tx Tx, n *domain.Note) error
	ListByTask(tx Tx, taskID string, limit int, before *time.Time) ([]domain.Note, error)
	CountByTasks(tx Tx, taskIDs []string) (map[string]int, error)
}

// EventRepo is append-only and backs SSE replay.
type EventRepo interface {
	Append(tx Tx, e *domain.Event) error
	// Since returns events with ID greater than afterID, oldest first. When
	// afterID predates the retained range the caller must send a resync instead
	// of a partial stream.
	Since(tx Tx, projectID string, afterID int64, limit int) ([]domain.Event, error)
	Latest(tx Tx, projectID string, limit int) ([]domain.Event, error)
	MinID(tx Tx) (int64, error)
}

// TokenRepo persists API tokens. Only the hash is stored.
type TokenRepo interface {
	Create(tx Tx, t *domain.Token) error
	GetByName(tx Tx, name string) (*domain.Token, error)
	// GetByHash is the authentication hot path; the comparison against the
	// candidate hash must be constant-time at the call site.
	GetByHash(tx Tx, hash []byte) (*domain.Token, error)
	List(tx Tx) ([]*domain.Token, error)
	// UpdateHash replaces a token's secret hash in place, for `kanban token
	// rotate`. Rotation cannot go through Create: name and hash both carry
	// UNIQUE indexes, so re-creating an existing token is a constraint
	// violation rather than an upsert.
	UpdateHash(tx Tx, id string, hash []byte) error
	Revoke(tx Tx, name string) error
	TouchLastUsed(tx Tx, id string) error
	Count(tx Tx) (int, error)
}

// SessionRepo persists browser sessions created by pasting a token.
type SessionRepo interface {
	Create(tx Tx, s *domain.Session) error
	Get(tx Tx, id string) (*domain.Session, error)
	Touch(tx Tx, id string, now time.Time) error
	Delete(tx Tx, id string) error
	DeleteExpired(tx Tx, now time.Time) (int, error)
}

// IdempotencyRepo makes retried creates safe.
type IdempotencyRepo interface {
	// Get returns the stored record for (tokenID, key) if it has not expired.
	Get(tx Tx, tokenID, key string) (*domain.IdempotencyRecord, error)
	// Put stores the response in the SAME transaction as the mutation it
	// describes; a record that outlives its mutation is worse than none.
	Put(tx Tx, r *domain.IdempotencyRecord) error
	DeleteExpired(tx Tx, now time.Time) (int, error)
}
