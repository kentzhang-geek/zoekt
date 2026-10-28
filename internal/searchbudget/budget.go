// Package searchbudget carries opt-in, in-process search limits and completion
// evidence. HTTP and RPC contracts are unchanged when no budget is attached.
package searchbudget

import (
	"context"
	"sync/atomic"

	"github.com/sourcegraph/zoekt"
)

type key struct{}

const (
	Known uint32 = 1 << iota
	Timeout
	MatchLimit
	ResponseLimit
	ShardError
	IndexNotReady
)

// Budget bounds retained result data across concurrent shards. Bytes are not
// refunded when ranking drops a result: the limit is deliberately conservative.
type Budget struct {
	limit int64
	used  atomic.Int64
	flags atomic.Uint32
}

func With(ctx context.Context, bytes int64) (context.Context, *Budget) {
	b := &Budget{limit: bytes}
	return context.WithValue(ctx, key{}, b), b
}

func From(ctx context.Context) *Budget {
	b, _ := ctx.Value(key{}).(*Budget)
	return b
}

func (b *Budget) Mark(flags uint32) {
	if b != nil {
		b.flags.Or(flags)
	}
}

func (b *Budget) Flags() uint32 {
	if b == nil {
		return 0
	}
	return b.flags.Load()
}

func (b *Budget) Exhausted() bool { return b.Flags()&ResponseLimit != 0 }

func (b *Budget) Finish(ctx context.Context, err error) {
	if b == nil {
		return
	}
	b.Mark(Known)
	if ctx.Err() != nil {
		b.Mark(Timeout)
	}
	if err != nil && ctx.Err() == nil {
		b.Mark(ShardError)
	}
}

// Reserve is called before retaining a file's result. It counts source bytes,
// ranges and container overhead, rather than the eventual text wire encoding.
func (b *Budget) Reserve(f *zoekt.FileMatch) bool {
	if b == nil {
		return true
	}
	n := int64(512 + len(f.FileName) + len(f.Repository) + len(f.Content))
	for _, c := range f.ChunkMatches {
		n += int64(160 + len(c.Content) + len(c.Ranges)*64 + len(c.SymbolInfo)*16)
	}
	for _, l := range f.LineMatches {
		n += int64(160 + len(l.Line) + len(l.Before) + len(l.After) + len(l.LineFragments)*64)
	}
	for {
		used := b.used.Load()
		if n > b.limit-used {
			b.Mark(ResponseLimit)
			return false
		}
		if b.used.CompareAndSwap(used, used+n) {
			return true
		}
	}
}
