package v2

import (
	"math/rand"
	"time"

	"cosmossdk.io/core/comet"

	"github.com/cosmos/cosmos-sdk/simsx"
)

type historicValSet struct {
	blockTime time.Time
	vals      WeightedValidators
}
type ValSetHistory struct {
	maxElements int
	blockOffset int
	vals        []historicValSet
}

func NewValSetHistory(maxElements int) *ValSetHistory {
	return &ValSetHistory{
		maxElements: maxElements,
		blockOffset: 1, // start at height 1
		vals:        make([]historicValSet, 0, maxElements),
	}
}

func (h *ValSetHistory) Add(blockTime time.Time, vals WeightedValidators) {
	newEntry := historicValSet{blockTime: blockTime, vals: vals}
	if len(h.vals) >= h.maxElements {
		h.vals = append(h.vals[1:], newEntry)
		h.blockOffset++
		return
	}
	h.vals = append(h.vals, newEntry)
}

func (h *ValSetHistory) MissBehaviour(r *rand.Rand) []comet.Evidence {
	if r.Intn(100) != 0 { // 1% chance
		return nil
	}
	n := r.Intn(len(h.vals))
	badVal := simsx.OneOf(r, h.vals[n].vals)
	evidence := comet.Evidence{
		Type:             comet.DuplicateVote,
		Validator:        comet.Validator{Address: badVal.Address, Power: badVal.Power},
		Height:           int64(h.blockOffset + n),
		Time:             h.vals[n].blockTime,
		TotalVotingPower: h.vals[n].vals.TotalPower(),
	}
	if otherEvidence := h.MissBehaviour(r); otherEvidence != nil {
		return append([]comet.Evidence{evidence}, otherEvidence...)
	}
	return []comet.Evidence{evidence}
}
