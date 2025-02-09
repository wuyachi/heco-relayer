/*
* Copyright (C) 2020 The poly network Authors
* This file is part of The poly network library.
*
* The poly network is free software: you can redistribute it and/or modify
* it under the terms of the GNU Lesser General Public License as published by
* the Free Software Foundation, either version 3 of the License, or
* (at your option) any later version.
*
* The poly network is distributed in the hope that it will be useful,
* but WITHOUT ANY WARRANTY; without even the implied warranty of
* MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
* GNU Lesser General Public License for more details.
* You should have received a copy of the GNU Lesser General Public License
* along with The poly network . If not, see <http://www.gnu.org/licenses/>.
 */
package manager

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	poly_bridge_sdk "github.com/polynetwork/poly-bridge/bridgesdk"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ontio/ontology-crypto/keypair"
	"github.com/ontio/ontology-crypto/signature"
	"github.com/polynetwork/eth-contracts/go_abi/eccd_abi"
	"github.com/polynetwork/eth-contracts/go_abi/eccm_abi"
	"github.com/polynetwork/heco_relayer/config"
	"github.com/polynetwork/heco_relayer/db"
	"github.com/polynetwork/heco_relayer/log"
	sdk "github.com/polynetwork/poly-go-sdk"
	"github.com/polynetwork/poly/common"
	"github.com/polynetwork/poly/common/password"
	vconfig "github.com/polynetwork/poly/consensus/vbft/config"
	common2 "github.com/polynetwork/poly/native/service/cross_chain_manager/common"

	"github.com/ethereum/go-ethereum"
	"github.com/polynetwork/heco_relayer/tools"
	polytypes "github.com/polynetwork/poly/core/types"
)

const (
	ChanLen = 1
)
const (
	FEE_NOCHECK = iota
	FEE_HASPAY
	FEE_NOTPAY
)

type BridgeTransaction struct {
	header       *polytypes.Header
	param        *common2.ToMerkleValue
	headerProof  string
	anchorHeader *polytypes.Header
	polyTxHash   string
	rawAuditPath []byte
	hasPay       uint8
	fee          string
}

func CheckGasLimit(hash string, limit uint64) error {
	if limit > 300000 {
		return fmt.Errorf("Skipping poly tx %s for gas limit too high %d ", hash, limit)
	}
	return nil
}

func (this *BridgeTransaction) Serialization(sink *common.ZeroCopySink) {
	this.header.Serialization(sink)
	this.param.Serialization(sink)
	if this.headerProof != "" && this.anchorHeader != nil {
		sink.WriteUint8(1)
		sink.WriteString(this.headerProof)
		this.anchorHeader.Serialization(sink)
	} else {
		sink.WriteUint8(0)
	}
	sink.WriteString(this.polyTxHash)
	sink.WriteVarBytes(this.rawAuditPath)
	sink.WriteUint8(this.hasPay)
	sink.WriteString(this.fee)
}

func (this *BridgeTransaction) Deserialization(source *common.ZeroCopySource) error {
	this.header = new(polytypes.Header)
	err := this.header.Deserialization(source)
	if err != nil {
		return err
	}
	this.param = new(common2.ToMerkleValue)
	err = this.param.Deserialization(source)
	if err != nil {
		return err
	}
	anchor, eof := source.NextUint8()
	if eof {
		return fmt.Errorf("Waiting deserialize anchor error")
	}
	if anchor == 1 {
		this.headerProof, eof = source.NextString()
		if eof {
			return fmt.Errorf("Waiting deserialize header proof error")
		}
		this.anchorHeader = new(polytypes.Header)
		this.anchorHeader.Deserialization(source)
	}
	this.polyTxHash, eof = source.NextString()
	if eof {
		return fmt.Errorf("Waiting deserialize poly tx hash error")
	}
	this.rawAuditPath, eof = source.NextVarBytes()
	if eof {
		return fmt.Errorf("Waiting deserialize poly tx hash error")
	}
	this.hasPay, eof = source.NextUint8()
	if eof {
		return fmt.Errorf("Waiting deserialize has pay error")
	}
	this.fee, eof = source.NextString()
	if eof {
		return fmt.Errorf("Waiting deserialize fee error")
	}
	return nil
}

type PolyManager struct {
	config        *config.ServiceConfig
	polySdk       *sdk.PolySdk
	currentHeight uint32
	contractAbi   *abi.ABI
	exitChan      chan int
	db            *db.BoltDB
	ethClient     *ethclient.Client
	senders       []*EthSender
	bridgeSdk     *poly_bridge_sdk.BridgeSdk
	eccdInstance  *eccd_abi.EthCrossChainData
}

func NewPolyManager(servCfg *config.ServiceConfig, startblockHeight uint32, polySdk *sdk.PolySdk, ethereumsdk *ethclient.Client, boltDB *db.BoltDB) (*PolyManager, error) {
	contractabi, err := abi.JSON(strings.NewReader(eccm_abi.EthCrossChainManagerABI))
	if err != nil {
		return nil, err
	}
	chainId, err := ethereumsdk.ChainID(context.Background())
	if err != nil {
		return nil, err
	}
	ks := tools.NewHecoKeyStore(servCfg.HecoConfig, chainId)
	accArr := ks.GetAccounts()
	if len(servCfg.HecoConfig.KeyStorePwdSet) == 0 {
		fmt.Println("please input the passwords for heco(or ethereum) keystore: ")
		for _, v := range accArr {
			fmt.Printf("For address %s. ", v.Address.String())
			raw, err := password.GetPassword()
			if err != nil {
				log.Fatalf("failed to input password: %v", err)
				panic(err)
			}
			servCfg.HecoConfig.KeyStorePwdSet[strings.ToLower(v.Address.String())] = string(raw)
		}
	}
	if err = ks.UnlockKeys(servCfg.HecoConfig); err != nil {
		return nil, err
	}

	senders := make([]*EthSender, len(accArr))
	for i, v := range senders {
		v = &EthSender{}
		v.acc = accArr[i]

		v.ethClient = ethereumsdk
		v.keyStore = ks
		v.config = servCfg
		v.polySdk = polySdk
		v.contractAbi = &contractabi
		v.nonceManager = tools.NewNonceManager(ethereumsdk)
		v.cmap = make(map[string]chan *EthTxInfo)

		senders[i] = v
	}
	bridgeSdk := poly_bridge_sdk.NewBridgeSdk(servCfg.BridgeUrl[0][0])
	address := ethcommon.HexToAddress(servCfg.HecoConfig.ECCDContractAddress)
	instance, err := eccd_abi.NewEthCrossChainData(address, ethereumsdk)
	if err != nil {
		log.Errorf("NewPolyManager - new eth cross chain failed: %s", err.Error())
		return nil, fmt.Errorf("new eccd instance from abi error")
	}
	return &PolyManager{
		exitChan:      make(chan int),
		config:        servCfg,
		polySdk:       polySdk,
		currentHeight: startblockHeight,
		contractAbi:   &contractabi,
		db:            boltDB,
		ethClient:     ethereumsdk,
		senders:       senders,
		bridgeSdk:     bridgeSdk,
		eccdInstance:  instance,
	}, nil
}

func (this *PolyManager) findLatestHeight() uint32 {
	height, err := this.eccdInstance.GetCurEpochStartHeight(nil)
	if err != nil {
		log.Errorf("findLatestHeight - GetLatestHeight failed: %s", err.Error())
		return 0
	}
	return uint32(height)
}

func (this *PolyManager) init() bool {
	if this.currentHeight > 0 {
		log.Infof("PolyManager init - start height from flag: %d", this.currentHeight)
		return true
	}
	this.currentHeight = this.db.GetPolyHeight()
	latestHeight := this.findLatestHeight()
	if latestHeight > this.currentHeight {
		this.currentHeight = latestHeight
		log.Infof("PolyManager init - latest height from ECCM: %d", this.currentHeight)
		return true
	}
	log.Infof("PolyManager init - latest height from DB: %d", this.currentHeight)

	return true
}

func (this *PolyManager) MonitorPolyChain() {
	ret := this.init()
	if ret == false {
		log.Errorf("MonitorChain - init failed\n")
	}
	monitorTicker := time.NewTicker(config.POLY_MONITOR_INTERVAL)
	var blockHandleResult bool
	for {
		select {
		case <-monitorTicker.C:
			latestheight, err := this.polySdk.GetCurrentBlockHeight()
			if err != nil {
				log.Errorf("MonitorChain - get poly chain block height error: %s", err)
				continue
			}
			latestheight--
			if latestheight-this.currentHeight < config.POLY_USEFUL_BLOCK_NUM {
				continue
			}
			log.Infof("MonitorChain - poly chain current height: %d", latestheight)
			blockHandleResult = true
			for this.currentHeight <= latestheight-config.POLY_USEFUL_BLOCK_NUM {
				if this.currentHeight%10 == 0 {
					log.Infof("handle new poly Block height: %d", this.currentHeight)
				}
				blockHandleResult = this.handleDepositEvents(this.currentHeight)
				if blockHandleResult == false {
					break
				}
				this.currentHeight++
				if this.currentHeight%1000 == 0 {
					break
				}
			}
			if err = this.db.UpdatePolyHeight(this.currentHeight - 1); err != nil {
				log.Errorf("MonitorChain - failed to save height of poly: %v", err)
			}
		case <-this.exitChan:
			return
		}
	}
}

func (this *PolyManager) IsEpoch(hdr *polytypes.Header) (bool, []byte, error) {
	blkInfo := &vconfig.VbftBlockInfo{}
	if err := json.Unmarshal(hdr.ConsensusPayload, blkInfo); err != nil {
		return false, nil, fmt.Errorf("commitHeader - unmarshal blockInfo error: %s", err)
	}
	if hdr.NextBookkeeper == common.ADDRESS_EMPTY || blkInfo.NewChainConfig == nil {
		return false, nil, nil
	}

	eccdAddr := ethcommon.HexToAddress(this.config.HecoConfig.ECCDContractAddress)
	eccd, err := eccd_abi.NewEthCrossChainData(eccdAddr, this.ethClient)
	if err != nil {
		return false, nil, fmt.Errorf("failed to new eccm: %v", err)
	}
	rawKeepers, err := eccd.GetCurEpochConPubKeyBytes(nil)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get current epoch keepers: %v", err)
	}

	var bookkeepers []keypair.PublicKey
	for _, peer := range blkInfo.NewChainConfig.Peers {
		keystr, _ := hex.DecodeString(peer.ID)
		key, _ := keypair.DeserializePublicKey(keystr)
		bookkeepers = append(bookkeepers, key)
	}
	bookkeepers = keypair.SortPublicKeys(bookkeepers)
	publickeys := make([]byte, 0)
	sink := common.NewZeroCopySink(nil)
	sink.WriteUint64(uint64(len(bookkeepers)))
	for _, key := range bookkeepers {
		raw := tools.GetNoCompresskey(key)
		publickeys = append(publickeys, raw...)
		sink.WriteVarBytes(crypto.Keccak256(tools.GetEthNoCompressKey(key)[1:])[12:])
	}
	if bytes.Equal(rawKeepers, sink.Bytes()) {
		return false, nil, nil
	}
	return true, publickeys, nil
}

func (this *PolyManager) handleDepositEvents(height uint32) bool {
	lastEpoch := this.findLatestHeight()
	hdr, err := this.polySdk.GetHeaderByHeight(height + 1)
	if err != nil {
		log.Errorf("handleDepositEvents - GetNodeHeader on height :%d failed", height)
		return false
	}
	isCurr := lastEpoch < height+1
	isEpoch, pubkList, err := this.IsEpoch(hdr)
	if err != nil {
		log.Errorf("falied to check isEpoch: %v", err)
		return false
	}
	var (
		anchor *polytypes.Header
		hp     string
	)
	if !isCurr {
		anchor, _ = this.polySdk.GetHeaderByHeight(lastEpoch + 1)
		proof, _ := this.polySdk.GetMerkleProof(height+1, lastEpoch+1)
		hp = proof.AuditPath
	} else if isEpoch {
		anchor, _ = this.polySdk.GetHeaderByHeight(height + 2)
		proof, _ := this.polySdk.GetMerkleProof(height+1, height+2)
		hp = proof.AuditPath
	}

	cnt := 0
	events, err := this.polySdk.GetSmartContractEventByBlock(height)
	for err != nil {
		log.Errorf("handleDepositEvents - get block event at height:%d error: %s", height, err.Error())
		return false
	}
	for _, event := range events {
		for _, notify := range event.Notify {
			if notify.ContractAddress == this.config.PolyConfig.EntranceContractAddress {
				states := notify.States.([]interface{})
				method, _ := states[0].(string)
				if method != "makeProof" {
					continue
				}
				if uint64(states[2].(float64)) != this.config.HecoConfig.SideChainId {
					continue
				}
				proof, err := this.polySdk.GetCrossStatesProof(hdr.Height-1, states[5].(string))
				if err != nil {
					log.Errorf("handleDepositEvents - failed to get proof for key %s: %v", states[5].(string), err)
					continue
				}
				auditpath, _ := hex.DecodeString(proof.AuditPath)
				value, _, _, _ := tools.ParseAuditpath(auditpath)
				param := &common2.ToMerkleValue{}
				if err := param.Deserialization(common.NewZeroCopySource(value)); err != nil {
					log.Errorf("handleDepositEvents - failed to deserialize MakeTxParam (value: %x, err: %v)", value, err)
					continue
				}
				if !METHODS[param.MakeTxParam.Method] {
					log.Errorf("Invalid target contract method %s %s", param.MakeTxParam.Method, event.TxHash)
					continue
				}
				var isTarget bool
				if len(this.config.TargetContracts) > 0 {
					toContractStr := ethcommon.BytesToAddress(param.MakeTxParam.ToContractAddress).String()
					for _, v := range this.config.TargetContracts {
						toChainIdArr, ok := v[toContractStr]
						if ok {
							if len(toChainIdArr["inbound"]) == 0 {
								isTarget = true
								break
							}
							for _, id := range toChainIdArr["inbound"] {
								if id == param.FromChainID {
									isTarget = true
									break
								}
							}
							if isTarget {
								break
							}
						}
					}
					if !isTarget {
						continue
					}
				}
				cnt++
				//sender := this.selectSender()
				//log.Infof("sender %s is handling poly tx ( hash: %s, height: %d )",
				//	sender.acc.Address.String(), event.TxHash, height)
				//// temporarily ignore the error for tx
				//sender.commitDepositEventsWithHeader(hdr, param, hp, anchor, event.TxHash, auditpath)
				bridgeTransaction := &BridgeTransaction{
					header:       hdr,
					param:        param,
					headerProof:  hp,
					anchorHeader: anchor,
					polyTxHash:   event.TxHash,
					rawAuditPath: auditpath,
					hasPay:       FEE_NOCHECK,
					fee:          "",
				}
				sink := common.NewZeroCopySink(nil)
				bridgeTransaction.Serialization(sink)
				this.db.PutBridgeTransactions(hex.EncodeToString(param.MakeTxParam.TxHash), sink.Bytes())
				//if !sender.commitDepositEventsWithHeader(hdr, param, hp, anchor, event.TxHash, auditpath) {
				//	return false
				//}
			}
		}
	}
	if cnt == 0 && isEpoch && isCurr && this.config.HecoConfig.EnableChangeBookKeeper {
		sender := this.selectSender()
		return sender.commitHeader(hdr, pubkList)
	}

	return true
}

func (this *PolyManager) selectSender() *EthSender {
	sum := big.NewInt(0)
	balArr := make([]*big.Int, len(this.senders))
	for i, v := range this.senders {
	RETRY:
		bal, err := v.Balance()
		if err != nil {
			log.Errorf("failed to get balance for %s: %v", v.acc.Address.String(), err)
			time.Sleep(time.Second)
			goto RETRY
		}
		sum.Add(sum, bal)
		balArr[i] = big.NewInt(sum.Int64())
	}
	sum.Rand(rand.New(rand.NewSource(time.Now().Unix())), sum)
	for i, v := range balArr {
		res := v.Cmp(sum)
		if res == 1 || res == 0 {
			return this.senders[i]
		}
	}
	return this.senders[0]
}

func (this *PolyManager) MonitorDeposit() {
	monitorTicker := time.NewTicker(time.Duration(this.config.HecoConfig.MonitorInterval) * time.Second)
	for {
		select {
		case <-monitorTicker.C:
			this.handleLockDepositEvents()
		case <-this.exitChan:
			return
		}
	}
}

func (this *PolyManager) handleLockDepositEvents() error {
	retryList, err := this.db.GetAllBridgeTransactions()
	if err != nil {
		return fmt.Errorf("handleLockDepositEvents - this.db.GetAllBridgeTransactions error: %s", err)
	}
	if len(retryList) == 0 {
		return nil
	}
	bridgeTransactions := make(map[string]*BridgeTransaction, 0)
	for k, v := range retryList {
		bridgeTransaction := new(BridgeTransaction)
		err := bridgeTransaction.Deserialization(common.NewZeroCopySource(v))
		if err != nil {
			log.Errorf("handleLockDepositEvents - retry.Deserialization error: %s", err)
			continue
		}
		bridgeTransactions[k] = bridgeTransaction
	}
	noCheckFees := make([]*poly_bridge_sdk.CheckFeeReq, 0)
	for k, v := range bridgeTransactions {
		if v.hasPay == FEE_NOCHECK {
			noCheckFees = append(noCheckFees, &poly_bridge_sdk.CheckFeeReq{
				ChainId: v.param.FromChainID,
				Hash:    k,
			})
		}
	}
	if len(noCheckFees) > 0 {
		checkFees, err := this.checkFee(noCheckFees)
		if err != nil {
			log.Errorf("handleLockDepositEvents - checkFee error: %s", err)
		}
		if checkFees != nil {
			for _, checkFee := range checkFees {
				if checkFee.Error != "" {
					log.Errorf("check fee err: %s", checkFee.Error)
					continue
				}
				//checkFee.PayState = FEE_NOTPAY
				item, ok := bridgeTransactions[checkFee.Hash]
				if ok {
					if checkFee.PayState == poly_bridge_sdk.STATE_HASPAY {
						log.Infof("tx(%d,%s) has payed fee", checkFee.ChainId, checkFee.Hash)
						item.hasPay = FEE_HASPAY
						item.fee = checkFee.Amount
					} else if checkFee.PayState == poly_bridge_sdk.STATE_NOTPAY {
						log.Infof("tx(%d,%s) has not payed fee", checkFee.ChainId, checkFee.Hash)
						item.hasPay = FEE_NOTPAY
					} else if checkFee.PayState == poly_bridge_sdk.STATE_NOTPOLYPROXY {
						log.Infof("tx(%d,%s) has not POLYPROXY", checkFee.ChainId, checkFee.Hash)
						item.hasPay = FEE_NOTPAY
					} else {
						log.Errorf("check fee of tx(%d,%s) failed", checkFee.ChainId, checkFee.Hash)
					}
				}
			}
		}
	}
	for k, v := range bridgeTransactions {
		if v.hasPay == FEE_NOTPAY {
			this.db.DeleteBridgeTransactions(k)
			log.Infof("tx (src %d, %s, poly %s) has not pay proxy fee, ignore it, payed: %s",
				v.param.FromChainID, hex.EncodeToString(v.param.MakeTxParam.TxHash), v.polyTxHash, v.fee)
			delete(bridgeTransactions, k)
		}
	}
	var maxFeeOfTransaction *BridgeTransaction = nil
	maxFee := new(big.Float).SetUint64(0)
	maxFeeOfTxHash := ""
	for k, v := range bridgeTransactions {
		fee, ok := new(big.Float).SetString(v.fee)
		if ok != true {
			continue
		}
		if v.hasPay == FEE_HASPAY || fee.Cmp(maxFee) > 0 {
			maxFee = fee
			maxFeeOfTransaction = v
			maxFeeOfTxHash = k
		}
	}
	if maxFeeOfTransaction != nil {
		sender := this.selectSender()
		log.Infof("sender %s is handling poly tx ( hash: %x)", sender.acc.Address.String(), maxFeeOfTransaction.param.TxHash)
		res := sender.commitDepositEventsWithHeader(maxFeeOfTransaction.header, maxFeeOfTransaction.param, maxFeeOfTransaction.headerProof,
			maxFeeOfTransaction.anchorHeader, hex.EncodeToString(maxFeeOfTransaction.param.TxHash), maxFeeOfTransaction.rawAuditPath)
		if res == true {
			this.db.DeleteBridgeTransactions(maxFeeOfTxHash)
			delete(bridgeTransactions, maxFeeOfTxHash)
		}
	}
	for k, v := range bridgeTransactions {
		sink := common.NewZeroCopySink(nil)
		v.Serialization(sink)
		this.db.PutBridgeTransactions(k, sink.Bytes())
	}
	return nil
}

func (this *PolyManager) checkFee(checks []*poly_bridge_sdk.CheckFeeReq) ([]*poly_bridge_sdk.CheckFeeRsp, error) {
	return this.bridgeSdk.CheckFee(checks)
}

func (this *PolyManager) Stop() {
	this.exitChan <- 1
	close(this.exitChan)
	log.Infof("poly chain manager exit.")
}

type EthSender struct {
	acc          accounts.Account
	keyStore     *tools.HecoKeyStore
	cmap         map[string]chan *EthTxInfo
	nonceManager *tools.NonceManager
	ethClient    *ethclient.Client
	polySdk      *sdk.PolySdk
	config       *config.ServiceConfig
	contractAbi  *abi.ABI
}

func (this *EthSender) sendTxToEth(info *EthTxInfo) error {
	nonce := this.nonceManager.GetAddressNonce(this.acc.Address)
	tx := types.NewTransaction(nonce, info.contractAddr, big.NewInt(0), info.gasLimit, info.gasPrice, info.txData)
	signedtx, err := this.keyStore.SignTransaction(tx, this.acc)
	if err != nil {
		this.nonceManager.ReturnNonce(this.acc.Address, nonce)
		return fmt.Errorf("commitDepositEventsWithHeader - sign raw tx error and return nonce %d: %v", nonce, err)
	}

	for {
		err = this.ethClient.SendTransaction(context.Background(), signedtx)
		if err != nil {
			log.Errorf("poly to heco SendTransaction error: %v, nonce %d", err, nonce)
			time.Sleep(time.Second)
			continue
		}
		hash := signedtx.Hash()

		isSuccess := this.waitTransactionConfirm(info.polyTxHash, hash)
		if isSuccess {
			log.Infof("successful to relay tx to huobi_eco: (heco_hash: %s, nonce: %d, poly_hash: %s, heco_explorer: %s)",
				hash.String(), nonce, info.polyTxHash, tools.GetExplorerUrl(this.keyStore.GetChainId())+hash.String())
			return nil
		}

		log.Errorf("failed to relay tx to huobi_eco: (heco_hash: %s, nonce: %d, poly_hash: %s, heco_explorer: %s)",
			hash.String(), nonce, info.polyTxHash, tools.GetExplorerUrl(this.keyStore.GetChainId())+hash.String())

		return nil
	}

}

func (this *EthSender) commitDepositEventsWithHeader(header *polytypes.Header, param *common2.ToMerkleValue, headerProof string, anchorHeader *polytypes.Header, polyTxHash string, rawAuditPath []byte) bool {
	var (
		sigs       []byte
		headerData []byte
	)
	if anchorHeader != nil && headerProof != "" {
		for _, sig := range anchorHeader.SigData {
			temp := make([]byte, len(sig))
			copy(temp, sig)
			newsig, _ := signature.ConvertToEthCompatible(temp)
			sigs = append(sigs, newsig...)
		}
	} else {
		for _, sig := range header.SigData {
			temp := make([]byte, len(sig))
			copy(temp, sig)
			newsig, _ := signature.ConvertToEthCompatible(temp)
			sigs = append(sigs, newsig...)
		}
	}

	eccdAddr := ethcommon.HexToAddress(this.config.HecoConfig.ECCDContractAddress)
	eccd, err := eccd_abi.NewEthCrossChainData(eccdAddr, this.ethClient)
	if err != nil {
		panic(fmt.Errorf("failed to new eccm: %v", err))
	}
	fromTx := [32]byte{}
	copy(fromTx[:], param.TxHash[:32])
	res, _ := eccd.CheckIfFromChainTxExist(nil, param.FromChainID, fromTx)
	if res {
		log.Debugf("already relayed to heco: ( from_chain_id: %d, from_txhash: %x,  param.Txhash: %x)",
			param.FromChainID, param.TxHash, param.MakeTxParam.TxHash)
		return true
	}
	//log.Infof("poly proof with header, height: %d, key: %s, proof: %s", header.Height-1, string(key), proof.AuditPath)

	rawProof, _ := hex.DecodeString(headerProof)
	var rawAnchor []byte
	if anchorHeader != nil {
		rawAnchor = anchorHeader.GetMessage()
	}
	headerData = header.GetMessage()
	txData, err := this.contractAbi.Pack("verifyHeaderAndExecuteTx", rawAuditPath, headerData, rawProof, rawAnchor, sigs)
	if err != nil {
		log.Errorf("commitDepositEventsWithHeader - err:" + err.Error())
		return false
	}

	gasPrice, err := this.ethClient.SuggestGasPrice(context.Background())
	if err != nil {
		log.Errorf("commitDepositEventsWithHeader - get suggest sas price failed error: %s", err.Error())
		return false
	}
	contractaddr := ethcommon.HexToAddress(this.config.HecoConfig.ECCMContractAddress)
	callMsg := ethereum.CallMsg{
		From: this.acc.Address, To: &contractaddr, Gas: 0, GasPrice: gasPrice,
		Value: big.NewInt(0), Data: txData,
	}
	gasLimit, err := this.ethClient.EstimateGas(context.Background(), callMsg)
	if err != nil {
		log.Errorf("commitDepositEventsWithHeader - estimate gas limit error: %s", err.Error())
		return false
	}

	// Check gas limit
	gasLimit = uint64(float32(gasLimit) * 1.1)
	if e := CheckGasLimit(polyTxHash, gasLimit); e != nil {
		log.Errorf("Skipped poly tx %s for gas limit too high %v", polyTxHash, gasLimit)
		return true
	}

	k := this.getRouter()
	c, ok := this.cmap[k]
	if !ok {
		c = make(chan *EthTxInfo, ChanLen)
		this.cmap[k] = c
		go func() {
			for v := range c {
				if err = this.sendTxToEth(v); err != nil {
					log.Errorf("failed to send tx to heco: error: %v, txData: %s", err, hex.EncodeToString(v.txData))
				}
			}
		}()
	}
	//TODO: could be blocked
	c <- &EthTxInfo{
		txData:       txData,
		contractAddr: contractaddr,
		gasPrice:     gasPrice,
		gasLimit:     gasLimit,
		polyTxHash:   polyTxHash,
	}
	return true
}

func (this *EthSender) commitHeader(header *polytypes.Header, pubkList []byte) bool {
	headerdata := header.GetMessage()
	var (
		txData []byte
		txErr  error
		sigs   []byte
	)
	gasPrice, err := this.ethClient.SuggestGasPrice(context.Background())
	if err != nil {
		log.Errorf("commitHeader - get suggest sas price failed error: %s", err.Error())
		return false
	}
	for _, sig := range header.SigData {
		temp := make([]byte, len(sig))
		copy(temp, sig)
		newsig, _ := signature.ConvertToEthCompatible(temp)
		sigs = append(sigs, newsig...)
	}

	txData, txErr = this.contractAbi.Pack("changeBookKeeper", headerdata, pubkList, sigs)
	if txErr != nil {
		log.Errorf("commitHeader - err:" + err.Error())
		return false
	}

	contractaddr := ethcommon.HexToAddress(this.config.HecoConfig.ECCMContractAddress)
	callMsg := ethereum.CallMsg{
		From: this.acc.Address, To: &contractaddr, Gas: 0, GasPrice: gasPrice,
		Value: big.NewInt(0), Data: txData,
	}

	gasLimit, err := this.ethClient.EstimateGas(context.Background(), callMsg)
	if err != nil {
		log.Errorf("commitHeader - estimate gas limit error: %s", err.Error())
		return false
	}

	nonce := this.nonceManager.GetAddressNonce(this.acc.Address)
	tx := types.NewTransaction(nonce, contractaddr, big.NewInt(0), gasLimit, gasPrice, txData)
	signedtx, err := this.keyStore.SignTransaction(tx, this.acc)
	if err != nil {
		log.Errorf("commitHeader - sign raw tx error: %s", err.Error())
		return false
	}
	if err = this.ethClient.SendTransaction(context.Background(), signedtx); err != nil {
		log.Errorf("commitHeader - send transaction error:%s\n", err.Error())
		return false
	}

	hash := header.Hash()
	txhash := signedtx.Hash()
	isSuccess := this.waitTransactionConfirm(fmt.Sprintf("header: %d", header.Height), txhash)
	if isSuccess {
		log.Infof("successful to relay poly header to heco: (header_hash: %s, height: %d, heco_txhash: %s, nonce: %d, heco_explorer: %s)",
			hash.ToHexString(), header.Height, txhash.String(), nonce, tools.GetExplorerUrl(this.keyStore.GetChainId())+txhash.String())
	} else {
		log.Errorf("failed to relay poly header to heco: (header_hash: %s, height: %d, heco_txhash: %s, nonce: %d, heco_explorer: %s)",
			hash.ToHexString(), header.Height, txhash.String(), nonce, tools.GetExplorerUrl(this.keyStore.GetChainId())+txhash.String())
	}
	return true
}

func (this *EthSender) getRouter() string {
	return strconv.FormatInt(rand.Int63n(this.config.RoutineNum), 10)
}

func (this *EthSender) Balance() (*big.Int, error) {
	balance, err := this.ethClient.BalanceAt(context.Background(), this.acc.Address, nil)
	if err != nil {
		return nil, err
	}
	return balance, nil
}

// TODO: check the status of tx
func (this *EthSender) waitTransactionConfirm(polyTxHash string, hash ethcommon.Hash) bool {
	start := time.Now()
	for {
		if time.Now().After(start.Add(time.Minute * 5)) {
			return false
		}
		time.Sleep(time.Second * 1)
		_, ispending, err := this.ethClient.TransactionByHash(context.Background(), hash)
		if err != nil {
			continue
		}
		log.Debugf("( heco_transaction %s, poly_tx %s ) is pending: %v", hash.String(), polyTxHash, ispending)
		if ispending == true {
			continue
		} else {
			receipt, err := this.ethClient.TransactionReceipt(context.Background(), hash)
			if err != nil {
				continue
			}
			return receipt.Status == types.ReceiptStatusSuccessful
		}
	}
}

type EthTxInfo struct {
	txData       []byte
	gasLimit     uint64
	gasPrice     *big.Int
	contractAddr ethcommon.Address
	polyTxHash   string
}
