package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

type EthError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type CommonResponse struct {
	Version   string    `json:"jsonrpc"`
	RequestId int       `json:"id"`
	Error     *EthError `json:"error"`
}

type EthBlockNumber struct {
	CommonResponse
	Number hexutil.Big `json:"result"`
}

type EthBalance struct {
	CommonResponse
	Balance hexutil.Big `json:"result"`
}

type EthTransaction struct {
	From common.Address  `json:"from"`
	To   *common.Address `json:"to"` // Pointer because it might be missing
	Hash string          `json:"hash"`
	Gas  hexutil.Big     `json:"gas"`
}

type EthBlockByNumberResult struct {
	Difficulty   hexutil.Big      `json:"difficulty"`
	Miner        common.Address   `json:"miner"`
	Transactions []EthTransaction `json:"transactions"`
	TxRoot       common.Hash      `json:"transactionsRoot"`
	Hash         common.Hash      `json:"hash"`
}

type EthBlockByNumber struct {
	CommonResponse
	Result EthBlockByNumberResult `json:"result"`
}

type StructLog struct {
	Op      string            `json:"op"`
	Pc      uint64            `json:"pc"`
	Depth   uint64            `json:"depth"`
	Error   *EthError         `json:"error"`
	Gas     uint64            `json:"gas"`
	GasCost uint64            `json:"gasCost"`
	Memory  []string          `json:"memory"`
	Stack   []string          `json:"stack"`
	Storage map[string]string `json:"storage"`
}

type EthTxTraceResult struct {
	Gas         uint64      `json:"gas"`
	Failed      bool        `json:"failed"`
	ReturnValue string      `json:"returnValue"`
	StructLogs  []StructLog `json:"structLogs"`
}

type EthTxTrace struct {
	CommonResponse
	Result EthTxTraceResult `json:"result"`
}

type DebugModifiedAccounts struct {
	CommonResponse
	Result []common.Address `json:"result"`
}

// StorageRangeResult is the result of a debug_storageRangeAt API call.
type StorageRangeResult struct {
	Storage storageMap   `json:"storage"`
	NextKey *common.Hash `json:"nextKey"` // nil if Storage includes the last key in the trie.
}

type storageMap map[common.Hash]storageEntry

type storageEntry struct {
	Key   *common.Hash `json:"key"`
	Value common.Hash  `json:"value"`
}

type DebugStorageRange struct {
	CommonResponse
	Result StorageRangeResult `json:"result"`
}

func post(client *http.Client, url, request string, response interface{}) error {
	start := time.Now()
	r, err := client.Post(url, "application/json", strings.NewReader(request))
	if err != nil {
		return err
	}
	if r.StatusCode != 200 {
		return fmt.Errorf("Status %s", r.Status)
	}
	decoder := json.NewDecoder(r.Body)
	defer r.Body.Close()
	err = decoder.Decode(response)
	fmt.Printf("%s %s %f\n", url, request, time.Since(start).Seconds())
	return err
}

func print(client *http.Client, url, request string) {
	r, err := client.Post(url, "application/json", strings.NewReader(request))
	if err != nil {
		fmt.Printf("Could not print: %v\n", err)
		return
	}
	if r.StatusCode != 200 {
		fmt.Printf("Status %s", r.Status)
		return
	}
	fmt.Printf("ContentLength: %d\n", r.ContentLength)
	buf := make([]byte, 2000000)
	l, err := r.Body.Read(buf)
	if err != nil && err != io.EOF {
		fmt.Printf("Could not read response: %v\n", err)
		return
	}
	if l < len(buf) {
		fmt.Printf("Could not read response: %d out of %d\n", l, len(buf))
		//return
	}
	fmt.Printf("%s\n", buf[:l])
}

func compareBlocks(b, bg *EthBlockByNumber) bool {
	r := b.Result
	rg := bg.Result
	if r.Difficulty.ToInt().Cmp(rg.Difficulty.ToInt()) != 0 {
		fmt.Printf("Difficulty difference %d %d\n", r.Difficulty.ToInt(), rg.Difficulty.ToInt())
		return false
	}
	if r.Miner != rg.Miner {
		fmt.Printf("Miner different %x %x\n", r.Miner, rg.Miner)
		return false
	}
	if len(r.Transactions) != len(rg.Transactions) {
		fmt.Printf("Num of txs different: %d %d\n", len(r.Transactions), len(rg.Transactions))
		return false
	}
	for i, tx := range r.Transactions {
		txg := rg.Transactions[i]
		if tx.From != txg.From {
			fmt.Printf("Tx %d different From: %x %x\n", i, tx.From, txg.From)
			return false
		}
		if (tx.To == nil && txg.To != nil) || (tx.To != nil && txg.To == nil) {
			fmt.Printf("Tx %d different To nilness: %t %t\n", i, (tx.To==nil), (txg.To==nil))
			return false
		}
		if tx.To != nil && txg.To != nil && *tx.To != *txg.To {
			fmt.Printf("Tx %d different To: %x %x\n", i, *tx.To, *txg.To)
			return false
		}
		if tx.Hash != txg.Hash {
			fmt.Printf("Tx %x different Hash: %s %s\n", i, tx.Hash, txg.Hash)
			return false
		}
	}
	return true
}

func compareTraces(trace, traceg *EthTxTrace) bool {
	r := trace.Result
	rg := traceg.Result
	if r.Gas != rg.Gas {
		fmt.Printf("Trace different Gas: %d %d\n", r.Gas, rg.Gas)
		return false
	}
	if r.Failed != rg.Failed {
		fmt.Printf("Trace different Failed: %t %t\n", r.Failed, rg.Failed)
		return false
	}
	if r.ReturnValue != rg.ReturnValue {
		fmt.Printf("Trace different ReturnValue: %s %s\n", r.ReturnValue, rg.ReturnValue)
		return false
	}
	if len(r.StructLogs) != len(rg.StructLogs) {
		fmt.Printf("Trace different length: %d %d\n", len(r.StructLogs), len(rg.StructLogs))
		return false
	}
	for i, l := range r.StructLogs {
		lg := rg.StructLogs[i]
		if l.Op != lg.Op {
			fmt.Printf("Trace different Op: %d %s %s\n", i, l.Op, lg.Op)
			return false
		}
		if l.Pc != lg.Pc {
			fmt.Printf("Trace different Pc: %d %d %d\n", i, l.Pc, lg.Pc)
			return false
		}
	}
	return true
}

func compareBalances(balance, balanceg *EthBalance) bool {
	if balance.Balance.ToInt().Cmp(balanceg.Balance.ToInt()) != 0 {
		fmt.Printf("Different balance: %d %d\n", balance.Balance.ToInt(), balanceg.Balance.ToInt())
		return false
	}
	return true
}

func compareModifiedAccounts(ma, mag *DebugModifiedAccounts) bool {
	r := ma.Result
	rg := mag.Result
	rset := make(map[common.Address]struct{})
	rsetg := make(map[common.Address]struct{})
	for _, a := range r {
		rset[a] = struct{}{}
	}
	for _, a := range rg {
		rsetg[a] = struct{}{}
	}
	for _, a := range r {
		if _, ok := rsetg[a]; !ok {
			fmt.Printf("%x not present in rg\n", a)
			// We tolerate that
			//return false
		}
	}
	for _, a := range rg {
		if _, ok := rset[a]; !ok {
			fmt.Printf("%x not present in r\n", a)
			return false
		}
	}
	return true
}

func compareStorageRanges(sm, smg map[common.Hash]storageEntry) bool {
	for k, v := range sm {
		if vg, ok := smg[k]; !ok {
			fmt.Printf("%x not present in smg\n", k)
			return false
		} else {
			if k != crypto.Keccak256Hash(v.Key[:]){
				fmt.Printf("Sec key %x does not match key %x\n", k, *v.Key)
				return false
			}
			if v.Value != vg.Value {
				fmt.Printf("Different values for %x: %x %x [%x]\n", k, v.Value, vg.Value, *v.Key)
				return false				
			}
		}
	}
	for k, _ := range smg {
		if _, ok := sm[k]; !ok {
			fmt.Printf("%x not present in sm\n", k)
			return false
		}
	}
	return true
}

func bench1() {
	var client = &http.Client{
		Timeout: time.Second * 600,
	}
	req_id := 0
	//geth_url := "http://192.168.1.96:8545"
	geth_url := "http://localhost:8545"
	turbogeth_url := "http://localhost:9545"
	req_id++
	template := `
{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}
`
	var blockNumber EthBlockNumber
	if err := post(client, turbogeth_url, fmt.Sprintf(template, req_id), &blockNumber); err != nil {
		fmt.Printf("Could not get block number: %v\n", err)
		return
	}
	if blockNumber.Error != nil {
		fmt.Printf("Error getting block number: %d %s\n", blockNumber.Error.Code, blockNumber.Error.Message)
		return
	}
	lastBlock := blockNumber.Number.ToInt().Int64()
	fmt.Printf("Last block: %d\n", lastBlock)
	accounts := make(map[common.Address]struct{})
	firstBn := 900000
	prevBn := firstBn
	for bn := firstBn; bn <= int(lastBlock); bn++ {
		req_id++
		template := `
{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x%x",true],"id":%d}
`
		var b EthBlockByNumber
		if err := post(client, turbogeth_url, fmt.Sprintf(template, bn, req_id), &b); err != nil {
			fmt.Printf("Could not retrieve block %d: %v\n", bn, err)
			return
		}
		if b.Error != nil {
			fmt.Printf("Error retrieving block: %d %s\n", b.Error.Code, b.Error.Message)
		}
		var bg EthBlockByNumber
		if err := post(client, geth_url, fmt.Sprintf(template, bn, req_id), &bg); err != nil {
			fmt.Printf("Could not retrieve block g %d: %v\n", bn, err)
			return
		}
		if bg.Error != nil {
			fmt.Printf("Error retrieving block g: %d %s\n", bg.Error.Code, bg.Error.Message)
			return
		}
		if !compareBlocks(&b, &bg) {
			fmt.Printf("Block difference for %d\n", bn)
			return
		}
		accounts[b.Result.Miner] = struct{}{}
		for i, tx := range b.Result.Transactions {
			accounts[tx.From] = struct{}{}
			if tx.To != nil {
				accounts[*tx.To] = struct{}{}
			}
			if tx.To != nil && tx.Gas.ToInt().Uint64() > 21000 {
				req_id++
				template = `
	{"jsonrpc":"2.0","method":"debug_storageRangeAt","params":["0x%x", %d,"0x%x","0x%x",%d],"id":%d}
	`
				sm := make(map[common.Hash]storageEntry)
				nextKey := &common.Hash{}
				for nextKey != nil {
					var sr DebugStorageRange
					if err := post(client, turbogeth_url, fmt.Sprintf(template, b.Result.Hash, i, tx.To, *nextKey, 1024, req_id), &sr); err != nil {
						fmt.Printf("Could not get storageRange: %s: %v\n", tx.Hash, err)
						return
					}
					if sr.Error != nil {
						fmt.Printf("Error getting storageRange: %d %s\n", sr.Error.Code, sr.Error.Message)
						break
					} else {
						nextKey = sr.Result.NextKey
						for k, v := range sr.Result.Storage {
							sm[k] = v
						}
					}
				}
				fmt.Printf("storageRange: %d\n", len(sm))
				smg := make(map[common.Hash]storageEntry)
				nextKey = &common.Hash{}
				for nextKey != nil {
					var srg DebugStorageRange
					if err := post(client, geth_url, fmt.Sprintf(template, b.Result.Hash, i, tx.To, *nextKey, 1024, req_id), &srg); err != nil {
						fmt.Printf("Could not get storageRange g: %s: %v\n", tx.Hash, err)
						return
					}
					if srg.Error != nil {
						fmt.Printf("Error getting storageRange g: %d %s\n", srg.Error.Code, srg.Error.Message)
						break
					} else {
						nextKey = srg.Result.NextKey
						for k, v := range srg.Result.Storage {
							smg[k] = v
						}
					}
				}
				fmt.Printf("storageRange g: %d\n", len(smg))
				if !compareStorageRanges(sm, smg) {
					fmt.Printf("Different in storage ranges tx %s\n", tx.Hash)
					return
				}
			}
			req_id++
			template =`
{"jsonrpc":"2.0","method":"debug_traceTransaction","params":["%s"],"id":%d}
`
			var trace EthTxTrace
			if err := post(client, turbogeth_url, fmt.Sprintf(template, tx.Hash, req_id), &trace); err != nil {
				fmt.Printf("Could not trace transaction %s: %v\n", tx.Hash, err)
				print(client, turbogeth_url, fmt.Sprintf(template, tx.Hash, req_id))
				return
			}
			if trace.Error != nil {
				fmt.Printf("Error tracing transaction: %d %s\n", trace.Error.Code, trace.Error.Message)
			}
			var traceg EthTxTrace
			if err := post(client, geth_url, fmt.Sprintf(template, tx.Hash, req_id), &traceg); err != nil {
				fmt.Printf("Could not trace transaction g %s: %v\n", tx.Hash, err)
				print(client, geth_url, fmt.Sprintf(template, tx.Hash, req_id))
				return
			}
			if traceg.Error != nil {
				fmt.Printf("Error tracing transaction g: %d %s\n", traceg.Error.Code, traceg.Error.Message)
				return
			}
			if !compareTraces(&trace, &traceg) {
				fmt.Printf("Different traces block %d, tx %s\n", bn, tx.Hash)
				return
			}
		}
		req_id++
		template = `
{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x%x", "0x%x"],"id":%d}
`
		var balance EthBalance
		if err := post(client, turbogeth_url, fmt.Sprintf(template, b.Result.Miner, bn, req_id), &balance); err != nil {
			fmt.Printf("Could not get account balance: %v\n", err)
			return
		}
		if balance.Error != nil {
			fmt.Printf("Error getting account balance: %d %s", balance.Error.Code, balance.Error.Message)
			return
		}
		var balanceg EthBalance
		if err := post(client, geth_url, fmt.Sprintf(template, b.Result.Miner, bn, req_id), &balanceg); err != nil {
			fmt.Printf("Could not get account balance g: %v\n", err)
			return
		}
		if balanceg.Error != nil {
			fmt.Printf("Error getting account balance g: %d %s\n", balanceg.Error.Code, balanceg.Error.Message)
			return
		}
		if !compareBalances(&balance, &balanceg) {
			fmt.Printf("Miner %x balance difference for block %d\n", b.Result.Miner, bn)
			return
		}
		if prevBn < bn && bn % 1000 == 0 {
			// Checking modified accounts
			req_id++
			template = `
{"jsonrpc":"2.0","method":"debug_getModifiedAccountsByNumber","params":[%d, %d],"id":%d}
`
			var ma DebugModifiedAccounts
			if err := post(client, turbogeth_url, fmt.Sprintf(template, prevBn, bn, req_id), &ma); err != nil {
				fmt.Printf("Could not get modified accounts: %v\n", err)
				return
			}
			if ma.Error != nil {
				fmt.Printf("Error getting modified accounts: %d %d\n", ma.Error.Code, ma.Error.Message)
				return
			}
			var mag DebugModifiedAccounts
			if err := post(client, geth_url, fmt.Sprintf(template, prevBn, bn, req_id), &mag); err != nil {
				fmt.Printf("Could not get modified accounts g: %v\n", err)
				return
			}
			if mag.Error != nil {
				fmt.Printf("Error getting modified accounts g: %d %d\n", mag.Error.Code, mag.Error.Message)
				return
			}
			if !compareModifiedAccounts(&ma, &mag) {
				fmt.Printf("Modified accouts different for blocks %d-%d\n", prevBn, bn)
				return
			}
			fmt.Printf("Done blocks %d-%d, modified accounts: %d (%d)\n", prevBn, bn, len(ma.Result), len(mag.Result))
			prevBn = bn
		}
	}
}

func bench2() {
	var client = &http.Client{
		Timeout: time.Second * 600,
	}
	req_id := 0
	turbogeth_url := "http://localhost:8545"
	req_id++
	template := `
{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}
`
	var blockNumber EthBlockNumber
	if err := post(client, turbogeth_url, fmt.Sprintf(template, req_id), &blockNumber); err != nil {
		fmt.Printf("Could not get block number: %v\n", err)
		return
	}
	if blockNumber.Error != nil {
		fmt.Printf("Error getting block number: %d %s\n", blockNumber.Error.Code, blockNumber.Error.Message)
		return
	}
	lastBlock := blockNumber.Number.ToInt().Int64()
	fmt.Printf("Last block: %d\n", lastBlock)
	firstBn := 1720000-2
	prevBn := firstBn
	for bn := firstBn; bn <= int(lastBlock); bn++ {
		req_id++
		template := `
{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x%x",true],"id":%d}
`
		var b EthBlockByNumber
		if err := post(client, turbogeth_url, fmt.Sprintf(template, bn, req_id), &b); err != nil {
			fmt.Printf("Could not retrieve block %d: %v\n", bn, err)
			return
		}
		if b.Error != nil {
			fmt.Printf("Error retrieving block: %d %s\n", b.Error.Code, b.Error.Message)
		}
		for i, tx := range b.Result.Transactions {
			if tx.To != nil && tx.Gas.ToInt().Uint64() > 21000 {
				// Request storage range
				// blockHash common.Hash, txIndex int, contractAddress common.Address, keyStart hexutil.Bytes, maxResult int
				req_id++
				template = `
	{"jsonrpc":"2.0","method":"debug_storageRangeAt","params":["0x%x", %d,"0x%x","0x%x",%d],"id":%d}
	`
				sm := make(map[common.Hash]storageEntry)
				nextKey := &common.Hash{}
				for nextKey != nil {
					var sr DebugStorageRange
					if err := post(client, turbogeth_url, fmt.Sprintf(template, b.Result.Hash, i, tx.To, *nextKey, 1024, req_id), &sr); err != nil {
						fmt.Printf("Could not get storageRange: %x: %v\n", tx.Hash, err)
						return
					}
					if sr.Error != nil {
						fmt.Printf("Error getting storageRange: %d %s\n", sr.Error.Code, sr.Error.Message)
						break
					} else {
						nextKey = sr.Result.NextKey
						for k, v := range sr.Result.Storage {
							sm[k] = v
							if v.Key == nil {
								fmt.Printf("No key for sec key: %x\n", k)
							} else if k != crypto.Keccak256Hash(v.Key[:]) {
								fmt.Printf("Different sec key: %x %x (%x), value %x\n", k, crypto.Keccak256Hash(v.Key[:]), *(v.Key), v.Value)
							} else {
								fmt.Printf("Keys: %x %x, value %x\n", *(v.Key), k, v.Value)
							}
						}
					}
				}
				fmt.Printf("storageRange: %d\n", len(sm))
			}
		}
		if prevBn < bn && bn % 1000 == 0 {
			// Checking modified accounts
			req_id++
			template = `
{"jsonrpc":"2.0","method":"debug_getModifiedAccountsByNumber","params":[%d, %d],"id":%d}
`
			var ma DebugModifiedAccounts
			if err := post(client, turbogeth_url, fmt.Sprintf(template, prevBn, bn, req_id), &ma); err != nil {
				fmt.Printf("Could not get modified accounts: %v\n", err)
				return
			}
			if ma.Error != nil {
				fmt.Printf("Error getting modified accounts: %d %d\n", ma.Error.Code, ma.Error.Message)
				return
			}
			fmt.Printf("Done blocks %d-%d, modified accounts: %d\n", prevBn, bn, len(ma.Result))
			prevBn = bn
		}
	}
}

func bench3() {
	var client = &http.Client{
		Timeout: time.Second * 600,
	}
	geth_url := "http://localhost:8545"
	turbogeth_url := "http://localhost:9545"
	blockhash := common.HexToHash("0xdf15213766f00680c6a20ba76ba2cc9534435e19bc490039f3a7ef42095c8d13")
	req_id := 1
	template := `
{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x%x",true],"id":%d}
`
	var b EthBlockByNumber
	if err := post(client, turbogeth_url, fmt.Sprintf(template, 1720000, req_id), &b); err != nil {
		fmt.Printf("Could not retrieve block %d: %v\n", 1720000, err)
		return
	}
	if b.Error != nil {
		fmt.Printf("Error retrieving block: %d %s\n", b.Error.Code, b.Error.Message)
	}
	for txindex := 0; txindex < 18; txindex++ {
		txhash := b.Result.Transactions[txindex].Hash
		req_id++
		template =`
{"jsonrpc":"2.0","method":"debug_traceTransaction","params":["%s"],"id":%d}
	`
		var trace EthTxTrace
		if err := post(client, turbogeth_url, fmt.Sprintf(template, txhash, req_id), &trace); err != nil {
			fmt.Printf("Could not trace transaction %s: %v\n", txhash, err)
			print(client, turbogeth_url, fmt.Sprintf(template, txhash, req_id))
			return
		}
		if trace.Error != nil {
			fmt.Printf("Error tracing transaction: %d %s\n", trace.Error.Code, trace.Error.Message)
		}
		var traceg EthTxTrace
		if err := post(client, geth_url, fmt.Sprintf(template, txhash, req_id), &traceg); err != nil {
			fmt.Printf("Could not trace transaction g %s: %v\n", txhash, err)
			print(client, geth_url, fmt.Sprintf(template, txhash, req_id))
			return
		}
		if traceg.Error != nil {
			fmt.Printf("Error tracing transaction g: %d %s\n", traceg.Error.Code, traceg.Error.Message)
			return
		}
		//print(client, turbogeth_url, fmt.Sprintf(template, txhash, req_id))
		if !compareTraces(&trace, &traceg) {
			fmt.Printf("Different traces block %d, tx %s\n", 1720000, txhash)
			return
		}
	}
	to := common.HexToAddress("0xbb9bc244d798123fde783fcc1c72d3bb8c189413")
	sm := make(map[common.Hash]storageEntry)
	start := common.HexToHash("0x5aa12c260b07325d83f0c9170a2c667948d0247cad4ad999cd00148658b0552d")

	req_id++
	template = `
{"jsonrpc":"2.0","method":"debug_storageRangeAt","params":["0x%x", %d,"0x%x","0x%x",%d],"id":%d}
	`
	i := 18
	nextKey := &start
	for nextKey != nil {
		var sr DebugStorageRange
		if err := post(client, turbogeth_url, fmt.Sprintf(template, blockhash, i, to, *nextKey, 1024, req_id), &sr); err != nil {
			fmt.Printf("Could not get storageRange: %v\n", err)
			return
		}
		if sr.Error != nil {
			fmt.Printf("Error getting storageRange: %d %s\n", sr.Error.Code, sr.Error.Message)
			break
		} else {
			nextKey = sr.Result.NextKey
			for k, v := range sr.Result.Storage {
				sm[k] = v
			}
		}
	}
	fmt.Printf("storageRange: %d\n", len(sm))
	smg := make(map[common.Hash]storageEntry)
	nextKey = &start
	for nextKey != nil {
		var srg DebugStorageRange
		if err := post(client, geth_url, fmt.Sprintf(template, blockhash, i, to, *nextKey, 1024, req_id), &srg); err != nil {
			fmt.Printf("Could not get storageRange g: %v\n", err)
			return
		}
		if srg.Error != nil {
			fmt.Printf("Error getting storageRange g: %d %s\n", srg.Error.Code, srg.Error.Message)
			break
		} else {
			nextKey = srg.Result.NextKey
			for k, v := range srg.Result.Storage {
				smg[k] = v
			}
		}
	}
	fmt.Printf("storageRange g: %d\n", len(smg))
	if !compareStorageRanges(sm, smg) {
		fmt.Printf("Different in storage ranges tx\n")
		return
	}
}

func bench4() {
	var client = &http.Client{
		Timeout: time.Second * 600,
	}
	turbogeth_url := "http://localhost:9545"
	blockhash := common.HexToHash("0xdf15213766f00680c6a20ba76ba2cc9534435e19bc490039f3a7ef42095c8d13")
	req_id := 1
	template := `
{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x%x",true],"id":%d}
`
	var b EthBlockByNumber
	if err := post(client, turbogeth_url, fmt.Sprintf(template, 1720000, req_id), &b); err != nil {
		fmt.Printf("Could not retrieve block %d: %v\n", 1720000, err)
		return
	}
	if b.Error != nil {
		fmt.Printf("Error retrieving block: %d %s\n", b.Error.Code, b.Error.Message)
	}
	for txindex := 0; txindex < 6; txindex++ {
		txhash := b.Result.Transactions[txindex].Hash
		req_id++
		template =`
{"jsonrpc":"2.0","method":"debug_traceTransaction","params":["%s"],"id":%d}
	`
		var trace EthTxTrace
		if err := post(client, turbogeth_url, fmt.Sprintf(template, txhash, req_id), &trace); err != nil {
			fmt.Printf("Could not trace transaction %s: %v\n", txhash, err)
			print(client, turbogeth_url, fmt.Sprintf(template, txhash, req_id))
			return
		}
		if trace.Error != nil {
			fmt.Printf("Error tracing transaction: %d %s\n", trace.Error.Code, trace.Error.Message)
		}
		print(client, turbogeth_url, fmt.Sprintf(template, txhash, req_id))
	}
	to := common.HexToAddress("0x8b3b3b624c3c0397d3da8fd861512393d51dcbac")
	sm := make(map[common.Hash]storageEntry)
	start := common.HexToHash("0xa283ff49a55f86420a4acd5835658d8f45180db430c7b0d7ae98da5c64f620dc")

	req_id++
	template = `
{"jsonrpc":"2.0","method":"debug_storageRangeAt","params":["0x%x", %d,"0x%x","0x%x",%d],"id":%d}
	`
	i := 6
	nextKey := &start
	for nextKey != nil {
		var sr DebugStorageRange
		if err := post(client, turbogeth_url, fmt.Sprintf(template, blockhash, i, to, *nextKey, 1024, req_id), &sr); err != nil {
			fmt.Printf("Could not get storageRange: %v\n", err)
			return
		}
		if sr.Error != nil {
			fmt.Printf("Error getting storageRange: %d %s\n", sr.Error.Code, sr.Error.Message)
			break
		} else {
			nextKey = sr.Result.NextKey
			for k, v := range sr.Result.Storage {
				sm[k] = v
			}
		}
	}
	fmt.Printf("storageRange: %d\n", len(sm))
}

func main() {
	bench1()
}
