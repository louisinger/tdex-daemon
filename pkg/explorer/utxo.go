package explorer

import (
	"encoding/hex"
	"errors"
	"github.com/tdex-network/tdex-daemon/pkg/bufferutil"
	"github.com/tdex-network/tdex-daemon/pkg/transactionutil"
	"github.com/vulpemventures/go-elements/confidential"
	"github.com/vulpemventures/go-elements/transaction"
)

type Utxo interface {
	Hash() string
	Index() uint32
	Value() uint64
	Asset() string
	ValueCommitment() string
	AssetCommitment() string
	Nonce() []byte
	Script() []byte
	RangeProof() []byte
	SurjectionProof() []byte
	IsConfidential() bool
	SetScript(script []byte)
	SetUnconfidential(asset string, value uint64)
	SetConfidential(nonce, rangeProof, surjectionProof []byte)
	Parse() (*transaction.TxInput, *transaction.TxOutput, error)
}

func NewUnconfidentialWitnessUtxo(
	hash string,
	index uint32,
	value uint64,
	asset string,
	script []byte,
) Utxo {
	return witnessUtxo{
		UHash:   hash,
		UIndex:  index,
		UValue:  value,
		UAsset:  asset,
		UScript: script,
	}
}

func NewConfidentialWitnessUtxo(
	hash string,
	index uint32,
	valueCommitment, assetCommitment string,
	script, nonce, rangeProof, surjectionProof []byte,
) Utxo {
	return witnessUtxo{
		UHash:            hash,
		UIndex:           index,
		UValueCommitment: valueCommitment,
		UAssetCommitment: assetCommitment,
		UScript:          script,
		UNonce:           nonce,
		URangeProof:      rangeProof,
		USurjectionProof: surjectionProof,
	}
}

type witnessUtxo struct {
	UHash            string `json:"txid"`
	UIndex           uint32 `json:"vout"`
	UValue           uint64 `json:"value"`
	UAsset           string `json:"asset"`
	UValueCommitment string `json:"valuecommitment"`
	UAssetCommitment string `json:"assetcommitment"`
	UScript          []byte
	UNonce           []byte
	URangeProof      []byte
	USurjectionProof []byte
}

func (wu witnessUtxo) Hash() string {
	return wu.UHash
}

func (wu witnessUtxo) Index() uint32 {
	return wu.UIndex
}

func (wu witnessUtxo) Value() uint64 {
	return wu.UValue
}

func (wu witnessUtxo) Asset() string {
	return wu.UAsset
}

func (wu witnessUtxo) ValueCommitment() string {
	return wu.UValueCommitment
}

func (wu witnessUtxo) AssetCommitment() string {
	return wu.UAssetCommitment
}

func (wu witnessUtxo) Nonce() []byte {
	return wu.UNonce
}

func (wu witnessUtxo) Script() []byte {
	return wu.UScript
}

func (wu witnessUtxo) RangeProof() []byte {
	return wu.URangeProof
}

func (wu witnessUtxo) SurjectionProof() []byte {
	return wu.USurjectionProof
}

func (wu witnessUtxo) IsConfidential() bool {
	return len(wu.UValueCommitment) > 0 && len(wu.UAssetCommitment) > 0
}

func (wu witnessUtxo) SetScript(script []byte) {
	wu.UScript = script
}

func (wu witnessUtxo) SetUnconfidential(asset string, value uint64) {
	wu.UAsset = asset
	wu.UValue = value
}

func (wu witnessUtxo) SetConfidential(nonce, rangeProof, surjectionProof []byte) {
	wu.UNonce = make([]byte, 0)
	wu.UNonce = nonce
	wu.URangeProof = make([]byte, 0)
	wu.URangeProof = rangeProof
	wu.USurjectionProof = make([]byte, 0)
	wu.USurjectionProof = surjectionProof
}

func (wu witnessUtxo) Parse() (*transaction.TxInput, *transaction.TxOutput, error) {
	inHash, err := hex.DecodeString(wu.UHash)
	if err != nil {
		return nil, nil, err
	}
	input := transaction.NewTxInput(bufferutil.ReverseBytes(inHash), wu.UIndex)

	var witnessUtxo *transaction.TxOutput
	if len(wu.URangeProof) != 0 && len(wu.USurjectionProof) != 0 {
		assetCommitment, err := hex.DecodeString(wu.UAssetCommitment)
		if err != nil {
			return nil, nil, err
		}
		valueCommitment, err := hex.DecodeString(wu.UValueCommitment)
		if err != nil {
			return nil, nil, err
		}
		witnessUtxo = &transaction.TxOutput{
			Nonce:           wu.UNonce,
			Script:          wu.UScript,
			Asset:           assetCommitment,
			Value:           valueCommitment,
			RangeProof:      wu.URangeProof,
			SurjectionProof: wu.USurjectionProof,
		}
	} else {
		asset, err := hex.DecodeString(wu.UAsset)
		if err != nil {
			return nil, nil, err
		}
		value, err := confidential.SatoshiToElementsValue(wu.UValue)
		if err != nil {
			return nil, nil, err
		}
		asset = append([]byte{0x01}, bufferutil.ReverseBytes(asset)...)

		witnessUtxo = transaction.NewTxOutput(asset, value[:], wu.UScript)
	}

	return input, witnessUtxo, nil
}

func unblindUtxo(
	utxo Utxo,
	blindKeys [][]byte,
	chUnspents chan Utxo,
	chErr chan error,
) {
	unspent := utxo.(witnessUtxo)
	for i := range blindKeys {
		blindKey := blindKeys[i]
		// ignore the following errors because this function is called only if
		// asset and value commitments are defined. However, if a bad (nil) nonce
		// is passed to the UnblindOutput function, this will not be able to reveal
		// secrets of the output.
		assetCommitment, _ := hex.DecodeString(utxo.AssetCommitment())
		valueCommitment, _ := hex.DecodeString(utxo.ValueCommitment())

		txOut := &transaction.TxOutput{
			Nonce:      utxo.Nonce(),
			Asset:      assetCommitment,
			Value:      valueCommitment,
			Script:     utxo.Script(),
			RangeProof: utxo.RangeProof(),
		}
		unBlinded, ok := transactionutil.UnblindOutput(txOut, blindKey)

		if ok {
			asset := unBlinded.AssetHash
			unspent.UAsset = asset
			unspent.UValue = unBlinded.Value
			chUnspents <- unspent
			return
		}
	}

	chErr <- errors.New("unable to unblind utxo with provided keys")
}

func getUtxoDetails(out Utxo, chUnspents chan Utxo, chErr chan error) {
	unspent := out.(witnessUtxo)

	prevoutTxHex, err := GetTransactionHex(unspent.Hash())
	if err != nil {
		chErr <- err
		return
	}
	trx, _ := transaction.NewTxFromHex(prevoutTxHex)
	prevout := trx.Outputs[unspent.Index()]

	if unspent.IsConfidential() {
		unspent.UNonce = prevout.Nonce
		unspent.URangeProof = prevout.RangeProof
		unspent.USurjectionProof = prevout.SurjectionProof
	}
	unspent.UScript = prevout.Script

	chUnspents <- unspent
}