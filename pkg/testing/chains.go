package testing

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/datachainlab/ibc-solidity/pkg/chains"
	"github.com/datachainlab/ibc-solidity/pkg/contract"
	"github.com/datachainlab/ibc-solidity/pkg/contract/ibcclient"
	"github.com/datachainlab/ibc-solidity/pkg/contract/ibcconnection"
	"github.com/datachainlab/ibc-solidity/pkg/contract/provablestore"
	ibctypes "github.com/datachainlab/ibc-solidity/pkg/ibc/types"
	"github.com/datachainlab/ibc-solidity/pkg/wallet"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

const (
	BesuIBFT2Client              = "BesuIBFT2"
	DefaultChannelVersion        = "ics20-1"
	DefaultDelayPeriod    uint64 = 0
	DefaultPrefix                = "ibc"
)

type Chain struct {
	t *testing.T

	client        contract.Client
	IBCClient     ibcclient.Ibcclient
	IBCConnection ibcconnection.Ibcconnection
	ProvableStore provablestore.Provablestore

	chainID              int64
	provableStoreAddress common.Address

	key0 *ecdsa.PrivateKey

	// State
	LastContractState *contract.ContractState

	// IBC specific helpers
	ClientIDs   []string          // ClientID's used on this chain
	Connections []*TestConnection // track connectionID's created for this chain
}

type ContractConfig interface {
	GetProvableStoreAddress() common.Address
	GetIBCClientAddress() common.Address
	GetIBCConnectionAddress() common.Address
}

func NewChain(t *testing.T, chainID int64, client contract.Client, config ContractConfig, mnemonicPhrase string) *Chain {
	ibcClient, err := ibcclient.NewIbcclient(config.GetIBCClientAddress(), client)
	if err != nil {
		t.Error(err)
	}
	ibcConnection, err := ibcconnection.NewIbcconnection(config.GetIBCConnectionAddress(), client)
	if err != nil {
		t.Error(err)
	}
	provableStore, err := provablestore.NewProvablestore(config.GetProvableStoreAddress(), client)
	if err != nil {
		t.Error(err)
	}

	key0, err := wallet.GetPrvKeyFromMnemonicAndHDWPath(mnemonicPhrase, "m/44'/60'/0'/0/0")
	if err != nil {
		t.Error(err)
	}

	return &Chain{t: t, client: client, IBCClient: *ibcClient, IBCConnection: *ibcConnection, ProvableStore: *provableStore, chainID: chainID, provableStoreAddress: config.GetProvableStoreAddress(), key0: key0}
}

func (chain *Chain) Client() contract.Client {
	return chain.client
}

func (chain *Chain) TxOpts(ctx context.Context) *bind.TransactOpts {
	return contract.MakeGenTxOpts(big.NewInt(chain.chainID), chain.key0)(ctx)
}

func (chain *Chain) CallOpts(ctx context.Context) *bind.CallOpts {
	opts := chain.TxOpts(ctx)
	return &bind.CallOpts{
		From:    opts.From,
		Context: opts.Context,
	}
}

func (chain *Chain) ChainID() int64 {
	return chain.chainID
}

func (chain *Chain) ChainIDString() string {
	return fmt.Sprint(chain.chainID)
}

func (chain *Chain) GetCommitmentPrefix() []byte {
	return []byte(DefaultPrefix)
}

func (chain *Chain) GetClientState(clientID string) *ibctypes.ClientState {
	ctx := context.Background()
	cs, found, err := chain.ProvableStore.GetClientState(chain.CallOpts(ctx), clientID)
	require.NoError(chain.t, err)
	require.True(chain.t, found)
	return (*ibctypes.ClientState)(&cs)
}

func (chain *Chain) GetContractState(counterparty *Chain, counterpartyClientID string, storageKeys [][]byte) (*contract.ContractState, error) {
	height := counterparty.GetClientState(counterpartyClientID).LatestHeight
	return chain.client.GetContractState(
		context.Background(),
		chain.provableStoreAddress,
		storageKeys,
		big.NewInt(int64(height)),
	)
}

func (chain *Chain) VerifyClientState(clientID string, counterparty *Chain, counterpartyClientID string) bool {
	ctx := context.Background()

	targetState, ok, err := counterparty.ProvableStore.GetClientState(counterparty.CallOpts(ctx), counterpartyClientID)
	require.NoError(chain.t, err)
	require.True(chain.t, ok)

	chain.UpdateHeader()
	counterparty.UpdateHeader()

	require.NoError(chain.t, chain.UpdateBesuClient(ctx, counterparty, clientID))

	key, err := counterparty.ProvableStore.ClientStateCommitmentSlot(counterparty.CallOpts(ctx), counterpartyClientID)
	require.NoError(chain.t, err)

	proof, err := counterparty.QueryProof(chain, clientID, "0x"+hex.EncodeToString(key[:]))
	require.NoError(chain.t, err)

	clientState, found, err := chain.ProvableStore.GetClientState(chain.CallOpts(ctx), clientID)
	require.NoError(chain.t, err)
	require.True(chain.t, found)

	ok, err = chain.IBCClient.VerifyClientState(
		chain.CallOpts(ctx),
		ibcclient.ClientStateData(clientState),
		clientID, proof.Height, chain.GetCommitmentPrefix(), counterpartyClientID, proof.Data, ibcclient.ClientStateData(targetState),
	)
	require.NoError(chain.t, err)
	return ok
}

func (chain *Chain) ConstructMsgCreateClient(counterparty *Chain) MsgCreateClient {
	clientState := &ibctypes.ClientState{
		ChainId:              counterparty.ChainIDString(),
		ProvableStoreAddress: counterparty.provableStoreAddress.Bytes(),
		LatestHeight:         counterparty.LastHeader().Base.Number.Uint64(),
	}
	consensusState := &ibctypes.ConsensusState{
		Timestamp:  counterparty.LastHeader().Base.Time,
		Root:       counterparty.LastHeader().Base.Root.Bytes(),
		Validators: counterparty.LastValidators(),
	}
	return NewMsgCreateClient(clientState, consensusState)
}

func (chain *Chain) ConstructMsgUpdateClient(counterparty *Chain, clientID string) MsgUpdateClient {
	trustedHeight := chain.GetClientState(clientID).LatestHeight
	var header = ibcclient.IBCClientHeader{
		BesuHeaderRLPBytes: counterparty.LastContractState.SealingHeaderRLP(),
		Seals:              counterparty.LastContractState.CommitSeals,
		TrustedHeight:      trustedHeight,
		AccountStateProof:  counterparty.LastContractState.AccountProofRLP(),
	}
	return NewMsgUpdateClient(header)
}

func (chain *Chain) UpdateHeader() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		state, err := chain.client.GetContractState(ctx, chain.provableStoreAddress, nil, nil)
		if err != nil {
			panic(err)
		}
		if chain.LastContractState == nil || state.ParsedHeader.Base.Number.Cmp(chain.LastHeader().Base.Number) == 1 {
			chain.LastContractState = state
			return
		} else {
			continue
		}
	}
}

func (chain *Chain) CreateBesuClient(ctx context.Context, counterparty *Chain, clientID string) error {
	msg := chain.ConstructMsgCreateClient(counterparty)
	return chain.WaitIfNoError(ctx)(
		chain.IBCClient.CreateClient(chain.TxOpts(ctx), clientID, msg.ClientStateData(), msg.ConsensusStateData()),
	)
}

func (chain *Chain) UpdateBesuClient(ctx context.Context, counterparty *Chain, clientID string) error {
	msg := chain.ConstructMsgUpdateClient(counterparty, clientID)
	return chain.WaitIfNoError(ctx)(
		chain.IBCClient.UpdateClient(chain.TxOpts(ctx), clientID, msg.Header),
	)
}

func (chain *Chain) ConnectionOpenInit(ctx context.Context, counterparty *Chain, connection, counterpartyConnection *TestConnection) error {
	return chain.WaitIfNoError(ctx)(
		chain.IBCConnection.ConnectionOpenInit(
			chain.TxOpts(ctx),
			connection.ClientID,
			connection.ID,
			ibcconnection.CounterpartyData{
				ClientId:     connection.CounterpartyClientID,
				ConnectionId: "",
				Prefix:       ibcconnection.MerklePrefixData{KeyPrefix: counterparty.GetCommitmentPrefix()},
			},
			DefaultDelayPeriod,
		),
	)
}

func (chain *Chain) ConnectionOpenTry(ctx context.Context, counterparty *Chain, connection, counterpartyConnection *TestConnection) error {
	proof, err := counterparty.QueryProof(chain, connection.ClientID, chain.ConnectionStateCommitmentSlot(counterpartyConnection.ID))
	if err != nil {
		return err
	}
	return chain.WaitIfNoError(ctx)(
		chain.IBCConnection.ConnectionOpenTry(
			chain.TxOpts(ctx),
			ibcconnection.IBCConnectionMsgConnectionOpenTry{
				ConnectionId: connection.ID,
				Counterparty: ibcconnection.CounterpartyData{
					ClientId:     counterpartyConnection.ClientID,
					ConnectionId: counterpartyConnection.ID,
					Prefix:       ibcconnection.MerklePrefixData{KeyPrefix: counterparty.GetCommitmentPrefix()},
				},
				DelayPeriod: DefaultDelayPeriod,
				ClientId:    connection.ClientID,
				// ClientState: ibcconnection.ClientStateData{}, // TODO set chain's clientState
				CounterpartyVersions: []ibcconnection.VersionData{
					{Identifier: "1", Features: []string{"ORDER_ORDERED", "ORDER_UNORDERED"}},
				},
				ProofHeight: proof.Height,
				ProofInit:   proof.Data,
			},
		),
	)
}

// ConnectionOpenAck will construct and execute a MsgConnectionOpenAck.
func (chain *Chain) ConnectionOpenAck(
	ctx context.Context,
	counterparty *Chain,
	connection, counterpartyConnection *TestConnection,
) error {
	proof, err := counterparty.QueryProof(chain, connection.ClientID, chain.ConnectionStateCommitmentSlot(counterpartyConnection.ID))
	if err != nil {
		return err
	}
	return chain.WaitIfNoError(ctx)(
		chain.IBCConnection.ConnectionOpenAck(
			chain.TxOpts(ctx),
			ibcconnection.IBCConnectionMsgConnectionOpenAck{
				ConnectionId:             connection.ID,
				CounterpartyConnectionID: counterpartyConnection.ID,
				// clientState
				Version:     ibcconnection.VersionData{Identifier: "1", Features: []string{"ORDER_ORDERED", "ORDER_UNORDERED"}},
				ProofTry:    proof.Data,
				ProofHeight: proof.Height,
			},
		),
	)
}

func (chain *Chain) ConnectionOpenConfirm(
	ctx context.Context,
	counterparty *Chain,
	connection, counterpartyConnection *TestConnection,
) error {
	proof, err := counterparty.QueryProof(chain, connection.ClientID, chain.ConnectionStateCommitmentSlot(counterpartyConnection.ID))
	if err != nil {
		return err
	}
	return chain.WaitIfNoError(ctx)(
		chain.IBCConnection.ConnectionOpenConfirm(
			chain.TxOpts(ctx),
			ibcconnection.IBCConnectionMsgConnectionOpenConfirm{
				ConnectionId: connection.ID,
				ProofAck:     proof.Data,
				ProofHeight:  proof.Height,
			},
		),
	)
}

// Slot calculator

func (chain *Chain) ConnectionStateCommitmentSlot(connectionID string) string {
	key, err := chain.ProvableStore.ConnectionCommitmentSlot(chain.CallOpts(context.Background()), connectionID)
	require.NoError(chain.t, err)
	return "0x" + hex.EncodeToString(key[:])
}

// Querier

type Proof struct {
	Height uint64
	Data   []byte
}

func (chain *Chain) QueryProof(counterparty *Chain, counterpartyClientID string, storageKey string) (*Proof, error) {
	if !strings.HasPrefix(storageKey, "0x") {
		return nil, fmt.Errorf("storageKey must be hex string")
	}
	s, err := chain.GetContractState(counterparty, counterpartyClientID, [][]byte{[]byte(storageKey)})
	if err != nil {
		return nil, err
	}
	return &Proof{Height: s.ParsedHeader.Base.Number.Uint64(), Data: s.StorageProofRLP(0)}, nil
}

func (chain *Chain) LastValidators() [][]byte {
	var addrs [][]byte
	for _, val := range chain.LastContractState.ParsedHeader.Validators {
		addrs = append(addrs, val.Bytes())
	}
	return addrs
}

func (chain *Chain) LastHeader() *chains.ParsedHeader {
	return chain.LastContractState.ParsedHeader
}

func (chain *Chain) WaitForReceiptAndGet(ctx context.Context, tx *gethtypes.Transaction) error {
	rc, err := chain.Client().WaitForReceiptAndGet(ctx, tx)
	if err != nil {
		return err
	}
	if rc.Status == 1 {
		return nil
	} else {
		return fmt.Errorf("failed to call transaction: %v %v", err, rc)
	}
}

func (chain *Chain) WaitIfNoError(ctx context.Context) func(tx *gethtypes.Transaction, err error) error {
	return func(tx *gethtypes.Transaction, err error) error {
		if err != nil {
			return err
		}
		if err := chain.WaitForReceiptAndGet(ctx, tx); err != nil {
			return err
		}
		return nil
	}
}

// NewClientID appends a new clientID string in the format:
// ClientFor<counterparty-chain-id><index>
func (chain *Chain) NewClientID(clientType string) string {
	clientID := fmt.Sprintf("%s-%s-%v", clientType, strconv.Itoa(len(chain.ClientIDs)), time.Now().Unix())
	chain.ClientIDs = append(chain.ClientIDs, clientID)
	return clientID
}

// AddTestConnection appends a new TestConnection which contains references
// to the connection id, client id and counterparty client id.
func (chain *Chain) AddTestConnection(clientID, counterpartyClientID string) *TestConnection {
	conn := chain.ConstructNextTestConnection(clientID, counterpartyClientID)

	chain.Connections = append(chain.Connections, conn)
	return conn
}

// ConstructNextTestConnection constructs the next test connection to be
// created given a clientID and counterparty clientID. The connection id
// format: <chainID>-conn<index>
func (chain *Chain) ConstructNextTestConnection(clientID, counterpartyClientID string) *TestConnection {
	connectionID := fmt.Sprintf("connection-%v-%v", uint64(len(chain.Connections)), time.Now().Unix())
	return &TestConnection{
		ID:                   connectionID,
		ClientID:             clientID,
		NextChannelVersion:   DefaultChannelVersion,
		CounterpartyClientID: counterpartyClientID,
	}
}