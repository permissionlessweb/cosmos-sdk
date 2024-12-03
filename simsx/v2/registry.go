package v2

import (
	"time"

	"github.com/huandu/skiplist"

	"github.com/cosmos/cosmos-sdk/simsx"
)

type FutureOpsRegistry struct {
	list *skiplist.SkipList
}

var _ skiplist.Comparable = timeComparator{}

// used for skiplist
type timeComparator struct{}

func (t timeComparator) Compare(lhs, rhs interface{}) int {
	return lhs.(time.Time).Compare(rhs.(time.Time))
}

func (t timeComparator) CalcScore(key interface{}) float64 {
	return float64(key.(time.Time).UnixNano())
}

func NewFutureOpsRegistry() *FutureOpsRegistry {
	return &FutureOpsRegistry{list: skiplist.New(skiplist.Int64)}
}

func (l *FutureOpsRegistry) Add(blockTime time.Time, fx simsx.SimMsgFactoryX) {
	if fx == nil {
		panic("message factory must not be nil")
	}
	if blockTime.IsZero() {
		return
	}
	var scheduledOps []simsx.SimMsgFactoryX
	if e := l.list.Get(blockTime); e != nil {
		scheduledOps = e.Value.([]simsx.SimMsgFactoryX)
	}
	scheduledOps = append(scheduledOps, fx)
	l.list.Set(blockTime, scheduledOps)
}

func (l *FutureOpsRegistry) FindScheduled(blockTime time.Time) []simsx.SimMsgFactoryX {
	var r []simsx.SimMsgFactoryX
	for {
		e := l.list.Front()
		if e == nil || e.Key().(time.Time).After(blockTime) {
			break
		}
		r = append(r, e.Value.([]simsx.SimMsgFactoryX)...)
		l.list.RemoveFront()
	}
	return r
}
