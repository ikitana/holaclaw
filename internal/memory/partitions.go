package memory

import (
	"context"
	"time"
)

// Enumerating the partitions of an agent is not part of the Store contract:
// nothing on the agent path needs it — a runner always knows which partition
// it is in — and a backend that cannot answer it is still a complete memory
// store. It lives here as an optional interface instead, so the one caller
// that does need it (a person browsing memory through the admin API) asks for
// it with a type assertion and is told `not implemented` when the backend
// cannot. Listing names is not a read across the partition rule: no item text
// crosses a partition here, only the counts of what each one holds.

// Partition is one memory space of an agent and what it holds.
type Partition struct {
	Name          string
	Notes, Recall int
	LastWritten   time.Time // newest write in the partition; zero when empty
}

// PartitionLister is implemented by backends that can enumerate an agent's
// partitions; the API answers 501 without it.
type PartitionLister interface {
	Partitions(ctx context.Context, agent string) ([]Partition, error)
}
