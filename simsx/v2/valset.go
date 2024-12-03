package v2

import (
	"bytes"
	"crypto/sha256"
	"math/rand"
	"slices"

	appmodulev2 "cosmossdk.io/core/appmodule/v2"
	"cosmossdk.io/core/comet"

	"github.com/cosmos/cosmos-sdk/simsx"
)

// NewValSet constructor
func NewValSet() WeightedValidators {
	return make(WeightedValidators, 0)
}

type WeightedValidators []WeightedValidator

func (v WeightedValidators) Update(updates []appmodulev2.ValidatorUpdate) WeightedValidators {
	if len(updates) == 0 {
		return v
	}
	const truncatedSize = 20
	valUpdates := simsx.Collect(updates, func(u appmodulev2.ValidatorUpdate) WeightedValidator {
		hash := sha256.Sum256(u.PubKey)
		return WeightedValidator{Power: u.Power, Address: hash[:truncatedSize]}
	})
	newValset := slices.Clone(v)
	for _, u := range valUpdates {
		pos := slices.IndexFunc(newValset, func(val WeightedValidator) bool {
			return bytes.Equal(u.Address, val.Address)
		})
		if pos == -1 {
			if u.Power > 0 {
				newValset = append(newValset, u)
			}
			continue
		}
		if u.Power == 0 {
			newValset = append(newValset[0:pos], newValset[pos+1:]...)
			continue
		}
		newValset[pos].Power = u.Power
	}

	// sort vals by Power
	slices.SortFunc(newValset, func(a, b WeightedValidator) int {
		switch {
		case a.Power < b.Power:
			return 1
		case a.Power > a.Power:
			return -1
		default:
			return bytes.Compare(a.Address, b.Address)
		}
	})
	return newValset
}

// NewCommitInfo build Comet commit info for the validator set
func (v WeightedValidators) NewCommitInfo(r *rand.Rand) comet.CommitInfo {
	// todo: refactor to transition matrix?
	if r.Intn(10) == 0 {
		v[rand.Intn(len(v))].Offline = r.Intn(2) == 0
	}
	votes := make([]comet.VoteInfo, 0, len(v))
	for i := range v {
		if v[i].Offline {
			continue
		}
		votes = append(votes, comet.VoteInfo{
			Validator:   comet.Validator{Address: v[i].Address, Power: v[i].Power},
			BlockIDFlag: comet.BlockIDFlagCommit,
		})
	}
	return comet.CommitInfo{Round: int32(r.Uint32()), Votes: votes}
}

func (v WeightedValidators) TotalPower() int64 {
	var r int64
	for _, val := range v {
		r += val.Power
	}
	return r
}

type WeightedValidator struct {
	Power   int64
	Address []byte
	Offline bool
}

func NextFactoryFn(factories []simsx.WeightedFactory, r *rand.Rand) func() simsx.SimMsgFactoryX {
	var totalWeight int
	for k := range factories {
		totalWeight += k
	}
	factCount := len(factories)
	return func() simsx.SimMsgFactoryX {
		// this is copied from old sims WeightedOperations.getSelectOpFn
		x := r.Intn(totalWeight)
		for i := 0; i < factCount; i++ {
			if x <= int(factories[i].Weight) {
				return factories[i].Factory
			}
			x -= int(factories[i].Weight)
		}
		// shouldn't happen
		return factories[0].Factory
	}
}
