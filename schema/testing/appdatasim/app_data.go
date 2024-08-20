package appdatasim

import (
	"encoding/json"
	"fmt"
	"sort"

	"pgregory.net/rapid"

	"cosmossdk.io/schema"
	"cosmossdk.io/schema/appdata"
	"cosmossdk.io/schema/testing/statesim"
	"cosmossdk.io/schema/view"
)

// Options are the options for creating an app data simulator.
type Options struct {
	// AppSchema is the schema to use. If it is nil, then schematesting.ExampleAppSchema
	// will be used.
	AppSchema map[string]schema.ModuleSchema

	// Listener is the listener to output appdata updates to.
	Listener appdata.Listener

	// StateSimOptions are the options to pass to the statesim.App instance used under
	// the hood.
	StateSimOptions statesim.Options
}

// Simulator simulates a stream of app data. Currently, it only simulates InitializeModuleData, OnObjectUpdate,
// StartBlock and Commit callbacks but others will be added in the future.
type Simulator struct {
	state    *statesim.App
	options  Options
	blockNum uint64
}

// BlockData represents the app data packets in a block.
type BlockData = []appdata.Packet

// NewSimulator creates a new app data simulator with the given options and runs its
// initialization methods.
func NewSimulator(options Options) (*Simulator, error) {
	sim := &Simulator{
		// we initialize the state simulator with no app schema because we'll do
		// that in the first block
		state:   statesim.NewApp(nil, options.StateSimOptions),
		options: options,
	}

	err := sim.initialize()
	if err != nil {
		return nil, err
	}

	return sim, nil
}

func (a *Simulator) initialize() error {
	// in block "0" we only pass module initialization data and don't
	// even generate any real block data
	var keys []string
	for key := range a.options.AppSchema {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, moduleName := range keys {
		err := a.ProcessPacket(appdata.ModuleInitializationData{
			ModuleName: moduleName,
			Schema:     a.options.AppSchema[moduleName],
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// BlockDataGen generates random block data. It is expected that generated data is passed to ProcessBlockData
// to simulate the app data stream and advance app state based on the object updates in the block. The first
// packet in the block data will be a StartBlockData packet with the height set to the next block height.
func (a *Simulator) BlockDataGen() *rapid.Generator[BlockData] {
	return a.BlockDataGenN(0, 100)
}

// BlockDataGenN creates a block data generator which allows specifying the maximum number of updates per block.
func (a *Simulator) BlockDataGenN(minUpdatesPerBlock, maxUpdatesPerBlock int) *rapid.Generator[BlockData] {
	numUpdatesGen := rapid.IntRange(minUpdatesPerBlock, maxUpdatesPerBlock)
	numEventsGen := rapid.IntRange(1, 5)

	return rapid.Custom(func(t *rapid.T) BlockData {
		var packets BlockData

		// gen block header
		header := JSONObjectGen.Draw(t, "blockHeader")
		packets = append(packets, appdata.StartBlockData{
			Height:      a.blockNum + 1,
			HeaderBytes: func() ([]byte, error) { return json.Marshal(header) },
			HeaderJSON:  func() (json.RawMessage, error) { return json.Marshal(header) },
		})

		// gen txs
		numTxs := numUpdatesGen.Draw(t, "numTxs")
		for i := 0; i < numTxs; i++ {
			tx := JSONObjectGen.Draw(t, fmt.Sprintf("tx[%d]", i))
			packets = append(packets, appdata.TxData{
				TxIndex: int32(i),
				Bytes:   func() ([]byte, error) { return json.Marshal(tx) },
				JSON:    func() (json.RawMessage, error) { return json.Marshal(tx) },
			})
		}

		updateSet := map[string]bool{}
		// filter out any updates to the same key from this block, otherwise we can end up with weird errors
		updateGen := a.state.UpdateGen().Filter(func(data appdata.ObjectUpdateData) bool {
			for _, update := range data.Updates {
				_, existing := updateSet[fmt.Sprintf("%s:%v", data.ModuleName, update.Key)]
				if existing {
					return false
				}
			}
			return true
		})
		numUpdates := numUpdatesGen.Draw(t, "numUpdates")
		for i := 0; i < numUpdates; i++ {
			data := updateGen.Draw(t, fmt.Sprintf("update[%d]", i))
			for _, update := range data.Updates {
				updateSet[fmt.Sprintf("%s:%v", data.ModuleName, update.Key)] = true
			}
			packets = append(packets, data)
		}

		// begin block events
		numBeginBlockEvents := numEventsGen.Draw(t, "numBeginBlockEvents")
		for i := 0; i < numBeginBlockEvents; i++ {
			evt := DefaultEventDataGen.Draw(t, fmt.Sprintf("beginBlockEvent[%d]", i))
			evt.TxIndex = appdata.BeginBlockTxIndex
			evt.EventIndex = uint32(i)
			packets = append(packets, evt)
		}

		// tx events
		for i := 0; i < numTxs; i++ {
			numMsgs := numEventsGen.Draw(t, fmt.Sprintf("numEvents[%d]", i))
			for j := 0; j < numMsgs; j++ {
				numEvts := numEventsGen.Draw(t, fmt.Sprintf("numEvents[%d][%d]", i, j))
				for k := 0; k < numEvts; k++ {
					evt := DefaultEventDataGen.Draw(t, fmt.Sprintf("event[%d][%d][%d]", i, j, k))
					evt.TxIndex = int32(i)
					evt.MsgIndex = uint32(j)
					evt.EventIndex = uint32(k)
				}
			}
		}

		// end block events
		numEndBlockEvents := numEventsGen.Draw(t, "numEndBlockEvents")
		for i := 0; i < numEndBlockEvents; i++ {
			evt := DefaultEventDataGen.Draw(t, fmt.Sprintf("endBlockEvent[%d]", i))
			evt.TxIndex = appdata.EndBlockTxIndex
			evt.EventIndex = uint32(i)
			packets = append(packets, evt)
		}

		packets = append(packets, appdata.CommitData{})

		return packets
	})
}

// ProcessBlockData processes the given block data, advancing the app state based on the object updates in the block
// and forwarding all packets to the attached listener. It is expected that the data passed came from BlockDataGen,
// however, other data can be passed as long as any StartBlockData packet has the height set to the current height + 1.
func (a *Simulator) ProcessBlockData(data BlockData) error {
	for _, packet := range data {
		err := a.ProcessPacket(packet)
		if err != nil {
			return err
		}
	}
	return nil
}

// ProcessPacket processes a single packet, advancing the app state based on the data in the packet,
// and forwarding the packet to any listener.
func (a *Simulator) ProcessPacket(packet appdata.Packet) error {
	err := a.options.Listener.SendPacket(packet)
	if err != nil {
		return err
	}
	switch packet := packet.(type) {
	case appdata.StartBlockData:
		if packet.Height != a.blockNum+1 {
			return fmt.Errorf("invalid StartBlockData packet: %v", packet)
		}
		a.blockNum++
	case appdata.ModuleInitializationData:
		return a.state.InitializeModule(packet)
	case appdata.ObjectUpdateData:
		return a.state.ApplyUpdate(packet)
	}
	return nil
}

// AppState returns the current app state backing the simulator.
func (a *Simulator) AppState() view.AppState {
	return a.state
}

// BlockNum returns the current block number of the simulator.
func (a *Simulator) BlockNum() (uint64, error) {
	return a.blockNum, nil
}
