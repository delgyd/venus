package state

import (
	"context"
	"sync"

	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	"github.com/libp2p/go-libp2p-core/peer"
	xerrors "github.com/pkg/errors"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	miner0 "github.com/filecoin-project/specs-actors/actors/builtin/miner"
	miner2 "github.com/filecoin-project/specs-actors/v2/actors/builtin/miner"

	"github.com/filecoin-project/go-filecoin/internal/pkg/block"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors/adt"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/account"
	notinit "github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/init"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/market"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/miner"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/multisig"
	paychActor "github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/paych"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/power"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/reward"
	"github.com/filecoin-project/go-filecoin/internal/pkg/specactors/builtin/verifreg"
	"github.com/filecoin-project/go-filecoin/internal/pkg/types"
	"github.com/filecoin-project/go-filecoin/internal/pkg/vm/actor"
	vmstate "github.com/filecoin-project/go-filecoin/internal/pkg/vm/state"
	"github.com/filecoin-project/go-filecoin/vendors/sector-storage/ffiwrapper"
)

var dealProviderCollateralNum = big.NewInt(110)
var dealProviderCollateralDen = big.NewInt(100)

// Viewer builds state views from state root CIDs.
type Viewer struct {
	ipldStore cbor.IpldStore
}

// NewViewer creates a new state
func NewViewer(store cbor.IpldStore) *Viewer {
	return &Viewer{store}
}

// StateView returns a new state view.
func (c *Viewer) StateView(root cid.Cid, version network.Version) *View {
	return NewView(c.ipldStore, root, version)
}

type genesisInfo struct {
	genesisMsigs []multisig.State
	// info about the Accounts in the genesis state
	genesisActors      []genesisActor
	genesisPledge      abi.TokenAmount
	genesisMarketFunds abi.TokenAmount
}

type genesisActor struct {
	addr    addr.Address
	initBal abi.TokenAmount
}

// View is a read-only interface to a snapshot of application-level actor state.
// This object interprets the actor state, abstracting the concrete on-chain structures so as to
// hide the complications of protocol versions.
// Exported methods on this type avoid exposing concrete state structures (which may be subject to versioning)
// where possible.
type View struct {
	ipldStore cbor.IpldStore
	root      cid.Cid

	genInfo       *genesisInfo
	genesisMsigLk sync.Mutex
	genesisRoot   cid.Cid

	// todo add by force
	networkVersion network.Version
}

// NewView creates a new state view
func NewView(store cbor.IpldStore, root cid.Cid, version network.Version) *View {
	return &View{
		ipldStore:      store,
		root:           root,
		networkVersion: version,
	}
}

// InitNetworkName Returns the network name from the init actor state.
func (v *View) InitNetworkName(ctx context.Context) (string, error) {
	initState, err := v.loadInitActor(ctx)
	if err != nil {
		return "", err
	}
	return initState.NetworkName() // todo review
}

// InitResolveAddress Returns ID address if public key address is given.
func (v *View) InitResolveAddress(ctx context.Context, a addr.Address) (addr.Address, error) {
	if a.Protocol() == addr.ID {
		return a, nil
	}

	initState, err := v.loadInitActor(ctx)
	if err != nil {
		return addr.Undef, err
	}

	// todo review
	rAddr, _, err := initState.ResolveAddress(a)
	if err != nil {
		return addr.Undef, err
	}

	//state := &notinit.State{
	//	AddressMap: initState.AddressMap,
	//}
	//rAddr, _, err := state.ResolveAddress(v.adtStore(ctx), a) //todo add by force bool?
	//if err != nil {
	//	return addr.Undef, err
	//}
	return rAddr, nil
}

// Returns public key address if id address is given
func (v *View) AccountSignerAddress(ctx context.Context, a addr.Address) (addr.Address, error) {
	if a.Protocol() == addr.SECP256K1 || a.Protocol() == addr.BLS {
		return a, nil
	}

	accountActorState, err := v.loadAccountActor(ctx, a)
	if err != nil {
		return addr.Undef, err
	}

	// todo review
	return accountActorState.PubkeyAddress()
}

// MinerControlAddresses returns the owner and worker addresses for a miner actor
func (v *View) MinerControlAddresses(ctx context.Context, maddr addr.Address) (owner, worker addr.Address, err error) {
	minerInfo, err := v.MinerInfo(ctx, maddr)
	if err != nil {
		return addr.Undef, addr.Undef, err
	}
	return minerInfo.Owner, minerInfo.Worker, nil
}

func (v *View) MinerInfo(ctx context.Context, maddr addr.Address) (*miner.MinerInfo, error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return nil, err
	}

	// todo review
	minerInfo, err := minerState.Info()
	if err != nil {
		return nil, err
	}
	return &minerInfo, nil
	// return minerState.GetInfo(v.adtStore(ctx))
}

// MinerPeerID returns the PeerID for a miner actor
func (v *View) MinerPeerID(ctx context.Context, maddr addr.Address) (peer.ID, error) {
	minerInfo, err := v.MinerInfo(ctx, maddr)
	if err != nil {
		return "", err
	}

	// todo review
	return *minerInfo.PeerId, nil
	// return peer.ID(minerInfo.PeerId), nil
}

type MinerSectorConfiguration struct {
	SealProofType              abi.RegisteredSealProof
	SectorSize                 abi.SectorSize
	WindowPoStPartitionSectors uint64
}

// MinerSectorConfiguration returns the sector size for a miner actor
func (v *View) MinerSectorConfiguration(ctx context.Context, maddr addr.Address) (*MinerSectorConfiguration, error) {
	minerInfo, err := v.MinerInfo(ctx, maddr)
	if err != nil {
		return nil, err
	}
	return &MinerSectorConfiguration{
		SealProofType:              minerInfo.SealProofType,
		SectorSize:                 minerInfo.SectorSize,
		WindowPoStPartitionSectors: minerInfo.WindowPoStPartitionSectors,
	}, nil
}

// MinerSectorCount counts all the on-chain sectors
func (v *View) MinerSectorCount(ctx context.Context, maddr addr.Address) (uint64, error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return 0, err
	}

	// todo review
	sc, err := minerState.SectorArray()
	if err != nil {
		return 0, err
	}

	return sc.Length(), nil

	//sectors, err := v.asArray(ctx, minerState.Sectors)
	//if err != nil {
	//	return 0, err
	//}
	//length := sectors.Length()
	//return length, nil
}

// Loads sector info from miner state.
func (v *View) MinerGetSector(ctx context.Context, maddr addr.Address, sectorNum abi.SectorNumber) (*miner.SectorOnChainInfo, bool, error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return nil, false, err
	}

	// todo review
	info, err := minerState.GetSector(sectorNum)
	if err != nil {
		return nil, false, err
	}

	return info, true, nil
	// return minerState.GetSector(v.adtStore(ctx), sectorNum)
}

func (v *View) GetPartsProving(ctx context.Context, maddr addr.Address) ([]bitfield.BitField, error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return nil, err
	}

	var partsProving []bitfield.BitField

	if err := minerState.ForEachDeadline(func(idx uint64, dl miner.Deadline) error{
		if err := dl.ForEachPartition(func(idx uint64, part miner.Partition) error{
			allSectors, err := part.AllSectors()
			if err != nil {
				return err
			}
			faultySectors, err := part.FaultySectors()
			if err != nil {
				return err
			}
			p, err := bitfield.SubtractBitField(allSectors, faultySectors)
			if err != nil {
				return xerrors.Errorf("subtract faults from partition sectors: %w", err)
			}

			partsProving = append(partsProving, p)

			return nil
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return partsProving, nil
}

// MinerDeadlineInfo returns information relevant to the current proving deadline
func (v *View) MinerDeadlineInfo(ctx context.Context, maddr addr.Address, epoch abi.ChainEpoch) (index uint64, open, close, challenge abi.ChainEpoch, _ error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	// todo review
	deadlineInfo, err := minerState.DeadlineInfo(epoch)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	return deadlineInfo.Index, deadlineInfo.Open, deadlineInfo.Close, deadlineInfo.Challenge, nil

	//deadlineInfo := minerState.DeadlineInfo(epoch)
	//return deadlineInfo.Index, deadlineInfo.Open, deadlineInfo.Close, deadlineInfo.Challenge, nil
}

// MinerSuccessfulPoSts counts how many successful window PoSts have been made this proving period so far.
func (v *View) MinerSuccessfulPoSts(ctx context.Context, maddr addr.Address) (uint64, error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return 0, err
	}

	// todo review
	return minerState.SuccessfulPoSts()

	//deadlines, err := minerState.LoadDeadlines(v.adtStore(ctx))
	//if err != nil {
	//	return 0, err
	//}
	//
	//count := uint64(0)
	//err = deadlines.ForEach(v.adtStore(ctx), func(dlIdx uint64, dl *miner.Deadline) error {
	//	dCount, err := dl.PostSubmissions.Count()
	//	if err != nil {
	//		return err
	//	}
	//	count += dCount
	//	return nil
	//})
	//if err != nil {
	//	return 0, err
	//}

	// return count, nil
}

// MinerDeadlines returns a bitfield of sectors in a proving period
// todo 这个接口貌似没存在必要,且返回参数有二义性
//func (v *View) MinerDeadlines(ctx context.Context, maddr addr.Address) (*miner.Deadlines, error) {
//	minerState, err := v.loadMinerActor(ctx, maddr)
//	if err != nil {
//		return nil, err
//	}
//
//	return minerState.LoadDeadlines(v.adtStore(ctx))
//}

func (v *View) MinerProvingPeriodStart(ctx context.Context, maddr addr.Address) (abi.ChainEpoch, error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return 0, err
	}

	// todo review
	return minerState.GetProvingPeriodStart(), nil
	//return minerState.ProvingPeriodStart, nil
}

// MinerSectorsForEach Iterates over the sectors in a miner's proving set.
func (v *View) MinerSectorsForEach(ctx context.Context, maddr addr.Address,
	f func(abi.SectorNumber, cid.Cid, abi.RegisteredSealProof, []abi.DealID) error) error {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return err
	}

	// todo review
	sectors, err  :=  minerState.SectorArray()
	if err != nil {
		return err
	}

	version := specactors.Version(v.networkVersion)
	switch version {
	case 0:
		var info0 miner0.SectorOnChainInfo
		if err := sectors.ForEach(&info0, func(_ int64) error {
			return f(info0.SectorNumber, info0.SealedCID, info0.SealProof, info0.DealIDs)
		}); err != nil {
			return err
		}
	case 2:
		var info2 miner2.SectorOnChainInfo
		if err := sectors.ForEach(&info2, func(_ int64) error {
			return f(info2.SectorNumber, info2.SealedCID, info2.SealProof, info2.DealIDs)
		}); err != nil {
			return err
		}
	}
	return nil

	//sectors, err := v.asArray(ctx, minerState.Sectors)
	//if err != nil {
	//	return err
	//}

	// // This version for the new actors
	//var sector miner.SectorOnChainInfo
	//return sectors.ForEach(&sector, func(secnum int64) error {
	//	// Add more fields here as required by new callers.
	//	return f(sector.SectorNumber, sector.SealedCID, sector.SealProof, sector.DealIDs)
	//})
}

// MinerExists Returns true iff the miner exists.
func (v *View) MinerExists(ctx context.Context, maddr addr.Address) (bool, error) {
	_, err := v.loadMinerActor(ctx, maddr)
	if err == nil {
		return true, nil
	}
	if err == types.ErrNotFound {
		return false, nil
	}
	return false, err
}

// MinerFaults Returns all sector ids that are faults
func (v *View) MinerFaults(ctx context.Context, maddr addr.Address) ([]uint64, error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return nil, err
	}

	return minerState.FaultsSectors() // todo review

	//out := bitfield.New()
	//
	//deallines, err := minerState.LoadDeadlines(v.adtStore(ctx))
	//if err != nil {
	//	return nil, err
	//}
	//
	//err = deallines.ForEach(v.adtStore(ctx), func(dlIdx uint64, dl *miner.Deadline) error {
	//	partitions, err := dl.PartitionsArray(v.adtStore(ctx))
	//	if err != nil {
	//		return err
	//	}
	//
	//	var partition miner.Partition
	//	return partitions.ForEach(&partition, func(i int64) error {
	//		out, err = bitfield.MergeBitFields(out, partition.Faults)
	//		return err
	//	})
	//})
	//
	//maxSectorNum, err := out.All(miner.SectorsMax)
	//if err != nil {
	//	return nil, err
	//}
	//return maxSectorNum, nil
}

// MinerGetPrecommittedSector Looks up info for a miners precommitted sector.
// NOTE: exposes on-chain structures directly for storage FSM API.
func (v *View) MinerGetPrecommittedSector(ctx context.Context, maddr addr.Address, sectorNum abi.SectorNumber) (*miner.SectorPreCommitOnChainInfo, bool, error) {
	minerState, err := v.loadMinerActor(ctx, maddr)
	if err != nil {
		return nil, false, err
	}

	// todo review
	info ,err := minerState.GetPrecommittedSector(sectorNum)
	if err != nil {
		return nil, false, err
	}
	return info, true, nil

	// return minerState.GetPrecommittedSector(v.adtStore(ctx), sectorNum)
}

// MarketEscrowBalance looks up a token amount in the escrow table for the given address
func (v *View) MarketEscrowBalance(ctx context.Context, addr addr.Address) (found bool, amount abi.TokenAmount, err error) {
	marketState, err := v.loadMarketActor(ctx)
	if err != nil {
		return false, abi.NewTokenAmount(0), err
	}

	state, err := marketState.EscrowTable()
	if err != nil {
		return false, abi.NewTokenAmount(0), err
	}

	amount, err = state.Get(addr)
	if err != nil {
		return false, abi.NewTokenAmount(0), err
	}

	return true, amount, nil

	//escrow, err := v.asMap(ctx, marketState.EscrowTable)
	//if err != nil {
	//	return false, abi.NewTokenAmount(0), err
	//}
	//
	//var value abi.TokenAmount
	//found, err = escrow.Get(abi.AddrKey(addr), &value)
	//return
}

// MarketComputeDataCommitment takes deal ids and uses associated commPs to compute commD for a sector that contains the deals
func (v *View) MarketComputeDataCommitment(ctx context.Context, registeredProof abi.RegisteredSealProof, dealIDs []abi.DealID) (cid.Cid, error) {
	marketState, err := v.loadMarketActor(ctx)
	if err != nil {
		return cid.Undef, err
	}

	// todo review
	proposals, err := marketState.Proposals()
	if err != nil {
		return cid.Undef, err
	}

	// map deals to pieceInfo
	pieceInfos := make([]abi.PieceInfo, len(dealIDs))
	for i, id := range dealIDs {
		proposal, bFound, err := proposals.Get(id)
		if err != nil {
			return cid.Undef, err
		}

		if !bFound {
			return cid.Undef, xerrors.Errorf("deal %d not found", id)
		}

		pieceInfos[i].PieceCID = proposal.PieceCID
		pieceInfos[i].Size = proposal.PieceSize
	}
	return ffiwrapper.GenerateUnsealedCID(registeredProof, pieceInfos)

	//deals, err := v.asArray(ctx, marketState.Proposals)
	//if err != nil {
	//	return cid.Undef, err
	//}
	//
	//// map deals to pieceInfo
	//pieceInfos := make([]abi.PieceInfo, len(dealIDs))
	//for i, id := range dealIDs {
	//	var proposal market.DealProposal
	//	found, err := deals.Get(uint64(id), &proposal)
	//	if err != nil {
	//		return cid.Undef, err
	//	}
	//
	//	if !found {
	//		return cid.Undef, xerrors.Errorf("deal %d not found", id)
	//	}
	//
	//	pieceInfos[i].PieceCID = proposal.PieceCID
	//	pieceInfos[i].Size = proposal.PieceSize
	//}
	//
	//return ffiwrapper.GenerateUnsealedCID(registeredProof, pieceInfos)
}

// NOTE: exposes on-chain structures directly for storage FSM interface.
func (v *View) MarketDealProposal(ctx context.Context, dealID abi.DealID) (market.DealProposal, error) {
	marketState, err := v.loadMarketActor(ctx)
	if err != nil {
		return market.DealProposal{}, err
	}

	// todo review
	proposals, err := marketState.Proposals()
	if err != nil {
		return market.DealProposal{}, err
	}

	// map deals to pieceInfo
	proposal, bFound, err := proposals.Get(dealID)
	if err != nil {
		return market.DealProposal{}, err
	}

	if !bFound {
		return market.DealProposal{}, xerrors.Errorf("deal %d not found", dealID)
	}
	return *proposal, nil

	//deals, err := v.asArray(ctx, marketState.Proposals)
	//if err != nil {
	//	return market.DealProposal{}, err
	//}
	//
	//var proposal market.DealProposal
	//found, err := deals.Get(uint64(dealID), &proposal)
	//if err != nil {
	//	return market.DealProposal{}, err
	//}
	//if !found {
	//	return market.DealProposal{}, xerrors.Errorf("deal %d not found", dealID)
	//}

	//return proposal, nil
}

// NOTE: exposes on-chain structures directly for storage FSM and market module interfaces.
func (v *View) MarketDealState(ctx context.Context, dealID abi.DealID) (*market.DealState, bool, error) {
	marketState, err := v.loadMarketActor(ctx)
	if err != nil {
		return nil, false, err
	}

	// todo review
	deals, err := marketState.States()
	if err != nil {
		return nil, false, err
	}

	return deals.Get(dealID)

	//dealStates, err := v.asDealStateArray(ctx, marketState.States)
	//if err != nil {
	//	return nil, false, err
	//}
	//return dealStates.Get(dealID)
}

// NOTE: exposes on-chain structures directly for market interface.
// The callback receives a pointer to a transient object; take a copy or drop the reference outside the callback.
func (v *View) MarketDealStatesForEach(ctx context.Context, f func(id abi.DealID, state *market.DealState) error) error {
	marketState, err := v.loadMarketActor(ctx)
	if err != nil {
		return err
	}

	// todo review
	deals, err := marketState.States()
	if err != nil {
		return err
	}

	ff := func(id abi.DealID, ds market.DealState) error {
		return f(abi.DealID(id), &ds)
	}
	if err := deals.ForEach(ff); err != nil {
		return err
	}
	return nil

	//dealStates, err := v.asDealStateArray(ctx, marketState.States)
	//if err != nil {
	//	return err
	//}
	//
	//var ds market.DealState
	//return dealStates.ForEach(&ds, func(dealId int64) error {
	//	return f(abi.DealID(dealId), &ds)
	//})
}

// StateDealProviderCollateralBounds returns the min and max collateral a storage provider
// can issue. It takes the deal size and verified status as parameters.
func (v *View) MarketDealProviderCollateralBounds(ctx context.Context, size abi.PaddedPieceSize, verified bool, height abi.ChainEpoch) (DealCollateralBounds, error) {
	panic("not impl")
}

func (v *View) StateVerifiedClientStatus(ctx context.Context, addr addr.Address) (abi.StoragePower, error) {
	// todo review
	actr, err := v.loadActor(ctx, builtin.VerifiedRegistryActorAddr)
	if err != nil {
		return abi.NewStoragePower(0), err
	}

	state, err := verifreg.Load(adt.WrapStore(ctx, v.ipldStore), actr)
	if err != nil {
		return abi.NewStoragePower(0), err
	}

	found, storagePower, err := state.VerifiedClientDataCap(addr)
	if err != nil {
		return abi.NewStoragePower(0), err
	}

	if !found {
		return abi.NewStoragePower(0), xerrors.New("address not found")
	}

	return storagePower, nil
}

func (v *View) StateMarketStorageDeal(ctx context.Context, dealID abi.DealID) (*MarketDeal, error) {
	state, err := v.loadMarketActor(ctx)
	if err != nil {
		return nil, err
	}

	// todo review
	dealProposals, err := state.Proposals()
	if err != nil {
		return nil, err
	}

	dealProposal, found, err := dealProposals.Get(dealID)
	if err != nil {
		return  nil, err
	}

	if !found {
		return nil, xerrors.New("deal proposal not found")
	}


	dealStates, err := state.States()
	if err != nil {
		return nil, err
	}

	dealState, found, err := dealStates.Get(dealID)
	if err != nil {
		return  nil, err
	}

	if !found {
		return nil, xerrors.New("deal state not found")
	}

	return &MarketDeal{
		Proposal: *dealProposal,
		State:    *dealState,
	}, nil
}

// Returns the storage power actor's values for network total power.
func (v *View) PowerNetworkTotal(ctx context.Context) (*NetworkPower, error) {
	st, err := v.loadPowerActor(ctx)
	if err != nil {
		return nil, err
	}

	tp, err := st.TotalPower()
	if err != nil {
		return nil, err
	}

	minPowerMinerCount, minerCount, err := st.MinerCounts()
	if err != nil {
		return nil, err
	}

	return &NetworkPower{
		RawBytePower:         tp.RawBytePower,
		QualityAdjustedPower: tp.QualityAdjPower,
		MinerCount:           int64(minerCount),
		MinPowerMinerCount:   int64(minPowerMinerCount),
	}, nil
}

// Returns the power of a miner's committed sectors.
func (v *View) MinerClaimedPower(ctx context.Context, miner addr.Address) (raw, qa abi.StoragePower, err error) {
	// todo review
	st, err := v.loadPowerActor(ctx)
	if err != nil {
		return big.Zero(), big.Zero(), err
	}

	p, found, err := st.MinerPower(miner)
	if err != nil {
		return big.Zero(), big.Zero(), err
	}

	if !found {
		return big.Zero(), big.Zero(), xerrors.New("miner not found")
	}

	return  p.RawBytePower, p.QualityAdjPower, nil
}

func (v *View) MinerNominalPowerMeetsConsensusMinimum(ctx context.Context, addr addr.Address) (bool, error) {
	st, err := v.loadPowerActor(ctx)
	if err != nil {
		return false, err
	}

	return st.MinerNominalPowerMeetsConsensusMinimum(addr)
}

// PaychActorParties returns the From and To addresses for the given payment channel
func (v *View) PaychActorParties(ctx context.Context, paychAddr addr.Address) (from, to addr.Address, err error) {
	a, err := v.loadActor(ctx, paychAddr)
	if err != nil {
		return addr.Undef, addr.Undef, err
	}

	state, err := paychActor.Load(adt.WrapStore(ctx, v.ipldStore), a)
	if err != nil {
		return addr.Undef, addr.Undef, err
	}

	from, err = state.From()
	if err != nil {
		return addr.Undef, addr.Undef, err
	}

	to, err = state.To()
	if err != nil {
		return addr.Undef, addr.Undef, err
	}

	return from, to, nil
}

func (v *View) StateMinerProvingDeadline(ctx context.Context, addr addr.Address, ts *block.TipSet) (*dline.Info, error) {
	mas, err := v.loadMinerActor(ctx, addr)
	if err != nil {
		return nil, xerrors.WithMessage(err, "failed to get proving dealline")
	}

	height, _ := ts.Height()
	return mas.DeadlineInfo(height)
}

func (v *View) StateMinerDeadlineForIdx(ctx context.Context, addr addr.Address, dlIdx uint64, key block.TipSetKey) (miner.Deadline, error) {
	mas, err := v.loadMinerActor(ctx, addr)
	if err != nil {
		return nil, xerrors.WithMessage(err, "failed to get proving dealline")
	}

	return mas.LoadDeadline(dlIdx)
}

func (v *View) StateMinerSectors(ctx context.Context, addr addr.Address, filter *bitfield.BitField, key block.TipSetKey) ([]*ChainSectorInfo, error) {
	// todo review
	mas, err := v.loadMinerActor(ctx, addr)
	if err != nil {
		return nil, xerrors.WithMessage(err, "failed to get proving dealline")
	}

	siset, err := mas.LoadSectors(filter)
	if err != nil {
		return nil, err
	}

	sset := make([]*ChainSectorInfo, len(siset))
	for i, val := range siset {
		sset[i] = &ChainSectorInfo {
			Info: *val,
			ID: val.SectorNumber,
		}
	}

	return sset,nil
}

func (v *View) GetFilLocked(ctx context.Context, st vmstate.Tree) (abi.TokenAmount, error) {
	filMarketLocked, err := getFilMarketLocked(ctx, v.ipldStore, st)
	if err != nil {
		return big.Zero(), xerrors.Errorf("failed to get filMarketLocked: %w", err)
	}

	powerState, err := v.loadPowerActor(ctx)
	if err != nil {
		return big.Zero(), xerrors.Errorf("failed to get filPowerLocked: %w", err)
	}

	filPowerLocked, err := powerState.TotalLocked()
	if err != nil {
		return big.Zero(), xerrors.Errorf("failed to get filPowerLocked: %w", err)
	}

	return big.Add(filMarketLocked, filPowerLocked), nil
}

func (v *View) LoadActor(ctx context.Context, address addr.Address) (*actor.Actor, error) {
	return v.loadActor(ctx, address)
}

func (v *View) ResolveToKeyAddr(ctx context.Context, address addr.Address) (addr.Address, error) {
	if address.Protocol() == addr.BLS || address.Protocol() == addr.SECP256K1 {
		return address, nil
	}

	act, err := v.LoadActor(context.TODO(), address)
	if err != nil {
		return addr.Undef, xerrors.Errorf("failed to find actor: %s", address)
	}

	if act.Code.Cid != builtin.AccountActorCodeID {
		return addr.Undef, xerrors.Errorf("address %s was not for an account actor", address)
	}

	// todo review
	aast, err := account.Load(adt.WrapStore(context.TODO(), v.ipldStore), act)
	if err != nil {
		return addr.Undef, xerrors.Errorf("failed to get account actor state for %s: %w", address, err)
	}

	//var aast account.State
	//if err := v.ipldStore.Get(context.TODO(), act.Head.Cid, &aast); err != nil {
	//	return addr.Undef, xerrors.Errorf("failed to get account actor state for %s: %w", address, err)
	//}

	return aast.PubkeyAddress()
}

func (v *View) loadInitActor(ctx context.Context) (notinit.State, error) {
	actr, err := v.loadActor(ctx, builtin.InitActorAddr)
	if err != nil {
		return nil, err
	}

	// todo review
	return notinit.Load(adt.WrapStore(ctx, v.ipldStore), actr)

	//var state notinit.State
	//err = v.ipldStore.Get(ctx, actr.Head.Cid, &state)
	//return &state, err
}

func (v *View) LoadMinerActor(ctx context.Context, address addr.Address) (miner.State, error) {
	return v.loadMinerActor(ctx, address)
}

func (v *View) loadMinerActor(ctx context.Context, address addr.Address) (miner.State, error) {
	resolvedAddr, err := v.InitResolveAddress(ctx, address)
	if err != nil {
		return nil, err
	}
	actr, err := v.loadActor(ctx, resolvedAddr)
	if err != nil {
		return nil, err
	}

	// todo review
	return miner.Load(adt.WrapStore(context.TODO(), v.ipldStore), actr)

	//var state miner.State
	//err = v.ipldStore.Get(ctx, actr.Head.Cid, &state)
	//return state, err
}

func (v *View) loadPowerActor(ctx context.Context) (power.State, error) {
	actr, err := v.loadActor(ctx, builtin.StoragePowerActorAddr)
	if err != nil {
		return nil, err
	}

	return power.Load(adt.WrapStore(ctx, v.ipldStore), actr)
	//var state power.State
	//err = v.ipldStore.Get(ctx, actr.Head.Cid, &state)
	//return &state, err
}

func (v *View) loadRewardActor(ctx context.Context) (reward.State, error) {
	actr, err := v.loadActor(ctx, builtin.RewardActorAddr)
	if err != nil {
		return nil, err
	}

	return reward.Load(adt.WrapStore(ctx, v.ipldStore), actr) // todo review

	//var state reward.State
	//err = v.ipldStore.Get(ctx, actr.Head.Cid, &state)
	//return &state, err
}

func (v *View) loadMarketActor(ctx context.Context) (market.State, error) {
	actr, err := v.loadActor(ctx, builtin.StorageMarketActorAddr)
	if err != nil {
		return nil, err
	}

	return market.Load(adt.WrapStore(ctx, v.ipldStore), actr) // todo review

	//var state market.State
	//err = v.ipldStore.Get(ctx, actr.Head.Cid, &state)
	//return &state, err
}

func (v *View) loadAccountActor(ctx context.Context, a addr.Address) (account.State, error) {
	resolvedAddr, err := v.InitResolveAddress(ctx, a)
	if err != nil {
		return nil, err
	}
	actr, err := v.loadActor(ctx, resolvedAddr)
	if err != nil {
		return nil, err
	}

	// var state account.State
	// err = v.ipldStore.Get(ctx, actr.Head.Cid, &state)

	// todo review
	return account.Load(adt.WrapStore(context.TODO(), v.ipldStore), actr)
}

func (v *View) loadActor(ctx context.Context, address addr.Address) (*actor.Actor, error) {
	tree, err := v.asMap(ctx, v.root)
	if err != nil {
		return nil, err
	}

	var actr actor.Actor
	found, err := tree.Get(abi.AddrKey(address), &actr)
	if !found {
		return nil, types.ErrNotFound
	}

	return &actr, err
}

func (v *View) adtStore(ctx context.Context) adt.Store {
	return StoreFromCbor(ctx, v.ipldStore)
}

func (v *View) asArray(ctx context.Context, root cid.Cid) (adt.Array, error) {
	// todo review

	return adt.AsArray(v.adtStore(ctx), root, v.networkVersion)
	// return adt.AsArray(v.adtStore(ctx), root)
}

func (v *View) asMap(ctx context.Context, root cid.Cid) (adt.Map, error) {
	// todo review
	return adt.AsMap(v.adtStore(ctx), root, specactors.Version(v.networkVersion))
	// return adt.AsMap(v.adtStore(ctx), root)
}

func getFilMarketLocked(ctx context.Context, ipldStore cbor.IpldStore, st vmstate.Tree) (abi.TokenAmount, error) {
	// todo review
	mactor, found, err := st.GetActor(ctx, builtin.StorageMarketActorAddr)
	if !found || err != nil {
		return big.Zero(), xerrors.Errorf("failed to load market actor: %w", err)
	}

	mst, err := market.Load(adt.WrapStore(ctx, ipldStore), mactor);
	if err != nil {
		return big.Zero(), xerrors.Errorf("failed to load market state: %w", err)
	}

	return mst.TotalLocked()
}

// StoreFromCbor wraps a cbor ipldStore for ADT access.
func StoreFromCbor(ctx context.Context, ipldStore cbor.IpldStore) adt.Store {
	return &cstore{ctx, ipldStore}
}

type cstore struct {
	ctx context.Context
	cbor.IpldStore
}

func (s *cstore) Context() context.Context {
	return s.ctx
}
