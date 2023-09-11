package network

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/celestiaorg/celestia-app/app"
	"github.com/celestiaorg/celestia-app/app/encoding"
	"github.com/celestiaorg/celestia-app/cmd/celestia-appd/cmd"
	"github.com/celestiaorg/celestia-app/test/util/testnode"
	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	srvconfig "github.com/cosmos/cosmos-sdk/server/config"
	srvtypes "github.com/cosmos/cosmos-sdk/server/types"
	tmconfig "github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/crypto/ed25519"
	cmtos "github.com/tendermint/tendermint/libs/os"
	tmos "github.com/tendermint/tendermint/libs/os"
	"github.com/tendermint/tendermint/node"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/privval"
)

// NodeConfig is a portable configuration for a node. This is originally created
// and published by the Leader node and then downloaded by the other follower
// nodes. It is used to create a consensus node that
type NodeConfig struct {
	Role        string            `json:"role"`
	Validator   bool              `json:"validator"`
	Seed        bool              `json:"seed"`
	Name        string            `json:"name"`
	ChainID     string            `json:"chain_id,omitempty"`
	StartHeight int64             `json:"start_height"`
	Keys        KeySet            `json:"keys"`
	CmtConfig   *tmconfig.Config  `json:"cmt_config"`
	AppConfig   *srvconfig.Config `json:"app_config"`
	P2PID       string            `json:"p2p_id"`
}

type KeySet struct {
	// NetworkKey is the key used for signing gossiped messages.
	NetworkKey ed25519.PrivKey `json:"network_key"`
	// ConsensusKey is the key used for signing votes.
	ConsensusKey ed25519.PrivKey `json:"consensus_key"`
	// AccountKey is the key used for signing transactions.
	AccountMnemonic string `json:"account_mnemonic"`
}

func (c *Config) ConsensusNode(ctx context.Context, name string) (ConsensusNode, error) {
	cfg, ok := c.Nodes[name]
	if !ok {
		return ConsensusNode{}, fmt.Errorf("node %s not found", name)
	}
	cfg.ChainID = c.ChainID
	return NewConsensusNode(ctx, c.Genesis, cfg)
}

type ConsensusNode struct {
	NodeConfig
	kr        keyring.Keyring
	genesis   json.RawMessage
	ecfg      encoding.Config
	stopFuncs []func() error
	ctx       context.Context
	// AppOptions are the application options of the test node.
	AppOptions *testnode.KVAppOptions
	// AppCreator is used to create the application for the testnode.
	AppCreator srvtypes.AppCreator

	cmtNode *node.Node
	cctx    testnode.Context
}

func NewConsensusNode(ctx context.Context, genesis json.RawMessage, cfg NodeConfig) (ConsensusNode, error) {
	ecfg := encoding.MakeConfig(app.ModuleEncodingRegisters...)
	kr := keyring.NewInMemory(ecfg.Codec)
	kr, err := ImportKey(kr, cfg.Keys.AccountMnemonic, cfg.Name)
	if err != nil {
		return ConsensusNode{}, fmt.Errorf("failed to import key: %w", err)
	}
	return ConsensusNode{
		genesis:    genesis,
		NodeConfig: cfg,
		AppCreator: cmd.NewAppServer,
		AppOptions: testnode.DefaultAppOptions(),
		ecfg:       ecfg,
		kr:         kr,
		ctx:        ctx,
	}, nil
}

// Init creates the files required by tendermint and celestia-app using the data
// downloaded from the Leader node.
func (c *ConsensusNode) Init(baseDir string) (string, error) {
	basePath := filepath.Join(baseDir, ".celestia-app")
	c.CmtConfig.SetRoot(basePath)

	// save the genesis file
	configPath := filepath.Join(basePath, "config")
	err := os.MkdirAll(configPath, os.ModePerm)
	if err != nil {
		return "", err
	}
	// save the genesis file as configured
	err = cmtos.WriteFile(c.CmtConfig.GenesisFile(), c.genesis, 0o644)
	if err != nil {
		return "", err
	}
	pvStateFile := c.CmtConfig.PrivValidatorStateFile()
	if err := tmos.EnsureDir(filepath.Dir(pvStateFile), 0o777); err != nil {
		return "", err
	}
	pvKeyFile := c.CmtConfig.PrivValidatorKeyFile()
	if err := tmos.EnsureDir(filepath.Dir(pvKeyFile), 0o777); err != nil {
		return "", err
	}
	filePV := privval.NewFilePV(c.Keys.ConsensusKey, pvKeyFile, pvStateFile)
	filePV.Save()

	nodeKeyFile := c.CmtConfig.NodeKeyFile()
	if err := tmos.EnsureDir(filepath.Dir(nodeKeyFile), 0o777); err != nil {
		return "", err
	}
	nodeKey := &p2p.NodeKey{
		PrivKey: c.Keys.NetworkKey,
	}
	if err := nodeKey.SaveAs(nodeKeyFile); err != nil {
		return "", err
	}

	return basePath, nil
}

// StartNode uses the testnode package to start a tendermint node with
// celestia-app and the provided configuration.
func (c *ConsensusNode) StartNode(ctx context.Context, baseDir string) error {
	ucfg := c.UniversalTestingConfig()
	tmNode, app, err := testnode.NewCometNode(baseDir, &ucfg)
	if err != nil {
		return err
	}

	c.cmtNode = tmNode
	cctx := testnode.NewContext(ctx, c.kr, ucfg.TmConfig, c.ChainID)

	cctx, stopNode, err := testnode.StartNode(tmNode, cctx)
	c.stopFuncs = append(c.stopFuncs, stopNode)
	if err != nil {
		return err
	}

	cctx, cleanupGRPC, err := testnode.StartGRPCServer(app, ucfg.AppConfig, cctx)
	c.stopFuncs = append(c.stopFuncs, cleanupGRPC)

	c.cctx = cctx

	return err
}

// UniversalTestingConfig returns the configuration used by the testnode package.
func (c *ConsensusNode) UniversalTestingConfig() testnode.UniversalTestingConfig {
	return testnode.UniversalTestingConfig{
		TmConfig:    c.CmtConfig,
		AppConfig:   c.AppConfig,
		AppOptions:  c.AppOptions,
		AppCreator:  c.AppCreator,
		SupressLogs: false,
	}
}

// Stop stops the node and cleans up the data directory.
func (c *ConsensusNode) Stop() error {
	var err error
	for _, stop := range c.stopFuncs {
		if err := stop(); err != nil {
			err = err
		}
	}
	return err
}

// ImportKey imports the provided mnemonic into the keyring with the provided name.
func ImportKey(kr keyring.Keyring, accountMnemonic string, name string) (keyring.Keyring, error) {
	kr.Delete(name)
	_, err := kr.Key(name)
	if err == nil {
		return kr, fmt.Errorf("key %s already exists", name)
	}
	_, err = kr.NewAccount(name, accountMnemonic, "", "", hd.Secp256k1)
	if err != nil {
		return kr, fmt.Errorf("failed to import key: %w", err)
	}
	return kr, nil
}