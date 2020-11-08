package application

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"time"

	mm "github.com/tdex-network/tdex-daemon/pkg/marketmaking"

	"github.com/btcsuite/btcutil"
	"github.com/shopspring/decimal"
	"github.com/tdex-network/tdex-daemon/internal/core/domain"
	dbbadger "github.com/tdex-network/tdex-daemon/internal/infrastructure/storage/db/badger"
	"github.com/tdex-network/tdex-daemon/pkg/crawler"
	"github.com/tdex-network/tdex-daemon/pkg/explorer"
	"github.com/tdex-network/tdex-daemon/pkg/swap"
	"github.com/tdex-network/tdex-daemon/pkg/trade"
	pb "github.com/tdex-network/tdex-protobuf/generated/go/swap"
	"github.com/vulpemventures/go-elements/address"
	"github.com/vulpemventures/go-elements/network"
	"github.com/vulpemventures/go-elements/pset"
	"google.golang.org/protobuf/proto"
)

const testDir = "testDatadir"

//RegtestExplorerAPI ...
const RegtestExplorerAPI = "http://127.0.0.1:3001"

var connectedToTestDb = false
var dbManager *dbbadger.DbManager

type mockedWallet struct {
	mnemonic          []string
	encryptedMnemonic string
	password          string
}

func h2b(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

func b2h(b []byte) string {
	return hex.EncodeToString(b)
}

func startNigiriAndWait() {
	cmd := exec.Command("nigiri", "start", "--liquid")
	runCommand(cmd)
	time.Sleep(10000 * time.Millisecond)
}

func stopNigiri() {
	cmd := exec.Command("nigiri", "stop", "--delete")
	runCommand(cmd)
}

func runCommand(cmd *exec.Cmd) {
	// configure `Stderr`
	cmd.Stdout = nil
	cmd.Stderr = os.Stdout

	// run command
	if err := cmd.Run(); err != nil {
		panic(err)
	}
}

func connectToTestDb() {
	if !connectedToTestDb {
		var err error
		if _, err := os.Stat(testDir); os.IsNotExist(err) {
			os.Mkdir(testDir, os.ModePerm)
		}
		dbManager, err = dbbadger.NewDbManager(testDir, nil)
		if err != nil {
			panic(err)
		}
		connectedToTestDb = true
	}
}

func newTestOperator(
	marketRepositoryIsEmpty bool,
	tradeRepositoryIsEmpty bool,
	vaultRepositoryIsEmpty bool,
) (
	OperatorService,
	TradeService,
	context.Context,
	func(),
) {
	connectToTestDb()
	tx := dbManager.NewTransaction()
	ctx := context.WithValue(context.Background(), "tx", tx)

	marketRepo := dbbadger.NewMarketRepositoryImpl(dbManager)
	if marketRepositoryIsEmpty == false {
		err := fillMarketRepo(ctx, &marketRepo)
		if err != nil {
			panic(err)
		}
	}

	tradeRepo := dbbadger.NewTradeRepositoryImpl(dbManager)

	if !tradeRepositoryIsEmpty {
		err := fillTradeRepo(ctx, tradeRepo, marketUnspents[1].AssetHash, network.Regtest.AssetID)
		if err != nil {
			panic(err)
		}
	}

	vaultRepo := newMockedVaultRepositoryImpl(*tradeWallet)

	if !vaultRepositoryIsEmpty {
		vaultRepo.UpdateVault(ctx, nil, "", func(v *domain.Vault) (*domain.Vault, error) {
			v.DeriveNextExternalAddressForAccount(domain.FeeAccount)
			v.DeriveNextExternalAddressForAccount(domain.MarketAccountStart)
			v.DeriveNextExternalAddressForAccount(domain.MarketAccountStart + 1)
			return v, nil
		})
	}

	unspentRepo := dbbadger.NewUnspentRepositoryImpl(dbManager)

	explorerSvc := explorer.NewService(RegtestExplorerAPI)
	crawlerSvc := crawler.NewService(crawler.Opts{
		ExplorerSvc:            explorerSvc,
		Observables:            []crawler.Observable{},
		ErrorHandler:           func(err error) { fmt.Println(err) },
		IntervalInMilliseconds: 100,
	})

	tradeSvc := newTradeService(
		marketRepo,
		tradeRepo,
		vaultRepo,
		unspentRepo,
		explorerSvc,
		crawlerSvc,
	)

	operatorService := NewOperatorService(
		marketRepo,
		vaultRepo,
		tradeRepo,
		unspentRepo,
		explorerSvc,
		crawlerSvc,
	)

	blockchainListener := NewBlockchainListener(
		unspentRepo,
		marketRepo,
		vaultRepo,
		crawlerSvc,
		explorerSvc,
		dbManager,
	)
	blockchainListener.ObserveBlockchain()

	close := func() {
		if connectedToTestDb {
			dbManager.Store.Close()
			dbManager.UnspentStore.Close()
			connectedToTestDb = false
		}
		crawlerSvc.Stop()
		os.RemoveAll(testDir)
	}

	return operatorService, tradeSvc, ctx, close
}

// returns a TradeService intialized with some mocked data:
//	- 1 open market
//	- 1 close market
//	- unlocked wallet
// 	- unspents funding the open market (1 LBTC utxo of amount 1 BTC, 1 ASS utxo of amount 6500 BTC)
// 	- unspents funding the close market (1 LBTC utxo of amount 1 BTC)
//	- unspents funding the fee account (1 LBTC utxo of amount 1 BTC)
func newTestTrader() (*tradeService, context.Context, func()) {
	connectToTestDb()

	tx := dbManager.NewTransaction()
	ctx := context.WithValue(context.Background(), "tx", tx)

	// vault repo with fee and markets (1 open and 1 closed) accounts initialized
	// with some derived addresses
	vaultRepo := newMockedVaultRepositoryImpl(*tradeWallet)
	vaultRepo.UpdateVault(ctx, nil, "", func(v *domain.Vault) (*domain.Vault, error) {
		v.DeriveNextExternalAddressForAccount(domain.FeeAccount)
		v.DeriveNextExternalAddressForAccount(domain.MarketAccountStart)
		v.DeriveNextExternalAddressForAccount(domain.MarketAccountStart + 1)
		return v, nil
	})

	// unspent repo with unspents funding market and fee account
	unspents := append([]domain.Unspent{}, feeUnspents...)
	unspents = append(unspents, marketUnspents...)

	unspentsRepo := dbbadger.NewUnspentRepositoryImpl(dbManager)
	unspentsRepo.AddUnspents(ctx, unspents)

	// market repo with open market
	marketRepo := dbbadger.NewMarketRepositoryImpl(dbManager)
	fillMarketRepo(ctx, &marketRepo)
	// trade repo, this doesn't need to be prepared
	tradeRepo := dbbadger.NewTradeRepositoryImpl(dbManager)
	explorerSvc := explorer.NewService(RegtestExplorerAPI)
	crawlerSvc := crawler.NewService(crawler.Opts{
		ExplorerSvc:            explorerSvc,
		Observables:            []crawler.Observable{},
		ErrorHandler:           func(err error) { fmt.Println(err) },
		IntervalInMilliseconds: 100,
	})

	traderSvc := newTradeService(
		marketRepo,
		tradeRepo,
		vaultRepo,
		unspentsRepo,
		explorerSvc,
		crawlerSvc,
	)

	close := func() {
		if connectedToTestDb {
			dbManager.Store.Close()
			dbManager.UnspentStore.Close()
			connectedToTestDb = false
		}
		os.RemoveAll(testDir)
	}

	return traderSvc, ctx, close
}

func fillMarketRepo(ctx context.Context, marketRepo *domain.MarketRepository) error {
	// TODO: create and open market
	// opened market
	(*marketRepo).UpdateMarket(
		ctx,
		domain.MarketAccountStart,
		func(market *domain.Market) (*domain.Market, error) {
			market.FundMarket([]domain.OutpointWithAsset{
				{
					Asset: marketUnspents[0].AssetHash,
					Txid:  marketUnspents[0].TxID,
					Vout:  int(marketUnspents[0].VOut),
				},
				{
					Asset: marketUnspents[1].AssetHash,
					Txid:  marketUnspents[1].TxID,
					Vout:  int(marketUnspents[1].VOut),
				},
			})
			err := market.MakeTradable()
			if err != nil {
				return nil, err
			}

			return market, nil
		},
	)
	// closed market (and also not funded)
	(*marketRepo).GetOrCreateMarket(ctx, domain.MarketAccountStart+1)
	return nil
}

func fillTradeRepo(ctx context.Context, tradeRepo domain.TradeRepository, quoteAsset string, baseAsset string) error {
	proposerWallet, err := trade.NewRandomWallet(&network.Regtest)
	if err != nil {
		return err
	}

	swapRequest, err := newSwapRequest(
		proposerWallet,
		baseAsset, 30000000,
		quoteAsset, 20000000,
	)

	if err != nil {
		return err
	}

	tradeRepo.UpdateTrade(ctx, nil, func(trade *domain.Trade) (*domain.Trade, error) {
		trade.Propose(swapRequest, quoteAsset, nil)
		return trade, nil
	})

	return nil
}

func newTestWallet(w *mockedWallet) (*walletService, context.Context, func()) {
	if _, err := os.Stat(testDir); os.IsNotExist(err) {
		os.Mkdir(testDir, os.ModePerm)
	}
	dbManager, err := dbbadger.NewDbManager(testDir, nil)
	if err != nil {
		panic(err)
	}

	vaultRepo := dbbadger.NewVaultRepositoryImpl(dbManager)
	if w != nil {
		vaultRepo = newMockedVaultRepositoryImpl(*w)
	}
	unspentRepo := dbbadger.NewUnspentRepositoryImpl(dbManager)
	explorerSvc := explorer.NewService(RegtestExplorerAPI)
	crawlerSvc := crawler.NewService(crawler.Opts{
		ExplorerSvc:            explorerSvc,
		Observables:            []crawler.Observable{},
		ErrorHandler:           func(err error) { fmt.Println(err) },
		IntervalInMilliseconds: 5000,
	})
	walletSvc := newWalletService(
		vaultRepo,
		unspentRepo,
		crawlerSvc,
		explorerSvc,
	)
	ctx := context.WithValue(
		context.Background(),
		"tx",
		dbManager.NewTransaction(),
	)
	close := func() {
		recover()
		dbManager.Store.Close()
		dbManager.UnspentStore.Close()
		dbManager.PriceStore.Close()
		if w == nil {
			crawlerSvc.Stop()
		}
		os.RemoveAll(testDir)
	}
	return walletSvc, ctx, close
}

func newSwapRequest(
	w *trade.Wallet,
	assetP string, amountP uint64,
	assetR string, amountR uint64,
) (*pb.SwapRequest, error) {
	explorerSvc := explorer.NewService(RegtestExplorerAPI)
	if _, err := explorerSvc.Faucet(w.Address()); err != nil {
		return nil, err
	}
	time.Sleep(5 * time.Second)

	utxos, err := explorerSvc.GetUnspents(w.Address(), [][]byte{w.BlindingKey()})
	if err != nil {
		return nil, err
	}
	_, witnessScript := w.Script()

	psetBase64, err := trade.NewSwapTx(
		utxos, w.BlindingKey(), assetP, amountP, assetR, amountR, witnessScript,
	)
	if err != nil {
		return nil, err
	}

	blindKeyMap := map[string][]byte{
		b2h(witnessScript): w.BlindingKey(),
	}

	msg, err := swap.Request(swap.RequestOpts{
		AssetToBeSent:      assetP,
		AmountToBeSent:     amountP,
		AssetToReceive:     assetR,
		AmountToReceive:    amountR,
		PsetBase64:         psetBase64,
		InputBlindingKeys:  blindKeyMap,
		OutputBlindingKeys: blindKeyMap,
	})

	if err != nil {
		return nil, err
	}
	req := &pb.SwapRequest{}
	proto.Unmarshal(msg, req)
	return req, nil
}

func newSwapComplete(
	w *trade.Wallet,
	swapAccept *pb.SwapAccept,
) (*pb.SwapComplete, error) {
	swapAcceptMsg, _ := proto.Marshal(swapAccept)
	completedPsetBase64, err := w.Sign(swapAccept.GetTransaction())
	if err != nil {
		return nil, err
	}

	_, msg, err := swap.Complete(swap.CompleteOpts{
		Message:    swapAcceptMsg,
		PsetBase64: completedPsetBase64,
	})
	if err != nil {
		return nil, err
	}
	com := &pb.SwapComplete{}
	proto.Unmarshal(msg, com)
	return com, nil
}

func isFinalizableTransaction(psetBase64 string) bool {
	ptx, _ := pset.NewPsetFromBase64(psetBase64)
	err := pset.MaybeFinalizeAll(ptx)
	return err == nil
}

type priceAndPreviewTestData struct {
	unspents           []domain.Unspent
	market             *domain.Market
	lbtcAmount         uint64
	expectedBuyAmount  uint64
	expectedSellAmount uint64
	expectedPrice      Price
}

func mocksForPriceAndPreview(withDefaultStrategy bool) (*priceAndPreviewTestData, error) {
	addr := "el1qqfmmhdayrxdqs60hecn6yzfzmpquwlhn5m39ytngr8gu63ar6zhqngyj0ak7n3jr8ypfz7s6v7nmnkdvmu8n5pev33ac5thm7"
	script, _ := address.ToOutputScript(addr, network.Regtest)
	unspents := []domain.Unspent{
		{
			TxID:            "0000000000000000000000000000000000000000000000000000000000000000",
			VOut:            0,
			Value:           100000000,
			AssetHash:       network.Regtest.AssetID,
			ValueCommitment: "080000000000000000000000000000000000000000000000000000000000000000",
			AssetCommitment: "090000000000000000000000000000000000000000000000000000000000000000",
			ScriptPubKey:    script,
			Nonce:           make([]byte, 33),
			RangeProof:      make([]byte, 4174),
			SurjectionProof: make([]byte, 64),
			Address:         addr,
			Spent:           false,
			Locked:          false,
			LockedBy:        nil,
			Confirmed:       true,
		},
		{
			TxID:            "0000000000000000000000000000000000000000000000000000000000000000",
			VOut:            0,
			Value:           650000000000,
			AssetHash:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ValueCommitment: "080000000000000000000000000000000000000000000000000000000000000000",
			AssetCommitment: "090000000000000000000000000000000000000000000000000000000000000000",
			ScriptPubKey:    script,
			Nonce:           make([]byte, 33),
			RangeProof:      make([]byte, 4174),
			SurjectionProof: make([]byte, 64),
			Address:         addr,
			Spent:           false,
			Locked:          false,
			LockedBy:        nil,
			Confirmed:       true,
		},
	}

	market, _ := domain.NewMarket(domain.MarketAccountStart)
	market.FundMarket([]domain.OutpointWithAsset{
		// LBTC
		domain.OutpointWithAsset{
			Asset: network.Regtest.AssetID,
			Txid:  "0000000000000000000000000000000000000000000000000000000000000000",
			Vout:  0,
		},
		// ASS
		domain.OutpointWithAsset{
			Asset: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Txid:  "0000000000000000000000000000000000000000000000000000000000000000",
			Vout:  1,
		},
	})

	bp, _ := decimal.NewFromString("0.00015385")
	qp, _ := decimal.NewFromString("6500")
	price := Price{
		BasePrice:  bp,
		QuotePrice: qp,
	}

	if withDefaultStrategy {
		market.MakeTradable()

		return &priceAndPreviewTestData{
			unspents:           unspents,
			market:             market,
			lbtcAmount:         10000, // 0.0001 LBTC
			expectedBuyAmount:  65169016,
			expectedSellAmount: 65155984,
			expectedPrice:      price,
		}, nil
	}

	market.MakeStrategyPluggable()
	market.ChangeBasePrice(bp)
	market.ChangeQuotePrice(qp)

	return &priceAndPreviewTestData{
		unspents:           unspents,
		market:             market,
		lbtcAmount:         10000, // 0.0001 LBTC
		expectedBuyAmount:  81250000,
		expectedSellAmount: 48750000,
		expectedPrice:      price,
	}, nil
}

func mockWrongSwapComplete() *pb.SwapComplete {
	mockedSwapAccept := &pb.SwapAccept{
		Id:          "6c563406c0a840f5",
		RequestId:   "342557bb6156c063",
		Transaction: "cHNldP8BAP1LVwIAAAABA2P81miwvH419N66FkIksBJD3qS7RZobsJHPxgVXI351AQAAAAD/////QSRf2sy3woRAw2j7iFcd3ABsWgovjRad9e2coN5sRpABAAAAAP////8n4QRoNsnmlBK+ymbW1uP+Ozbp8KKi3p+ViOaRoJgN7gAAAAAA/////wYL5xmYCRuzC0v+npwtbaUVY1dTxH47DDphXsaCRkOyoJ4JhPG7TNloOMK/DzDBAsMW6cH8yonRtdIlGvlA0OUwAgwDteUtrq8P5FuLe4TXBYqrb+4nBE0oauU6Tc1Z+KAUrOkWABQDHJbS85epshVdbGrh0UQMua31ugslQGIVX60FT+G2N+MeqNK7lkKE3zqH4whsB7Ux0Xj3qQhRxItOTExF4VsOEb97Db0f2DwFkA9UZaKEjz4HQGPeaQNALr4tdRj4xGF4LdpoQlCKWQyCFRw6o8WEUiTEzJw2UBYAFAMcltLzl6myFV1sauHRRAy5rfW6Cz7j2Xe7eOZDJk7JxRws1EEXnQlV8YESf8atnOo/a8geCfZXaBs+F0t3+jzXogs/GUa0MtYEjTmanIJq5NZJZGFeAu1+68eu7fkN63nYzF5lVu2FeVxziQnL4EOqjd1gX+U+FgAUDh/DN12LfnB/ydOlCe5G7oA38OsKhgT6OZYnON3/7y5Zi5kSw01I1O5W7KZxK2RKMExzDg8J0Lk5m6KjQL7H3KLPo76t98oKKS4zorqdRRiTlBZzsMMD17LWkobBn76iQqtv/uCld8QvBn8Os4D8iooPJUAwWEwWABQQAEmg+GTH2GS6VJUlLtg4YsKaIAseyyLf7oXKpXaFoHIQ0jwGqH/USUYu5uGYa86NVmRy/wgvpzv7MufiX3EzHs+WG2SUllmi/+PnhGMfj6+cnOZ2TQJi/28jMA1l6q1R6YjlhZ1m1hIBe309GmTtvt74Sqq0eRYAFJ1UNq/hQmALiCznrSs9smn4v0xlASWyUQcOKcoZBDzzPM1zJOLdqwPsxK4LXnfE/A5c9slaAQAAAAAAAAKKAAAAAAAAAAAAAAAAAAAAAAAAgwMAB4NWrVx+vXvHfd0NFUDTjX7hGgbGZEsOun5cBzko/pm/mbCIzDF3LKW6Jp1GiTxnNqAOE4Cdm6IezPojeKPtAmxD0o9kUqBxdZUVSjUUL1h6eNv00ukNnPQRyzUEy/jYqa02IYUVFh04heGuM5r04cZaxH2oPEB21CNRTIULCeU1/U4QYDMAAAAAAAAAAeihswDHt/xxgboJKBFhd3MnEc3Xy9w4Kj4zHigg+Ct/8M/ZMkKlx0khZFzOSlUrxsMrwZc78PllnDEpYDTC8BNFOcBUgpiXiHEQFhbxrkvgZbqLEQMmaIa3AL0trceRQLE1DE1kMW5d1sxnDwOknDj1NKQUHPccCHERvzbyUW7IX1C6YLLGYbRutVUZyHW+iPuFX3suMnfAYjZ12Tt9+rE8yQ8IZSzPovzcTRIjXutkqlF0fhjWco65MtP4pjTuXBLtaQsoySLae24gqaBQigBFBqWsGXbUvzqtn/a0bQRNUn1iSlibRNbhAdM/E54sSe+XqOhbEfFvYPn79kYaJzKsLyEwual+2MCkhEcSL1J0wHbHT07Ucz0SqwkUm6Xc6OC49toprM9SoKsfut4lPQSPTYROXXMufp4eaYRbF96E3uw78twRVlqssgIaPclMqHnCZ73PhKPXJNtUkJEL3gUdPxbwsNCbru08oLq035jB4zt0juPVHqNIdz9U22MmmNdkZQtvS248DQBVJiHrYdIRY9GkMpBID3VZOxNiBvTu14d3PkNNGjGkONPtYMNLz0UjIEoTJr2kykUr53BuEpSTsqo99G8+fMe884zfWYy0tWcbQHk/1umFmPV5khLHmtPXWMrk7rp5RJayD7y0Y1vzgZLNqOplkIASyoTaFhoDLpt6JFNuY6Cb2azbrPYejndRhdKaC8BEUweIEuSLVzB7avn1aju8kaimHLd++lDj96cmhI2bYxQYp4AhAcdulHmzMfNiYplPfYRmvyhefiFOxb+RKZDuyB9cyp/Rz4a9NdEB0JaRavIUFVlzcONjGuZ2PetvfGoAdmudHSEYWq5qxfVzXdutVr2lBxnGK08iJMr+IyGTG9EV8nNsFKb9kGTeLDrgAtuJ1SoZSYsXV3QIPuXLvOLD9WNYcfulyLiEfM75wj3r2j+PyH7aYrNMom+36V35xEp02WM61fiNzNxMsD2DVAjlhlgr28XgJVZd26XtmTXeIU1dtCv+Mj/4hZkK/KKrAK3e/42KMurhFukDOdFFL4Jb6Rr3hcwPiV9kheFExABbcdwnANFvce9u75vMswfA+KlHPNSx/WqO2SmWHQxxDEODm96AI12Tkz65cdq5BHblNBRzaWP4IDqu6ThqCD5U8I9shJDedV6xmOTEPFCLPT+LofSEHKcezHtg+fpz1a085Whjq+IhsH5Xhy6N0xpU4HWtQUM6NPg2Y9hHt02RtYgeMUArzu8HrUGinIoEieNYcBcyUa7iWzqkmdNO6bjQb3+MifNXsSHdsHFuzcUZRwY+lgXSvT9Cvn3J9dIwLF7LGnrClaeTbGdDDBErlivk1zHQWc6zIyuYTFw1oIS2gYb9WXxHgLpgczeYDlMqxnWZnQZN9Cjxm6oFlQMz5JX7ZDTV9gSqCfbA6irCt2Sh54xhfh93lZMJkPRvCU1YlOqq+SNgFZsKyJfDcARHXdP9ruEG3XpbY1Zj7Jfs574zWw1t6mOzpJQUIkytMfudrNk1Z906Hs2Yg4J6AoIrDSA3SGSD2s1olmHBqVUAgT1L1Bo/aZpIDi2k4lLCtQ9a+5p0+LvTkJZ9L0MRo/WykLoFOJnHL2jACBjVGgkh2yJMcoZYh1SlxsQ4HRTk5qfzW72vfD+a8/AoT6XirfhgYApDonA47arrc4lnYwodIBCAh8Gg7TVfWX0IZyttok1t6Ya+M573bAtfALMybWZlNmuBfZcrMbS9WyHSTrJUt+Z4g01UUMeKrzAZsJCzlrrHviM2cZXcCfLTfqv1YcwlJICBgdbFa5M0Wvz5KCMFmk1Ub5PLqXiTvwNT9f12wyfoY1FtWYYuDm5gjy9LX7a7yBsL5Fx8wJuZhR/5lzjvO2EYYAqV7je7M/kkxwJVNK5J7kMMo07cMpxLHohVA21DNHpCvaydBQjvL1ukrSwbpyFG+CKaf44mg3xAOJ+EL9ciSE3NN9ATtGwiNk8Vejn6t53PX1ZVFM2QiYTZioFTXCLPuL4XLbtWMfsVKBhQ7pe+GViEJ6bvoGHnBVJvywwKRemaq+SrRq5R2LOICRReBDng23duUPNe0ytgPQCrkL9DvVdxkYX3dCfN1TYey4AAh8G6S5httw5tEtSmUg7TEfiTbQHpWxBwSosFi9WgbYyeDvpY7XFhQ2uExJACWopWdAV+VBZtFtCnMQpHd0iomspkGMk7FEidFB5dCnJ75yWDkQZTxuVYqU+kT/EMkvImBFDd5g8THjHP/vYXmLRgsY7USXAXMwHNO3fFVA4j6CpsEHO7soaQwWtYbGGb5NMLZie9p1pbCLC0M4uQuGC/+UMvZ2he0+TiTLjwiC6OfG/7KFNRFvG2EKuNKQhPHjcJ8skckYg56QRCFzGS+e0Ax207/HCPkI3EKEr5R4Z1Vds+OihysKWcwlyymEiAnPj5TjVUM7+Tza1lDd1qw4eVX7qXE/9ikm475WjJLcJkvThg/ajOwHUWJg40y9S/qZ3Mtm/j7oUN3OIEVwwN0fe+xCUQjHZ5b/zNJ+t8LMNECu/HUUotdLt4HCrf1r86QDBZBq47n6vBqvZsRhG2pyxJTrnFtPhBH8AhDEaLdxPnmMcxcFQIqJQHGOcuzv+uEt0I3TRMfPrfYLikQ5kqkh2AIONDWSI6vshkL3Ad/KGmvHQvp4PWhyHZSLKGqSIPVGvxINI3n2q5qwZPyV1REi4U1PBxgTPknQ+3acKcuFD8qk0L5ojAC7B5LnPoLGGpfM3Hm4klo18PHfDNSH921KOqWRkrLxTWaRc1/BZLkqM+aHPDNL6b6jWLg6v06rfXxbuV3hIhJGdOiAJMiRUtZ+5CK+lvm1DCQXxVY4k3S8WV+XmkLEI3Srd9c6pi9oK/WODI9Gh2ZzMyCIQNxVDdbIQvdauRaLNe4/JyuytoqkSHAap2Wc5RoNlCm9jOr21fketXtC38Kjuj86YmsPyjKrWnItRn7wnHgkMJTQNYF9LgsYph1/867Yh7bVGC24q+DQ8pglsx661mb2QZFZgzr1Pd+nVfQKaKXkfbhN0SnMjE1B7Z/pe6FVy3OmP+/pBESzZbfxWwTvtWvbC1U0Kr6H7m57j9/z1XTblFQrNOPQuP/lTLKk7njTCsXLnKQnZBjszIfsFoxs2yX+G7KlM6tOrROk17l33Y3iNeRlOCX9eNaCN7zmJ6TrzaedaVzEEii9Wr1D3hyx8mz9APvcqHJSDqQHew9FjgBXvEfMHsADf4hSPc+uaX6FUSy3W1DTpDysQ99Udi30tz3nXRnRLYKbMp6I9T96vzwikim8d3isakAQrlOMX8YPoea3tHHGH5jcIRRdIu8dh154vbAyg/MdjiVoOervsivTbf1u4RU4+MK7/s8Y8N4+tSb1whJYad/nE1l6vNq6EMLO8QWFVa/oBcCiV6HjhU5ZRZVjhtZArjPK5v/noq5lxuGuvQIjhIEJ/cyHjK493+STBIavaSHvxSPuhZdWqp0FNq7Wv3sdf3zV/ablV3y4rWVsGuhxMOz6oinc5SQyd/7HmBrrrjFI+z5acwcIbRg98Yhddr3rUDjMfFizWpyxs15Z21vukFjAUK0XAFshOyZfAsnHSNtYHCMdWIIlpwqFIsyT7sQ+1BwpyMwUfZVM73Y8jHF3f0vE0pD1O+48284uzY1iShzMTWVTpfeIKYbzMheatZQYsV4zpFWHploehUeR9ndOViehW96jUyBIplXcEblQGRIje8DHovGNLjwMicjjBmr1fEoN8duEJY/nPplQq9L8B9zC+/WQe6m9NtMxm/PbHIbaJ94a+QBRFAtzWQrJlZszGxC6YGtL9nQ6gXtd3ViRmaO/BmUm46tDy8aeDbCXgjPUEXMJVoHNieHKVVru0szGFYLEpcd4BvDkWfLhljQiX94f0i8sbWggw0a2uCr5I82lYg6e5NcoNakKqNX2THpYk5QWCS4/+knVho7t41v2yBGFpiIhTuk9DmQ/at/VGobwvcOZ2px+3NWxVOjkrvVzwTd6NnutsTs+xcVFWk06zRMk6/i72rvCoiDZ9J/guPIIIA4/7BjlmMRufMR3MDNgna/6n+Cy6HuDJ6uigZAOiDC5ivAo/sqtNTZl3WjmefBC8D29b0dwJ9sG9YcAPpdTzmi+FjjijAlOYvrX+FdK3Id1gkpypAa9jbhPEclr0LvxVwnXFSSpw1z0IgIPwyH6JaO9p9ZGOSEtlSnXJWCM5arW3KCV7tjVZqBbPTj6Xbnys/7Xl5iUgaTloXT4UdiLScJRc78D608u81t0thq0xs1fqpmfS+yvu9aeO39r2VGiGSlnma4IipaESH5FCGFnT7u2rshPQK4CFBMby7vmDRbAUEGZptznkpHSh+rrNpaaVt/Oi102vvtzy1+7qC5bzdxx55fLEh5qKHQl8MJZ1LsUcpGKTpckl1TpuAm04eKz0R4DlwbF5YetbSSghVjYiq5pbMPv0ov5OzoyxLRHE97XNh+8Flz6aS1iV0DWdGgQY51sxohOtMzFSELjwvfrT19kobNBcSGyPOPrDQAXGB3eZ8levZJ951FGQqwRU4aESinQYbi1+CTcW78pkYg/u6fV7qZpwkxYUhD7nBAxhLkDkDDj1OuO0833/XyGKs+0y1ORWMPhupPNfUb4i0DpiNAaG6ecvEs2BwqiLBAhOc7BVTyZw77P6Lpxqm5xjfy+WHP06dKOsndmz0liCjv127rwXuD7j2GDuKd1ktNbPxps4CzkYMUpgD0S/3K2PTZPkvP7Ga1HKdgZeU1WElQM8OQxsbMFeUFakL3H2i1GlOK+v4uU7Y1iRh4LHLsAftiuOzqUg6rzZS+jMFqLQj6fYNjuwXbkNBT/0icIA2L5TsOo/8lfhBLCiKuLrVn3htmsQfceXA7W6SM9nkFCyjaGh9byYHag/mrXkHUAu2meh3fvMrlpCLMl6PQvpGi7TPYRDogXDjGgNJNyvd9t+TIcdY5ss0J/xIPiZ/iE4DYBtI05IoOJN+t6qg2Llo09nIFLP8bKt/fNi5NyMU3NJGNa3mvgNu2aO3yaCaQ9Zv4xVMn5BuwS/nJvbbOzdCLLbOyLy1bIRjjvo2Rc5VU55Z0IEShragu96dvUURTVfzV5qRJq239STKPHE0hVZZWIs5oferRjb+NgV421wD2pGVa6P7zT7QE8PFpoCPPpvGArsFpsccgDIGsBdDRR3eJPtK5uxdC2K4CWTYy3BTIL2Fg1wp5aHeqi3Nwse4DHBVqLpfRMUejuLRVkwvF+zdA2oeZz4LmfbimuQiPYzsZ1QezlEQTTdRAf7jezoG4JhQhWyCJRJM1nY7GxFcoyaSqRRwchjTiSwkxtfXel1rmXmrcfg5XtQUm7cu5qzc5VwRrpJZ5KdeRZI3wO1ZRDdLLl1AXw+PikKSKo0EwVFyGW6A2JXR7j0ZlSDR0nOqOQnESsXoSWGTu/tvMc4X3IoxdaIKBAgHvf8lTS0KqXPvr+UXkC8EBU3lHrABM0ux9kdDm3C67hXSyOl8bEvVCXvs5hcKI1wLIC2brI1aEmVB6oytDVhCEJ3Hg4MDAAdGTZBL4LzT9P692/sEcA64+cFqcWSQfAD1+p5nG4M97zIslgWMR/Cdqu2Y7FUdH3T+vSN5xz+6Pb8PPeCFxaF74zEiojFu+qmaEHoSREJZyFfl748MfMGvmoGySYwi1jG8tpknf8ioyqXROPqgBLUY1Nq/wQkABKW3mkI69GOZ3v1OEGAzAAAAAAAAAAH2Fh0B1dkG0tr0x8C6m3b8nq76vYPAzuqrEYPbc18Mf78Ln5Z4JKa98fZg1Mx5gLjRYEKRj7BAlRRRpvhyqbQyH84lKMOtnkaDxoM1KTbMQ/HNZ+yVYnFgFWz27dONiXQRtOCncZbMobnKwcEcV3knZp+hdD/i3V2PyFTsyoqrZ2JLkZjAGoMQi04Jpzrvq0aA62A9VXNeFe7kTwiZE43lFXrfSaKug+uaLDF7eiAZ2ZmFZ1qHTiRJcDfdXoQJRnWdmMYPLtDFc9MqJhI0iTeKJOE7tKUPfo78OxMhXuHWlRDIsfqe+VgafGeBolLoMBFtZL9zIRZ7t3hc+f/dhE+YweqEzMtp9IWfEgPYUQRU3pd/lUO2/VEI8EFf/amdR8gZv1fKmRyIUOUNHdECdNap5Pdw6lVpMlqw+VsBNDLhvFiX82+dAYCDRYbgWgwVo8mIe4McjB4wKRmpXiOu0PFsdrvP4VqNMwubC2mR/T64W/pXKE5C6Ny2ZLTnc9vXqZVdG7bg3tw+F5k79YmOFcgQ+kVFN2Vzm+EgVwRIK15fWFiw6fvTrQzDHryR67CttPMRUXgRRbgph82BYerxUUKtTUhP+KHyg7aVYjE3Az0z2QkqMpk4XztLz+iPNyNdGYg/1JMCy00kmtZkJxlvb7pURbosiFsyHbQm8u6aKuEO8SW0XUdr02jcX+buN/adH1FE3BSTpBMjRz2QsdkP3ZUyViILN8kHe68ttxXhtspMYloRf5/6PjVBkPL0pwOeZxdfhdXh6VS/LOod6Rx3pelGqtpwh06Zg3pDTf/hwBDLh4Zmcy7HdlUQmFvO/xSJEkwzKSmZ6aF6ktuSSkaQMgMeY+L9igEX0+IN4tPxhKB2fYK/b5GM6d1Xd/gI+DOlQ2I7xPJ46cevOK4rjT/ME7QnRR2S/l9aZ2dfd2o8tmHJeMBWnhhOc6r0siUPXNTUEVh+Nj+VEhrvaZS259VncsIDKK1N957kV5tsLQLxUr52h6GTmN1urH7oJkDC1mmHxYUuVwoHYiOPPZOxlx54BGqwk2+OV4/2jzAV1mv6C3B9DSkxqb4gH6he4lzQUFzFeJexnHlo4y7nuuDJWwYjLTe7Ypz45DuqBzbQQIyPeKXxWIHXyIM1oiLwyRDT/CoxrsBTcEA5r+2mY2kjX7bcQoWmQz3iBpcnjYMlcFyS2utQianFMIjXeE82C7Uv3gArg5vBuZ4HeKXJSX5IRBoYNf0FUkq9ICYr6CxfI/v20Sb46jLRFMzDtw7qQW9aCaXRKbePVkgmDSkdx1HxYEUJ+nRCNe1RxYzo8HS6pJz34sL+SGrPXeUo1t9Y77AQUugzMLhIFhXrb+pNh48xuc8Zkl7oTTPE5ccLaLQREAYxwdM8kxVEPaE2simE3WCngTmt0Du4ATLMfL5639ttm/qSVmbY/09k9SOPJ0Yf05HUajvNtsLEnpZN6aVq9RNXS07ubiEO5e+1QmdUw8qvJ5dc/rBXqmKBOWTNfT0uQywIjanhbwwzNVI2f0Xb6MDbltKYCuglx8y/79NGdOTcrN0j6SUMB0VAK0EM/Ri/PbzJcI7NhCuWMVvKBmJzAa82fVRcvJqUCfXYkiv0QZWTd7lJdJHEf7VFOJeZHsZJ5TawAbnBVibkJf4S88c911W84AgWj0XiXrcL4pfJIMI+qaeNmeRkA0IxmD75OJqzRvC33R+UJ5PNfXBrQpJwqlTTGpQi2G6fPlkpw3ac9fVs4L/OXWy2oP8vpAw7bFMDP/dTd0Wlsdzrdx9xbag8sS+0a+9dQFMre+JrXom6+LtmIU2p+QBCCRRC1+ge61bvOHT+oiL1bdy15g5+8w/KuOTSQcTOEHqnLeXEAHLyqdj4BB6DkbLFPOCb1z4u6Y4NPL9s9o3LMT9G2prcdMBv+LTMKKOclItJGT53QmiDEMAA5c6jF5MgZVg7dsOjwQINX/wjCqmrStSk3LRA72200qPLZjCi22QgHWdb30pjSg26djkZvUOr+1UOM5+hv43RSxQ+SuEgzYuxmTUG3cqGXxbE8A94aMQAbUdZJ4iTsP0oJdRWMWgPgITQbl/WnmvOFpSW6Lu6IO9vyPEpwvsVb8efltkmzhPToUb++aeWM1Xi4mXl9kRlE+FQTNu5+MBrGRXjq46axbV335Gew12M8GMePq9sDvkL1wmK2ojCzRRkIrIVF35W3mcRoP4HggLTfyW5JnM/aIoBaE9SelrE176q86eVVGlrosjrdKQKDQ3vNvoUpV/5wiJTiDUiLQDJuCGg5z/Rpuf3QvU9RRhHkcpA0d/rBXgofSaMcIOBKVZx6wIry//+IYBgJyEBolUv9tGVo9dnkS2IHYvEUdxhbRWo694KYZrILGbLd3l7N/L6oeaAQ+elbBvSql6Sl8op6XnTrgTYA5sTTi2MUg1AZZNHRtDNf4it8Kb3R94R+RapSM7nU20SyFDqo5VoDX/7XjlEAxzcRxBzwJWVL3rKvg4uhhBeb3C+sXNy5UyAIwE+Q+oyYJ6ePUp1vF03vXIja1DrDstrESq0YWdDvPpAKonuwuBa6KO6VB1NPl+hzKPcA0ZY6xIKnREK6vZNw85kp37m2eYvO+CBdzCVZRWSo/urEB7V+GchgoH5SQfDZK0siVmh+fKaY7RDokctlW6a4OBeCDkrJKucQYfD3NWRmy3XZCVlInbitu2d7QPjN9hD3Na67QbcIfr860tck+l2mv+FTVK5cpS/S/ARVEnEpuitft6T304YAXjjfX+aidVi5w0RfXMAc4ArZccCX7DsJ46BBpw+86qjwgSirGO4HJWYh9TL2hzAJc8/bv4BhJVpu4VN1cxEHUmBpPapEQtDN7+r/Olo6/vGkuuP6fKAuFmbzxftn7AuxwPT5GZ5mBGj7Y9LnMY9EIJagAdp4pIha6MscpgpxHuzDCNZFY9Iau+3Ic5Gi8pjAENL6oW3irytOZ07rI2e69fkQMwEOsxDNY/PXFgYFgP4/hDvmALhbNd4Dziva2uXJrqMBEYkqE/DFeLSIDu9Wxn8+KrV5x7O+hm79hSQs6IALt2bu0sNZkQsvxNjzs60fVr3utvRv61zTuZARfxRBmu5IjcI9lcfc/H77mRreJ7tf2qWi0IIKTZhk+gcjDD4uGhiUJszCTzcS9qQZMNgGmWkJg6HaE3Wa3KYXDetj1MaG0CpnXg9hKyLZa8oWZiv0VrW4OOR/KLh/bnPGcSiOSI9HpibetRnnQg5WC/g7UcjZFZgPRBP61AZKNDmFGIWY+ZgVGs/Mnz7rpL0Z/fSHntzehGdoEnirYTS3KmWP5w9EvQWVNH22+gOzhXzYuZyf3xxLNxXOAfrWNP27y0ihZDyFhDGewkDx4M88eaAbGnUDM/W2CxoMrypfF2Tj2x8QJstDoLmCq6DyihkPFSclB/sjUJjRDcEYddVQS4T3QdZVz3lN5VOsb+ykXFfJBSWi+gCSpJoN8JmBs70udP8I8jQLhQJlFHUTsl3ZIj12TI5m78u4w7sKd5kVnYfdc71vH/3VQU0hv/vBLuS1JS1dMFA2hFsd2p0xk/AH3PoHLcPj7Xrfy2QD1iyDJbX6ZH0USInVgpPZJ33AjnE1bZZSxwixzynFkli1hMmxnfjO/8eG5bWWEdILWiOD7ZBKEsfL0Q/eaux4ld6T0wB7H5NIsSKM+1Znr7OXE7TYJOisWbjsrz3dXO4i+hodV6IbBkiqIsrtdqyZ/1BZ7G9zHXZjr7JFoO4eZG87HrY/lLmS6vFpuFJ4FxOdx6AfRQzzTJDBQi+5kp9Hu+DVEFo09VKUswXyHCGNSIKVbydzypBAcDVLauwPPIPRWkSkSqwTCwTdxSEkAsB3sLH7A78N9DdZU90dIB3YUBFDstu7FEUkccWuf7SXQT7/c+YNZ0oHyK57AGsi966wsLEkA1XKl2OPCfNYM0i9FrtAExAdBjnoGwz2ES6J1qLPXuNoZNiC+InsHzaalCT2ZQxm9sVe0BJ7xmbB2XP5pxVlv8mJ+NurDSmnaE4XyHbsrAXK0p1/Wg5E7wsExZDNIFJ1YMnONOAs4H1U6pGeD2G14/junq10CMY6rwd9cWZeV62JxveDrtnjYZ6gi5ftPEI6IEL7Imm83UhOtL65Mx9HpAFMna8Y8bdlj7HMISmmiVVZdnm76WsXZUxAmC8lMMWWSt8YQLdrMw/CUHIcnJc9ArASYl3HD2Tzj4AQF9PtsiK26QCe21KZtquydt3moEuXHg05ht7ca7QhZT/yHcKypjI2Od6QDC7y6fpjJM4wjhPjxQVuy6LJzuxXfQnb2+t1wMgQBbvvOxzJYH2aRz9/L2x3TKp8QwXzNinh0AI9F8WCxmPlFA7zGeUlXNsxoTOwF+Xv8AYEEQKDEfqmhgM1c6gTWA2OquFd2uZh1j3LV+JC+4JI6LsEp+7JFYhrQbpdSPHpJS6Lr2CL5OgvmrWzatj/fdzFbu+RvOnWNFByTvD8WmRb1u7s/nFcBcfKNGSLySppoWu2esUpX8EpEYNjHIlDYjqqJy3rS7SbBc2zAEzm+kWUYSUR5a4Zk/A185m5elBhkljinlTRlEuGAWk2cN77ZabWW8KiThVRaGwm4tm6pVZJ8nmqPe4V3YWrnmjKf0aUIWGv6ipV4lIW9b+1gJ9Q34p++imXsRjpWEbOLLD5HrMKx9uVW62l2/4TYk6UomyzVDUf78k2raY8BfwHqSR9rhjLCF5yUdoX0CWNEtg3QD8w9oXWn/SQf6dRDGMamT3bjESe+ARqZcojgUpQPvE4NKlvNpwssAQmuoLF0OzauW32UBg1jQoWr+s1jsREYF9G6qXcTcq8n4vJsdNXr8CXkp5ZPxG9xUu7Z83JXT2v+8SkbR+GSunGkHVsp+EJCgTlbn3kwAyyZXMK7ju8KH/fjaH6oeUCv/eUl18PHmPmqhrLO2QH7Y9tu36DsxSLm8HFgR2KDHDzsNT15wdkjVbw0aQVk33l4iFFaY2k+LfnKnLwX2DkcFWOalxBCcCOKgP6JM8OTKiMLtEXDUDO5cezKdzx003pD3kqF8nMu8eJKPh46rTk79FMN6qRu/LkK+kEf26eN/w0XxXYPF/tjqUegW4GkUITjjSYDsaJ4kKzFag1H/ag10i6D2LNFssavwBajCh6QFVkCujIbyf3YLOcnup2LeX4npl49YfOiIF94LMU7kV3xxHcFBQcgoReACzwlM3CLcCMLIeZAOM1/wi24LwGTuMZXrv2hpDGhsZ9YcQLJaf8mEJgpeJE+tDMYdCYngHWaAUiKJNY9U0ESRm99GvCo+IFJyQ+NyHlKPyDtTfg4fAPPu7oLweEZpSqVqgkBMhdEVlF0TM/pl/O2mlC+SobZYeRjTcJQunyqnZh5wXW4nh+dtyLai2xw2FmV8Za79OUK4TF85Qm8Umyz8VBp7RCYK0r1EiyioE1z2S56qGHGE9z/SGPrEKfdnnBD5EcEdWmoab+gGVm/AtGZBKXXcElC9pZckT4P1rJ6rC2qcryhiY1d+DX94qMctSafA1wSzVD5dXU5gv/orgUegtc4WgwXs3JhSDAwAHA3Mc764e/TgpyCg67TjJUtsaOqwumI/sG4V28Xa3tguIQb8SbDBmNxbsHyh+vOh+mEfqJJ2bI/pUTmBpgr/BtqcSgZ+0u7RoEkma0PaHcWdyuMroW1IuJaGjlJdC9FniX/hBeCVWI+f4V6Cdkyz1uXZFA2s23E7N1sarDFCYc4T9ThBgMwAAAAAAAAABt1WBAYsTvyELmKY3BRGdFfxqiQbWJhKHlk6SQE9z4FT8MLF6TECKOa4sB3HNG+eQoRTQqJXoUCVnba2NMuA8aoLh/uJ6WzN//CMbcBOAWWp/PhQr2TSPDObeyqciQal8gNLLHGxg+EXlnArl1ZEIQJ3BJcSxLT/dhYb59kcIPCaqLttCpUAd9JnmtKphenCSsafC8vA6IqoqaUO0JtVqUJ9KdW2Q4g0hvWfKkLqzAUehK9PBUntEsaklgOZm+F6/JHsVWtloZOSWKDZoaaGanETflpFMAEd30MiNgi/9bqngRbtpubV4lamBrEIS/ZUm1/fn/2BQxwTcj6pPRblXK8aibCh8aF7ZYBniE1D3qYciJD5+4yN67uRpR67TiiAJjMeQ/GN7uJYHYtf6j09eMvQHJii+qSu6Reurh11E2TIaSWXjSmSBTnksl7l0FlQyHOTyR1oTiq6vaRopMVarKVTGlUj2Lh4UR4joVUNZEfSLxhrBfEd2XT1SENm+iFJ2xAHRiXiHiZmULJDlRqzvQSFXp6oRmUJnXKISb7RYAPSc18F+bA2oBZ50QG+3qy0Q54tuwFn4fdwCbj0ocjcq+gcVVRq01HHnaHAMIa4h0sBjxclonx629pZzIxm4E75VSqVxCb1May0u8GGuKQfDNHwdZ5p7ibiSwIU0WoxBFc7v1E9wVtvMAQG6W+d42/ooNX+bQFqool+ppHnU8ybD8JplBYdsEsuzBbMP4/vly+jnjsHoomAgN28YlpH13Gcb8biuYz6nTFY4Jy4pZBOY9W5sOdVg8hck3Ltevc7DAeLeHQCevRe2Qq5+DVQPNjnPfNvpmOb8ab8Z6g3fQPzikvM5Jex7OpdZMd7r1fYmoY08Dh6LIDWEsnblJS2U6U3kjDTeeAV0RCY7YKoHvS++geKEMbaHZ50iHrVzxMK6xAzSgP53sKgQLYSW1yzXfHGOJdy7+c2FV+ZcQpMEKsBzTL3LDyFF9jJbqPvj7qWO0cJNOvadzeNCRFKmcW+xuQjKr8FQpZbSjjDJNOw4SWyM8Q+W+8LOCpr1WVZLigq+aHr69VKOKKssL2QWp3BaAiRWxSWYYb7/9dGHBOBv7tVT82E1ZQ6bgMKbScDZGJVNtgKkQc7gZ7afet6xwYeGO6Hab+HVqy8gVVqEiFNwuOVBDTwGcItEFkT3eT2adVgYiInerIZkPOdueLxJZHm1NCOykWb4Fi4unn7cLVmRkp2g06nk9JQcqdMl8QhDfQq6nsjBjRsISd1vDVIExdIJ1k6NwMLyIk0di+NmUujx+oAeyuLhr5ewHXtvQqgpEf1gJlT8zBn3MoM7dR0GbCemlDzjoptlADn8Z9BJ1K1Yx7oHPTM6X2vQ7wxr4ZsICrklSmxh0eOcuS3+/kzWJgap33lhwSWO+f5wwlSsdP/MJkX6/syNrQtA0lzUuA5ggR8lrds+pPPV53+tgQEvC3e2ZT2GOZNIXP73nv7GNXFxfOfibpQw3dQepNOYdZlaRPpVVIz5SU9peAZtlUXa9ww+QSfatppN5vLfHS0vtOdjz/RufVwehxfWO+PMNOCLef/qLldk4PG4VNS0bhRc2zvz5+uttYnoS2+rqnuqzlPaAI0zfuXWHDmK3tS5TfF1/De+Z3zXwZJFGZxeOo+Vi3ivyT1fIYQLXsiz/EfjqQM3ZOH0PmZlpyD3+w19g32gJ72uy2jM9WCMfRmFcZ5DNBw/ljI4YvEM0OCzZlH2DqiC5dhb0rDKFw3g1z+/Ku3bZswpEohaZ6dGlksfUsbdRJf2K/XAWd54hy4cvEukfAaK4ezitr3z+uqbQy5voTNR1wnnLHTiSElV313vI1FLkqD1R7OjpbcJg4Mtsj94tR8Egl+ZRTEhqzx+rg96F4PQ8kYEhvM9WNMyapP+2HoxB8hOGvnRavmQ9ln1sgOb/4ho1Wmu3wckR+i9LMm2G9kruP+OtpgqzB1/vnZlXIwNvSbzddGXlqSP29QsV65Pb9MZrJupuRsFhvcp3ZUhuYr4OQwZF+oz+h1A2+qHPhUsdcywkb5vhuOu5GQMXPmBEHLbAJcwWojA+ilZ3OhBLLP1J/krRbr2rOIuz54dpdaCOMMkGk3LSPjSkFCBRv4dLAqIVo2NXjznASRA7va+LO+oPn9zztWa4iLfLYBVPE45BGoWUK0Qjm7v3+/jBmwo2hPvn44wnyC1xqia8CygnS0OBAK7OuICgSvmQbnX3lf0vPdkL6oKDhESrh6CWwn+hrv33pZXdSBPoxJ0AXu5Xyo8wHs4TR2AZxlKczss0f8Xv1K5rlNeSFTU+toPvDbRf+RB5EgxQz37t800Kqqu9d+YP8I+oKzAEkxap8Q9B9XB2Be4+m0DyhqJ0PYeF1lQ6qxfwqEJoR+A+u0EhRudMzAmvpNSNGK/OJ29jW/4iZ7PzYcYADhrJhwu8f1/nLr3QrqbSa2ObvjFtPtD9VX4pzNCpqUe5CL1MLzAEw0IY4Hb4gIDKvIsvuoA7kBDvr2SDZ/YTbfTQRpgwuSN8FN8zN4rENjGbpC1+RICByXzW09zsPOkaa+vMbZylPAVdbTd/2TYxGQiEaEVQkBI0LMsyYp7rwDgqHpn4dDphZW7MF9zW9GlpvuKosn9k1wm6zo1/6PrL9l+cui3HqSt8RPP4RLQaN0+OR1DkujwWOiK8vUnSdeGe59JUoC1TVJzzFclWQJOXYvto/GwePrjkzpBEW0q6UwUQ4H7HBi+MNt7zvjdZs4O4Ugf4ss/GYoUFzf2G3xl4hQDpr/EWRZScJ+bzAlQBDSBm34znQDKMRqpQkRcYYaoNdcGpBMYxNsN9yeqznTjzsQCwJ/TPvpL3Y4wUfWvRlNvsKLf3I74AFaR+XowlneZUVBNzGIuetUQ9JXpaZe8q0RweNzBswWswaM8Qhoo0pr0bZ/uokCSQL3iBGhT81A5NuZV6VUSAcd4JvtBjERiu9pwyLwRsaF0JtilIyjsxqNWi6p0qIn9yvbz9vtljPIH0rE0R1dojfeVLBCTVas3DvJ6xHY3EsLg9CIwqTEBWK+yOAYnTRCM8vMeruGunNY64ZzxtKvb5+3aqN02kdDH+4R747bVy4ROVjXWhywqxqYV57gs1j1QMLmhnTacZnRAhmMVjIkshwXERM5VwgVJ7OLdYzIDssjnyGT9gj8AS+u3EHxVg2wDx2HmfoGkAZpXqquU2PgNVIE2xKJ6irKkbGSW7ICALLh5FERcVhcAMrRru92eUL2KfruqWuDExJASHUO3XCv6XfaFFjdKFrewxBZJ/D01ztVimJ/NsD2pHDX1dmQ6NexU3gsetFkQzsoyvVjjH6bw4gD3Vnld8w7Wf6dR7vgZmv/PRWS6BfP3bk7D+XGiXvU9b9UHNwlabDrerAjkxS7Pn3BKIPpdPFwMRsp/WpnwmR3ZIpQ2466mpgTIlRbUUc4RRyxObB3HpbZYjNowjOBp/tuj6B3KLnh/vYtIjYTW8UCH5BNKqZGp+WGdVhvN15p7Xx5RFSE1B3/nhmGnI9EKA2TPQOUZ2ZSvWJ+iM5q6dQvO2xVXT/O3nkjxup5BwRQdIhIXFaBq25gZTLJ8lCal5sEIYNNhwOFJzdjr1Uob7pNlk3jACG5d/4+WE+m/QjaQCMJx1APWZxEnWdzgX6FOTDgFrYchToJiPM9fuGQ7T+ilsYwxKeSoqB4JqOAHs3Zvu1HwpDgb2EBtCtkCbmhg53F/ISi780yrOa67LtvxpMNJjCJ3uvqn7qKALwkrht2tomrIvmD8J2fj56a8jC98+f5MLCIDkjB8qLEBBXq0oWxkC4crtEGEDelnCl/WUbXE8aHQRldyMLrt48tOJK/h/0ZIZ0MFlbLAYqPDiMYNDvWI4L/kC5okEGfdvEIXymog9SDbvMTitEKe25BFCm9I/cTozyNlQjnuGTshL87jQHMr6URqExKR4r4liwRyYcIRxMonN44iUypW+Ktbb4nacG0ldJID2TV4NgHm4ozOyU/mlVC9FquP2siLKIikIV9CSblbcK91IyFGQFTRgA5Q4TNb6JXmI94ybk+wAVkc2RrCdotuf0ge4urFnn5X+CQplBc1096bzO8b+vsPycPswkfqlJFDddNfl+UMku/Ych+C+fqgLutsXe2efYHDLWqWno7+/NfLCmoR1NA5kvwi44+kbrQqPq8bBGAtnfn4gi3GXvoC1eMKRPtxMPURICegcZ+zscZru/TyqLaxlNW1E3spMxeVfO4kifFEKnkxZRH8ngRpDpFqRQ5Di5gqW/OR+X3QxpW7DZUVPeRkWGYubv47w2IHT82Tp3XfV9JowxV4EHGBm3s//LbUAxgwXwRTKl6+WcOjb2e1zrQP5r2LDR8BMtexp9UJ9PSWvkxm6IrBDzCFi0C+nvnKb1HFjcZjZj6xE8hstQUD7tcsIFTHxjh2hXvZU30simXrF9rkVflDGzYhV5eVcHxofoQIUIN50tJ9KVtWc+uldo6OO+8EbEMt5aoexUCnzFr6Vc2TMIgqMCn7m74rPXWh0y0WPgsqbqJ+SgInOEIUPEF4E65LmttyXO+6L3lkc/sJ+ZC+95a0FihKiggXhb3WwkoVhfO3nVieHs3UTfMs95beWK4Ix74OLTs+7zh1IbvcqZ6fa2o3irKJFeDWfQps8m/LAmNy3cVFkuXA6e5jI1o1BRofB0Nm46/CQzGQhsAyqdsOYzKtjvbOC2AAw8z3HJf0C248qvny5yLyHfpHypF2jTxhFTadp2FCtDhDz/pvTzb6A2bxwfr5tpdOVcp7304BrflXJ6GoMAkeOiplnRt0PnNpTP/tp10oX/Vltzd6hhju4isUMrTsrMjZWSZx6A0yhjZ3BSvU9811wEvODvPpfgGjgnCz62FHQpPOtPQvb1UfIw2y/GVVNMCGD68uEZklgVBupMLzTwKIyTHfdgM6aLgK9RfU/PO81Xy4NEJtDys/0Sn+J5K1LY2j7f4OhGlS3Nao9cxVfzCxzkoXbdmS9zbpOu+7PMo0d+l60Lv8UCPqWzRP78ax5pCtEAXCqfHwiwjj4UzYHKex82ySxjoOgU5NYe+U+y/LuS/ag14SECNWXqk4XAc0XWFTA8V4DGnnUk/Xjrw8f48OFINV1PTNoOaiTJlJcqEB3d44Fz4PwI/TR9nfJPe9Qp13A0zabwT92V3HiEJVFb63MLs67prCAI9Amu/D++H0KE5BcUHeE7P2h+q+E0PuErE/gwZ74363iAm3AxpoKHTHJ325n764WyrkyPrYNDofE7HpVcC5/787uCL9zkrRp3kfM+AjNBTKAWux9dpahtUmDjwD6dfgaAw4eqakHZ7SIWMPeduFvBSFlVYEa3JyXx3QQq0sXmYr36xPSJcHnAKdJKLs0yxjQhwJqxcxERBAFE30BUn1REJQQ89i9rnhoudQP3Dbjt+RwX65+eNI8ZbImmesZVrtgwGvR2VusdyohBrr6+Um1Gi81eRsawZp7qRPNhKj4IS/fUQPKksZoZoR/9+SuGNYyprTVSb+VK8QQ4YdmhI+2S9XTHxrN9sueJnofCAm1wpRN15xgwMAB4jf0PJae8wWp58xpSwOSOn7wGamyxQSY8EDFKDmUcSTdRGckTBk4JM0iVmfUy843jkGpGFqr6DrB3t5qc+26ljN/t1PkIF0kOtWaTgLMOCvzQDOcta9H3Nc3GqtllTcYKIE5z69WU/urZJcNQAp2IrRGvlJ9ebmnCIS4BbQOCB1/U4QYDMAAAAAAAAAAUSVGAH75+H3GJhTDFfNOGGd2FqKfk2UPgMz+BPC25w2Bt8jn73HVK9Rc+p8GFp2xxgIRbI6krNRynPGg2crAkHvIpgFtMq8L5Z3UW02doMzBJDsYVTx4maBCKMA9EIK9uwe2Ht43RciJSyn4Y+B/xvpEZvG5a1m07HwJ74fXmUfMIyE1ghB1Ut1CFF2pFDFrcurbVxd/PshbUz0PnW2Ac4C5WOgKT5V7YkIWv5bHYQoEl6gEjhwgaTTzXPl2v5nQBpQ6e/+zyC7E6CgI4Avtn+Qudh9uC5/It0ApvlHqLLaY6d62Jii2SI6KUwb7Bkv6z1+vOZXwSPMvl7YOyGBnTCp1webfW2+RYD6TThF4CaGYlGfrlkjC/xSMyGsmvLBP1UWJU2ZMCQoR1TmjHXkWnxHMeXFsBQMtgBpETMU1Lyt9S1CNjJtlAO+0KDAAa9qqLbta5BDJd1oa9aUyPOxasaPjYmDAILFKc1UAf3LUU4B57+bTIKo0diUoJsf4ypJlueXjgdAK9iYJ65J+0/I1vG18BgtjAX2B1vmdByTMhcQju1SrodIGuVBQadaApy6suRxdzCi+4FhCqwyCO5FgQBuSCaiCVB+vP+mAToqjE8oUh+aK0SFByFuOcxv5AGR79ZI0A9Nha5Lz9SGRcSUEqH9eCgQ3JJwOTP5lpfD4sEIPgFRlPLJ42p3PuH3Mw+Y3YhUXq6jqEfPTokdsfAnFGGJLCUJOWd/etxeHeQsNuRenFPDObQ1PVa1MCZczflebtTuLPHb0ZnOUIS4rTtZYZyijrLdcq8B3X4oEW6u/PQxT3YJJ2ubT3fACGR5nh+y8tA9tiZAV3GgkI+RljMJlqujkozmterXH9SzEqVCkFUnJf16nzdddks3KFhYA/LcgwmuqXxG2IJ9OZs3SuwvO8t9WSW1pktFsvKcO73CdtsmHxU4nWCbDHjS5ntc9AdZSVsne4bKVfzZhxDSDkrxPQMi9z10+pHuFHvsgOHX3eA1PWUp7Sr5dcVk705e+6eS6FGfL6zcJ88XhPtA4nrR/OHAFA/db8pUIhtqZ1e1N6gwIH2Z577jee4WNMMLiZSiGcnYE7+IXz8EJtFh8rmPAv/ErZtWLwdXO33i+WSD7XutgGS1LhbXH350EUIq4NiWx3gxV3EY1C71Mq7AOUTarDQj7fxzEo1pC7fZZqxfnIFnWdGfblO3qH5TF8nLYOvZ5u90E3Q8PbqupXmWbX1OlBM5ghwXAUxNM/l76l7Y5xhM3iktFcLudCLAWvy/8Nj91dCheOai96XtrHvfCi2+qMhOGxDy7WHjm4lXZewt09K9bQhLt3IQZGN8TsLxyydHsLmOpaQVeDF/HJdy7BI67LV6cum3y/zkjmK8+ig60OSeAmeNqFtBiCLEw5xVvhkUAiIwBGMRz4nknFkT6z5MTk8u1YfpB/A0lj7gQTBbz2XonRVVdeLs7fYt+5sm4J7LBiOhljHK9EyFSIMf2EwNzHHfVUfHPxlPcKcTKHuL4lIjEbbXwiowlE8s0ucyLv+J2EdieZg5ASihyVHhdu+QGfyRQ5L92Uzq6lNyraZFzhoE2pnoLzDohB25mEQRcjEv/oM4Rg03ai+BK/NKKeCDnEvSZj+2A1var7e92pmUmlEXOi/+4tqFwJ9YbBKcfhed+Q+nhh5KSpb46xsh0e/mLJEKNa103IExYK0j/noYl4HzAxxcEAHpRWbH1LZ3/AeO4nA5KuPtYb+uEJiOcpBshdds4cLhzsKvMF+UsUQCMFvOL8cmn26wCOQmWKtvImb97eKU9t18RJF9h6jiH4zdm4t/UuFhW08O+8C19ekziCqhiwQ9rxlT/Dit20eq7ftrCmU25JvngVYxQFiVVURi7IBC7JxadI5gfSYZjAbiFw+rN0DOIKS4AcSuMY2YkkPqAzoK4eaqzrBlN8yPQthQlD475UNa0hhz8RYPbEtgaz7MtYcO087f0QpEa67/9phG2liGqk0M91Ay3ztj2OlV8uxMADv7R4grJ0HBmxKnor4OATeKgMVJorbITtzRXHnQXUQNllGh3IgqSpCJam0/kxk8WBEseTNg1RX8j4kcq1sTsL8aDOnyMWE6yzvqonvXDv1vbsdADNaGxFmd7kj/+zvT2AjqBDWqRwg34qLmkYs3a8wqaI6jixZS0FsF2PhbUJC/vQtd5QLJ3QFpnN5GCYhgAnfopv+xEVTOJUSkKjUXm/MObrEN6WowkhM7FNyPblvM+5cRRQrdfl+FW2vgtMu47q415Kj72i8hGXy7XSjKl53AIqtSbjGPv/js4JWc7zYWMnMPCPUtlRAIWyvfr/laBFojctH8iDxAuOikglufNEcfcKY1BUPAcCYa95c/O5S4POYHktM3rI6iQgTJUdYLsDE2dL4vVqgvjGV9pnJvCohHwaW5kWifxShp5FPcsI9Yi/g09VgqPtivrLTZIFCUn9FfB/4o+3+dzkHVFimQFPKS+AxbHC4q+pYODSwiH3S0eY/OSlV/mvEIPtUvJIbqYhVHpOR0ivpt+iZu3bb4OdifQUkx1QslV5au/N67xOh1e5/oMgC2S7YYJPB+tcuJcyKzc4NR+iEziStlKTfnLNOc/unORjwDZXr1T23W7/D+gzX+/e7RIWJVw4y9mVu0KuqVAAmTKrZJbUcddtvNQ7NWsl9LwRuM5rIbMuyx1jzlpscFOSAUlIpT/JfYvt7CTND0+FyiZWttk3rK3iKUQ6wXHTo3+bpKViqT0/YSJJtF9uIqK62P2iJs45MzCX4Apy7Gnw7RGM9V9ZOUFFQ4vlkVnnokeJw2pV1B/TD5zJqe9lz9EK7Y1ysxsdDf/S2/B1gx9pJiewTvc+dp65Jz6f1MO3AtMdA+QQzdpnhwT1wBO8fPOjeSZq6I4n3XlkGD/v913t7gJn7AvxJluLfZmh/PO4qlU37SmpoKLd7nexLcLzcAVji2gjRBpLnyahZhF/DxBkSGlRchyZpLyN+7oPCQM/SwR3IVJGHmZg3tR9z7hGtoGhCuTsDFLGtghy0ApLbM0/Cbeo7sQI9ogsasrYlp7cHfjOuNzjqFvTsoAEm2urjKaNqul+iO9WyzCkp71AD48+gv3YEXq3Y4W7tgxwou1QGgWfIBKemA5wU87VQzLrmtsZCcrAFM19C71xw4tuyG9Az8Sacxz/CRtYeXNEofS1PcpYs2gBO55bm+wspqP+G1O1w2VHtQniQ4UXbJ2y1rVWZI7FnxAPDAHSRJw7tfRo+F8x/wxRQ2K6yk/eSdcxLeXaxakpobxzDrLy8EefQ52LPt7FF1m/qhL+UDExLSICeDFMkst5WVvoNvns80xzBFrKEV2dlaEvaxdL7ynkEu2XPLgFuy65QoBJe9lSwpaADmWAeBCXgaBNwrvIjq1R5flQFeOOJoDIZoIkgbilCj3xigTJfUS591V7wVmpF4LlQwTC977vn6OUsccw0RziOBUluXtEH0Xxah68PbbUjaAvup1cQgtDo+kcgBEkZz8XBkUkQYwjHA5TvELQO+fwJ601RmwzTcraVeHivdOcO/Fw9O0mYpkMFRDIARuUqZmCB3v5VXTkQJxDKvV31/lZ8iqkP3L7OdBEmAnCO9x2xjr4gDUs/sSKuZA1qzqtAyP5j+gfRA2nr9o6/koufzD9gcQP89DK56dQh4J6CsqL2wtBGwIUHYipAAGMfeplw62z4GeZh0ia8Jm0pSBSTXG9PGVuCkjwNpMw4ucuQwqin78GSfiYZ4TB7XZPLXFa+uCl8qibj3iZX3WqTA45OhXHEe4vHK0oRjpq/TVj+cmChoNTynFfNWSibz8btaoReasizweS1V7D3YxxZZ3Ogxdn3CWs7OGoB4gbM28UvWCQMj/BvbLtkM8srmW1hQ6P9wFsKt9pxU6Y3OBtPZZHdFsZr7x1uro/+V7c1fffP0zfhnBrEEj3qRzkeV6Gxl+yQV8TqnVdXP+Mm0eyfatbIoWyIpBJbaLOQaLsWI3yvLExKkOA6EdzxeZz3au2c6Z9IxeoFgXO1+qWlzv4ojlaz4u8XErgjVTRSh1kUn1V5j4122WUpO50qfp0yHStaYWDJ1PoqThxW09Yal/wlcuXL9sv7qNv6nsavpRgnPdD6jFx/du4og3qnXYoe4raWjUMOUom1EYpdbt6DThmqBNhft19JPNSAGRuGSu223LBM75HABmrY3UHxNlHsbh3a7QoC971Etxe3BYZARdSgsbPMvLChkndGD7Uf0F7lB+97eXnjkYABcyVXmYDx0N7srN/9X+c14P5OnT6uqluKe//+v724fJnM6/x5+doyomU+iVGpDdfNGHMx028HddMgBbF9KZXSa/TgBO4kUbOzKkShyId6wiSzhbfBjj3sus4us7GgesNN3ipBRtx92AGhcGsmiHvBkU6XFz1eQ8rj2sNOgsVtxrwXKysYXk04NLreNjLnGK8J4cP50ofIWDIVD7yNuKouNA/ELz8yuzsuPryp+/Nl+uOwnkRCrLbde9giJo8WUnufTmq+aFK5cg109HPN6+11VFJqcQRGR0OnpczlOJPgFQCTAN64AxTkto7PyFPngWMRQk+KldkC7QDwfbUuUSRkQODs9pmA6bJVrTgDy/WjgemwIfuza+HAFLwjlcQkI4urTyKV4t+jTaKmOY4c8JhG5GUqzG9l5dFaqpTK3q/BJNm7Dn3OiNwVQBQkFBvyRYZ/f/UTcHPAdneHSJdM3YWjvLXNAiGz5jVdTDqgu43Hs2vt3WhLcvVoqfcVkvYrDmTvDnIBxkY9vGdBQ4V1qXIlnpEF5FSjedMNVe6bDB4MLHQQEjao6wpRriHku1h3X75/Vak4JRH3VPjMNj0n4QM4UE6NdKzUks2Hv299gIsARLLMUfQPN3UyRofFI0WdXhqKgdedg9l1Pnv73gnKwYIHD5sbENJ11ZnhM3gk3uS3OJuG7n22+s6kH+faGqoP7Csqe+vfbWOnSi/jRIFM3wH/eMMmCrCCKKakfPcjK5fLP7BPTKcMzg8RxO8RvHMwLMqUbgnoTkLxbwB75wz2CyBU8d2pUTvvANOBxP8Q8VUl1TFIBBiS2h/7wX0S8MiSis0roBBreHWOXHxuyB/b/WAUuy6qkC65RWBnf0uJqKJZDr0BfFtX1ngW6opiPwVoewtufUYuXATMMMrLb7i28N85F7YbXNUedsRxaGnKnewyLBqCQI/1Zt5eF/5t0SYCq6TNXoczoZCnaLsAVQsYJvwM9HkSeizr7nJxLMeWKcNPkzZmnwK83/1U7fJeFLTIu0/NJYsJJOQKLFbYSWVyQvFMcrXaebf3REdOaKurh4UNpvbOIXICI+8g5Th5Ob7z4Tci6aHP7+slqcnYShDYFfWjBc3xbfYvcJp0VH3RmUK5sPIpkN4A9notkJzAIiquCQ7FgdH8U4hooLxw0rzHCJIaMMRc3C5Xp+961vlchbO6GdKxDo98CJ59xEXchLINKHXzMcB/vUVJkqTu5goPMFnbOuV1zL4e7HlwC0B4ocN6wYjUA2Y3yxdeZehlNN2PnBnis8hi+SOW4mgmEK61g3YMDAAfUhnSyNDCo1G2TFD9Tz5FoWuAcwmEtQv6qu8MzC1tYZ1bDp4h+GOVZfxYHKNTE+bY2BMrH3COBFDFzAQX/J1PVIrGgcSn35u4QJV1M9BjTZUTVTMQXqMdPgPPwzXgiUaH/pJUahnPKcH2znchCj2UdyCUB1Nhb3pWv3Rtxncig1v1OEGAzAAAAAAAAAAGHBSsB/tg9JsRWI1HClRQJJuOMht9DfXHRG6XJaXZ8GHLTQFD2jilesC+7zGtznR9wBqTW+qO/TrPHlgTFDAHseeTcjT5dkCt8jCMq6396Z4agI0Dqj81YzfuYVnE5jUCdFxrHFYsHW3G8PgvVK2VShsW0vbMtW80wG3dHKbPsIBJ+wleyVPf8AlZXrVrucUpQg3rpd4LD2uagL+8aKGFIW7YHpjQQFW55rMuv96ujdyXJz6G/gFB2tRLJkaeFoBsc2MCThPaotSaeBgdQ4AtoziTanq3mEUC9uoQ57EPe7E+7jjA37pdmpYKkx/0w0tL+wg0ijSSBNcRSErHZSa2jjC6JeJKbahMfGHyUPXcDw9Z+DOcKODMw6/OG/p/X609bpt0S+BYYpthXpxUYY/5/MxLD4Z6UE2t7Zl0xfzoTD6kK1j4pU/oWSJ9VDVsIlzbOEg4nJkR5x/NHuP7GUvyLUEH7+zkhDave/s3DfTdJyVparaEEL3p3AalWyjcjP407QQhGfYkmViSUgOY7DjfYB5hhtXXSz3Tb8af/lkWMpxonyBvk9QSfKHQvx9Cv6RWgUa+FYxCBjqqcpKK3MBJrIN/azXX2IpAWKkQvtaibAvUfukfYBlVHU68f9LgODqKmmc5pefmJzq1nE2EotgwbXnvA/ch48VQyxn80dJ0T5nVrmVrrQy2c/p/EGdJ8dVK2akEOydyPxmyfqs4XsPkYK/Y5vKNawGEAAoMG4dkH707uVd6n7baBYgS2piCayLZnyZw8QPAzUkx7fTpQbk339aCgDI8bR0KB7ab/v3Oe6SUe1Tuf/5kyb85/9pqySzlbmApjm01SbwwJkSL1cI/wXdaGUSjOrjREvY9GyuJjfvEDNxr6D0AcMWBVG0JlfYy7ZA5PEjsQJW3J+Sqnd/qkg+3KWrsSJGYrBxZ/Fh+we4rONJWaacPyNyVLXCJ9MbcdCqbmi2gpaI92QTV2nOLtFaU2FKkOE2Pqcog94vIkYYZrrZkcwA+wH2kq8IJe6k0B54toipybWsTADQ+wJBwfLcT7y4WDMbIiaCJLWO6g1hDU6iqS3ed/ANTEvDeIOxSREagqp3uSzGvl7P3S7cTY8StUU3TC8QxNbHThDthbt24Ps0gYBfcsy8pEunsmtNkZ2BLZGtuY7JZwGg28GcxwlzYF8zkKjTJcDOEjUaJTkP/pSyrcU3DBikmi1JmFn7o8KexfoTth2EpkO0fmpLaEyTm0HixO7Eh37tWSfChRP+1ZILPkP5Ug2qo93ynsTmTCH2/RdWwVyFMlMirlXCmtglpUadsx9I3Z4/UtnY3eDtkMjiuXwpj0YrHllc/hqi9h4m7TyXI0BvPo07Uj4V+9h6e95SmSf5It2pApKYKitEbVMnFJGhiHcMhyx1uiZgn5lNP0Em+5hthDKA4dmG+IuFOYkGrOlFShfdelG2/nNkZd5JU1Qw9y4CMnb3XTDCHSbjMH2FdwqTV6Slqc9y/3iFsZ9LMZw6XxQZBgStNRtRFaFMQPzU7F90moMgLmZPqyGV78TXxNiOIYJjDrH3zp/DUIsMZ1qba4b+RjquwXSdxSom1mInk8kaYYUIJyJez40UuhjVJ6DCbxf8ea4D8EhmPh7M3Dr7rxKGomD/BTuSK2B0fMZEWhEWuet40rKSqFoAXPHDH2kCPHutO4/0Epo26i/1lznyFodVqqyiQIgw/BwWhL9jLrPcDfaXzXuuX4UVEr18TP6cPmJ9PK+DrHQobBANxGPmVmiyECV+vlrujk9R6df8Z4Ky7adL9r7IEX+4BLHuDaJAbNBF33l+vRzs5aTNpt3LSdDEkHiIRfeTDGa8SWRwlNHFUyCaulTVZmvJN54VVunmnF+j6yTy5QgUH3U5yV5MGWh0ZiARPPLB4uz01Ye9LaAKkoI2pWAhdignQGff5o8tlMCXICYCa3yJ003nw9Z3Muz0sr5RuIjlwA28li+EpeLrWgen/kxy6Dqba3MeHYRufBdzsJV3BizY7X1MZCAmGjVyso5/cRZpWJT+wrytwurWoMYo73lvHFrMXATy+nIiP6CU30W4GU2kI1LnCDqoRJD3Nw1FJmtA7I9LiMF+W72nnI4igSWEQ2PQJm6+joAoqt2In5ibaH4xwKgE8zym+lbQOEEMTm3HmXXW1Qf51g8hiv3F5MXa0qypEE+lFXJTReSERx1DBpA4Ui1JUpyUdNaxKy0FPG7o59i7nW4pF3VHqkQLrQjXmngsZoQFZE9vdBjEuIxrN2jGYGPIgaeOP04jg7X4NADynqX1rq+H2A1o0kuMZ+bKMfkjOk+/eHnqZxECQAb6pg4sN3ZrFEvasaO/EU0+L6IksdRozvx5GOmvEfpZhAVsyz0RGRCcmqp+h7FJE5MC/oNIphVMRQK4+2j+jlRxGPi34QanWlmgiqAWnkOwq5bceKWGnHbj5Ct13AkCWTne9gLYUPwNoCzRhOCN5lwgPK7DZzXfUem//p3pPzAQc0UbeHbmmfIf4WSs29dY2A355vQzgynqYB+If9vZclq8m+rBjnQuo2iqMYMHmGlQ3FjwGSG6vcQu6IUFcSXmu5HDx3LqYt40CYgkPuyKQA2kvztwMq8k3hzs3P/vsk5894ll3kEZQBRZCS3WTzax3qIDgrSkO/X9KP7nfWKo6nM77F/lYmTwuRYgT6lt/HMPl1ku15nYEjGitDeN4WXguyn/IYtHuqs2frZ/Sius0/B6Zg+8F/QYMYySL3ZXs9+s0cPZ/1ORRLNi7lPC+GfRewpN91+5vmyUDrNpCi1x1ohAQbM2yAnBifQp2I9hM9sxXP5UaSDy9/+nNFKJNMlVbaVwfbL4hV4AqI1isVpQiYNPE9T+8BX4i8b3wy1mP4mqnbsAiS+j2yAeAbhxFY0iU8Hg2wCfkZS/0vhX2jyrCLe7QubKGYpb92rVqZ7jQUltsF6T3760MZU55pHF3VJego4Ggw12nMSA54yqcBbUH0UFBylMb+MR8lrtvzfbkf9NybLIjtAIRoh0o0qNlNveFroufACPKrUSvzrENWhc1pn8P2t0tUE93s3Mdfxz/lj8pcbvrIlfdyscfRVujWg7wfcNJ9qCU+1pegVEHaa8UcaXdoOjT0wunkE4T+Ivhl8pY85sEhS+e+zoS4wYsBK5Gr5u7qYvNBVkEVrqTBZdrRZrEo7r/a9gpF6e2BUwfIyUaRNTs3cn1XX/stYNOVEmANxq2ymDEfkP7oRSiPOCTVkQj3KdtU3mycsV5t+PXLbbHFsMsNY1ssvlya0MWldesnHaPnjtGGno/QW6Oqjy+KWWySgnM/V1dM04LomSICMZNZx5HP4hqMFQ1y3TlEcQVzF+41xhszNoC1+lpZElPgfXPE6WjY3Sio/vqgmxvZSqVD52K73BIwGd0QiPOxDH8ff26FmIAPKKCPvyoe4VUa+MbL18Aiofg7GVTTsU/1nWN2o7hcPKlIfPeMaGRXN0Sa1+tskkTo4cHlS8Fl/HzK7X78OL2LhjSs9PHKQ60Gm6yO8MdxOZzibNL2pvT88B8n23WIO5eqDnKIXLCECjtXDix1DQyxEW1fDbVfBqWtrXf8kVL8HaBeBCiaUgHthXb+n4kuKPhUxBSqy1yGf88OPibX8xMdJ5rHixJD+zvJ2Z6og/MyDXtHn+LLxulefD96KscIusZRWKmjDEM5zyBn3M4oeSP/63Npkb7sVNt+VzxQIaa2cNJnscJYOkWZRhV5DYag9e0PfSjkCi0R3J2QmwH/nnDQjJ968MiU0BVo/OmY30hp7/I4qoCz0pflF62Delexo/0olAeuIwB+YM6NmzlmQ4IA8z/mbJHIH1Tij4aiLU+jQduC9AAhPL8E8im0ZsXEac8hussFcoX1QF0wThD2rms+v9CaO4zVtIYvipfM9FfcPYcZ4jg35Fz3mjExkr7XhFI0sNQQqsUrwr9Oth8RhpHHzBq1v1B7Kd/nkrDEbg9kiCcUAPLycXM4ezJuXUDjB1AvUxYyG8ASzF1R3JTXEUb+a/7py9ndeuz7VoNYgAk4Io3Wny8oksEHZhIajhf5E3D3cgHtIUn3JW3GTtxgFDPR59CvY1XeeP9+o6JSuH5bh2fNMgV0SnOkVNbIlSaOlRGIm9C4rlrM4nBe5Bk0GRdOY4R7iWVmNW2mLTJ7rHxd3i2QFmFjbeKbvynSN6w4FgjduYFY1YX//2Zqu5BKCUf5o5YBHs2GmZX+GFG1Xjlg+4/KStOqsctRGftwPiiOvrN8DCJ/ul2uYGE5Anm9E6Q1eQp/DAPUo0u25QQPBEVlt16dOkkkczPscDlMtCGwUaGujZjXTeAXlaz+otlGqTGbbjlfx7XJW69z4jVXn4Kx0krkdr3zqNGGd2gAjrKYBnJWuv8KgmaCVYnw+AFQQBocWVnJX+aPfXhBlhv62fHuhE7XxYRWgytn0v7QfYkpemSgjCBzaYzXSTFocGzlHCFK4mX0eeREUkSzKQx8xituEgW8f3XqJ9F9qY1xTRwktwSauNyI8GXXJ7ETXmNL+GR9Ftxh8xvMghzUlywBXt7PEMXOJQdnqcc9L/7Ke7ZBDhcXvwIcb6ytl3qNVtRD/Jmuv7y8hYB0yEe9AP5omdthSPNBAr/W4SSO1aj1cgqL4LEum/pwJWZwQd/1mm9r8CCps4wqHzzF89Pa9VAYc9RDBfyUHsedW7lFVlipOPxAKUcprgiBIIZOWjOmUtdYBHlelSfdRtHfFTZHz7IwJAcKUUHGyG+t9fWcwwZU9Gcl6YsMByc8uZNZhFNrlPUcahVKO9BzirErrJmCF5XQQv5RqM5mcixG4DO+rwEurjKqYMb9IhCfMLdIrhXCSGqSfzuyPTFTfz/I0IW4YnRto+YvAPzqEd+5KIenLpnOP4EGY0+XQlY29HtGGQlYQ0Ba5SZbum9rc6ZRayEuyDEAnUVgOfWKsRkYXT7fdfF0dIprDhyaEyQXYBTwQu2WfC89VKgl2u8h2X/oU+4PCmKvpT1ETQhzBBky9qWL7G3/khQBdfzjfOrVPJimwy/8o02nQA8Td+hy8Yq/3bFM/Wx0LF8IhaKAmUAZDJ+cTg7dYfQuIuJYOS4XJVihmsloLlw9rs36sB9euEVjrUaHY9keCF9m9qMuq6J4KWGyiwfbiv4OKe4CCa8VkjOpUwIV2n6k24I6uS49JngBLitDhcrE1/pQ7FeUsJXu18CNMbsxTuBOCVlavAauqVuBIRXBpLZ+B9MA6n75e/mnDroJNj2ivnhRj/83VdSFBQP534IY265u9IAz9CJyeXlZ27ARawkYDGqHwwqEIzJW9UUcrBRSVbQg2Qf23OH406o+2Jerl4JGc8toe2bnT0JIlfpEDC0zx1dOtNl+YE/yxMbyH4BswK9uI5pCeHNogH0SU7tt/rTb8PFIdZ7Hz2CvK0ENUPqGWQoRnsa8kExiaFl3TCyl/83cBB8xb5sHaWRwo18BIb06E+0hh0TgD4F/zFFK7wTVfleJRRlVZgI2MbUKgZhmWHP4/FbUHjmwblnU8eYj/pFshHGZk0WmDRmUjSS6ZP8rMpJm/ZcAAAABAf0PEQvyykWqsV5x6oabQC8u5tYWZunUSQ/xU4eBjaSJhignZAgsVtZS6vbpi70ZbyvvSsRDQUZagVW2fihtV2yFZWcFDQMPtZjwsdIgOJf2zVwSY1tuNxCx2rQvNwQELxAhllXCjBYAFAMcltLzl6myFV1sauHRRAy5rfW6QwEAAe1NbbxODHyXDn0Fk/wcCVk8Zo+E9+aCt5N4THdeIQp3csf3eeB3DvESKF3DRjP2HthicAKkD5BadsO/DsKS6sv9ThBgMwAAAAAAAAABB3p0AY0E8miKs+t1/H84GS0HZzXywiwB8rcDQRQD8yWOgYv8ZRh/z/OJZTW6jvltJsj1iu0fHkKmkoCpdMtd5MACxf4r+DnibkIDWZaUEgEWeOZIyIliWhG6RfpofbWqSlx7db24e/vzUMPHnHSruQdiy47HKYxHXVUq1fjQI+L5B1RcKsaVlsoJrjMP7whHC+61+dJxXFd5OqxSrx8bmtZ4T6aGUDtuhAc7sF/IwgG0DpQ6AESbQ5CEkYEQE39dDNdYumcJi5tDbvCNn0g3IOsXZrjq9BbiHtgSXrcnzGh0sHR432dZlJ66Z+N7GqxALxX9SZkgVS+Uiach20+KIFsStNXiyDuvwLGLIrMAjXq1SOTW2vdZzDnp+/XKHOiO5IWYhk6akDYBtcTUVa2u040DocgGYhGgcCcaI6f24ZEAuBYXl+oHyr1p4Er8GgS+f4l/tlmP8CCdKY7W8peYRrlSt+QjSszz67/JgAI2Kz5I0PTEehMG8EuZOrkfq+Z9H4MMVZNQU6/gooarYQ1/xiCzF18ve3tQ+1J6z958sAjDVkCMvwqcgllx5TcCPyIYCpA5cZNNEI6RlXk80g4VRqzWKPmM1vQjwCTbJrR7FJ94qGULoOcRtqJ0P3oceJ/R6nqaWOdNrPVRIzTKeZRgJ73/mg2weH5FwBTNh6NyRtcnO8gR+jmw1Z/SVj3/L9RYF0BlR0cAoCFhcX9k/jtRpYQQgncRUsmqJ/0DiM7X6AWAt+kmBMtZrwSdg2P8uDNcKM0URJ1qbsNfbErffFgPGLHO9lqVc6seoTD3A5pwGs491WW/0FlSlxMtwC8ykXQmoxWZxHj1+BWIgPqZTyGY4goAdy1rXaF54yXTdBpICidpMEd0aWJ4losZfgayHMW1q7xWMoIAN4Po9gYs2G9D2qNgfJIPI0plqiYpFbMqSoVHhaBrWjIfba651Pm1vBuF7XZBJG/kaVoBq67rYIQp7hnh0Zw5mIN2bkt6bQrMYIOpbdwAD6yqW4MEm1aflGBLSUi3L70/b6YXnRSbsndOwnwpoDDMuMPoGspNE/wbH8SORGxpb1F+YOOXvCilUneqvGCnVB/fPQInB3nXc5u5RZAWQSoo+7u3h563/28W754Q6RX9h6ScBL99245LLXwpFZdPQf1BcGlqKYGDQp4Xdj90i8EDAZn3eekU3eTU23nORubff+2JhAurE5hyDwn/MaExVeFZlQCbswpwA8GHyAi0vB9Fp/IiAX0aiXlXXJmVzcQiDH5A6LbhRcNVpfiY3Ev+on7ke4SnX9tiIBmXffWabeNEyEjOHgNIIgou37mkwexFA5zBjrOkaH0RSXbFXXYhuE889c62Ig+5OpwDfNYimIO7e65bVMkCW1a1mmgMX/o61UN9Dtfrm3bcwnkpj8ixmukEZKD4cAwUwmfbUNqJb7qeIzA/HgfbQ2KpHuog0f4hnnTvqPhlibLzgkfGXrNLfKswtm8ZVDXoFszAugR5Wd877aEz94SbQX26+yUQnxgYqs/zYGSQ+SKQSVHT+wOO7NTuPxPBx/DD7mi0MPRdg1nW4GxSxRfbjrNTHNbDmjGhnp2HrSy38RDuVdnf5lFav9wq033FMxlGWcB/jUVzkWWggW0T+qGWgM5fxK3jAHb4cjvHNpGG755uhVOPoZv2Mp95dbGNWpvucnuP/FRY+Jybdpqf5tp9Ta/ldNeU9ejnhsTou7SBdYtnY1YsUIkJgHs1dIwRjhGtscgxVVP70bWWn22d+fAJxD5d64mki1ZtSQ2gbbYmOhXV0oW8qNFUIsysoXGqpig3Eh2oa99T0QnxZdt504nfhYwtUus3PfShI9lewb/0ftakMEh9OFpaKv2CY5zDEkYijxT91b2ZdEf34H3dYaLPwBJmGEtcg0Asc6cdhIFOTxfPXlCpOKoND34PNmqCMcI4p/zjYRBA6xRVTkiW0xLgqoHgyZtWLwwZqyrKIxo2s5I0OIny76xrwZPqzFyWG5GNa14gM0r1JvOcH1P+An72k6FP/k9W7rFkfhj+9olkBxAoZ1F1WlmBbQGdKngVYehx90Kbw/99FILuJg0QPHJlM/nmevAUfmz+ROcrU2NBx1YbUdGOgBcpz6t/gsK2XbZoiLf77J2fK+mkyqoxO1V0m8iXkMi3Y3PlmRNv3y9MaRO1svln7qkpAddc7iD2klET+/s9pnIPrEPb//7IHjPnB+Z0TgSwOid/jcljQ8HlVsMMNqQ482pPJnTM7s0q+mb+uCzLMMerC0+lp0rs4WR7HO4c1mRcnJOsqqwBiLgOsAPvK6pUzhpOknWib2Eriy50Ps0A52MRQXqRK4lhCW6xUTI8sGgwebUOxypqnrRw4i08Y0Hv/p3D4DUjTZLbEaUsZ25eYT7TY02ROc4+G/zVU7fIM3Eu1eT9O4tHbZgWdwfWbhRJY9t9ZuRBB5G//DIy31WIac6k/J45f0k3Q92hpZtBs2Wa1c1/oY6mb2aZmg6SsGlP2tFxsmRGhMzEuB75eb3phASKB4AFYPFIFCcot/veiROah3w4V4DKa+RF2mmEMxU7k/PD3IDdYpQFHY/eR6dHiMN9iQbZNwTrwC9WcFVa6oCowwhB4vOQgbvXNPeMOeR8M8Cu+KZOYCN+cSE3Srxp4rxsyS7o45CO7kG5im2jAhad0KRYFlAHYjK38ZO8zlDkwX1EZxPoYHW51NW8BBfJHOFfZ9uxHdfA18wDjBJH5Hh6rwPjlssz3XwAbBoqnSs0pOXGAEPA17Azry84tVuWVhINT6teQDmYN8W65lkaWfaJ2h8+V1BhoXREg8Xtkz5dt6vEVeJNfIHJk+Mp4NlGzM/QKeG5sIus6dzsQUORuurDkwmgJNEtb+tpXL8AKqw/027phlF4LabKURxg3GbWNMiJKQ19zGB0nDXkCcKRJvPRIWH3axDp155VASGRC4DixoZDW6Vu6OKrRPHndWu5gbYdrBXenqxDPVTf8gkpfEV3ynvGvmK5YWE5evWFhUd9Egk8pn0pni0ClmeW34DZ/KAKxLWhX0q2W1patETG3pzjArc8vGxcQCog6FR9CSww18Paa3xZhp0FV6in4dLHGLFUT4HDhiMdVMhRt4YU6pgjTB6SUR3JbQhPBnAtpTlXbBQsrGt2tu0Xuf8DbOvm7aScT1xNLSVhXTOzRfyzeWTH7/WwNwyZQwjD+Gj0CA+5snYcEcjNnOrLWY5T2PgVr2dNQbu0MCRxdGHi9KkyxJJKjC0RyteQvfFcRbyZGmklizVQ6liynLpHGlJaB/x3d2jIs6e3XYquW1MldLZ2K5qc5eIMuCTJv3Epfm74YYuQh1xr5Y/lvV6y9vzlQyuRhXHmRJ7Brmq6cws/jHThgEUX5JjEBJPafgUVgMn7qe7v0k3inSVgdnMr2MUUuQQJyUZRzVyStIP+4bwMzuiGasC4FzJohUop/W7bY2jque7mrRmkgqim9uoIqAp0mr6lTzyQNuYf1Dj54uePcFEfljD2tcohzqaHi+cibPTFnSEFXhHQWtMnzq4ywBCdtH5IjVcy4CXBi8oC19WaeyfSfByvxUM0SrbX4Myf6d4lhEBKW/GjArrDP/hCT88QdE9YE7Mwoh1pkJMDfdnjmxy7W7lt9lsLykZU3aK5CaP2N9Ju0RE5GlQxfOHJE7I0fthNuE20LHQYn1TLjPBtJyg3H+Jw8/nMXB3FTXR2JWQuoe3Ur807ALY4rAJhfl/0f52MD5rtaC0JmV0WZ8bZ4c4rBdoP+2t4fS1irSa/6MO0BUHWi74mtLJLgvu91BtYSsI9VNyPNYdg3+SeD59+EBcKi4dF14wUS01hOgcutwiuiNnMIEUChJkfsOMNqab2T7zgJqGV5tJeoJZtcW69pa+6PkDi6g+Ex/3HCOQb/Y8j1pnrizbPwGRuYAcqGPc0AwCn01iDpqBnNSO4hPnxpICxxYUN2KUi+3YUIVlFRlvKilmbCag8bJ/qg3yup4/O34c8U73Wim1D6pKlCNTtl2TzEV6lNeW0XldSuhdPuQ2t4WZzhPtOqb5s/jXL6/jpE2rGf+LTC1OhEkI5JHi0/bjMnpBqbnqS6f+RcRwjeZmkhNhSAJL93r6sWj7waHrDV2ea9RV6SM0Hpa9/4Is6fgnXi4DuityPub1pzUw47hT/EqnFZ8MUVN5j2JRZ4DC4fxGyd1yVvREt/qUSFPCFKx2xdv0M9ksigOf/p0Rwk2wG8NqV3M+LVHCFvHmc0pZ7BAvnQV3FcrQS9JiE+CCDgPFCeKT0nbj9nOfs8OBQz1qkYaLjJ8d0OY8mYaQBSjuWvBP5eJZCO0D5XpNG2Vc1AmO/Cv63uW9e+2OdxPnpO2+JRqkpZOwt99/I9YSB92MIzTwCY/KFwXKo2vrqGDEZgAlXvzynaiuZB1LrSgw0TGmlX7mluAwaEoaZI5OLHogRgQu+yrjc/HKpuGeWHdsuC6nUtH6E+7WUyqZ3dj3E7A5c0v6QHT+RQ8D7WWAMy7MJSCH+3qeesgyG4QnK6hCdsh+WreLK8W2v7aiXm/eu9sV5CrWvRfFHnHSN1nv7kDNJSCRLlGFRkvBOgSVpIMIhbrG7ZanDGx3Hd0xsIOO2GFOIQKwzuAKd87Q3FoYsUQAgpuvbvgrHp4HrlpN4hOpZT6bah1DZC00ecrAod3kPZlwFOYWsPfG/cxJfwR3wpTU71qxuxtoItd+Q+EdB3VMSdljkussxejKQ18HQyJ+o9en83SwBLuNt4LbYG30g8QDUjzSKDjWg/aNcVb+ITifIbsU6lbjXmicIO0YK6SwOgWLYUdKy7dA8d8GxnrcH5VBibPMwXMNus0flQPjG+b5MoxG3cCs7D5SgNWCUdw3ZTwIlht4z7SKzyHduAmz+3T+IzDIgIcKl6JDXf3VgHz3qezkHjZ0mbQ7a1PZZuECWXObdYdFTOGxyZGFQfC2IVp7iMSF1MKFyurHe5AL9g5JphhCbzk8eTYuXHGtTXiGta99c5pp1R78EhiUtDgvKBYAlBX9kEDKHAs7NXvI0f3aUP2a4Y1+a178sVRh9b3/kcmO2l3Mo1upiui2tRzV7wiVI5SIF++9UYLqOMhYknE7PEw61JDWVDmCiaUQYgO1PlRZH7u27oQ3axOBikNPPS2hAyd5Sku1kZqhPdB+xpl6E+rVBmuRG4YcPpClbVU6OwOhv/ahVj+J7bAYvL1cUn8wAuiAVD7N1Ed0JvNSIIXfSWXApVElPH89q6WskqoIgOYGjiFR8bjwKVgRgP33pb202NMGyYQOTtLpoYTZenSrD0jyynhFOpEphtXn8G3NiFjdA9VQ5gG4mV/T1+9ZOfb9xaM1YFLVRJOmh6/kJJBQvzP3hzJO0IyTKcq/2vHKFHJu4+FJaWVTMEwvOrNsad7+UEJyhuszkKIB8XaBtiqT2TkMnR1UV3R+7G+vxYRJ4fU8DIgqZpdRe1lq6ZX7mqv9kVtCaWXhnoRuu2ha0hiD39Sb2AK+CRebi1U//YhPFI85RXVegvkTEOODnDYQjQ/tbL1pIBPV6RScNu4dVu/O0xtM8qjdeVtGNAAEB/S8RCrcifD5BaR11NF0P2MPgMsUSV09Ddiyr4GGCMM2vB3JzCF66IwUtYjDeEglgvUKS3yU1QWnhb2s1Ykbpq7uvcod8A1WC/hwy/D973Mm9dVuIMWon0bIpokLIRMq7hyNkwgs+FgAUhttbSZ2iwqjlM8IHTeQxYiAuWL9jAgAD6rLaq6v8ZaRFiohh1kEZjOmEyjPJS2KwqSP9MFPvNzVQe40fjVywWj2c/oXzafzg9V0zAiBUhiCGxFLdh8yFdKa6MdUOJd4QOLlxYgVEt+Y2721gqsFR6wT3CKrkuIWZ/U4QYDMAAAAAAAAAAbDEugDPfDdM8ROL6JsLFrrRz9hulQgfLg4fza+q/1vqxoe+iKwDv4VcW3Td7fbeZtakHkGUTL8COcKmtTIQa5T2sFmNTsbgWBcQiTLpSfTpGubS3W4zmvwYUVtHOJ9Jl39MZHdPTIB63A9s4XMfrSMoATMN5pNG5CT4CGBa/Hw7ZkI+//OHmv1Sm9q/0HwaQIgtAUladngsNygWua4IzXEowAkhCGf6pkkTPn36NbB1ePhkKmIImfB0SW4UCmiF3OlA/fTsBj8BzoaFka5CvAzcqBKZYWjcXQHavAb+ij3ICQ1llcTgU6rXhBNdfdWSuwpEpnUoJkQ+gFcv8bEkl2niBmMttrbMRmyhywAkb+M2Ga1I/R9lDA8k5uLb8hi8cRuUlW0JRZdI8A4tgNIXz6Bz4WHVSmVveBRPs0iSZOoM++FqGZY9zEqT0L6D1701UtPQvszXb87/P7EAi9vz9Mttyyt+3cnF65l5VDWp/wweVXcU3qez2RKtmQx5mTgWv4uko4YKqV8RovAofT9W0Jy8rGvAL5iskFxzsjTnP3DimPPs/CEWE9mgxd54a4AtUl+zUNFunZcsANVHbrecs+0SGgRC//XsiXNUkMupg8yVUjyRaG86cUJAOx2vAca/X4XtgFeLdDfmYomxP3r+r+mdZC5cE5FfZxrujOcXY6fDU0kFM7gMq3bdj5XWhV7c8fqcUUJEEjwMbjdEx+9pALH/UgqmJY6SrPHumOiEOp5qd7BD3WjXecb3KupZJZJ+A+n2JJq5i9GEbNGC+VbxTocFFDIU9OY7dEBwWs0Iw8rFw2ALGkgtoUWXBflFHIVUXPh0JL1BkR+TSTdQ7A5xDIm21flF9A92wEBUrcBVrt9TC/WjBUt9HIYPG0m2ZhU2uPi4TWJ8VdY2YXkgbu5DiAQ3WG3SLZbWHnXKy989W6t+Y4JVT+PDr44vVicET8h++soDWKj1No5ZW3zi4Qe+uY2KOe61LD+MTfntQ30Va+hBeVnLMTBbYDSnIk/7ks10/6MDH1FzrAlWSrEBV9FWrX3f0oj83tiUzT5rhMLzCWquRzzI9CqOei5tjnqvGkurgbUr5Eh61KahGEEgKixkywPjTkHCPurYMc8s2V4EE8TXeQucZNIVxhfntvxReZ3IWHWY6roLKFSsu1GPPM89cg15IHa/4iI1oBxMqK9gzf464da4TiS79J3EUTBI3U3YgVYSQRLWMvoZ1kFkD+2GWsJmWCtmNNK+khzlNByC+GcxB3K7kxVrrJwCd/KosHVnXACPLdWHPxIiVGIOb/OQQUWOC9McgtvwsKa7DBlUMdhidzHowpw66N4kXuNxdwgS9/W1ctoNVK2lKKFauS6ZWPfqjKstwIGwRBcGGeAWJndqjxoKdpThu4HG+D/3x+rJV8LvlfLoJaAyFANAJdwdn1W2GbB9JgqP+Apf6RYQN84zKR4E+8+fw6eidE4WsW7jgRwmXPZ4k1KH3s+zsFHv2BGOOe1tmI8hUu15a3XVhJamqulHnQ7tlgk/ToFvqaKOeQwvns6hPviBosUGBsTGH8JbCwNhDFiUN7tdD4T2oJ914TlqYkNfXLFuGjAmzAznik5taTH2JyUghZiTr1/kiprAmHWjA9B8GTZRTkm+rRBCSn7ZEGqY5Tq25uRTBEPGhatww1n69/tW5d1SQLk5DnzOTmwbyobIlaRNPQKJRvndqJtSOMyhFXK/if0xdutX5PJDkBmywiEpQfEF0XTB+l6lMYExJBq/UDB3hT5E+guAUC51JFl4hs4IjO+t1e95P6CyU0DOVyPWfLEBkFPO19WqKFXR67kmyGOQCDY7dx2+TC/7TDffksMlO+99zaMFv5yWlbKl3A8kxgRKiNteILrMN2MX3bZLwkHxmuAfOIn3r7wtJ4t9HCgIokrRtSl7Poeh2qT67B2LuqA07BWCPnWFayYTPI2L2jFTom8ZtasSbe5J4UL6zGyZvALQyaxYsPouFuWE5eMFD6Gr3U04yVoBmbqwrTv5jFELh4rCeTKuZVjVJkyNoT7p/BgzAKaOrtmf2NZZ5VIuqL+5SegM7dxAdNzPE2NG9mlJnU+0KT5cSnI7sk0k3foExgA/O67+43PCRqRNVMQHl4HvRl8EJ5cilvMuy16TvG3MfgtPelOPS68nQAPUUrbRZrqq8KfH1zhKp53RR+a8QSd3paWGlgOzO4xtr+1n117GKae7uvGjt6qD/+jB5eP2s4tmPULVWqQ2sEp+yXDNGXhsrqhcHAHHWvGqZRvPwsXQ0xMpXYM8bdJDMs0L4MfpHK3jf2KK2cCWpyRCqsNHGVi5OWNq2/ZFcY14GDbEWTktu2YOFybHpzOgIfnR5TLjQvn/ldFWAovKAJZAH+Y930J9BJUYMreGTzWjcrTn81bv2ZtLSd8Phq5NVg3LhKESg4js+JCJva0rfcigfv9esDLK0QLnaoKUiG3bu5yz2YnlnjdcgUtYYW1zTIWPy2l9ByQLr0OyTxaQFMoghB3pr1kpkEOzARZILL6x6VF7M2ZOhgmij0g/oznjGxri6p+LSTxYds5KpPkkx+0GXtrF6Jge5FVuHpBUvn47C3L/Yge/cm0Kxsmj6IA98WbfAVhU2yF5p1ecP1NlG3sFYhW9LLXQ1SXqynDiSX8fBHSWAKEMc3sdP3APaEcCBwmV6DhLfqm1jobsEymftYQ4K+3oP/DtBNUlRgjGbO8l6ehDa7fR2by0SAxWyQC9yZZCYb6mKnE6SBQ9kt9Jl14dTFTUbQErPeRlQXFOcrSewIan4ku9o16OMLBSJ6XlkyglF+a+QaOSBqYaE/Z/TT36IXYQ5NoGbvEWSaJnTHwgO80vnZVQTooBIxu1/u/0lvA1jhQZveOgX7NjaFuAubdKeSyJgJP4bnIp9Ka3SJkXImn632PuLM54q5qL7Q8QYzYMA9HAGSWosAbBKnnabcq5CnBL3gFquF8V+0FX3/NVBtvnob+skuXWW4ZuM5s5V3o6/N8Fcn+O7Z4bbp7GwqQNRR2/I4qLe81jL2ftHHRctGAnZyfta3pFD/BhCoCoSd6lNEvY0X2AXsCJfe92n2xO1T1XjWwvaIz8unxeh8hyQrQbIvlKOu8zkwhVKVNHAZHPPQDUtbgsFpqp4QnHYq8wbVA+yfggikMtnEGkYpBr+syzrO4rIp4gi/NejQD5c064SXSZsoPd1BS+EK7ytKeoiEANI7OMN9uEbxmGpii6qlMhhMQL2Ielrd5j1XId5WlJajbcJ3fIQCvumey0mvN5CdV1ws7Vj6n3pW66Uqbs98ssXLIChcVXdMRo0eP4uQfxrQiiMTuPeggUm3Nxh5nU/4HPWkio8wcBXs2oyOr8dUEcCcwuqKftx4J75APDg2OhS3QQNsJy/cFdU64B9LKvERJ/wW39iqSMzmwy9K4h/khCJjpP4fP6+Xswd2pLpa9rfmolHRqELTC0RVZZtZRBJIobz6c0KbO6I+N8Xg8b4UI4BCKcbLcLcqX4NWirq+Z5MVMpH0tQ3Gd9nTRz3gZDKW8U9QovFvICaIDH0wAFRyJ2leCU+GWOQQ1F9ctUdWrBBKOPvZYtLvKNnktds8+S1V8i/2RKfVECPvqtEiY2chQNQD6KEasjzHA4svRC0OFGlAq2Al36JIiuEVmBz3Rg3BRnViP1TXUKK2i80tlX8N9oBut3S6nCGHYKwzZylsB2p3vlqVx3tj2Yx5oyPmUOeyMfU/uI+jiAhbR4HYJ6OS3l10FhIPL+61XOdrrIzLzEgcZaRUliwELYzWeV4aahAbLoyVYUDKpjOTfqpC6+ae3EAMRPo6ZL1fmXJclRam3H0n6ge8ud7X7puYLOmo3lZ7OHWZP5MWMfEzA+GaylqJNWY4DKc+Nl9SfZJH2OMKwvfWGq4wykHJwgdAXrlyX0nnBqr8vXX3V3ruBTc/bNDEXkNmUqGJtZ0xQhyFORfOx/6rQdGsZMJ0sKSYg/aH2Xew+VPtMO2qlMazzCFGCJEfDxmnRfGH1nLCs4D3RAg4wGYy416E1RC0K1KuhUo1bKeNp8ckCZX1uu5w5eUEk2isEn4r5CnuBADzF9cRjmkkbyj4SZRjblvv4uk4zvU3oeLNBMMXKaqjci9akTa1YQ2ndtnRFx7/gCXP9SceHOas8AfVidL//ZZ4E2xFYwCxoIodYzDIGVMq1QDy40vjOs+eSeZ72CwlGjo3+sBKbzDoYavpOxlW1VW9DXusUI9aDicwtFO/xRI90vzYJcFM/1NPhwiKmiFPq1iHPI/6fkixH1f1P/nMsLf/B/pQogadPPVsqR2NzcUjV/8dmT+yih3GS84Nsfj9FobOfrxm2DucYEmeNWrUIGVyOINVDwjgdupZAXRZySx6vTQfNCJzJ333AZ5gw+9NtC/BPFNE/K6vwvzfEs4I8QJR09/jOT1SpW2l1qFj4WveTisQcyI5Fp9ZuwF1IcVdavzzh3TOiX3UCJZCVqM9/rD/Gw4TyQt96qYKNfN67dQEx7eKR+rPjy5vWtb4Bt38rTIP8BzmCotQ8B16asWm/EdVz/+TCEdsReICJ5jA7KDaYY3Yy5GdSpLuXteXl1eBvLO3WfD6GlFJltKMGPssJnF9wXXs9dXAC65Ycetz7W0ZhMfG4A/xRtv1i5TwZgObdAHLdK93d9LEzdz4wnukQl7SU1vO0qUSUlwJiE4bRXwUAPAe54bSjPV5Rr2biuPOxXxIjgRee/CNLXgzW3oLk9epbNE1i7qgnMUHnVytoTA90zQVGe3knM18BdjoQbpRpQav4pdF7rZJj5OD2WQYRacQcW6spBi50wnOw1l4FSGDBFXHe40M1yH7it0Q8EoUznCvIQu7paPFWH/bikNw6827y/CIgbDcEDXxajahfd2Yaz1ASHLPF7LCyszVbz7z4Q0hbabnZXepnV0in+dB9hdQnNaZXDl7reVbpLJQp+/cxdmFGlj4ofUBLdjeWHJl+RtWRg7ZQdDZ2+hIlQx1rCDYa0UPxSE+erHCEglmjXKT9GlNbqCU9UBVZyEV5sfnvun2GUSajXp+TIzX0lhJybA1oiS4G3t7yWlGdsiwbCHWwpmoDntkj5RBCZZtaxaOdhZX7UKouxJe5dsObjK3kXwVzZreBGHOLBWmukJOLbipmGFJhW4AsT/ONlWmXzwJGwjOB4GSuVvZ3uk4l4+SOwlDjjP7qraUOM8LlbvXA1BW+B6uFLI7P3sR9Ccl8t/3mqNo11KUAMVoVUfD9cvLmbspFgGjfj9wtZcQMuO0iHEHTvod39KMB5HEf9FssNNdm4ZN/6qBHYowIb3FQZwv2KzUUlN4ttKny3ALvz64PcpShcZA8YFnJ/8JtGYSnitSzVuce2+G797ka6bONMfpSdrAAh/vDLuc7plRCOg57sIzy1UVqTG/yOJ4qJHUQbjn2tfJT7TTt9Clom6kBknGcRdoT9u0ym1ELLvjz9kPuwVd4roRUZQtV9diuolJnuoIouKipRFvAJ0GtkECDI1P6pv1bbfntLoDGMs5n0NOo75u4deGjqNq1scua0nZwao/NtynLOtfNeeyICA75I7mDap8QvIq97JySY1eUzdKYtcakyo4RJ/a8PmEPXRzBEAiAuTGpz/Xoh4skWasgKjALjJOpnO+nchYhwBWjRugT0xwIgPnJAnFgd6jqE+AVQ6GOPz6R+6va2rmk6483+mEzke14BAAEB/Q8RCsQoMyvoxu5rCuNuzqlpxp3SGgHGni3jbxEcLyhO/D8XCRjBqvZmusIzder9rJQJy9vw3quQ4TUP9sLKpbsswDkJAyWGJw2qKL2wN/loSFCpFZdLvwIaL6sE+RgQI5A0iKxIFgAUyWUJcVKk9K4VUsvdSfl/JUq+JoxDAQABcurGu46ra7zLwuOVWeEEV11IM8Oeqx1BVy2aA03qJQHWPm/5g4EE4p6VNsvyFj5glRSIoFBCdOvYEapy1KpTz/1OEGAzAAAAAAAAAAEjkcUAelo0DfiLnRwuzNKaisV50+9nzJWgY+865Wnj668P0DCkagF9iOmOZag82DVALqXhFnv4mSM+zIZYwKlupfcgT9oYTDF77WjZYCloY7lrDaMlw8G1CzTs7B/7DIfkRigEX2uap1c2ukw3r/Q3CrRvvhXJR2wHvQ9yedkX3ZJZ5IQP/n17MjGIrdB9cIgP11zhiLRIwfI0Sl7nvFdOJCxbXEizfoCKbxKCTbh/608VGKCS7MIyYCgeVPhxbhxT128T/pddQZQVgZwAZkNOUZHVMEC6ZwB095NnalWOBBQ8WgvFQN/MU4s5aBEgojnp3oNUlM+8kP9ieqGOZwPwNsI7XHFHKM1tG9sdnpqvsa9Ofn14wCexaRkS0mO02+zi4E4e/DLlwhuaTpsMjUU6BieSsXoCqXEndBglo4pDwgE57dZTttIChr5u7x7pm6CDKrrwGbfgl49vCnFWgSs2Z6PAbr5V9RpkAXfZRsaFcRNybixTTGSaey8TaQOEoIc5E1/Uf5+oD1LWH2qC5YMyRbKNZYfyLGEugbZ12P1io9jFlbZ6OWjC9lAsPwucir9f0qxVxoI0YbPjVaWVoyEdOo4vbQgTHyw6SL7Z6NNl/7fjbBNKpljayxGi3Vrfx/yRIzkySBtjKAYHvyB5dyEmM7cIB6m9fJCBKddb04ecrOshKQW+AYoQ+HiJBtC9M0honLyeQXJxpS2Cfk+q5rtfRBcEAYwgAoJl67IIO80dQvc7VyMts2pr6dfOthEASd2miavGlnq7JnoadgdhUU9xKXdlnTaT679chcjY/jnsYqVjj4niNN4O41EM5E3LujGJpfavek/EmLNmumUlzlkwfSrY+A2CAQRlmkGp5/dFES8sepRqq4lAr6AbaWEyJVeluWK0D2ki31dCh7jLmKaJtnkPiMoYGqPWiwAWU8JhhXZDVotQCDp542xZ1zb39TPe7kQyBNqVDdF7u7OssGxmViKMGfae1F0AOfvYg0oHSARN6zenb08SwkPJdQNHpkwj4sfMJRCh9GKowxN03AIpgBNDxLckTxsOutgs4/J7VZjSjuNouLNlYN938kzI1GuudNbgN/SWlUum10xTguPge0tWsda2KR4zWaKglUJzmOJP4aiGlc7jk+Hvaq0Ug3gab1htHAFXylvmjRVviS5uiKXZHcQAECk45KIUa6hR5pT+b357vh+2EVpTidAeF1e/VKTAqM2vlanv0ydEaIUpKFq2K/8jU4aQlhU5JeKIcRx+B+e6YhXycoCX/Mo+sfO7c2FnzcSLG3ZVvAZ+NN5iSlMGqtokshmnHwqS8kxaueuz3ncPOgp5LrQBbVD1yJmGWFvoz8F1Pq5cmA3OA8BPtS7kw/q5LO6zed2vzUlDJuV/AJNtqV5NJIzCj/sRNoGV9wDV4aqpQA4ElNkXjpm/B6bbP5qxeKuOa0b6VLCRJtwWm+Xvczo4/q2oRroX9/+M4vFzBLJa95fhOxtE+/gKcLj49YolpEaO3qbnITrYJNuE65eXFSAuXWM9d4OZxTOXBTt820vyDWWJaOoij+A9Q8TZ3yKmw4w3HXJhda3196QUNxiY8wuaiESz3mOpfe5klEcIlpd2kAALxNAxC61sJn3XQVDusNF5RV8ROCp1amEHW6wBOSs/+9mr84m3RdTvEiAVm29OjT1TW1Iv4JZ20lmRtXNKc2N/aFuikC5t3IhzlsyEGfZe6E8jNbRXLclBolo6umQ3WH800oTuJNYU0QYJp4cj1LfJawUy0A6KzPeBlv0L//5zrgGbbiRI0JwmydkgXIV00IteNvNOLHoLytRH3BOrqTY8bujXFLTLJW+18VnUv9s63j3dbfP22CoUNPILM/OUcMRyv8V/TNuR67rsleYL6b//l5k3JePzDEXoJMq2SlSSBYlE/9y0yX/cdyqKntgsr7GIlRQ98uvVZ1Te/SwV4h+EIN9zfYCxM5QCWjx4ohnhXJxHfsrnnQVBjGFdM3pUgTCS9wxTHKusXxjJ503YfrDKCc6KN+XNA6fTKOAqAB58bdPxLERE+kTuMeslM924CPBD7seC0exBR3qWPj0ZkHxJFDOnCABGtnBbPjkdNf82j4sschclP8dZN/aKucwIH7if7v/LsQ4jErwX5XK8PZrxceB7awNN/XqaTtrN59UK8UeLEfzWuVmPBrBA0MlZneUf6bRyDwbWvrl4kXGaH9EKHh0z9sjPWTMtJKr4nA+o8S6D3pF4FyqR2cwYQ1WQ4XchORzEutJFXZdXkoyt4FZX/Bj46gMCFlziHkMlHikh+FfEb4VNSRFPC7tmR8qRcKJhAEhy/7dOB/L9Tx1e26+M/LIjO0Q0gJ89nBfvcuA/F7ikBGFgz9prL3RmEhFcq2UeaG3hXjnd+g0HsMA5zKs3taWwbuSoaEZUjQo3WyK9OqJ/UfXHZ3QrQsiq/buH+ffdTQqjZhyOSz++w60cGID9ATwUczGV/JC09ITB+WTHrj/l+rimTwH+B9eLQMSbmzH+5MkZXrHBzVa1iNRdHHIoXpl1r2TBxoSn/R+K/f9nTeaSgN3wuz3mXSKk9yRqtFp6aSs7MrOuIAqkKCaaeNq/H6Ks4cniaOpTaTWK+eNTNopsUqHZkRWmWbA/SxB2Bjuc+nO4J6EzeNnySrEzw+sgvczOArmbsMZKxXgIhNlmyxRVmolRGRbY3bI9CSbFQQ8uVM+SF9az/VldqKWIbj1Vqdc1xu7LmBkKVD+6bqdB07TZ9bAJTdgmc0+IEZ3vOsahih4wA2CWtxwFXHuFGHYSQVvbSB+CVwFyCDF9UxUhp0ZmmGrCZCHwpzAYu7ghx5IgIfN1ZLgWDsVGf3ldL6untWLRgZiaiLoZh7L/P25l8m6ZgWvInn2cTkdMv3+rKwqF93Zky3KRTVr5iaO7sEUj0sj69jtlgfJLYjdrhET+mSrEM7Wq1GmDCYtZmNEcLHs/TPs2D4sYSwBIzRMVBpQwruykhb5xpvFkD4gAXLuaIGpZ0zs4O8ZHC5dNyIT4sHSW2UnDiQapVP2Cs/9jZseNuM/zwm/DAGkYrvf1THl93wq+2F4pvlzp0H3F/VM81xYMpu5EelYAK00fjtDgB34pSd7mDYjrg6RtUBAKIyozhZpCGbx4Biwt8Zg+MIwKp6DdyBeirxeDUWOw5rjl7y8RUi8rjzB9bBeWv6uXC6UPJ8MfPtLuz8iMi2S7xsvUXPyP5FFoL3akKibdGN2JfYI8khb5O041mwLONQAwkN9POPpehDliPlIO8NIZ6NZAFO2ISeH/66rQtXbrG+wAwHzcXISQLNQjq8qrLxqYzZ99nejrV5Y7jlU3Np2UIGCef9SAIyihorJDvqh6xvLHllvqKnQxdElScORrojYuuQAaYnC/TGwHvZJaIbW37SNxVjOLnlPhavqm9ZFuYVVmiMXMunm145W35widUkHtoj/s1kDkLfmZ8+orl8fWx4rsV5wH16/HfuZtUBFu3EOEEohs0C+pJJEyOyvNEJLgVmM6yvd0kCylUOiln/jFyVCE1ueMWSzmD4JZfqFJQCJzb6pWRMzJwzN+7bM7/zmSC/C3H94p4uMa/qt8ZWVvFSBai5KR5/kGavW1yqInkBGGjeohb0ija0xIIWMe8RnuPoZ0AFt1HhpE5uYhcPP6BwTxijqWA0Ni6kayWaLiuR2P6OUq+SfD1vYHSUHS46mvG+mtYEKFGPb1nQ6FvzF0mWZmgsaqeZ6UFWf+0KdDgizm1RXYKdWqPsZFZbpNbc1fftS09yhmsOVzWcrSsEDTslGEkUACdqHo15dAT+jAQ+gUE5l3nbjNOYxgDOdHc9bFAbdxZukizmypsnzRix5g4rEupe/JzK3yrP2pOAZr3OqImAsIUHCQ415uo75VgdCTKEURMm8w2eQ1kYQO56KS4Rlx18IIk3boNOPRCEVSSmzRic1G7hJy91T7E60AcwNk1psYikq2B2/qqpkIFaFbHqXFM0l5MvipssNa5U3CL4ylGDqiVDhn+OKWKdsgGX9Kx0lEqhrsnNM3aoxQ9qHbo1xgbwDTJK+4TDaK/guS0xP0FeJRlbkYo5dpmUX8kNib+iBgNz2PUvjMfVmTGhd0A0KMXEShds1rUNbOoTADKed57akVtQQjEqVIpkZArEmim5ym6fnyYjPxwvtfTVKIPsvuSNCbdE+gQ/ashbzJKocS56Qnx/afd9vpnownpdxlkFOL4HD/HKa0PfhcRJrJhFQhExDllfIGhU2M/hk7YunqqfkbT0aHWPlDmj/CeitLtCbU4XNm3qb2j+mYN9/ShPQbcG9fCWcbWs/Y1iN1OhKmYLl7rcxfX5qMjl4LV7zpZSzqyikg6kk5V+Jbc8AtD/X8GL56TWhI3pE49UH1g4SJIphdOCPPYgrh84RlEPZOUbFuERqqn2Ns3Jt5v5i+v2KNGtpw3mlg4E57DMKi6bHg0rHSbB5PSzJPwIoarOEH02avQ8SC70RYEvHEaWnDFa7L4JsRbiCTeP2qGrTAjcilCTcPpuElsrDWCZUmjHz78uqPOAJL3kBF17vtTmJOVYtsdXYnayr/LmCJ+f3Sqfhodc3qIE5T837KT4lwKyy598dRaAA5quJ0EC5Y7RVzsulpAfrWh1R8K1UHAomd2wvjsNqb0hY3qN3Tq7FoFm0fxeezz7UUe8/KmWwjZU2vXX92d3HbzRIAgIGZDePomlAMsw1G//xnia9i7wo/Qtw+Y6F5dhGzebrfa1oR5YdSSVBT0zee4BZbcCBEhf7P8nsC7+Mw8x3vImC1oekbE9f6pgzki6BMLbL2pGyrWnNhkDXg0mJlHw5b2A2LbJhR2owXviDdhZ875bDPU/l81+GCjcJ4S5VXpzpIYbGxNmh5hwm7sDYXDuNWF+0FK9+Zcaq88vqdRo5rJbDgwh/XPum/a5Q211TE0bbdhQb4PXnlldWdbFU6vJuT/X7uE9Jy9TlDHwmCajPzy1/VjTeYBpAoT5cd58L2s+GX2AVV9dbpERrowiXuHORQaIG8/OvNVdg4H38UWEzSa6pfy9gfkO2Q14eDlGdnO4rPbuyBIzmD2HQiWj+Ml2PMLktc3GvDJ9eYiGcX296y4HNOo3GU/Xbgj2KNAHrPt9vv0NP89kc+5JKfYyd+APVBT9I2F2CpLsFCpEByHFBxw/4hweVZelXi27cr/jjNPdyB5sP1J62blDa5ITAiOBkFmWTAUZcztmTQJkSJxvrSPC/Qsa18iWc0T71PosgwSt6T+4QfosbFgSzhaZlfqfiZuZbsFfMV8IhV9Jg8Bmt06flLIYvJywaz0yT/Cb2Jh8g04xpoN/j+RN700hOIwkc+yex+4XPA8j4UW0A4dyUSxRjSh7uH1CeKOjkV3oSWLdAn+LEy+eFL9pDR2I5Uf/TNrmAtHOmAZ5sRIYVvgsMCxdNYkgpTUZ6+3NgIm76C2OzcC5sj+L0sWB/ujp98jW5fpJFglpCalDnlsZ1DxN5mr5zmbxELZIRr4Z9rRRX3UK61m+DLj6qrOXpsxxJzQMv9ilbo3Rp8PXtD3d7+RcZNSGUWoSGupRUiAgOrOKBW7Xs6cPTPIV8dUsbCITuVlsHdofVlqT80IesVj0cwRAIga2DosaFanf0qV8/jsMUnsD8swW35P4eTIWGN2SBOUXMCIGtKVoin9d8fPgkCfv6U0BXlv92nTWr8lI3juHOSn0bmAQAAAAAAAAA=",
		InputBlindingKey: map[string][]byte{
			"0014d97150d7ca4450f6db60a9c6c026e74a91e99f91": h2b("d2422829100b19c9987ea617b2e51bc7b1aeb3cefb4283ceb1bfdc5d247876bc"),
			"001486db5b499da2c2a8e533c2074de43162202e58bf": h2b("60d842c09e84b8e1df2d53a2a8fe1d3ca4f92de58ff299f5fcb78e210b0c1cf5"),
			"0014c965097152a4f4ae1552cbdd49f97f254abe268c": h2b("5fbaa07189a1fbcaadbb6e09b7d3f11f51f02b63f4f6fbb9a8bbd5d2ba94b208"),
		},
		OutputBlindingKey: map[string][]byte{
			"00140e1fc3375d8b7e707fc9d3a509ee46ee8037f0eb": h2b("5071da5d80f6f86c6d81b947843fe756791031094ba39d33fd25ae4e2b56c97e"),
			"0014100049a0f864c7d864ba5495252ed83862c29a20": h2b("230985108249dda53fcb0f771aef034d6b4fa6115822fc1f92a3f8b70138f282"),
			"00149d5436afe142600b882ce7ad2b3db269f8bf4c65": h2b("03f5e78d651ca05c110d4d2803e9d4dd2fb99648552e530357b336fb046f887597"),
			"0014d97150d7ca4450f6db60a9c6c026e74a91e99f91": h2b("d2422829100b19c9987ea617b2e51bc7b1aeb3cefb4283ceb1bfdc5d247876bc"),
		},
	}
	acceptMsg, _ := proto.Marshal(mockedSwapAccept)

	_, msg, _ := swap.Complete(swap.CompleteOpts{
		Message:    acceptMsg,
		PsetBase64: "cHNldP8BAP1LVwIAAAABA2P81miwvH419N66FkIksBJD3qS7RZobsJHPxgVXI351AQAAAAD/////QSRf2sy3woRAw2j7iFcd3ABsWgovjRad9e2coN5sRpABAAAAAP////8n4QRoNsnmlBK+ymbW1uP+Ozbp8KKi3p+ViOaRoJgN7gAAAAAA/////wYL5xmYCRuzC0v+npwtbaUVY1dTxH47DDphXsaCRkOyoJ4JhPG7TNloOMK/DzDBAsMW6cH8yonRtdIlGvlA0OUwAgwDteUtrq8P5FuLe4TXBYqrb+4nBE0oauU6Tc1Z+KAUrOkWABQDHJbS85epshVdbGrh0UQMua31ugslQGIVX60FT+G2N+MeqNK7lkKE3zqH4whsB7Ux0Xj3qQhRxItOTExF4VsOEb97Db0f2DwFkA9UZaKEjz4HQGPeaQNALr4tdRj4xGF4LdpoQlCKWQyCFRw6o8WEUiTEzJw2UBYAFAMcltLzl6myFV1sauHRRAy5rfW6Cz7j2Xe7eOZDJk7JxRws1EEXnQlV8YESf8atnOo/a8geCfZXaBs+F0t3+jzXogs/GUa0MtYEjTmanIJq5NZJZGFeAu1+68eu7fkN63nYzF5lVu2FeVxziQnL4EOqjd1gX+U+FgAUDh/DN12LfnB/ydOlCe5G7oA38OsKhgT6OZYnON3/7y5Zi5kSw01I1O5W7KZxK2RKMExzDg8J0Lk5m6KjQL7H3KLPo76t98oKKS4zorqdRRiTlBZzsMMD17LWkobBn76iQqtv/uCld8QvBn8Os4D8iooPJUAwWEwWABQQAEmg+GTH2GS6VJUlLtg4YsKaIAseyyLf7oXKpXaFoHIQ0jwGqH/USUYu5uGYa86NVmRy/wgvpzv7MufiX3EzHs+WG2SUllmi/+PnhGMfj6+cnOZ2TQJi/28jMA1l6q1R6YjlhZ1m1hIBe309GmTtvt74Sqq0eRYAFJ1UNq/hQmALiCznrSs9smn4v0xlASWyUQcOKcoZBDzzPM1zJOLdqwPsxK4LXnfE/A5c9slaAQAAAAAAAAKKAAAAAAAAAAAAAAAAAAAAAAAAgwMAB4NWrVx+vXvHfd0NFUDTjX7hGgbGZEsOun5cBzko/pm/mbCIzDF3LKW6Jp1GiTxnNqAOE4Cdm6IezPojeKPtAmxD0o9kUqBxdZUVSjUUL1h6eNv00ukNnPQRyzUEy/jYqa02IYUVFh04heGuM5r04cZaxH2oPEB21CNRTIULCeU1/U4QYDMAAAAAAAAAAeihswDHt/xxgboJKBFhd3MnEc3Xy9w4Kj4zHigg+Ct/8M/ZMkKlx0khZFzOSlUrxsMrwZc78PllnDEpYDTC8BNFOcBUgpiXiHEQFhbxrkvgZbqLEQMmaIa3AL0trceRQLE1DE1kMW5d1sxnDwOknDj1NKQUHPccCHERvzbyUW7IX1C6YLLGYbRutVUZyHW+iPuFX3suMnfAYjZ12Tt9+rE8yQ8IZSzPovzcTRIjXutkqlF0fhjWco65MtP4pjTuXBLtaQsoySLae24gqaBQigBFBqWsGXbUvzqtn/a0bQRNUn1iSlibRNbhAdM/E54sSe+XqOhbEfFvYPn79kYaJzKsLyEwual+2MCkhEcSL1J0wHbHT07Ucz0SqwkUm6Xc6OC49toprM9SoKsfut4lPQSPTYROXXMufp4eaYRbF96E3uw78twRVlqssgIaPclMqHnCZ73PhKPXJNtUkJEL3gUdPxbwsNCbru08oLq035jB4zt0juPVHqNIdz9U22MmmNdkZQtvS248DQBVJiHrYdIRY9GkMpBID3VZOxNiBvTu14d3PkNNGjGkONPtYMNLz0UjIEoTJr2kykUr53BuEpSTsqo99G8+fMe884zfWYy0tWcbQHk/1umFmPV5khLHmtPXWMrk7rp5RJayD7y0Y1vzgZLNqOplkIASyoTaFhoDLpt6JFNuY6Cb2azbrPYejndRhdKaC8BEUweIEuSLVzB7avn1aju8kaimHLd++lDj96cmhI2bYxQYp4AhAcdulHmzMfNiYplPfYRmvyhefiFOxb+RKZDuyB9cyp/Rz4a9NdEB0JaRavIUFVlzcONjGuZ2PetvfGoAdmudHSEYWq5qxfVzXdutVr2lBxnGK08iJMr+IyGTG9EV8nNsFKb9kGTeLDrgAtuJ1SoZSYsXV3QIPuXLvOLD9WNYcfulyLiEfM75wj3r2j+PyH7aYrNMom+36V35xEp02WM61fiNzNxMsD2DVAjlhlgr28XgJVZd26XtmTXeIU1dtCv+Mj/4hZkK/KKrAK3e/42KMurhFukDOdFFL4Jb6Rr3hcwPiV9kheFExABbcdwnANFvce9u75vMswfA+KlHPNSx/WqO2SmWHQxxDEODm96AI12Tkz65cdq5BHblNBRzaWP4IDqu6ThqCD5U8I9shJDedV6xmOTEPFCLPT+LofSEHKcezHtg+fpz1a085Whjq+IhsH5Xhy6N0xpU4HWtQUM6NPg2Y9hHt02RtYgeMUArzu8HrUGinIoEieNYcBcyUa7iWzqkmdNO6bjQb3+MifNXsSHdsHFuzcUZRwY+lgXSvT9Cvn3J9dIwLF7LGnrClaeTbGdDDBErlivk1zHQWc6zIyuYTFw1oIS2gYb9WXxHgLpgczeYDlMqxnWZnQZN9Cjxm6oFlQMz5JX7ZDTV9gSqCfbA6irCt2Sh54xhfh93lZMJkPRvCU1YlOqq+SNgFZsKyJfDcARHXdP9ruEG3XpbY1Zj7Jfs574zWw1t6mOzpJQUIkytMfudrNk1Z906Hs2Yg4J6AoIrDSA3SGSD2s1olmHBqVUAgT1L1Bo/aZpIDi2k4lLCtQ9a+5p0+LvTkJZ9L0MRo/WykLoFOJnHL2jACBjVGgkh2yJMcoZYh1SlxsQ4HRTk5qfzW72vfD+a8/AoT6XirfhgYApDonA47arrc4lnYwodIBCAh8Gg7TVfWX0IZyttok1t6Ya+M573bAtfALMybWZlNmuBfZcrMbS9WyHSTrJUt+Z4g01UUMeKrzAZsJCzlrrHviM2cZXcCfLTfqv1YcwlJICBgdbFa5M0Wvz5KCMFmk1Ub5PLqXiTvwNT9f12wyfoY1FtWYYuDm5gjy9LX7a7yBsL5Fx8wJuZhR/5lzjvO2EYYAqV7je7M/kkxwJVNK5J7kMMo07cMpxLHohVA21DNHpCvaydBQjvL1ukrSwbpyFG+CKaf44mg3xAOJ+EL9ciSE3NN9ATtGwiNk8Vejn6t53PX1ZVFM2QiYTZioFTXCLPuL4XLbtWMfsVKBhQ7pe+GViEJ6bvoGHnBVJvywwKRemaq+SrRq5R2LOICRReBDng23duUPNe0ytgPQCrkL9DvVdxkYX3dCfN1TYey4AAh8G6S5httw5tEtSmUg7TEfiTbQHpWxBwSosFi9WgbYyeDvpY7XFhQ2uExJACWopWdAV+VBZtFtCnMQpHd0iomspkGMk7FEidFB5dCnJ75yWDkQZTxuVYqU+kT/EMkvImBFDd5g8THjHP/vYXmLRgsY7USXAXMwHNO3fFVA4j6CpsEHO7soaQwWtYbGGb5NMLZie9p1pbCLC0M4uQuGC/+UMvZ2he0+TiTLjwiC6OfG/7KFNRFvG2EKuNKQhPHjcJ8skckYg56QRCFzGS+e0Ax207/HCPkI3EKEr5R4Z1Vds+OihysKWcwlyymEiAnPj5TjVUM7+Tza1lDd1qw4eVX7qXE/9ikm475WjJLcJkvThg/ajOwHUWJg40y9S/qZ3Mtm/j7oUN3OIEVwwN0fe+xCUQjHZ5b/zNJ+t8LMNECu/HUUotdLt4HCrf1r86QDBZBq47n6vBqvZsRhG2pyxJTrnFtPhBH8AhDEaLdxPnmMcxcFQIqJQHGOcuzv+uEt0I3TRMfPrfYLikQ5kqkh2AIONDWSI6vshkL3Ad/KGmvHQvp4PWhyHZSLKGqSIPVGvxINI3n2q5qwZPyV1REi4U1PBxgTPknQ+3acKcuFD8qk0L5ojAC7B5LnPoLGGpfM3Hm4klo18PHfDNSH921KOqWRkrLxTWaRc1/BZLkqM+aHPDNL6b6jWLg6v06rfXxbuV3hIhJGdOiAJMiRUtZ+5CK+lvm1DCQXxVY4k3S8WV+XmkLEI3Srd9c6pi9oK/WODI9Gh2ZzMyCIQNxVDdbIQvdauRaLNe4/JyuytoqkSHAap2Wc5RoNlCm9jOr21fketXtC38Kjuj86YmsPyjKrWnItRn7wnHgkMJTQNYF9LgsYph1/867Yh7bVGC24q+DQ8pglsx661mb2QZFZgzr1Pd+nVfQKaKXkfbhN0SnMjE1B7Z/pe6FVy3OmP+/pBESzZbfxWwTvtWvbC1U0Kr6H7m57j9/z1XTblFQrNOPQuP/lTLKk7njTCsXLnKQnZBjszIfsFoxs2yX+G7KlM6tOrROk17l33Y3iNeRlOCX9eNaCN7zmJ6TrzaedaVzEEii9Wr1D3hyx8mz9APvcqHJSDqQHew9FjgBXvEfMHsADf4hSPc+uaX6FUSy3W1DTpDysQ99Udi30tz3nXRnRLYKbMp6I9T96vzwikim8d3isakAQrlOMX8YPoea3tHHGH5jcIRRdIu8dh154vbAyg/MdjiVoOervsivTbf1u4RU4+MK7/s8Y8N4+tSb1whJYad/nE1l6vNq6EMLO8QWFVa/oBcCiV6HjhU5ZRZVjhtZArjPK5v/noq5lxuGuvQIjhIEJ/cyHjK493+STBIavaSHvxSPuhZdWqp0FNq7Wv3sdf3zV/ablV3y4rWVsGuhxMOz6oinc5SQyd/7HmBrrrjFI+z5acwcIbRg98Yhddr3rUDjMfFizWpyxs15Z21vukFjAUK0XAFshOyZfAsnHSNtYHCMdWIIlpwqFIsyT7sQ+1BwpyMwUfZVM73Y8jHF3f0vE0pD1O+48284uzY1iShzMTWVTpfeIKYbzMheatZQYsV4zpFWHploehUeR9ndOViehW96jUyBIplXcEblQGRIje8DHovGNLjwMicjjBmr1fEoN8duEJY/nPplQq9L8B9zC+/WQe6m9NtMxm/PbHIbaJ94a+QBRFAtzWQrJlZszGxC6YGtL9nQ6gXtd3ViRmaO/BmUm46tDy8aeDbCXgjPUEXMJVoHNieHKVVru0szGFYLEpcd4BvDkWfLhljQiX94f0i8sbWggw0a2uCr5I82lYg6e5NcoNakKqNX2THpYk5QWCS4/+knVho7t41v2yBGFpiIhTuk9DmQ/at/VGobwvcOZ2px+3NWxVOjkrvVzwTd6NnutsTs+xcVFWk06zRMk6/i72rvCoiDZ9J/guPIIIA4/7BjlmMRufMR3MDNgna/6n+Cy6HuDJ6uigZAOiDC5ivAo/sqtNTZl3WjmefBC8D29b0dwJ9sG9YcAPpdTzmi+FjjijAlOYvrX+FdK3Id1gkpypAa9jbhPEclr0LvxVwnXFSSpw1z0IgIPwyH6JaO9p9ZGOSEtlSnXJWCM5arW3KCV7tjVZqBbPTj6Xbnys/7Xl5iUgaTloXT4UdiLScJRc78D608u81t0thq0xs1fqpmfS+yvu9aeO39r2VGiGSlnma4IipaESH5FCGFnT7u2rshPQK4CFBMby7vmDRbAUEGZptznkpHSh+rrNpaaVt/Oi102vvtzy1+7qC5bzdxx55fLEh5qKHQl8MJZ1LsUcpGKTpckl1TpuAm04eKz0R4DlwbF5YetbSSghVjYiq5pbMPv0ov5OzoyxLRHE97XNh+8Flz6aS1iV0DWdGgQY51sxohOtMzFSELjwvfrT19kobNBcSGyPOPrDQAXGB3eZ8levZJ951FGQqwRU4aESinQYbi1+CTcW78pkYg/u6fV7qZpwkxYUhD7nBAxhLkDkDDj1OuO0833/XyGKs+0y1ORWMPhupPNfUb4i0DpiNAaG6ecvEs2BwqiLBAhOc7BVTyZw77P6Lpxqm5xjfy+WHP06dKOsndmz0liCjv127rwXuD7j2GDuKd1ktNbPxps4CzkYMUpgD0S/3K2PTZPkvP7Ga1HKdgZeU1WElQM8OQxsbMFeUFakL3H2i1GlOK+v4uU7Y1iRh4LHLsAftiuOzqUg6rzZS+jMFqLQj6fYNjuwXbkNBT/0icIA2L5TsOo/8lfhBLCiKuLrVn3htmsQfceXA7W6SM9nkFCyjaGh9byYHag/mrXkHUAu2meh3fvMrlpCLMl6PQvpGi7TPYRDogXDjGgNJNyvd9t+TIcdY5ss0J/xIPiZ/iE4DYBtI05IoOJN+t6qg2Llo09nIFLP8bKt/fNi5NyMU3NJGNa3mvgNu2aO3yaCaQ9Zv4xVMn5BuwS/nJvbbOzdCLLbOyLy1bIRjjvo2Rc5VU55Z0IEShragu96dvUURTVfzV5qRJq239STKPHE0hVZZWIs5oferRjb+NgV421wD2pGVa6P7zT7QE8PFpoCPPpvGArsFpsccgDIGsBdDRR3eJPtK5uxdC2K4CWTYy3BTIL2Fg1wp5aHeqi3Nwse4DHBVqLpfRMUejuLRVkwvF+zdA2oeZz4LmfbimuQiPYzsZ1QezlEQTTdRAf7jezoG4JhQhWyCJRJM1nY7GxFcoyaSqRRwchjTiSwkxtfXel1rmXmrcfg5XtQUm7cu5qzc5VwRrpJZ5KdeRZI3wO1ZRDdLLl1AXw+PikKSKo0EwVFyGW6A2JXR7j0ZlSDR0nOqOQnESsXoSWGTu/tvMc4X3IoxdaIKBAgHvf8lTS0KqXPvr+UXkC8EBU3lHrABM0ux9kdDm3C67hXSyOl8bEvVCXvs5hcKI1wLIC2brI1aEmVB6oytDVhCEJ3Hg4MDAAdGTZBL4LzT9P692/sEcA64+cFqcWSQfAD1+p5nG4M97zIslgWMR/Cdqu2Y7FUdH3T+vSN5xz+6Pb8PPeCFxaF74zEiojFu+qmaEHoSREJZyFfl748MfMGvmoGySYwi1jG8tpknf8ioyqXROPqgBLUY1Nq/wQkABKW3mkI69GOZ3v1OEGAzAAAAAAAAAAH2Fh0B1dkG0tr0x8C6m3b8nq76vYPAzuqrEYPbc18Mf78Ln5Z4JKa98fZg1Mx5gLjRYEKRj7BAlRRRpvhyqbQyH84lKMOtnkaDxoM1KTbMQ/HNZ+yVYnFgFWz27dONiXQRtOCncZbMobnKwcEcV3knZp+hdD/i3V2PyFTsyoqrZ2JLkZjAGoMQi04Jpzrvq0aA62A9VXNeFe7kTwiZE43lFXrfSaKug+uaLDF7eiAZ2ZmFZ1qHTiRJcDfdXoQJRnWdmMYPLtDFc9MqJhI0iTeKJOE7tKUPfo78OxMhXuHWlRDIsfqe+VgafGeBolLoMBFtZL9zIRZ7t3hc+f/dhE+YweqEzMtp9IWfEgPYUQRU3pd/lUO2/VEI8EFf/amdR8gZv1fKmRyIUOUNHdECdNap5Pdw6lVpMlqw+VsBNDLhvFiX82+dAYCDRYbgWgwVo8mIe4McjB4wKRmpXiOu0PFsdrvP4VqNMwubC2mR/T64W/pXKE5C6Ny2ZLTnc9vXqZVdG7bg3tw+F5k79YmOFcgQ+kVFN2Vzm+EgVwRIK15fWFiw6fvTrQzDHryR67CttPMRUXgRRbgph82BYerxUUKtTUhP+KHyg7aVYjE3Az0z2QkqMpk4XztLz+iPNyNdGYg/1JMCy00kmtZkJxlvb7pURbosiFsyHbQm8u6aKuEO8SW0XUdr02jcX+buN/adH1FE3BSTpBMjRz2QsdkP3ZUyViILN8kHe68ttxXhtspMYloRf5/6PjVBkPL0pwOeZxdfhdXh6VS/LOod6Rx3pelGqtpwh06Zg3pDTf/hwBDLh4Zmcy7HdlUQmFvO/xSJEkwzKSmZ6aF6ktuSSkaQMgMeY+L9igEX0+IN4tPxhKB2fYK/b5GM6d1Xd/gI+DOlQ2I7xPJ46cevOK4rjT/ME7QnRR2S/l9aZ2dfd2o8tmHJeMBWnhhOc6r0siUPXNTUEVh+Nj+VEhrvaZS259VncsIDKK1N957kV5tsLQLxUr52h6GTmN1urH7oJkDC1mmHxYUuVwoHYiOPPZOxlx54BGqwk2+OV4/2jzAV1mv6C3B9DSkxqb4gH6he4lzQUFzFeJexnHlo4y7nuuDJWwYjLTe7Ypz45DuqBzbQQIyPeKXxWIHXyIM1oiLwyRDT/CoxrsBTcEA5r+2mY2kjX7bcQoWmQz3iBpcnjYMlcFyS2utQianFMIjXeE82C7Uv3gArg5vBuZ4HeKXJSX5IRBoYNf0FUkq9ICYr6CxfI/v20Sb46jLRFMzDtw7qQW9aCaXRKbePVkgmDSkdx1HxYEUJ+nRCNe1RxYzo8HS6pJz34sL+SGrPXeUo1t9Y77AQUugzMLhIFhXrb+pNh48xuc8Zkl7oTTPE5ccLaLQREAYxwdM8kxVEPaE2simE3WCngTmt0Du4ATLMfL5639ttm/qSVmbY/09k9SOPJ0Yf05HUajvNtsLEnpZN6aVq9RNXS07ubiEO5e+1QmdUw8qvJ5dc/rBXqmKBOWTNfT0uQywIjanhbwwzNVI2f0Xb6MDbltKYCuglx8y/79NGdOTcrN0j6SUMB0VAK0EM/Ri/PbzJcI7NhCuWMVvKBmJzAa82fVRcvJqUCfXYkiv0QZWTd7lJdJHEf7VFOJeZHsZJ5TawAbnBVibkJf4S88c911W84AgWj0XiXrcL4pfJIMI+qaeNmeRkA0IxmD75OJqzRvC33R+UJ5PNfXBrQpJwqlTTGpQi2G6fPlkpw3ac9fVs4L/OXWy2oP8vpAw7bFMDP/dTd0Wlsdzrdx9xbag8sS+0a+9dQFMre+JrXom6+LtmIU2p+QBCCRRC1+ge61bvOHT+oiL1bdy15g5+8w/KuOTSQcTOEHqnLeXEAHLyqdj4BB6DkbLFPOCb1z4u6Y4NPL9s9o3LMT9G2prcdMBv+LTMKKOclItJGT53QmiDEMAA5c6jF5MgZVg7dsOjwQINX/wjCqmrStSk3LRA72200qPLZjCi22QgHWdb30pjSg26djkZvUOr+1UOM5+hv43RSxQ+SuEgzYuxmTUG3cqGXxbE8A94aMQAbUdZJ4iTsP0oJdRWMWgPgITQbl/WnmvOFpSW6Lu6IO9vyPEpwvsVb8efltkmzhPToUb++aeWM1Xi4mXl9kRlE+FQTNu5+MBrGRXjq46axbV335Gew12M8GMePq9sDvkL1wmK2ojCzRRkIrIVF35W3mcRoP4HggLTfyW5JnM/aIoBaE9SelrE176q86eVVGlrosjrdKQKDQ3vNvoUpV/5wiJTiDUiLQDJuCGg5z/Rpuf3QvU9RRhHkcpA0d/rBXgofSaMcIOBKVZx6wIry//+IYBgJyEBolUv9tGVo9dnkS2IHYvEUdxhbRWo694KYZrILGbLd3l7N/L6oeaAQ+elbBvSql6Sl8op6XnTrgTYA5sTTi2MUg1AZZNHRtDNf4it8Kb3R94R+RapSM7nU20SyFDqo5VoDX/7XjlEAxzcRxBzwJWVL3rKvg4uhhBeb3C+sXNy5UyAIwE+Q+oyYJ6ePUp1vF03vXIja1DrDstrESq0YWdDvPpAKonuwuBa6KO6VB1NPl+hzKPcA0ZY6xIKnREK6vZNw85kp37m2eYvO+CBdzCVZRWSo/urEB7V+GchgoH5SQfDZK0siVmh+fKaY7RDokctlW6a4OBeCDkrJKucQYfD3NWRmy3XZCVlInbitu2d7QPjN9hD3Na67QbcIfr860tck+l2mv+FTVK5cpS/S/ARVEnEpuitft6T304YAXjjfX+aidVi5w0RfXMAc4ArZccCX7DsJ46BBpw+86qjwgSirGO4HJWYh9TL2hzAJc8/bv4BhJVpu4VN1cxEHUmBpPapEQtDN7+r/Olo6/vGkuuP6fKAuFmbzxftn7AuxwPT5GZ5mBGj7Y9LnMY9EIJagAdp4pIha6MscpgpxHuzDCNZFY9Iau+3Ic5Gi8pjAENL6oW3irytOZ07rI2e69fkQMwEOsxDNY/PXFgYFgP4/hDvmALhbNd4Dziva2uXJrqMBEYkqE/DFeLSIDu9Wxn8+KrV5x7O+hm79hSQs6IALt2bu0sNZkQsvxNjzs60fVr3utvRv61zTuZARfxRBmu5IjcI9lcfc/H77mRreJ7tf2qWi0IIKTZhk+gcjDD4uGhiUJszCTzcS9qQZMNgGmWkJg6HaE3Wa3KYXDetj1MaG0CpnXg9hKyLZa8oWZiv0VrW4OOR/KLh/bnPGcSiOSI9HpibetRnnQg5WC/g7UcjZFZgPRBP61AZKNDmFGIWY+ZgVGs/Mnz7rpL0Z/fSHntzehGdoEnirYTS3KmWP5w9EvQWVNH22+gOzhXzYuZyf3xxLNxXOAfrWNP27y0ihZDyFhDGewkDx4M88eaAbGnUDM/W2CxoMrypfF2Tj2x8QJstDoLmCq6DyihkPFSclB/sjUJjRDcEYddVQS4T3QdZVz3lN5VOsb+ykXFfJBSWi+gCSpJoN8JmBs70udP8I8jQLhQJlFHUTsl3ZIj12TI5m78u4w7sKd5kVnYfdc71vH/3VQU0hv/vBLuS1JS1dMFA2hFsd2p0xk/AH3PoHLcPj7Xrfy2QD1iyDJbX6ZH0USInVgpPZJ33AjnE1bZZSxwixzynFkli1hMmxnfjO/8eG5bWWEdILWiOD7ZBKEsfL0Q/eaux4ld6T0wB7H5NIsSKM+1Znr7OXE7TYJOisWbjsrz3dXO4i+hodV6IbBkiqIsrtdqyZ/1BZ7G9zHXZjr7JFoO4eZG87HrY/lLmS6vFpuFJ4FxOdx6AfRQzzTJDBQi+5kp9Hu+DVEFo09VKUswXyHCGNSIKVbydzypBAcDVLauwPPIPRWkSkSqwTCwTdxSEkAsB3sLH7A78N9DdZU90dIB3YUBFDstu7FEUkccWuf7SXQT7/c+YNZ0oHyK57AGsi966wsLEkA1XKl2OPCfNYM0i9FrtAExAdBjnoGwz2ES6J1qLPXuNoZNiC+InsHzaalCT2ZQxm9sVe0BJ7xmbB2XP5pxVlv8mJ+NurDSmnaE4XyHbsrAXK0p1/Wg5E7wsExZDNIFJ1YMnONOAs4H1U6pGeD2G14/junq10CMY6rwd9cWZeV62JxveDrtnjYZ6gi5ftPEI6IEL7Imm83UhOtL65Mx9HpAFMna8Y8bdlj7HMISmmiVVZdnm76WsXZUxAmC8lMMWWSt8YQLdrMw/CUHIcnJc9ArASYl3HD2Tzj4AQF9PtsiK26QCe21KZtquydt3moEuXHg05ht7ca7QhZT/yHcKypjI2Od6QDC7y6fpjJM4wjhPjxQVuy6LJzuxXfQnb2+t1wMgQBbvvOxzJYH2aRz9/L2x3TKp8QwXzNinh0AI9F8WCxmPlFA7zGeUlXNsxoTOwF+Xv8AYEEQKDEfqmhgM1c6gTWA2OquFd2uZh1j3LV+JC+4JI6LsEp+7JFYhrQbpdSPHpJS6Lr2CL5OgvmrWzatj/fdzFbu+RvOnWNFByTvD8WmRb1u7s/nFcBcfKNGSLySppoWu2esUpX8EpEYNjHIlDYjqqJy3rS7SbBc2zAEzm+kWUYSUR5a4Zk/A185m5elBhkljinlTRlEuGAWk2cN77ZabWW8KiThVRaGwm4tm6pVZJ8nmqPe4V3YWrnmjKf0aUIWGv6ipV4lIW9b+1gJ9Q34p++imXsRjpWEbOLLD5HrMKx9uVW62l2/4TYk6UomyzVDUf78k2raY8BfwHqSR9rhjLCF5yUdoX0CWNEtg3QD8w9oXWn/SQf6dRDGMamT3bjESe+ARqZcojgUpQPvE4NKlvNpwssAQmuoLF0OzauW32UBg1jQoWr+s1jsREYF9G6qXcTcq8n4vJsdNXr8CXkp5ZPxG9xUu7Z83JXT2v+8SkbR+GSunGkHVsp+EJCgTlbn3kwAyyZXMK7ju8KH/fjaH6oeUCv/eUl18PHmPmqhrLO2QH7Y9tu36DsxSLm8HFgR2KDHDzsNT15wdkjVbw0aQVk33l4iFFaY2k+LfnKnLwX2DkcFWOalxBCcCOKgP6JM8OTKiMLtEXDUDO5cezKdzx003pD3kqF8nMu8eJKPh46rTk79FMN6qRu/LkK+kEf26eN/w0XxXYPF/tjqUegW4GkUITjjSYDsaJ4kKzFag1H/ag10i6D2LNFssavwBajCh6QFVkCujIbyf3YLOcnup2LeX4npl49YfOiIF94LMU7kV3xxHcFBQcgoReACzwlM3CLcCMLIeZAOM1/wi24LwGTuMZXrv2hpDGhsZ9YcQLJaf8mEJgpeJE+tDMYdCYngHWaAUiKJNY9U0ESRm99GvCo+IFJyQ+NyHlKPyDtTfg4fAPPu7oLweEZpSqVqgkBMhdEVlF0TM/pl/O2mlC+SobZYeRjTcJQunyqnZh5wXW4nh+dtyLai2xw2FmV8Za79OUK4TF85Qm8Umyz8VBp7RCYK0r1EiyioE1z2S56qGHGE9z/SGPrEKfdnnBD5EcEdWmoab+gGVm/AtGZBKXXcElC9pZckT4P1rJ6rC2qcryhiY1d+DX94qMctSafA1wSzVD5dXU5gv/orgUegtc4WgwXs3JhSDAwAHA3Mc764e/TgpyCg67TjJUtsaOqwumI/sG4V28Xa3tguIQb8SbDBmNxbsHyh+vOh+mEfqJJ2bI/pUTmBpgr/BtqcSgZ+0u7RoEkma0PaHcWdyuMroW1IuJaGjlJdC9FniX/hBeCVWI+f4V6Cdkyz1uXZFA2s23E7N1sarDFCYc4T9ThBgMwAAAAAAAAABt1WBAYsTvyELmKY3BRGdFfxqiQbWJhKHlk6SQE9z4FT8MLF6TECKOa4sB3HNG+eQoRTQqJXoUCVnba2NMuA8aoLh/uJ6WzN//CMbcBOAWWp/PhQr2TSPDObeyqciQal8gNLLHGxg+EXlnArl1ZEIQJ3BJcSxLT/dhYb59kcIPCaqLttCpUAd9JnmtKphenCSsafC8vA6IqoqaUO0JtVqUJ9KdW2Q4g0hvWfKkLqzAUehK9PBUntEsaklgOZm+F6/JHsVWtloZOSWKDZoaaGanETflpFMAEd30MiNgi/9bqngRbtpubV4lamBrEIS/ZUm1/fn/2BQxwTcj6pPRblXK8aibCh8aF7ZYBniE1D3qYciJD5+4yN67uRpR67TiiAJjMeQ/GN7uJYHYtf6j09eMvQHJii+qSu6Reurh11E2TIaSWXjSmSBTnksl7l0FlQyHOTyR1oTiq6vaRopMVarKVTGlUj2Lh4UR4joVUNZEfSLxhrBfEd2XT1SENm+iFJ2xAHRiXiHiZmULJDlRqzvQSFXp6oRmUJnXKISb7RYAPSc18F+bA2oBZ50QG+3qy0Q54tuwFn4fdwCbj0ocjcq+gcVVRq01HHnaHAMIa4h0sBjxclonx629pZzIxm4E75VSqVxCb1May0u8GGuKQfDNHwdZ5p7ibiSwIU0WoxBFc7v1E9wVtvMAQG6W+d42/ooNX+bQFqool+ppHnU8ybD8JplBYdsEsuzBbMP4/vly+jnjsHoomAgN28YlpH13Gcb8biuYz6nTFY4Jy4pZBOY9W5sOdVg8hck3Ltevc7DAeLeHQCevRe2Qq5+DVQPNjnPfNvpmOb8ab8Z6g3fQPzikvM5Jex7OpdZMd7r1fYmoY08Dh6LIDWEsnblJS2U6U3kjDTeeAV0RCY7YKoHvS++geKEMbaHZ50iHrVzxMK6xAzSgP53sKgQLYSW1yzXfHGOJdy7+c2FV+ZcQpMEKsBzTL3LDyFF9jJbqPvj7qWO0cJNOvadzeNCRFKmcW+xuQjKr8FQpZbSjjDJNOw4SWyM8Q+W+8LOCpr1WVZLigq+aHr69VKOKKssL2QWp3BaAiRWxSWYYb7/9dGHBOBv7tVT82E1ZQ6bgMKbScDZGJVNtgKkQc7gZ7afet6xwYeGO6Hab+HVqy8gVVqEiFNwuOVBDTwGcItEFkT3eT2adVgYiInerIZkPOdueLxJZHm1NCOykWb4Fi4unn7cLVmRkp2g06nk9JQcqdMl8QhDfQq6nsjBjRsISd1vDVIExdIJ1k6NwMLyIk0di+NmUujx+oAeyuLhr5ewHXtvQqgpEf1gJlT8zBn3MoM7dR0GbCemlDzjoptlADn8Z9BJ1K1Yx7oHPTM6X2vQ7wxr4ZsICrklSmxh0eOcuS3+/kzWJgap33lhwSWO+f5wwlSsdP/MJkX6/syNrQtA0lzUuA5ggR8lrds+pPPV53+tgQEvC3e2ZT2GOZNIXP73nv7GNXFxfOfibpQw3dQepNOYdZlaRPpVVIz5SU9peAZtlUXa9ww+QSfatppN5vLfHS0vtOdjz/RufVwehxfWO+PMNOCLef/qLldk4PG4VNS0bhRc2zvz5+uttYnoS2+rqnuqzlPaAI0zfuXWHDmK3tS5TfF1/De+Z3zXwZJFGZxeOo+Vi3ivyT1fIYQLXsiz/EfjqQM3ZOH0PmZlpyD3+w19g32gJ72uy2jM9WCMfRmFcZ5DNBw/ljI4YvEM0OCzZlH2DqiC5dhb0rDKFw3g1z+/Ku3bZswpEohaZ6dGlksfUsbdRJf2K/XAWd54hy4cvEukfAaK4ezitr3z+uqbQy5voTNR1wnnLHTiSElV313vI1FLkqD1R7OjpbcJg4Mtsj94tR8Egl+ZRTEhqzx+rg96F4PQ8kYEhvM9WNMyapP+2HoxB8hOGvnRavmQ9ln1sgOb/4ho1Wmu3wckR+i9LMm2G9kruP+OtpgqzB1/vnZlXIwNvSbzddGXlqSP29QsV65Pb9MZrJupuRsFhvcp3ZUhuYr4OQwZF+oz+h1A2+qHPhUsdcywkb5vhuOu5GQMXPmBEHLbAJcwWojA+ilZ3OhBLLP1J/krRbr2rOIuz54dpdaCOMMkGk3LSPjSkFCBRv4dLAqIVo2NXjznASRA7va+LO+oPn9zztWa4iLfLYBVPE45BGoWUK0Qjm7v3+/jBmwo2hPvn44wnyC1xqia8CygnS0OBAK7OuICgSvmQbnX3lf0vPdkL6oKDhESrh6CWwn+hrv33pZXdSBPoxJ0AXu5Xyo8wHs4TR2AZxlKczss0f8Xv1K5rlNeSFTU+toPvDbRf+RB5EgxQz37t800Kqqu9d+YP8I+oKzAEkxap8Q9B9XB2Be4+m0DyhqJ0PYeF1lQ6qxfwqEJoR+A+u0EhRudMzAmvpNSNGK/OJ29jW/4iZ7PzYcYADhrJhwu8f1/nLr3QrqbSa2ObvjFtPtD9VX4pzNCpqUe5CL1MLzAEw0IY4Hb4gIDKvIsvuoA7kBDvr2SDZ/YTbfTQRpgwuSN8FN8zN4rENjGbpC1+RICByXzW09zsPOkaa+vMbZylPAVdbTd/2TYxGQiEaEVQkBI0LMsyYp7rwDgqHpn4dDphZW7MF9zW9GlpvuKosn9k1wm6zo1/6PrL9l+cui3HqSt8RPP4RLQaN0+OR1DkujwWOiK8vUnSdeGe59JUoC1TVJzzFclWQJOXYvto/GwePrjkzpBEW0q6UwUQ4H7HBi+MNt7zvjdZs4O4Ugf4ss/GYoUFzf2G3xl4hQDpr/EWRZScJ+bzAlQBDSBm34znQDKMRqpQkRcYYaoNdcGpBMYxNsN9yeqznTjzsQCwJ/TPvpL3Y4wUfWvRlNvsKLf3I74AFaR+XowlneZUVBNzGIuetUQ9JXpaZe8q0RweNzBswWswaM8Qhoo0pr0bZ/uokCSQL3iBGhT81A5NuZV6VUSAcd4JvtBjERiu9pwyLwRsaF0JtilIyjsxqNWi6p0qIn9yvbz9vtljPIH0rE0R1dojfeVLBCTVas3DvJ6xHY3EsLg9CIwqTEBWK+yOAYnTRCM8vMeruGunNY64ZzxtKvb5+3aqN02kdDH+4R747bVy4ROVjXWhywqxqYV57gs1j1QMLmhnTacZnRAhmMVjIkshwXERM5VwgVJ7OLdYzIDssjnyGT9gj8AS+u3EHxVg2wDx2HmfoGkAZpXqquU2PgNVIE2xKJ6irKkbGSW7ICALLh5FERcVhcAMrRru92eUL2KfruqWuDExJASHUO3XCv6XfaFFjdKFrewxBZJ/D01ztVimJ/NsD2pHDX1dmQ6NexU3gsetFkQzsoyvVjjH6bw4gD3Vnld8w7Wf6dR7vgZmv/PRWS6BfP3bk7D+XGiXvU9b9UHNwlabDrerAjkxS7Pn3BKIPpdPFwMRsp/WpnwmR3ZIpQ2466mpgTIlRbUUc4RRyxObB3HpbZYjNowjOBp/tuj6B3KLnh/vYtIjYTW8UCH5BNKqZGp+WGdVhvN15p7Xx5RFSE1B3/nhmGnI9EKA2TPQOUZ2ZSvWJ+iM5q6dQvO2xVXT/O3nkjxup5BwRQdIhIXFaBq25gZTLJ8lCal5sEIYNNhwOFJzdjr1Uob7pNlk3jACG5d/4+WE+m/QjaQCMJx1APWZxEnWdzgX6FOTDgFrYchToJiPM9fuGQ7T+ilsYwxKeSoqB4JqOAHs3Zvu1HwpDgb2EBtCtkCbmhg53F/ISi780yrOa67LtvxpMNJjCJ3uvqn7qKALwkrht2tomrIvmD8J2fj56a8jC98+f5MLCIDkjB8qLEBBXq0oWxkC4crtEGEDelnCl/WUbXE8aHQRldyMLrt48tOJK/h/0ZIZ0MFlbLAYqPDiMYNDvWI4L/kC5okEGfdvEIXymog9SDbvMTitEKe25BFCm9I/cTozyNlQjnuGTshL87jQHMr6URqExKR4r4liwRyYcIRxMonN44iUypW+Ktbb4nacG0ldJID2TV4NgHm4ozOyU/mlVC9FquP2siLKIikIV9CSblbcK91IyFGQFTRgA5Q4TNb6JXmI94ybk+wAVkc2RrCdotuf0ge4urFnn5X+CQplBc1096bzO8b+vsPycPswkfqlJFDddNfl+UMku/Ych+C+fqgLutsXe2efYHDLWqWno7+/NfLCmoR1NA5kvwi44+kbrQqPq8bBGAtnfn4gi3GXvoC1eMKRPtxMPURICegcZ+zscZru/TyqLaxlNW1E3spMxeVfO4kifFEKnkxZRH8ngRpDpFqRQ5Di5gqW/OR+X3QxpW7DZUVPeRkWGYubv47w2IHT82Tp3XfV9JowxV4EHGBm3s//LbUAxgwXwRTKl6+WcOjb2e1zrQP5r2LDR8BMtexp9UJ9PSWvkxm6IrBDzCFi0C+nvnKb1HFjcZjZj6xE8hstQUD7tcsIFTHxjh2hXvZU30simXrF9rkVflDGzYhV5eVcHxofoQIUIN50tJ9KVtWc+uldo6OO+8EbEMt5aoexUCnzFr6Vc2TMIgqMCn7m74rPXWh0y0WPgsqbqJ+SgInOEIUPEF4E65LmttyXO+6L3lkc/sJ+ZC+95a0FihKiggXhb3WwkoVhfO3nVieHs3UTfMs95beWK4Ix74OLTs+7zh1IbvcqZ6fa2o3irKJFeDWfQps8m/LAmNy3cVFkuXA6e5jI1o1BRofB0Nm46/CQzGQhsAyqdsOYzKtjvbOC2AAw8z3HJf0C248qvny5yLyHfpHypF2jTxhFTadp2FCtDhDz/pvTzb6A2bxwfr5tpdOVcp7304BrflXJ6GoMAkeOiplnRt0PnNpTP/tp10oX/Vltzd6hhju4isUMrTsrMjZWSZx6A0yhjZ3BSvU9811wEvODvPpfgGjgnCz62FHQpPOtPQvb1UfIw2y/GVVNMCGD68uEZklgVBupMLzTwKIyTHfdgM6aLgK9RfU/PO81Xy4NEJtDys/0Sn+J5K1LY2j7f4OhGlS3Nao9cxVfzCxzkoXbdmS9zbpOu+7PMo0d+l60Lv8UCPqWzRP78ax5pCtEAXCqfHwiwjj4UzYHKex82ySxjoOgU5NYe+U+y/LuS/ag14SECNWXqk4XAc0XWFTA8V4DGnnUk/Xjrw8f48OFINV1PTNoOaiTJlJcqEB3d44Fz4PwI/TR9nfJPe9Qp13A0zabwT92V3HiEJVFb63MLs67prCAI9Amu/D++H0KE5BcUHeE7P2h+q+E0PuErE/gwZ74363iAm3AxpoKHTHJ325n764WyrkyPrYNDofE7HpVcC5/787uCL9zkrRp3kfM+AjNBTKAWux9dpahtUmDjwD6dfgaAw4eqakHZ7SIWMPeduFvBSFlVYEa3JyXx3QQq0sXmYr36xPSJcHnAKdJKLs0yxjQhwJqxcxERBAFE30BUn1REJQQ89i9rnhoudQP3Dbjt+RwX65+eNI8ZbImmesZVrtgwGvR2VusdyohBrr6+Um1Gi81eRsawZp7qRPNhKj4IS/fUQPKksZoZoR/9+SuGNYyprTVSb+VK8QQ4YdmhI+2S9XTHxrN9sueJnofCAm1wpRN15xgwMAB4jf0PJae8wWp58xpSwOSOn7wGamyxQSY8EDFKDmUcSTdRGckTBk4JM0iVmfUy843jkGpGFqr6DrB3t5qc+26ljN/t1PkIF0kOtWaTgLMOCvzQDOcta9H3Nc3GqtllTcYKIE5z69WU/urZJcNQAp2IrRGvlJ9ebmnCIS4BbQOCB1/U4QYDMAAAAAAAAAAUSVGAH75+H3GJhTDFfNOGGd2FqKfk2UPgMz+BPC25w2Bt8jn73HVK9Rc+p8GFp2xxgIRbI6krNRynPGg2crAkHvIpgFtMq8L5Z3UW02doMzBJDsYVTx4maBCKMA9EIK9uwe2Ht43RciJSyn4Y+B/xvpEZvG5a1m07HwJ74fXmUfMIyE1ghB1Ut1CFF2pFDFrcurbVxd/PshbUz0PnW2Ac4C5WOgKT5V7YkIWv5bHYQoEl6gEjhwgaTTzXPl2v5nQBpQ6e/+zyC7E6CgI4Avtn+Qudh9uC5/It0ApvlHqLLaY6d62Jii2SI6KUwb7Bkv6z1+vOZXwSPMvl7YOyGBnTCp1webfW2+RYD6TThF4CaGYlGfrlkjC/xSMyGsmvLBP1UWJU2ZMCQoR1TmjHXkWnxHMeXFsBQMtgBpETMU1Lyt9S1CNjJtlAO+0KDAAa9qqLbta5BDJd1oa9aUyPOxasaPjYmDAILFKc1UAf3LUU4B57+bTIKo0diUoJsf4ypJlueXjgdAK9iYJ65J+0/I1vG18BgtjAX2B1vmdByTMhcQju1SrodIGuVBQadaApy6suRxdzCi+4FhCqwyCO5FgQBuSCaiCVB+vP+mAToqjE8oUh+aK0SFByFuOcxv5AGR79ZI0A9Nha5Lz9SGRcSUEqH9eCgQ3JJwOTP5lpfD4sEIPgFRlPLJ42p3PuH3Mw+Y3YhUXq6jqEfPTokdsfAnFGGJLCUJOWd/etxeHeQsNuRenFPDObQ1PVa1MCZczflebtTuLPHb0ZnOUIS4rTtZYZyijrLdcq8B3X4oEW6u/PQxT3YJJ2ubT3fACGR5nh+y8tA9tiZAV3GgkI+RljMJlqujkozmterXH9SzEqVCkFUnJf16nzdddks3KFhYA/LcgwmuqXxG2IJ9OZs3SuwvO8t9WSW1pktFsvKcO73CdtsmHxU4nWCbDHjS5ntc9AdZSVsne4bKVfzZhxDSDkrxPQMi9z10+pHuFHvsgOHX3eA1PWUp7Sr5dcVk705e+6eS6FGfL6zcJ88XhPtA4nrR/OHAFA/db8pUIhtqZ1e1N6gwIH2Z577jee4WNMMLiZSiGcnYE7+IXz8EJtFh8rmPAv/ErZtWLwdXO33i+WSD7XutgGS1LhbXH350EUIq4NiWx3gxV3EY1C71Mq7AOUTarDQj7fxzEo1pC7fZZqxfnIFnWdGfblO3qH5TF8nLYOvZ5u90E3Q8PbqupXmWbX1OlBM5ghwXAUxNM/l76l7Y5xhM3iktFcLudCLAWvy/8Nj91dCheOai96XtrHvfCi2+qMhOGxDy7WHjm4lXZewt09K9bQhLt3IQZGN8TsLxyydHsLmOpaQVeDF/HJdy7BI67LV6cum3y/zkjmK8+ig60OSeAmeNqFtBiCLEw5xVvhkUAiIwBGMRz4nknFkT6z5MTk8u1YfpB/A0lj7gQTBbz2XonRVVdeLs7fYt+5sm4J7LBiOhljHK9EyFSIMf2EwNzHHfVUfHPxlPcKcTKHuL4lIjEbbXwiowlE8s0ucyLv+J2EdieZg5ASihyVHhdu+QGfyRQ5L92Uzq6lNyraZFzhoE2pnoLzDohB25mEQRcjEv/oM4Rg03ai+BK/NKKeCDnEvSZj+2A1var7e92pmUmlEXOi/+4tqFwJ9YbBKcfhed+Q+nhh5KSpb46xsh0e/mLJEKNa103IExYK0j/noYl4HzAxxcEAHpRWbH1LZ3/AeO4nA5KuPtYb+uEJiOcpBshdds4cLhzsKvMF+UsUQCMFvOL8cmn26wCOQmWKtvImb97eKU9t18RJF9h6jiH4zdm4t/UuFhW08O+8C19ekziCqhiwQ9rxlT/Dit20eq7ftrCmU25JvngVYxQFiVVURi7IBC7JxadI5gfSYZjAbiFw+rN0DOIKS4AcSuMY2YkkPqAzoK4eaqzrBlN8yPQthQlD475UNa0hhz8RYPbEtgaz7MtYcO087f0QpEa67/9phG2liGqk0M91Ay3ztj2OlV8uxMADv7R4grJ0HBmxKnor4OATeKgMVJorbITtzRXHnQXUQNllGh3IgqSpCJam0/kxk8WBEseTNg1RX8j4kcq1sTsL8aDOnyMWE6yzvqonvXDv1vbsdADNaGxFmd7kj/+zvT2AjqBDWqRwg34qLmkYs3a8wqaI6jixZS0FsF2PhbUJC/vQtd5QLJ3QFpnN5GCYhgAnfopv+xEVTOJUSkKjUXm/MObrEN6WowkhM7FNyPblvM+5cRRQrdfl+FW2vgtMu47q415Kj72i8hGXy7XSjKl53AIqtSbjGPv/js4JWc7zYWMnMPCPUtlRAIWyvfr/laBFojctH8iDxAuOikglufNEcfcKY1BUPAcCYa95c/O5S4POYHktM3rI6iQgTJUdYLsDE2dL4vVqgvjGV9pnJvCohHwaW5kWifxShp5FPcsI9Yi/g09VgqPtivrLTZIFCUn9FfB/4o+3+dzkHVFimQFPKS+AxbHC4q+pYODSwiH3S0eY/OSlV/mvEIPtUvJIbqYhVHpOR0ivpt+iZu3bb4OdifQUkx1QslV5au/N67xOh1e5/oMgC2S7YYJPB+tcuJcyKzc4NR+iEziStlKTfnLNOc/unORjwDZXr1T23W7/D+gzX+/e7RIWJVw4y9mVu0KuqVAAmTKrZJbUcddtvNQ7NWsl9LwRuM5rIbMuyx1jzlpscFOSAUlIpT/JfYvt7CTND0+FyiZWttk3rK3iKUQ6wXHTo3+bpKViqT0/YSJJtF9uIqK62P2iJs45MzCX4Apy7Gnw7RGM9V9ZOUFFQ4vlkVnnokeJw2pV1B/TD5zJqe9lz9EK7Y1ysxsdDf/S2/B1gx9pJiewTvc+dp65Jz6f1MO3AtMdA+QQzdpnhwT1wBO8fPOjeSZq6I4n3XlkGD/v913t7gJn7AvxJluLfZmh/PO4qlU37SmpoKLd7nexLcLzcAVji2gjRBpLnyahZhF/DxBkSGlRchyZpLyN+7oPCQM/SwR3IVJGHmZg3tR9z7hGtoGhCuTsDFLGtghy0ApLbM0/Cbeo7sQI9ogsasrYlp7cHfjOuNzjqFvTsoAEm2urjKaNqul+iO9WyzCkp71AD48+gv3YEXq3Y4W7tgxwou1QGgWfIBKemA5wU87VQzLrmtsZCcrAFM19C71xw4tuyG9Az8Sacxz/CRtYeXNEofS1PcpYs2gBO55bm+wspqP+G1O1w2VHtQniQ4UXbJ2y1rVWZI7FnxAPDAHSRJw7tfRo+F8x/wxRQ2K6yk/eSdcxLeXaxakpobxzDrLy8EefQ52LPt7FF1m/qhL+UDExLSICeDFMkst5WVvoNvns80xzBFrKEV2dlaEvaxdL7ynkEu2XPLgFuy65QoBJe9lSwpaADmWAeBCXgaBNwrvIjq1R5flQFeOOJoDIZoIkgbilCj3xigTJfUS591V7wVmpF4LlQwTC977vn6OUsccw0RziOBUluXtEH0Xxah68PbbUjaAvup1cQgtDo+kcgBEkZz8XBkUkQYwjHA5TvELQO+fwJ601RmwzTcraVeHivdOcO/Fw9O0mYpkMFRDIARuUqZmCB3v5VXTkQJxDKvV31/lZ8iqkP3L7OdBEmAnCO9x2xjr4gDUs/sSKuZA1qzqtAyP5j+gfRA2nr9o6/koufzD9gcQP89DK56dQh4J6CsqL2wtBGwIUHYipAAGMfeplw62z4GeZh0ia8Jm0pSBSTXG9PGVuCkjwNpMw4ucuQwqin78GSfiYZ4TB7XZPLXFa+uCl8qibj3iZX3WqTA45OhXHEe4vHK0oRjpq/TVj+cmChoNTynFfNWSibz8btaoReasizweS1V7D3YxxZZ3Ogxdn3CWs7OGoB4gbM28UvWCQMj/BvbLtkM8srmW1hQ6P9wFsKt9pxU6Y3OBtPZZHdFsZr7x1uro/+V7c1fffP0zfhnBrEEj3qRzkeV6Gxl+yQV8TqnVdXP+Mm0eyfatbIoWyIpBJbaLOQaLsWI3yvLExKkOA6EdzxeZz3au2c6Z9IxeoFgXO1+qWlzv4ojlaz4u8XErgjVTRSh1kUn1V5j4122WUpO50qfp0yHStaYWDJ1PoqThxW09Yal/wlcuXL9sv7qNv6nsavpRgnPdD6jFx/du4og3qnXYoe4raWjUMOUom1EYpdbt6DThmqBNhft19JPNSAGRuGSu223LBM75HABmrY3UHxNlHsbh3a7QoC971Etxe3BYZARdSgsbPMvLChkndGD7Uf0F7lB+97eXnjkYABcyVXmYDx0N7srN/9X+c14P5OnT6uqluKe//+v724fJnM6/x5+doyomU+iVGpDdfNGHMx028HddMgBbF9KZXSa/TgBO4kUbOzKkShyId6wiSzhbfBjj3sus4us7GgesNN3ipBRtx92AGhcGsmiHvBkU6XFz1eQ8rj2sNOgsVtxrwXKysYXk04NLreNjLnGK8J4cP50ofIWDIVD7yNuKouNA/ELz8yuzsuPryp+/Nl+uOwnkRCrLbde9giJo8WUnufTmq+aFK5cg109HPN6+11VFJqcQRGR0OnpczlOJPgFQCTAN64AxTkto7PyFPngWMRQk+KldkC7QDwfbUuUSRkQODs9pmA6bJVrTgDy/WjgemwIfuza+HAFLwjlcQkI4urTyKV4t+jTaKmOY4c8JhG5GUqzG9l5dFaqpTK3q/BJNm7Dn3OiNwVQBQkFBvyRYZ/f/UTcHPAdneHSJdM3YWjvLXNAiGz5jVdTDqgu43Hs2vt3WhLcvVoqfcVkvYrDmTvDnIBxkY9vGdBQ4V1qXIlnpEF5FSjedMNVe6bDB4MLHQQEjao6wpRriHku1h3X75/Vak4JRH3VPjMNj0n4QM4UE6NdKzUks2Hv299gIsARLLMUfQPN3UyRofFI0WdXhqKgdedg9l1Pnv73gnKwYIHD5sbENJ11ZnhM3gk3uS3OJuG7n22+s6kH+faGqoP7Csqe+vfbWOnSi/jRIFM3wH/eMMmCrCCKKakfPcjK5fLP7BPTKcMzg8RxO8RvHMwLMqUbgnoTkLxbwB75wz2CyBU8d2pUTvvANOBxP8Q8VUl1TFIBBiS2h/7wX0S8MiSis0roBBreHWOXHxuyB/b/WAUuy6qkC65RWBnf0uJqKJZDr0BfFtX1ngW6opiPwVoewtufUYuXATMMMrLb7i28N85F7YbXNUedsRxaGnKnewyLBqCQI/1Zt5eF/5t0SYCq6TNXoczoZCnaLsAVQsYJvwM9HkSeizr7nJxLMeWKcNPkzZmnwK83/1U7fJeFLTIu0/NJYsJJOQKLFbYSWVyQvFMcrXaebf3REdOaKurh4UNpvbOIXICI+8g5Th5Ob7z4Tci6aHP7+slqcnYShDYFfWjBc3xbfYvcJp0VH3RmUK5sPIpkN4A9notkJzAIiquCQ7FgdH8U4hooLxw0rzHCJIaMMRc3C5Xp+961vlchbO6GdKxDo98CJ59xEXchLINKHXzMcB/vUVJkqTu5goPMFnbOuV1zL4e7HlwC0B4ocN6wYjUA2Y3yxdeZehlNN2PnBnis8hi+SOW4mgmEK61g3YMDAAfUhnSyNDCo1G2TFD9Tz5FoWuAcwmEtQv6qu8MzC1tYZ1bDp4h+GOVZfxYHKNTE+bY2BMrH3COBFDFzAQX/J1PVIrGgcSn35u4QJV1M9BjTZUTVTMQXqMdPgPPwzXgiUaH/pJUahnPKcH2znchCj2UdyCUB1Nhb3pWv3Rtxncig1v1OEGAzAAAAAAAAAAGHBSsB/tg9JsRWI1HClRQJJuOMht9DfXHRG6XJaXZ8GHLTQFD2jilesC+7zGtznR9wBqTW+qO/TrPHlgTFDAHseeTcjT5dkCt8jCMq6396Z4agI0Dqj81YzfuYVnE5jUCdFxrHFYsHW3G8PgvVK2VShsW0vbMtW80wG3dHKbPsIBJ+wleyVPf8AlZXrVrucUpQg3rpd4LD2uagL+8aKGFIW7YHpjQQFW55rMuv96ujdyXJz6G/gFB2tRLJkaeFoBsc2MCThPaotSaeBgdQ4AtoziTanq3mEUC9uoQ57EPe7E+7jjA37pdmpYKkx/0w0tL+wg0ijSSBNcRSErHZSa2jjC6JeJKbahMfGHyUPXcDw9Z+DOcKODMw6/OG/p/X609bpt0S+BYYpthXpxUYY/5/MxLD4Z6UE2t7Zl0xfzoTD6kK1j4pU/oWSJ9VDVsIlzbOEg4nJkR5x/NHuP7GUvyLUEH7+zkhDave/s3DfTdJyVparaEEL3p3AalWyjcjP407QQhGfYkmViSUgOY7DjfYB5hhtXXSz3Tb8af/lkWMpxonyBvk9QSfKHQvx9Cv6RWgUa+FYxCBjqqcpKK3MBJrIN/azXX2IpAWKkQvtaibAvUfukfYBlVHU68f9LgODqKmmc5pefmJzq1nE2EotgwbXnvA/ch48VQyxn80dJ0T5nVrmVrrQy2c/p/EGdJ8dVK2akEOydyPxmyfqs4XsPkYK/Y5vKNawGEAAoMG4dkH707uVd6n7baBYgS2piCayLZnyZw8QPAzUkx7fTpQbk339aCgDI8bR0KB7ab/v3Oe6SUe1Tuf/5kyb85/9pqySzlbmApjm01SbwwJkSL1cI/wXdaGUSjOrjREvY9GyuJjfvEDNxr6D0AcMWBVG0JlfYy7ZA5PEjsQJW3J+Sqnd/qkg+3KWrsSJGYrBxZ/Fh+we4rONJWaacPyNyVLXCJ9MbcdCqbmi2gpaI92QTV2nOLtFaU2FKkOE2Pqcog94vIkYYZrrZkcwA+wH2kq8IJe6k0B54toipybWsTADQ+wJBwfLcT7y4WDMbIiaCJLWO6g1hDU6iqS3ed/ANTEvDeIOxSREagqp3uSzGvl7P3S7cTY8StUU3TC8QxNbHThDthbt24Ps0gYBfcsy8pEunsmtNkZ2BLZGtuY7JZwGg28GcxwlzYF8zkKjTJcDOEjUaJTkP/pSyrcU3DBikmi1JmFn7o8KexfoTth2EpkO0fmpLaEyTm0HixO7Eh37tWSfChRP+1ZILPkP5Ug2qo93ynsTmTCH2/RdWwVyFMlMirlXCmtglpUadsx9I3Z4/UtnY3eDtkMjiuXwpj0YrHllc/hqi9h4m7TyXI0BvPo07Uj4V+9h6e95SmSf5It2pApKYKitEbVMnFJGhiHcMhyx1uiZgn5lNP0Em+5hthDKA4dmG+IuFOYkGrOlFShfdelG2/nNkZd5JU1Qw9y4CMnb3XTDCHSbjMH2FdwqTV6Slqc9y/3iFsZ9LMZw6XxQZBgStNRtRFaFMQPzU7F90moMgLmZPqyGV78TXxNiOIYJjDrH3zp/DUIsMZ1qba4b+RjquwXSdxSom1mInk8kaYYUIJyJez40UuhjVJ6DCbxf8ea4D8EhmPh7M3Dr7rxKGomD/BTuSK2B0fMZEWhEWuet40rKSqFoAXPHDH2kCPHutO4/0Epo26i/1lznyFodVqqyiQIgw/BwWhL9jLrPcDfaXzXuuX4UVEr18TP6cPmJ9PK+DrHQobBANxGPmVmiyECV+vlrujk9R6df8Z4Ky7adL9r7IEX+4BLHuDaJAbNBF33l+vRzs5aTNpt3LSdDEkHiIRfeTDGa8SWRwlNHFUyCaulTVZmvJN54VVunmnF+j6yTy5QgUH3U5yV5MGWh0ZiARPPLB4uz01Ye9LaAKkoI2pWAhdignQGff5o8tlMCXICYCa3yJ003nw9Z3Muz0sr5RuIjlwA28li+EpeLrWgen/kxy6Dqba3MeHYRufBdzsJV3BizY7X1MZCAmGjVyso5/cRZpWJT+wrytwurWoMYo73lvHFrMXATy+nIiP6CU30W4GU2kI1LnCDqoRJD3Nw1FJmtA7I9LiMF+W72nnI4igSWEQ2PQJm6+joAoqt2In5ibaH4xwKgE8zym+lbQOEEMTm3HmXXW1Qf51g8hiv3F5MXa0qypEE+lFXJTReSERx1DBpA4Ui1JUpyUdNaxKy0FPG7o59i7nW4pF3VHqkQLrQjXmngsZoQFZE9vdBjEuIxrN2jGYGPIgaeOP04jg7X4NADynqX1rq+H2A1o0kuMZ+bKMfkjOk+/eHnqZxECQAb6pg4sN3ZrFEvasaO/EU0+L6IksdRozvx5GOmvEfpZhAVsyz0RGRCcmqp+h7FJE5MC/oNIphVMRQK4+2j+jlRxGPi34QanWlmgiqAWnkOwq5bceKWGnHbj5Ct13AkCWTne9gLYUPwNoCzRhOCN5lwgPK7DZzXfUem//p3pPzAQc0UbeHbmmfIf4WSs29dY2A355vQzgynqYB+If9vZclq8m+rBjnQuo2iqMYMHmGlQ3FjwGSG6vcQu6IUFcSXmu5HDx3LqYt40CYgkPuyKQA2kvztwMq8k3hzs3P/vsk5894ll3kEZQBRZCS3WTzax3qIDgrSkO/X9KP7nfWKo6nM77F/lYmTwuRYgT6lt/HMPl1ku15nYEjGitDeN4WXguyn/IYtHuqs2frZ/Sius0/B6Zg+8F/QYMYySL3ZXs9+s0cPZ/1ORRLNi7lPC+GfRewpN91+5vmyUDrNpCi1x1ohAQbM2yAnBifQp2I9hM9sxXP5UaSDy9/+nNFKJNMlVbaVwfbL4hV4AqI1isVpQiYNPE9T+8BX4i8b3wy1mP4mqnbsAiS+j2yAeAbhxFY0iU8Hg2wCfkZS/0vhX2jyrCLe7QubKGYpb92rVqZ7jQUltsF6T3760MZU55pHF3VJego4Ggw12nMSA54yqcBbUH0UFBylMb+MR8lrtvzfbkf9NybLIjtAIRoh0o0qNlNveFroufACPKrUSvzrENWhc1pn8P2t0tUE93s3Mdfxz/lj8pcbvrIlfdyscfRVujWg7wfcNJ9qCU+1pegVEHaa8UcaXdoOjT0wunkE4T+Ivhl8pY85sEhS+e+zoS4wYsBK5Gr5u7qYvNBVkEVrqTBZdrRZrEo7r/a9gpF6e2BUwfIyUaRNTs3cn1XX/stYNOVEmANxq2ymDEfkP7oRSiPOCTVkQj3KdtU3mycsV5t+PXLbbHFsMsNY1ssvlya0MWldesnHaPnjtGGno/QW6Oqjy+KWWySgnM/V1dM04LomSICMZNZx5HP4hqMFQ1y3TlEcQVzF+41xhszNoC1+lpZElPgfXPE6WjY3Sio/vqgmxvZSqVD52K73BIwGd0QiPOxDH8ff26FmIAPKKCPvyoe4VUa+MbL18Aiofg7GVTTsU/1nWN2o7hcPKlIfPeMaGRXN0Sa1+tskkTo4cHlS8Fl/HzK7X78OL2LhjSs9PHKQ60Gm6yO8MdxOZzibNL2pvT88B8n23WIO5eqDnKIXLCECjtXDix1DQyxEW1fDbVfBqWtrXf8kVL8HaBeBCiaUgHthXb+n4kuKPhUxBSqy1yGf88OPibX8xMdJ5rHixJD+zvJ2Z6og/MyDXtHn+LLxulefD96KscIusZRWKmjDEM5zyBn3M4oeSP/63Npkb7sVNt+VzxQIaa2cNJnscJYOkWZRhV5DYag9e0PfSjkCi0R3J2QmwH/nnDQjJ968MiU0BVo/OmY30hp7/I4qoCz0pflF62Delexo/0olAeuIwB+YM6NmzlmQ4IA8z/mbJHIH1Tij4aiLU+jQduC9AAhPL8E8im0ZsXEac8hussFcoX1QF0wThD2rms+v9CaO4zVtIYvipfM9FfcPYcZ4jg35Fz3mjExkr7XhFI0sNQQqsUrwr9Oth8RhpHHzBq1v1B7Kd/nkrDEbg9kiCcUAPLycXM4ezJuXUDjB1AvUxYyG8ASzF1R3JTXEUb+a/7py9ndeuz7VoNYgAk4Io3Wny8oksEHZhIajhf5E3D3cgHtIUn3JW3GTtxgFDPR59CvY1XeeP9+o6JSuH5bh2fNMgV0SnOkVNbIlSaOlRGIm9C4rlrM4nBe5Bk0GRdOY4R7iWVmNW2mLTJ7rHxd3i2QFmFjbeKbvynSN6w4FgjduYFY1YX//2Zqu5BKCUf5o5YBHs2GmZX+GFG1Xjlg+4/KStOqsctRGftwPiiOvrN8DCJ/ul2uYGE5Anm9E6Q1eQp/DAPUo0u25QQPBEVlt16dOkkkczPscDlMtCGwUaGujZjXTeAXlaz+otlGqTGbbjlfx7XJW69z4jVXn4Kx0krkdr3zqNGGd2gAjrKYBnJWuv8KgmaCVYnw+AFQQBocWVnJX+aPfXhBlhv62fHuhE7XxYRWgytn0v7QfYkpemSgjCBzaYzXSTFocGzlHCFK4mX0eeREUkSzKQx8xituEgW8f3XqJ9F9qY1xTRwktwSauNyI8GXXJ7ETXmNL+GR9Ftxh8xvMghzUlywBXt7PEMXOJQdnqcc9L/7Ke7ZBDhcXvwIcb6ytl3qNVtRD/Jmuv7y8hYB0yEe9AP5omdthSPNBAr/W4SSO1aj1cgqL4LEum/pwJWZwQd/1mm9r8CCps4wqHzzF89Pa9VAYc9RDBfyUHsedW7lFVlipOPxAKUcprgiBIIZOWjOmUtdYBHlelSfdRtHfFTZHz7IwJAcKUUHGyG+t9fWcwwZU9Gcl6YsMByc8uZNZhFNrlPUcahVKO9BzirErrJmCF5XQQv5RqM5mcixG4DO+rwEurjKqYMb9IhCfMLdIrhXCSGqSfzuyPTFTfz/I0IW4YnRto+YvAPzqEd+5KIenLpnOP4EGY0+XQlY29HtGGQlYQ0Ba5SZbum9rc6ZRayEuyDEAnUVgOfWKsRkYXT7fdfF0dIprDhyaEyQXYBTwQu2WfC89VKgl2u8h2X/oU+4PCmKvpT1ETQhzBBky9qWL7G3/khQBdfzjfOrVPJimwy/8o02nQA8Td+hy8Yq/3bFM/Wx0LF8IhaKAmUAZDJ+cTg7dYfQuIuJYOS4XJVihmsloLlw9rs36sB9euEVjrUaHY9keCF9m9qMuq6J4KWGyiwfbiv4OKe4CCa8VkjOpUwIV2n6k24I6uS49JngBLitDhcrE1/pQ7FeUsJXu18CNMbsxTuBOCVlavAauqVuBIRXBpLZ+B9MA6n75e/mnDroJNj2ivnhRj/83VdSFBQP534IY265u9IAz9CJyeXlZ27ARawkYDGqHwwqEIzJW9UUcrBRSVbQg2Qf23OH406o+2Jerl4JGc8toe2bnT0JIlfpEDC0zx1dOtNl+YE/yxMbyH4BswK9uI5pCeHNogH0SU7tt/rTb8PFIdZ7Hz2CvK0ENUPqGWQoRnsa8kExiaFl3TCyl/83cBB8xb5sHaWRwo18BIb06E+0hh0TgD4F/zFFK7wTVfleJRRlVZgI2MbUKgZhmWHP4/FbUHjmwblnU8eYj/pFshHGZk0WmDRmUjSS6ZP8rMpJm/ZcAAAABAf0PEQvyykWqsV5x6oabQC8u5tYWZunUSQ/xU4eBjaSJhignZAgsVtZS6vbpi70ZbyvvSsRDQUZagVW2fihtV2yFZWcFDQMPtZjwsdIgOJf2zVwSY1tuNxCx2rQvNwQELxAhllXCjBYAFAMcltLzl6myFV1sauHRRAy5rfW6QwEAAe1NbbxODHyXDn0Fk/wcCVk8Zo+E9+aCt5N4THdeIQp3csf3eeB3DvESKF3DRjP2HthicAKkD5BadsO/DsKS6sv9ThBgMwAAAAAAAAABB3p0AY0E8miKs+t1/H84GS0HZzXywiwB8rcDQRQD8yWOgYv8ZRh/z/OJZTW6jvltJsj1iu0fHkKmkoCpdMtd5MACxf4r+DnibkIDWZaUEgEWeOZIyIliWhG6RfpofbWqSlx7db24e/vzUMPHnHSruQdiy47HKYxHXVUq1fjQI+L5B1RcKsaVlsoJrjMP7whHC+61+dJxXFd5OqxSrx8bmtZ4T6aGUDtuhAc7sF/IwgG0DpQ6AESbQ5CEkYEQE39dDNdYumcJi5tDbvCNn0g3IOsXZrjq9BbiHtgSXrcnzGh0sHR432dZlJ66Z+N7GqxALxX9SZkgVS+Uiach20+KIFsStNXiyDuvwLGLIrMAjXq1SOTW2vdZzDnp+/XKHOiO5IWYhk6akDYBtcTUVa2u040DocgGYhGgcCcaI6f24ZEAuBYXl+oHyr1p4Er8GgS+f4l/tlmP8CCdKY7W8peYRrlSt+QjSszz67/JgAI2Kz5I0PTEehMG8EuZOrkfq+Z9H4MMVZNQU6/gooarYQ1/xiCzF18ve3tQ+1J6z958sAjDVkCMvwqcgllx5TcCPyIYCpA5cZNNEI6RlXk80g4VRqzWKPmM1vQjwCTbJrR7FJ94qGULoOcRtqJ0P3oceJ/R6nqaWOdNrPVRIzTKeZRgJ73/mg2weH5FwBTNh6NyRtcnO8gR+jmw1Z/SVj3/L9RYF0BlR0cAoCFhcX9k/jtRpYQQgncRUsmqJ/0DiM7X6AWAt+kmBMtZrwSdg2P8uDNcKM0URJ1qbsNfbErffFgPGLHO9lqVc6seoTD3A5pwGs491WW/0FlSlxMtwC8ykXQmoxWZxHj1+BWIgPqZTyGY4goAdy1rXaF54yXTdBpICidpMEd0aWJ4losZfgayHMW1q7xWMoIAN4Po9gYs2G9D2qNgfJIPI0plqiYpFbMqSoVHhaBrWjIfba651Pm1vBuF7XZBJG/kaVoBq67rYIQp7hnh0Zw5mIN2bkt6bQrMYIOpbdwAD6yqW4MEm1aflGBLSUi3L70/b6YXnRSbsndOwnwpoDDMuMPoGspNE/wbH8SORGxpb1F+YOOXvCilUneqvGCnVB/fPQInB3nXc5u5RZAWQSoo+7u3h563/28W754Q6RX9h6ScBL99245LLXwpFZdPQf1BcGlqKYGDQp4Xdj90i8EDAZn3eekU3eTU23nORubff+2JhAurE5hyDwn/MaExVeFZlQCbswpwA8GHyAi0vB9Fp/IiAX0aiXlXXJmVzcQiDH5A6LbhRcNVpfiY3Ev+on7ke4SnX9tiIBmXffWabeNEyEjOHgNIIgou37mkwexFA5zBjrOkaH0RSXbFXXYhuE889c62Ig+5OpwDfNYimIO7e65bVMkCW1a1mmgMX/o61UN9Dtfrm3bcwnkpj8ixmukEZKD4cAwUwmfbUNqJb7qeIzA/HgfbQ2KpHuog0f4hnnTvqPhlibLzgkfGXrNLfKswtm8ZVDXoFszAugR5Wd877aEz94SbQX26+yUQnxgYqs/zYGSQ+SKQSVHT+wOO7NTuPxPBx/DD7mi0MPRdg1nW4GxSxRfbjrNTHNbDmjGhnp2HrSy38RDuVdnf5lFav9wq033FMxlGWcB/jUVzkWWggW0T+qGWgM5fxK3jAHb4cjvHNpGG755uhVOPoZv2Mp95dbGNWpvucnuP/FRY+Jybdpqf5tp9Ta/ldNeU9ejnhsTou7SBdYtnY1YsUIkJgHs1dIwRjhGtscgxVVP70bWWn22d+fAJxD5d64mki1ZtSQ2gbbYmOhXV0oW8qNFUIsysoXGqpig3Eh2oa99T0QnxZdt504nfhYwtUus3PfShI9lewb/0ftakMEh9OFpaKv2CY5zDEkYijxT91b2ZdEf34H3dYaLPwBJmGEtcg0Asc6cdhIFOTxfPXlCpOKoND34PNmqCMcI4p/zjYRBA6xRVTkiW0xLgqoHgyZtWLwwZqyrKIxo2s5I0OIny76xrwZPqzFyWG5GNa14gM0r1JvOcH1P+An72k6FP/k9W7rFkfhj+9olkBxAoZ1F1WlmBbQGdKngVYehx90Kbw/99FILuJg0QPHJlM/nmevAUfmz+ROcrU2NBx1YbUdGOgBcpz6t/gsK2XbZoiLf77J2fK+mkyqoxO1V0m8iXkMi3Y3PlmRNv3y9MaRO1svln7qkpAddc7iD2klET+/s9pnIPrEPb//7IHjPnB+Z0TgSwOid/jcljQ8HlVsMMNqQ482pPJnTM7s0q+mb+uCzLMMerC0+lp0rs4WR7HO4c1mRcnJOsqqwBiLgOsAPvK6pUzhpOknWib2Eriy50Ps0A52MRQXqRK4lhCW6xUTI8sGgwebUOxypqnrRw4i08Y0Hv/p3D4DUjTZLbEaUsZ25eYT7TY02ROc4+G/zVU7fIM3Eu1eT9O4tHbZgWdwfWbhRJY9t9ZuRBB5G//DIy31WIac6k/J45f0k3Q92hpZtBs2Wa1c1/oY6mb2aZmg6SsGlP2tFxsmRGhMzEuB75eb3phASKB4AFYPFIFCcot/veiROah3w4V4DKa+RF2mmEMxU7k/PD3IDdYpQFHY/eR6dHiMN9iQbZNwTrwC9WcFVa6oCowwhB4vOQgbvXNPeMOeR8M8Cu+KZOYCN+cSE3Srxp4rxsyS7o45CO7kG5im2jAhad0KRYFlAHYjK38ZO8zlDkwX1EZxPoYHW51NW8BBfJHOFfZ9uxHdfA18wDjBJH5Hh6rwPjlssz3XwAbBoqnSs0pOXGAEPA17Azry84tVuWVhINT6teQDmYN8W65lkaWfaJ2h8+V1BhoXREg8Xtkz5dt6vEVeJNfIHJk+Mp4NlGzM/QKeG5sIus6dzsQUORuurDkwmgJNEtb+tpXL8AKqw/027phlF4LabKURxg3GbWNMiJKQ19zGB0nDXkCcKRJvPRIWH3axDp155VASGRC4DixoZDW6Vu6OKrRPHndWu5gbYdrBXenqxDPVTf8gkpfEV3ynvGvmK5YWE5evWFhUd9Egk8pn0pni0ClmeW34DZ/KAKxLWhX0q2W1patETG3pzjArc8vGxcQCog6FR9CSww18Paa3xZhp0FV6in4dLHGLFUT4HDhiMdVMhRt4YU6pgjTB6SUR3JbQhPBnAtpTlXbBQsrGt2tu0Xuf8DbOvm7aScT1xNLSVhXTOzRfyzeWTH7/WwNwyZQwjD+Gj0CA+5snYcEcjNnOrLWY5T2PgVr2dNQbu0MCRxdGHi9KkyxJJKjC0RyteQvfFcRbyZGmklizVQ6liynLpHGlJaB/x3d2jIs6e3XYquW1MldLZ2K5qc5eIMuCTJv3Epfm74YYuQh1xr5Y/lvV6y9vzlQyuRhXHmRJ7Brmq6cws/jHThgEUX5JjEBJPafgUVgMn7qe7v0k3inSVgdnMr2MUUuQQJyUZRzVyStIP+4bwMzuiGasC4FzJohUop/W7bY2jque7mrRmkgqim9uoIqAp0mr6lTzyQNuYf1Dj54uePcFEfljD2tcohzqaHi+cibPTFnSEFXhHQWtMnzq4ywBCdtH5IjVcy4CXBi8oC19WaeyfSfByvxUM0SrbX4Myf6d4lhEBKW/GjArrDP/hCT88QdE9YE7Mwoh1pkJMDfdnjmxy7W7lt9lsLykZU3aK5CaP2N9Ju0RE5GlQxfOHJE7I0fthNuE20LHQYn1TLjPBtJyg3H+Jw8/nMXB3FTXR2JWQuoe3Ur807ALY4rAJhfl/0f52MD5rtaC0JmV0WZ8bZ4c4rBdoP+2t4fS1irSa/6MO0BUHWi74mtLJLgvu91BtYSsI9VNyPNYdg3+SeD59+EBcKi4dF14wUS01hOgcutwiuiNnMIEUChJkfsOMNqab2T7zgJqGV5tJeoJZtcW69pa+6PkDi6g+Ex/3HCOQb/Y8j1pnrizbPwGRuYAcqGPc0AwCn01iDpqBnNSO4hPnxpICxxYUN2KUi+3YUIVlFRlvKilmbCag8bJ/qg3yup4/O34c8U73Wim1D6pKlCNTtl2TzEV6lNeW0XldSuhdPuQ2t4WZzhPtOqb5s/jXL6/jpE2rGf+LTC1OhEkI5JHi0/bjMnpBqbnqS6f+RcRwjeZmkhNhSAJL93r6sWj7waHrDV2ea9RV6SM0Hpa9/4Is6fgnXi4DuityPub1pzUw47hT/EqnFZ8MUVN5j2JRZ4DC4fxGyd1yVvREt/qUSFPCFKx2xdv0M9ksigOf/p0Rwk2wG8NqV3M+LVHCFvHmc0pZ7BAvnQV3FcrQS9JiE+CCDgPFCeKT0nbj9nOfs8OBQz1qkYaLjJ8d0OY8mYaQBSjuWvBP5eJZCO0D5XpNG2Vc1AmO/Cv63uW9e+2OdxPnpO2+JRqkpZOwt99/I9YSB92MIzTwCY/KFwXKo2vrqGDEZgAlXvzynaiuZB1LrSgw0TGmlX7mluAwaEoaZI5OLHogRgQu+yrjc/HKpuGeWHdsuC6nUtH6E+7WUyqZ3dj3E7A5c0v6QHT+RQ8D7WWAMy7MJSCH+3qeesgyG4QnK6hCdsh+WreLK8W2v7aiXm/eu9sV5CrWvRfFHnHSN1nv7kDNJSCRLlGFRkvBOgSVpIMIhbrG7ZanDGx3Hd0xsIOO2GFOIQKwzuAKd87Q3FoYsUQAgpuvbvgrHp4HrlpN4hOpZT6bah1DZC00ecrAod3kPZlwFOYWsPfG/cxJfwR3wpTU71qxuxtoItd+Q+EdB3VMSdljkussxejKQ18HQyJ+o9en83SwBLuNt4LbYG30g8QDUjzSKDjWg/aNcVb+ITifIbsU6lbjXmicIO0YK6SwOgWLYUdKy7dA8d8GxnrcH5VBibPMwXMNus0flQPjG+b5MoxG3cCs7D5SgNWCUdw3ZTwIlht4z7SKzyHduAmz+3T+IzDIgIcKl6JDXf3VgHz3qezkHjZ0mbQ7a1PZZuECWXObdYdFTOGxyZGFQfC2IVp7iMSF1MKFyurHe5AL9g5JphhCbzk8eTYuXHGtTXiGta99c5pp1R78EhiUtDgvKBYAlBX9kEDKHAs7NXvI0f3aUP2a4Y1+a178sVRh9b3/kcmO2l3Mo1upiui2tRzV7wiVI5SIF++9UYLqOMhYknE7PEw61JDWVDmCiaUQYgO1PlRZH7u27oQ3axOBikNPPS2hAyd5Sku1kZqhPdB+xpl6E+rVBmuRG4YcPpClbVU6OwOhv/ahVj+J7bAYvL1cUn8wAuiAVD7N1Ed0JvNSIIXfSWXApVElPH89q6WskqoIgOYGjiFR8bjwKVgRgP33pb202NMGyYQOTtLpoYTZenSrD0jyynhFOpEphtXn8G3NiFjdA9VQ5gG4mV/T1+9ZOfb9xaM1YFLVRJOmh6/kJJBQvzP3hzJO0IyTKcq/2vHKFHJu4+FJaWVTMEwvOrNsad7+UEJyhuszkKIB8XaBtiqT2TkMnR1UV3R+7G+vxYRJ4fU8DIgqZpdRe1lq6ZX7mqv9kVtCaWXhnoRuu2ha0hiD39Sb2AK+CRebi1U//YhPFI85RXVegvkTEOODnDYQjQ/tbL1pIBPV6RScNu4dVu/O0xtM8qjdeVtGNIgID0zVnreavp6j3GhG5sir3fuSlwbvlYs/WJ6o2rrG7LG1HMEQCIFP/mTpxDD1AQfaJEJ2+Ay685MvupJ4XPMSYc1QrAasPAiBKnoEHaJZxD4SiV3nlBmUtdaQLljaOB7aDGWcv8x4RVAEAAQH9LxEKtyJ8PkFpHXU0XQ/Yw+AyxRJXT0N2LKvgYYIwza8HcnMIXrojBS1iMN4SCWC9QpLfJTVBaeFvazViRumru69yh3wDVYL+HDL8P3vcyb11W4gxaifRsimiQshEyruHI2TCCz4WABSG21tJnaLCqOUzwgdN5DFiIC5Yv2MCAAPqstqrq/xlpEWKiGHWQRmM6YTKM8lLYrCpI/0wU+83NVB7jR+NXLBaPZz+hfNp/OD1XTMCIFSGIIbEUt2HzIV0prox1Q4l3hA4uXFiBUS35jbvbWCqwVHrBPcIquS4hZn9ThBgMwAAAAAAAAABsMS6AM98N0zxE4vomwsWutHP2G6VCB8uDh/Nr6r/W+rGh76IrAO/hVxbdN3t9t5m1qQeQZRMvwI5wqa1MhBrlPawWY1OxuBYFxCJMulJ9Oka5tLdbjOa/BhRW0c4n0mXf0xkd09MgHrcD2zhcx+tIygBMw3mk0bkJPgIYFr8fDtmQj7/84ea/VKb2r/QfBpAiC0BSVp2eCw3KBa5rgjNcSjACSEIZ/qmSRM+ffo1sHV4+GQqYgiZ8HRJbhQKaIXc6UD99OwGPwHOhoWRrkK8DNyoEplhaNxdAdq8Bv6KPcgJDWWVxOBTqteEE1191ZK7CkSmdSgmRD6AVy/xsSSXaeIGYy22tsxGbKHLACRv4zYZrUj9H2UMDyTm4tvyGLxxG5SVbQlFl0jwDi2A0hfPoHPhYdVKZW94FE+zSJJk6gz74WoZlj3MSpPQvoPXvTVS09C+zNdvzv8/sQCL2/P0y23LK37dycXrmXlUNan/DB5VdxTep7PZEq2ZDHmZOBa/i6SjhgqpXxGi8Ch9P1bQnLysa8AvmKyQXHOyNOc/cOKY8+z8IRYT2aDF3nhrgC1SX7NQ0W6dlywA1Udut5yz7RIaBEL/9eyJc1SQy6mDzJVSPJFobzpxQkA7Ha8Bxr9fhe2AV4t0N+ZiibE/ev6v6Z1kLlwTkV9nGu6M5xdjp8NTSQUzuAyrdt2PldaFXtzx+pxRQkQSPAxuN0TH72kAsf9SCqYljpKs8e6Y6IQ6nmp3sEPdaNd5xvcq6lklkn4D6fYkmrmL0YRs0YL5VvFOhwUUMhT05jt0QHBazQjDysXDYAsaSC2hRZcF+UUchVRc+HQkvUGRH5NJN1DsDnEMibbV+UX0D3bAQFStwFWu31ML9aMFS30chg8bSbZmFTa4+LhNYnxV1jZheSBu7kOIBDdYbdItltYedcrL3z1bq35jglVP48Ovji9WJwRPyH76ygNYqPU2jllbfOLhB765jYo57rUsP4xN+e1DfRVr6EF5WcsxMFtgNKciT/uSzXT/owMfUXOsCVZKsQFX0Vatfd/SiPze2JTNPmuEwvMJaq5HPMj0Ko56Lm2Oeq8aS6uBtSvkSHrUpqEYQSAqLGTLA+NOQcI+6tgxzyzZXgQTxNd5C5xk0hXGF+e2/FF5nchYdZjqugsoVKy7UY88zz1yDXkgdr/iIjWgHEyor2DN/jrh1rhOJLv0ncRRMEjdTdiBVhJBEtYy+hnWQWQP7YZawmZYK2Y00r6SHOU0HIL4ZzEHcruTFWusnAJ38qiwdWdcAI8t1Yc/EiJUYg5v85BBRY4L0xyC2/CwprsMGVQx2GJ3MejCnDro3iRe43F3CBL39bVy2g1UraUooVq5LplY9+qMqy3AgbBEFwYZ4BYmd2qPGgp2lOG7gcb4P/fH6slXwu+V8ugloDIUA0Al3B2fVbYZsH0mCo/4Cl/pFhA3zjMpHgT7z5/Dp6J0ThaxbuOBHCZc9niTUofez7OwUe/YEY457W2YjyFS7XlrddWElqaq6UedDu2WCT9OgW+poo55DC+ezqE++IGixQYGxMYfwlsLA2EMWJQ3u10PhPagn3XhOWpiQ19csW4aMCbMDOeKTm1pMfYnJSCFmJOvX+SKmsCYdaMD0HwZNlFOSb6tEEJKftkQapjlOrbm5FMEQ8aFq3DDWfr3+1bl3VJAuTkOfM5ObBvKhsiVpE09AolG+d2om1I4zKEVcr+J/TF261fk8kOQGbLCISlB8QXRdMH6XqUxgTEkGr9QMHeFPkT6C4BQLnUkWXiGzgiM763V73k/oLJTQM5XI9Z8sQGQU87X1aooVdHruSbIY5AINjt3Hb5ML/tMN9+SwyU7733NowW/nJaVsqXcDyTGBEqI214gusw3YxfdtkvCQfGa4B84ifevvC0ni30cKAiiStG1KXs+h6HapPrsHYu6oDTsFYI+dYVrJhM8jYvaMVOibxm1qxJt7knhQvrMbJm8AtDJrFiw+i4W5YTl4wUPoavdTTjJWgGZurCtO/mMUQuHisJ5Mq5lWNUmTI2hPun8GDMApo6u2Z/Y1lnlUi6ov7lJ6Azt3EB03M8TY0b2aUmdT7QpPlxKcjuyTSTd+gTGAD87rv7jc8JGpE1UxAeXge9GXwQnlyKW8y7LXpO8bcx+C096U49LrydAA9RSttFmuqrwp8fXOEqnndFH5rxBJ3elpYaWA7M7jG2v7WfXXsYpp7u68aO3qoP/6MHl4/azi2Y9QtVapDawSn7JcM0ZeGyuqFwcAcda8aplG8/CxdDTEyldgzxt0kMyzQvgx+kcreN/YorZwJanJEKqw0cZWLk5Y2rb9kVxjXgYNsRZOS27Zg4XJsenM6Ah+dHlMuNC+f+V0VYCi8oAlkAf5j3fQn0ElRgyt4ZPNaNytOfzVu/Zm0tJ3w+Grk1WDcuEoRKDiOz4kIm9rSt9yKB+/16wMsrRAudqgpSIbdu7nLPZieWeN1yBS1hhbXNMhY/LaX0HJAuvQ7JPFpAUyiCEHemvWSmQQ7MBFkgsvrHpUXszZk6GCaKPSD+jOeMbGuLqn4tJPFh2zkqk+STH7QZe2sXomB7kVW4ekFS+fjsLcv9iB79ybQrGyaPogD3xZt8BWFTbIXmnV5w/U2UbewViFb0stdDVJerKcOJJfx8EdJYAoQxzex0/cA9oRwIHCZXoOEt+qbWOhuwTKZ+1hDgr7eg/8O0E1SVGCMZs7yXp6ENrt9HZvLRIDFbJAL3JlkJhvqYqcTpIFD2S30mXXh1MVNRtASs95GVBcU5ytJ7AhqfiS72jXo4wsFInpeWTKCUX5r5Bo5IGphoT9n9NPfohdhDk2gZu8RZJomdMfCA7zS+dlVBOigEjG7X+7/SW8DWOFBm946Bfs2NoW4C5t0p5LImAk/hucin0prdImRciafrfY+4sznirmovtDxBjNgwD0cAZJaiwBsEqedptyrkKcEveAWq4XxX7QVff81UG2+ehv6yS5dZbhm4zmzlXejr83wVyf47tnhtunsbCpA1FHb8jiot7zWMvZ+0cdFy0YCdnJ+1rekUP8GEKgKhJ3qU0S9jRfYBewIl973afbE7VPVeNbC9ojPy6fF6HyHJCtBsi+Uo67zOTCFUpU0cBkc89ANS1uCwWmqnhCcdirzBtUD7J+CCKQy2cQaRikGv6zLOs7isiniCL816NAPlzTrhJdJmyg93UFL4QrvK0p6iIQA0js4w324RvGYamKLqqUyGExAvYh6Wt3mPVch3laUlqNtwnd8hAK+6Z7LSa83kJ1XXCztWPqfelbrpSpuz3yyxcsgKFxVd0xGjR4/i5B/GtCKIxO496CBSbc3GHmdT/gc9aSKjzBwFezajI6vx1QRwJzC6op+3HgnvkA8ODY6FLdBA2wnL9wV1TrgH0sq8REn/Bbf2KpIzObDL0riH+SEImOk/h8/r5ezB3akulr2t+aiUdGoQtMLRFVlm1lEEkihvPpzQps7oj43xeDxvhQjgEIpxstwtypfg1aKur5nkxUykfS1DcZ32dNHPeBkMpbxT1Ci8W8gJogMfTAAVHInaV4JT4ZY5BDUX1y1R1asEEo4+9li0u8o2eS12zz5LVXyL/ZEp9UQI++q0SJjZyFA1APooRqyPMcDiy9ELQ4UaUCrYCXfokiK4RWYHPdGDcFGdWI/VNdQoraLzS2Vfw32gG63dLqcIYdgrDNnKWwHane+WpXHe2PZjHmjI+ZQ57Ix9T+4j6OICFtHgdgno5LeXXQWEg8v7rVc52usjMvMSBxlpFSWLAQtjNZ5XhpqEBsujJVhQMqmM5N+qkLr5p7cQAxE+jpkvV+ZclyVFqbcfSfqB7y53tfum5gs6ajeVns4dZk/kxYx8TMD4ZrKWok1ZjgMpz42X1J9kkfY4wrC99YarjDKQcnCB0BeuXJfSecGqvy9dfdXeu4FNz9s0MReQ2ZSoYm1nTFCHIU5F87H/qtB0axkwnSwpJiD9ofZd7D5U+0w7aqUxrPMIUYIkR8PGadF8YfWcsKzgPdECDjAZjLjXoTVELQrUq6FSjVsp42nxyQJlfW67nDl5QSTaKwSfivkKe4EAPMX1xGOaSRvKPhJlGNuW+/i6TjO9Teh4s0EwxcpqqNyL1qRNrVhDad22dEXHv+AJc/1Jx4c5qzwB9WJ0v/9lngTbEVjALGgih1jMMgZUyrVAPLjS+M6z55J5nvYLCUaOjf6wEpvMOhhq+k7GVbVVb0Ne6xQj1oOJzC0U7/FEj3S/NglwUz/U0+HCIqaIU+rWIc8j/p+SLEfV/U/+cywt/8H+lCiBp089WypHY3NxSNX/x2ZP7KKHcZLzg2x+P0Whs5+vGbYO5xgSZ41atQgZXI4g1UPCOB26lkBdFnJLHq9NB80InMnffcBnmDD7020L8E8U0T8rq/C/N8SzgjxAlHT3+M5PVKlbaXWoWPha95OKxBzIjkWn1m7AXUhxV1q/POHdM6JfdQIlkJWoz3+sP8bDhPJC33qpgo183rt1ATHt4pH6s+PLm9a1vgG3fytMg/wHOYKi1DwHXpqxab8R1XP/5MIR2xF4gInmMDsoNphjdjLkZ1Kku5e15eXV4G8s7dZ8PoaUUmW0owY+ywmcX3Bdez11cALrlhx63PtbRmEx8bgD/FG2/WLlPBmA5t0Act0r3d30sTN3PjCe6RCXtJTW87SpRJSXAmIThtFfBQA8B7nhtKM9XlGvZuK487FfEiOBF578I0teDNbeguT16ls0TWLuqCcxQedXK2hMD3TNBUZ7eSczXwF2OhBulGlBq/il0XutkmPk4PZZBhFpxBxbqykGLnTCc7DWXgVIYMEVcd7jQzXIfuK3RDwShTOcK8hC7ulo8VYf9uKQ3DrzbvL8IiBsNwQNfFqNqF93ZhrPUBIcs8XssLKzNVvPvPhDSFtpudld6mdXSKf50H2F1Cc1plcOXut5VukslCn79zF2YUaWPih9QEt2N5YcmX5G1ZGDtlB0Nnb6EiVDHWsINhrRQ/FIT56scISCWaNcpP0aU1uoJT1QFVnIRXmx+e+6fYZRJqNen5MjNfSWEnJsDWiJLgbe3vJaUZ2yLBsIdbCmagOe2SPlEEJlm1rFo52FlftQqi7El7l2w5uMreRfBXNmt4EYc4sFaa6Qk4tuKmYYUmFbgCxP842VaZfPAkbCM4HgZK5W9ne6TiXj5I7CUOOM/uqtpQ4zwuVu9cDUFb4Hq4Usjs/exH0JyXy3/eao2jXUpQAxWhVR8P1y8uZuykWAaN+P3C1lxAy47SIcQdO+h3f0owHkcR/0Wyw012bhk3/qoEdijAhvcVBnC/YrNRSU3i20qfLcAu/Prg9ylKFxkDxgWcn/wm0ZhKeK1LNW5x7b4bv3uRrps40x+lJ2sACH+8Mu5zumVEI6DnuwjPLVRWpMb/I4niokdRBuOfa18lPtNO30KWibqQGScZxF2hP27TKbUQsu+PP2Q+7BV3iuhFRlC1X12K6iUme6gii4qKlEW8AnQa2QQIMjU/qm/Vtt+e0ugMYyzmfQ06jvm7h14aOo2rWxy5rSdnBqj823Kcs618157IgIDvkjuYNqnxC8ir3snJJjV5TN0pi1xqTKjhEn9rw+YQ9dHMEQCIC5ManP9eiHiyRZqyAqMAuMk6mc76dyFiHAFaNG6BPTHAiA+ckCcWB3qOoT4BVDoY4/PpH7q9rauaTrjzf6YTOR7XgEAAQH9DxEKxCgzK+jG7msK427OqWnGndIaAcaeLeNvERwvKE78PxcJGMGq9ma6wjN16v2slAnL2/Deq5DhNQ/2wsqluyzAOQkDJYYnDaoovbA3+WhIUKkVl0u/AhovqwT5GBAjkDSIrEgWABTJZQlxUqT0rhVSy91J+X8lSr4mjEMBAAFy6sa7jqtrvMvC45VZ4QRXXUgzw56rHUFXLZoDTeolAdY+b/mDgQTinpU2y/IWPmCVFIigUEJ069gRqnLUqlPP/U4QYDMAAAAAAAAAASORxQB6WjQN+IudHC7M0pqKxXnT72fMlaBj7zrlaePrrw/QMKRqAX2I6Y5lqDzYNUAupeEWe/iZIz7MhljAqW6l9yBP2hhMMXvtaNlgKWhjuWsNoyXDwbULNOzsH/sMh+RGKARfa5qnVza6TDev9DcKtG++FclHbAe9D3J52RfdklnkhA/+fXsyMYit0H1wiA/XXOGItEjB8jRKXue8V04kLFtcSLN+gIpvEoJNuH/rTxUYoJLswjJgKB5U+HFuHFPXbxP+l11BlBWBnABmQ05RkdUwQLpnAHT3k2dqVY4EFDxaC8VA38xTizloESCiOeneg1SUz7yQ/2J6oY5nA/A2wjtccUcozW0b2x2emq+xr05+fXjAJ7FpGRLSY7Tb7OLgTh78MuXCG5pOmwyNRToGJ5KxegKpcSd0GCWjikPCATnt1lO20gKGvm7vHumboIMquvAZt+CXj28KcVaBKzZno8BuvlX1GmQBd9lGxoVxE3JuLFNMZJp7LxNpA4SghzkTX9R/n6gPUtYfaoLlgzJFso1lh/IsYS6BtnXY/WKj2MWVtno5aML2UCw/C5yKv1/SrFXGgjRhs+NVpZWjIR06ji9tCBMfLDpIvtno02X/t+NsE0qmWNrLEaLdWt/H/JEjOTJIG2MoBge/IHl3ISYztwgHqb18kIEp11vTh5ys6yEpBb4BihD4eIkG0L0zSGicvJ5BcnGlLYJ+T6rmu19EFwQBjCACgmXrsgg7zR1C9ztXIy2zamvp1862EQBJ3aaJq8aWersmehp2B2FRT3Epd2WdNpPrv1yFyNj+OexipWOPieI03g7jUQzkTcu6MYml9q96T8SYs2a6ZSXOWTB9Ktj4DYIBBGWaQann90URLyx6lGqriUCvoBtpYTIlV6W5YrQPaSLfV0KHuMuYpom2eQ+Iyhgao9aLABZTwmGFdkNWi1AIOnnjbFnXNvf1M97uRDIE2pUN0Xu7s6ywbGZWIowZ9p7UXQA5+9iDSgdIBE3rN6dvTxLCQ8l1A0emTCPix8wlEKH0YqjDE3TcAimAE0PEtyRPGw662Czj8ntVmNKO42i4s2Vg33fyTMjUa6501uA39JaVS6bXTFOC4+B7S1ax1rYpHjNZoqCVQnOY4k/hqIaVzuOT4e9qrRSDeBpvWG0cAVfKW+aNFW+JLm6IpdkdxAAQKTjkohRrqFHmlP5vfnu+H7YRWlOJ0B4XV79UpMCoza+Vqe/TJ0RohSkoWrYr/yNThpCWFTkl4ohxHH4H57piFfJygJf8yj6x87tzYWfNxIsbdlW8Bn403mJKUwaq2iSyGacfCpLyTFq567Pedw86CnkutAFtUPXImYZYW+jPwXU+rlyYDc4DwE+1LuTD+rks7rN53a/NSUMm5X8Ak22pXk0kjMKP+xE2gZX3ANXhqqlADgSU2ReOmb8Hpts/mrF4q45rRvpUsJEm3Bab5e9zOjj+rahGuhf3/4zi8XMEslr3l+E7G0T7+ApwuPj1iiWkRo7epuchOtgk24Trl5cVIC5dYz13g5nFM5cFO3zbS/INZYlo6iKP4D1DxNnfIqbDjDcdcmF1rfX3pBQ3GJjzC5qIRLPeY6l97mSURwiWl3aQAAvE0DELrWwmfddBUO6w0XlFXxE4KnVqYQdbrAE5Kz/72avzibdF1O8SIBWbb06NPVNbUi/glnbSWZG1c0pzY39oW6KQLm3ciHOWzIQZ9l7oTyM1tFctyUGiWjq6ZDdYfzTShO4k1hTRBgmnhyPUt8lrBTLQDorM94GW/Qv//nOuAZtuJEjQnCbJ2SBchXTQi142804segvK1EfcE6upNjxu6NcUtMslb7XxWdS/2zrePd1t8/bYKhQ08gsz85RwxHK/xX9M25HruuyV5gvpv/+XmTcl4/MMRegkyrZKVJIFiUT/3LTJf9x3Koqe2CyvsYiVFD3y69VnVN79LBXiH4Qg33N9gLEzlAJaPHiiGeFcnEd+yuedBUGMYV0zelSBMJL3DFMcq6xfGMnnTdh+sMoJzoo35c0Dp9Mo4CoAHnxt0/EsRET6RO4x6yUz3bgI8EPux4LR7EFHepY+PRmQfEkUM6cIAEa2cFs+OR01/zaPiyxyFyU/x1k39oq5zAgfuJ/u/8uxDiMSvBflcrw9mvFx4HtrA039eppO2s3n1QrxR4sR/Na5WY8GsEDQyVmd5R/ptHIPBta+uXiRcZof0QoeHTP2yM9ZMy0kqvicD6jxLoPekXgXKpHZzBhDVZDhdyE5HMS60kVdl1eSjK3gVlf8GPjqAwIWXOIeQyUeKSH4V8RvhU1JEU8Lu2ZHypFwomEASHL/t04H8v1PHV7br4z8siM7RDSAnz2cF+9y4D8XuKQEYWDP2msvdGYSEVyrZR5obeFeOd36DQewwDnMqze1pbBu5KhoRlSNCjdbIr06on9R9cdndCtCyKr9u4f5991NCqNmHI5LP77DrRwYgP0BPBRzMZX8kLT0hMH5ZMeuP+X6uKZPAf4H14tAxJubMf7kyRlescHNVrWI1F0ccihemXWvZMHGhKf9H4r9/2dN5pKA3fC7PeZdIqT3JGq0WnppKzsys64gCqQoJpp42r8foqzhyeJo6lNpNYr541M2imxSodmRFaZZsD9LEHYGO5z6c7gnoTN42fJKsTPD6yC9zM4CuZuwxkrFeAiE2WbLFFWaiVEZFtjdsj0JJsVBDy5Uz5IX1rP9WV2opYhuPVWp1zXG7suYGQpUP7pup0HTtNn1sAlN2CZzT4gRne86xqGKHjADYJa3HAVce4UYdhJBW9tIH4JXAXIIMX1TFSGnRmaYasJkIfCnMBi7uCHHkiAh83VkuBYOxUZ/eV0vq6e1YtGBmJqIuhmHsv8/bmXybpmBa8iefZxOR0y/f6srCoX3dmTLcpFNWvmJo7uwRSPSyPr2O2WB8ktiN2uERP6ZKsQztarUaYMJi1mY0Rwsez9M+zYPixhLAEjNExUGlDCu7KSFvnGm8WQPiABcu5ogalnTOzg7xkcLl03IhPiwdJbZScOJBqlU/YKz/2Nmx424z/PCb8MAaRiu9/VMeX3fCr7YXim+XOnQfcX9UzzXFgym7kR6VgArTR+O0OAHfilJ3uYNiOuDpG1QEAojKjOFmkIZvHgGLC3xmD4wjAqnoN3IF6KvF4NRY7DmuOXvLxFSLyuPMH1sF5a/q5cLpQ8nwx8+0u7PyIyLZLvGy9Rc/I/kUWgvdqQqJt0Y3Yl9gjySFvk7TjWbAs41ADCQ3084+l6EOWI+Ug7w0hno1kAU7YhJ4f/rqtC1dusb7ADAfNxchJAs1COryqsvGpjNn32d6OtXljuOVTc2nZQgYJ5/1IAjKKGiskO+qHrG8seWW+oqdDF0SVJw5GuiNi65ABpicL9MbAe9klohtbftI3FWM4ueU+Fq+qb1kW5hVWaIxcy6ebXjlbfnCJ1SQe2iP+zWQOQt+Znz6iuXx9bHiuxXnAfXr8d+5m1QEW7cQ4QSiGzQL6kkkTI7K80QkuBWYzrK93SQLKVQ6KWf+MXJUITW54xZLOYPgll+oUlAInNvqlZEzMnDM37tszv/OZIL8Lcf3ini4xr+q3xlZW8VIFqLkpHn+QZq9bXKoieQEYaN6iFvSKNrTEghYx7xGe4+hnQAW3UeGkTm5iFw8/oHBPGKOpYDQ2LqRrJZouK5HY/o5Sr5J8PW9gdJQdLjqa8b6a1gQoUY9vWdDoW/MXSZZmaCxqp5npQVZ/7Qp0OCLObVFdgp1ao+xkVluk1tzV9+1LT3KGaw5XNZytKwQNOyUYSRQAJ2oejXl0BP6MBD6BQTmXeduM05jGAM50dz1sUBt3Fm6SLObKmyfNGLHmDisS6l78nMrfKs/ak4Bmvc6oiYCwhQcJDjXm6jvlWB0JMoRREybzDZ5DWRhA7nopLhGXHXwgiTdug049EIRVJKbNGJzUbuEnL3VPsTrQBzA2TWmxiKSrYHb+qqmQgVoVsepcUzSXky+Kmyw1rlTcIvjKUYOqJUOGf44pYp2yAZf0rHSUSqGuyc0zdqjFD2odujXGBvANMkr7hMNor+C5LTE/QV4lGVuRijl2mZRfyQ2Jv6IGA3PY9S+Mx9WZMaF3QDQoxcRKF2zWtQ1s6hMAMp53ntqRW1BCMSpUimRkCsSaKbnKbp+fJiM/HC+19NUog+y+5I0Jt0T6BD9qyFvMkqhxLnpCfH9p932+mejCel3GWQU4vgcP8cprQ9+FxEmsmEVCETEOWV8gaFTYz+GTti6eqp+RtPRodY+UOaP8J6K0u0JtThc2bepvaP6Zg339KE9Btwb18JZxtaz9jWI3U6EqZguXutzF9fmoyOXgtXvOllLOrKKSDqSTlX4ltzwC0P9fwYvnpNaEjekTj1QfWDhIkimF04I89iCuHzhGUQ9k5RsW4RGqqfY2zcm3m/mL6/Yo0a2nDeaWDgTnsMwqLpseDSsdJsHk9LMk/Aihqs4QfTZq9DxILvRFgS8cRpacMVrsvgmxFuIJN4/aoatMCNyKUJNw+m4SWysNYJlSaMfPvy6o84AkveQEXXu+1OYk5Vi2x1didrKv8uYIn5/dKp+Gh1zeogTlPzfspPiXArLLn3x1FoADmq4nQQLljtFXOy6WkB+taHVHwrVQcCiZ3bC+Ow2pvSFjeo3dOrsWgWbR/F57PPtRR7z8qZbCNlTa9df3Z3cdvNEgCAgZkN4+iaUAyzDUb//GeJr2LvCj9C3D5joXl2EbN5ut9rWhHlh1JJUFPTN57gFltwIESF/s/yewLv4zDzHe8iYLWh6RsT1/qmDOSLoEwtsvakbKtac2GQNeDSYmUfDlvYDYtsmFHajBe+IN2FnzvlsM9T+XzX4YKNwnhLlVenOkhhsbE2aHmHCbuwNhcO41YX7QUr35lxqrzy+p1GjmslsODCH9c+6b9rlDbXVMTRtt2FBvg9eeWV1Z1sVTq8m5P9fu4T0nL1OUMfCYJqM/PLX9WNN5gGkChPlx3nwvaz4ZfYBVX11ukRGujCJe4c5FBogbz8681V2DgffxRYTNJrql/L2B+Q7ZDXh4OUZ2c7is9u7IEjOYPYdCJaP4yXY8wuS1zca8Mn15iIZxfb3rLgc06jcZT9duCPYo0Aes+32+/Q0/z2Rz7kkp9jJ34A9UFP0jYXYKkuwUKkQHIcUHHD/iHB5Vl6VeLbtyv+OM093IHmw/UnrZuUNrkhMCI4GQWZZMBRlzO2ZNAmRInG+tI8L9CxrXyJZzRPvU+iyDBK3pP7hB+ixsWBLOFpmV+p+Jm5luwV8xXwiFX0mDwGa3Tp+Ushi8nLBrPTJP8JvYmHyDTjGmg3+P5E3vTSE4jCRz7J7H7hc8DyPhRbQDh3JRLFGNKHu4fUJ4o6ORXehJYt0Cf4sTL54Uv2kNHYjlR/9M2uYC0c6YBnmxEhhW+CwwLF01iSClNRnr7c2AibvoLY7NwLmyP4vSxYH+6On3yNbl+kkWCWkJqUOeWxnUPE3mavnOZvEQtkhGvhn2tFFfdQrrWb4MuPqqs5emzHEnNAy/2KVujdGnw9e0Pd3v5Fxk1IZRahIa6lFSICA6s4oFbtezpw9M8hXx1SxsIhO5WWwd2h9WWpPzQh6xWPRzBEAiBrYOixoVqd/SpXz+OwxSewPyzBbfk/h5MhYY3ZIE5RcwIga0pWiKf13x8+CQJ+/pTQFeW/3adNavyUjeO4c5KfRuYBAAAAAAAAAA==",
	})
	com := &pb.SwapComplete{}
	proto.Unmarshal(msg, com)
	return com
}

// Mocked vault repository with harcoded mnemonic, passphrase and encrypted
// mnemonic. It can be initialized either locked or unlocked
type mockedVaultRepository struct {
	vault *domain.Vault
}

func newMockedVaultRepositoryImpl(w mockedWallet) domain.VaultRepository {
	return &mockedVaultRepository{
		&domain.Vault{
			Mnemonic:               w.mnemonic,
			EncryptedMnemonic:      w.encryptedMnemonic,
			PassphraseHash:         btcutil.Hash160([]byte(w.password)),
			Accounts:               map[int]*domain.Account{},
			AccountAndKeyByAddress: map[string]domain.AccountAndKey{},
		},
	}
}

func (r *mockedVaultRepository) GetAllDerivedExternalAddressesForAccount(
	ctx context.Context,
	accountIndex int,
) ([]string, error) {
	return r.vault.AllDerivedExternalAddressesForAccount(accountIndex)
}

func (r *mockedVaultRepository) GetOrCreateVault(ctx context.Context, mnemonic []string, passphrase string) (*domain.Vault, error) {
	return r.vault, nil
}

func (r *mockedVaultRepository) UpdateVault(
	ctx context.Context,
	mnemonic []string,
	passphrase string,
	updateFn func(v *domain.Vault) (*domain.Vault, error),
) error {
	v, err := updateFn(r.vault)
	if err != nil {
		return err
	}
	r.vault = v
	return nil
}

func (r *mockedVaultRepository) GetAccountByIndex(ctx context.Context, accountIndex int) (*domain.Account, error) {
	return r.vault.AccountByIndex(accountIndex)
}

func (r *mockedVaultRepository) GetAccountByAddress(ctx context.Context, addr string) (*domain.Account, int, error) {
	return r.vault.AccountByAddress(addr)
}

func (r *mockedVaultRepository) GetAllDerivedAddressesAndBlindingKeysForAccount(
	ctx context.Context,
	accountIndex int,
) ([]string, [][]byte, error) {
	return r.vault.AllDerivedAddressesAndBlindingKeysForAccount(accountIndex)
}

func (r *mockedVaultRepository) GetDerivationPathByScript(ctx context.Context, accountIndex int, scripts []string) (map[string]string, error) {
	a, _ := r.GetAccountByIndex(ctx, accountIndex)
	m := map[string]string{}
	for _, script := range scripts {
		m[script] = a.DerivationPathByScript[script]
	}
	return m, nil
}

var (
	dryWallet = &mockedWallet{
		mnemonic: []string{
			"leave", "dice", "fine", "decrease", "dune", "ribbon", "ocean", "earn",
			"lunar", "account", "silver", "admit", "cheap", "fringe", "disorder", "trade",
			"because", "trade", "steak", "clock", "grace", "video", "jacket", "equal",
		},
		password:          "pass",
		encryptedMnemonic: "dVoBFte1oeRkPl8Vf8DzBP3PRnzPA3fxtyvDHXFGYAS9MP8V2Sc9nHcQW4PrMkQNnf2uGrDg81dFgBrwqv1n3frXxRBKhp83fSsTm4xqj8+jdwTI3nouFmi1W/O4UqpHdQ62EYoabJQtKpptWO11TFJzw8WF02pfS6git8YjLR4xrnfp2LkOEjSU9CI82ZasF46WZFKcpeUJTAsxU/03ONpAdwwEsC96f1KAvh8tqaO0yLDOcmPf8a5B82jefgncCRrt32kCpbpIE4YiCFrqqdUHXKH+",
	}
	dryLockedWallet = &mockedWallet{
		mnemonic:          []string{},
		password:          "pass",
		encryptedMnemonic: "dVoBFte1oeRkPl8Vf8DzBP3PRnzPA3fxtyvDHXFGYAS9MP8V2Sc9nHcQW4PrMkQNnf2uGrDg81dFgBrwqv1n3frXxRBKhp83fSsTm4xqj8+jdwTI3nouFmi1W/O4UqpHdQ62EYoabJQtKpptWO11TFJzw8WF02pfS6git8YjLR4xrnfp2LkOEjSU9CI82ZasF46WZFKcpeUJTAsxU/03ONpAdwwEsC96f1KAvh8tqaO0yLDOcmPf8a5B82jefgncCRrt32kCpbpIE4YiCFrqqdUHXKH+",
	}
	emptyWallet = &mockedWallet{
		mnemonic: []string{
			"curtain", "summer", "juice", "thought", "release", "velvet", "dress", "fantasy",
			"price", "hard", "core", "friend", "reopen", "myth", "giant", "consider",
			"seminar", "ladder", "thought", "spell", "state", "home", "diamond", "gold",
		},
		password:          "Sup3rS3cr3tP4ssw0rd!",
		encryptedMnemonic: "um8H1ulZShOz+zSZLUBjWVysVVqq8LGOneKte6fCSVHRDYsP6FG40W+NZ9IHCwSeigrGyr0rGazoNqIJy9Q9CaLMs2MA5yQVw1g19OuagZqXAsPrGY75FNKgcAYRRieSICC/ZnlwzPqZVxFGNIPza4bYe8JIflekPHKJ2y8kY4A6JThq4hWzVa7Icw7E4MautmpNYq9ic5ERcYL5lizamXYZ0u8KiRQr6bMW36d4jdgaIfizhbVxylBCtncriR4yOhSYB3Vi20YrzTorBwaDu1xcD5m552Bp6MKbcQ==",
	}
	usedWallet = &mockedWallet{
		mnemonic: []string{
			"trophy", "situate", "mobile", "royal", "disease", "obvious", "ramp", "buddy",
			"turn", "robust", "trust", "company", "wheel", "adult", "produce", "spawn",
			"afford", "inspire", "topic", "farm", "sword", "embark", "body", "runway",
		},
		password:          "Sup3rS3cr3tP4ssw0rd!",
		encryptedMnemonic: "46OIUILJEmvmdb/BbaTOEjMM743D5TnfqLBhl9c+E/PSG+7miMCpP3maRNttCP3RF/jdJnbzG6KkAbcKGXJROpF9tSGV5oizjp07lRG85fQH8OSJajn515sclXlKjX2aaB76b3Vt3a94pIzeZrQ2g5c8voupYnL0TDAjLd1Iltl5ApKLuPf5WfEJtvZ5Klb4rF+cLlvIjPtdqFHIwjotB8fR0LGr9yw1hfduDOWe+DPyNCkgbtKBKe0qWjBnnng88eMdlD8bsanuEkoiDlyHDnIvZ+JwgYOOUw==",
	}
	tradeWallet = &mockedWallet{
		mnemonic: []string{
			"useful", "crime", "awful", "net", "paper", "beef", "cousin", "kid",
			"theory", "ski", "sponsor", "april", "stable", "device", "sadness", "radio",
			"outdoor", "cook", "spare", "critic", "situate", "girl", "trend", "noise",
		},
		password:          "Sup3rS3cr3tP4ssw0rd!",
		encryptedMnemonic: "HI4irZdJa+4t/JEeFbZyehyuFb0pUmbaK+wpWUNZOBW72r8XgwTe8lpVciD7jCgDDFiy5oR/SuS+WAerfH3jAr4VU7lidptY1Ru4IbMS2o0nX3wtycdOEQJB4tD3Z8eGQC5ULZkPzk0cZKN5xF1+cwQMz/xe0x7C0St5Gkb0FjMKf/o0NGJO1DbrE71QV6ouECjxL9SpjIRb5aRvi5TsUgR7ZlOq7fHD0oVaDL/AMzZGlAtHIQJdXKb+SkzMD8JO2HFTnUns7PTGfMysOK6bZw==",
	}

	feeUnspents = []domain.Unspent{
		{
			TxID:            "ee0d98a091e688959fdea2a2f0e9363bfee3d6d666cabe1294e6c9366804e127",
			VOut:            0,
			Value:           100000000,
			AssetHash:       network.Regtest.AssetID,
			ValueCommitment: "0918c1aaf666bac23375eafdac9409cbdbf0deab90e1350ff6c2caa5bb2cc03909",
			AssetCommitment: "0ac428332be8c6ee6b0ae36ecea969c69dd21a01c69e2de36f111c2f284efc3f17",
			ScriptPubKey:    h2b("0014c965097152a4f4ae1552cbdd49f97f254abe268c"),
			Nonce:           h2b("032586270daa28bdb037f9684850a915974bbf021a2fab04f9181023903488ac48"),
			RangeProof:      h2b("603300000000000000012391c5007a5a340df88b9d1c2eccd29a8ac579d3ef67cc95a063ef3ae569e3ebaf0fd030a46a017d88e98e65a83cd835402ea5e1167bf899233ecc8658c0a96ea5f7204fda184c317bed68d960296863b96b0da325c3c1b50b34ecec1ffb0c87e44628045f6b9aa75736ba4c37aff4370ab46fbe15c9476c07bd0f7279d917dd9259e4840ffe7d7b323188add07d70880fd75ce188b448c1f2344a5ee7bc574e242c5b5c48b37e808a6f12824db87feb4f1518a092ecc23260281e54f8716e1c53d76f13fe975d419415819c0066434e5191d53040ba670074f793676a558e04143c5a0bc540dfcc538b39681120a239e9de835494cfbc90ff627aa18e6703f036c23b5c714728cd6d1bdb1d9e9aafb1af4e7e7d78c027b1691912d263b4dbece2e04e1efc32e5c21b9a4e9b0c8d453a062792b17a02a97127741825a38a43c20139edd653b6d20286be6eef1ee99ba0832abaf019b7e0978f6f0a7156812b3667a3c06ebe55f51a640177d946c6857113726e2c534c649a7b2f13690384a08739135fd47f9fa80f52d61f6a82e5833245b28d6587f22c612e81b675d8fd62a3d8c595b67a3968c2f6502c3f0b9c8abf5fd2ac55c6823461b3e355a595a3211d3a8e2f6d08131f2c3a48bed9e8d365ffb7e36c134aa658dacb11a2dd5adfc7fc91233932481b63280607bf207977212633b70807a9bd7c908129d75bd3879caceb212905be018a10f8788906d0bd3348689cbc9e417271a52d827e4faae6bb5f441704018c20028265ebb2083bcd1d42f73b57232db36a6be9d7ceb6110049dda689abc6967abb267a1a760761514f712977659d3693ebbf5c85c8d8fe39ec62a5638f89e234de0ee3510ce44dcbba3189a5f6af7a4fc498b366ba6525ce59307d2ad8f80d820104659a41a9e7f745112f2c7a946aab8940afa01b6961322557a5b962b40f6922df574287b8cb98a689b6790f88ca181aa3d68b001653c261857643568b50083a79e36c59d736f7f533deee443204da950dd17bbbb3acb06c6656228c19f69ed45d0039fbd8834a0748044deb37a76f4f12c243c9750347a64c23e2c7cc2510a1f462a8c31374dc0229801343c4b7244f1b0ebad82ce3f27b5598d28ee368b8b36560df77f24cc8d46bae74d6e037f496954ba6d74c5382e3e07b4b56b1d6b6291e3359a2a095427398e24fe1a88695cee393e1ef6aad1483781a6f586d1c0157ca5be68d156f892e6e88a5d91dc400102938e4a2146ba851e694fe6f7e7bbe1fb6115a5389d01e1757bf54a4c0a8cdaf95a9efd32744688529285ab62bff2353869096153925e288711c7e07e7ba6215f2728097fcca3eb1f3bb736167cdc48b1b7655bc067e34de624a5306aada24b219a71f0a92f24c5ab9ebb3de770f3a0a792eb4016d50f5c89986585be8cfc1753eae5c980dce03c04fb52ee4c3fab92ceeb379ddafcd494326e57f00936da95e4d248cc28ffb11368195f700d5e1aaa9400e0494d9178e99bf07a6db3f9ab178ab8e6b46fa54b09126dc169be5ef733a38feada846ba17f7ff8ce2f17304b25af797e13b1b44fbf80a70b8f8f58a25a4468edea6e7213ad824db84eb979715202e5d633d778399c53397053b7cdb4bf20d658968ea228fe03d43c4d9df22a6c38c371d726175adf5f7a414371898f30b9a8844b3de63a97dee6494470896977690000bc4d0310bad6c267dd74150eeb0d179455f11382a756a61075bac01392b3ffbd9abf389b745d4ef1220159b6f4e8d3d535b522fe09676d25991b5734a73637f685ba2902e6ddc887396cc8419f65ee84f2335b4572dc941a25a3aba6437587f34d284ee24d614d10609a78723d4b7c96b0532d00e8accf78196fd0bfffe73ae019b6e2448d09c26c9d9205c8574d08b5e36f34e2c7a0bcad447dc13aba9363c6ee8d714b4cb256fb5f159d4bfdb3ade3ddd6df3f6d82a1434f20b33f39470c472bfc57f4cdb91ebbaec95e60be9bfff97993725e3f30c45e824cab64a5492058944ffdcb4c97fdc772a8a9ed82cafb18895143df2ebd56754defd2c15e21f8420df737d80b13394025a3c78a219e15c9c477ecae79d05418c615d337a54813092f70c531cabac5f18c9e74dd87eb0ca09ce8a37e5cd03a7d328e02a001e7c6dd3f12c4444fa44ee31eb2533ddb808f043eec782d1ec41477a963e3d19907c491433a7080046b6705b3e391d35ff368f8b2c7217253fc75937f68ab9cc081fb89feeffcbb10e2312bc17e572bc3d9af171e07b6b034dfd7a9a4edacde7d50af1478b11fcd6b9598f06b040d0c9599de51fe9b4720f06d6beb97891719a1fd10a1e1d33f6c8cf59332d24aaf89c0fa8f12e83de9178172a91d9cc18435590e17721391cc4bad2455d9757928cade05657fc18f8ea0302165ce21e43251e2921f857c46f854d49114f0bbb6647ca9170a261004872ffb74e07f2fd4f1d5edbaf8cfcb2233b4434809f3d9c17ef72e03f17b8a4046160cfda6b2f746612115cab651e686de15e39ddfa0d07b0c039ccab37b5a5b06ee4a86846548d0a375b22bd3aa27f51f5c767742b42c8aafdbb87f9f7dd4d0aa3661c8e4b3fbec3ad1c1880fd013c14733195fc90b4f484c1f964c7ae3fe5fab8a64f01fe07d78b40c49b9b31fee4c9195eb1c1cd56b588d45d1c72285e9975af64c1c684a7fd1f8afdff674de69280ddf0bb3de65d22a4f7246ab45a7a692b3b32b3ae200aa428269a78dabf1fa2ace1c9e268ea5369358af9e353368a6c52a1d99115a659b03f4b1076063b9cfa73b827a13378d9f24ab133c3eb20bdccce02b99bb0c64ac5780884d966cb14559a89511916d8ddb23d0926c5410f2e54cf9217d6b3fd595da8a5886e3d55a9d735c6eecb98190a543fba6ea741d3b4d9f5b0094dd826734f88119def3ac6a18a1e30036096b71c055c7b85187612415bdb481f8257017208317d531521a74666986ac26421f0a73018bbb821c7922021f37564b8160ec5467f795d2faba7b562d181989a88ba1987b2ff3f6e65f26e99816bc89e7d9c4e474cbf7fab2b0a85f77664cb72914d5af989a3bbb04523d2c8faf63b6581f24b62376b8444fe992ac433b5aad46983098b5998d11c2c7b3f4cfb360f8b184b0048cd1315069430aeeca485be71a6f1640f88005cbb9a206a59d33b383bc6470b974dc884f8b07496d949c38906a954fd82b3ff6366c78db8cff3c26fc3006918aef7f54c797ddf0abed85e29be5ce9d07dc5fd533cd7160ca6ee447a56002b4d1f8ed0e0077e2949dee60d88eb83a46d50100a232a33859a4219bc78062c2df1983e308c0aa7a0ddc817a2af17835163b0e6b8e5ef2f11522f2b8f307d6c1796bfab970ba50f27c31f3ed2eecfc88c8b64bbc6cbd45cfc8fe451682f76a42a26dd18dd897d823c9216f93b4e359b02ce35003090df4f38fa5e8439623e520ef0d219e8d64014ed8849e1ffebaad0b576eb1bec00c07cdc5c84902cd423abcaab2f1a98cd9f7d9de8eb57963b8e5537369d9420609e7fd4802328a1a2b243bea87ac6f2c7965bea2a743174495270e46ba2362eb9001a6270bf4c6c07bd925a21b5b7ed237156338b9e53e16afaa6f5916e61556688c5ccba79b5e395b7e7089d5241eda23fecd640e42df999f3ea2b97c7d6c78aec579c07d7afc77ee66d50116edc438412886cd02fa92491323b2bcd1092e056633acaf774902ca550e8a59ff8c5c95084d6e78c592ce60f82597ea1494022736faa5644ccc9c3337eedb33bff39920bf0b71fde29e2e31afeab7c65656f15205a8b9291e7f9066af5b5caa2279011868dea216f48a36b4c4821631ef119ee3e8674005b751e1a44e6e62170f3fa0704f18a3a96034362ea46b259a2e2b91d8fe8e52af927c3d6f6074941d2e3a9af1be9ad60428518f6f59d0e85bf317499666682c6aa799e941567fed0a743822ce6d515d829d5aa3ec64565ba4d6dcd5f7ed4b4f72866b0e57359cad2b040d3b2518491400276a1e8d797404fe8c043e8141399779db8cd398c600ce74773d6c501b77166e922ce6ca9b27cd18b1e60e2b12ea5efc9ccadf2acfda938066bdcea88980b08507090e35e6ea3be5581d093284511326f30d9e43591840ee7a292e11971d7c2089376e834e3d10845524a6cd189cd46ee1272f754fb13ad00730364d69b188a4ab6076feaaa990815a15b1ea5c533497932f8a9b2c35ae54dc22f8ca5183aa2543867f8e29629db20197f4ac74944aa1aec9cd3376a8c50f6a1dba35c606f00d324afb84c368afe0b92d313f415e25195b918a397699945fc90d89bfa2060373d8f52f8cc7d59931a177403428c5c44a176cd6b50d6cea1300329e779eda915b5042312a548a64640ac49a29b9ca6e9f9f26233f1c2fb5f4d52883ecbee48d09b744fa043f6ac85bcc92a8712e7a427c7f69f77dbe99e8c27a5dc6590538be070ff1ca6b43df85c449ac98454211310e595f206854d8cfe193b62e9eaa9f91b4f468758f9439a3fc27a2b4bb426d4e17366dea6f68fe99837dfd284f41b706f5f09671b5acfd8d623753a12a660b97badcc5f5f9a8c8e5e0b57bce9652ceaca2920ea493957e25b73c02d0ff5fc18be7a4d6848de9138f541f583848922985d3823cf620ae1f3846510f64e51b16e111aaa9f636cdc9b79bf98bebf628d1ada70de6960e04e7b0cc2a2e9b1e0d2b1d26c1e4f4b324fc08a1aace107d366af43c482ef445812f1c46969c315aecbe09b116e209378fdaa1ab4c08dc8a509370fa6e125b2b0d60995268c7cfbf2ea8f38024bde4045d7bbed4e624e558b6c7576276b2aff2e6089f9fdd2a9f86875cdea204e53f37eca4f89702b2cb9f7c751680039aae274102e58ed1573b2e96901fad687547c2b550702899ddb0be3b0da9bd21637a8ddd3abb168166d1fc5e7b3cfb5147bcfca996c23654daf5d7f767771dbcd12008081990de3e89a500cb30d46fffc6789af62ef0a3f42dc3e63a1797611b379badf6b5a11e58752495053d3379ee0165b70204485fecff27b02efe330f31def2260b5a1e91b13d7faa60ce48ba04c2db2f6a46cab5a73619035e0d262651f0e5bd80d8b6c9851da8c17be20dd859f3be5b0cf53f97cd7e1828dc2784b9557a73a4861b1b13668798709bbb036170ee35617ed052bdf9971aabcf2fa9d468e6b25b0e0c21fd73ee9bf6b9436d754c4d1b6dd8506f83d79e595d59d6c553abc9b93fd7eee13d272f539431f09826a33f3cb5fd58d37980690284f971de7c2f6b3e197d80555f5d6e9111ae8c225ee1ce4506881bcfcebcd55d8381f7f14584cd26baa5fcbd81f90ed90d787839467673b8acf6eec81233983d874225a3f8c9763cc2e4b5cdc6bc327d798886717dbdeb2e0734ea37194fd76e08f628d007acfb7dbefd0d3fcf6473ee4929f63277e00f5414fd2361760a92ec142a440721c5071c3fe21c1e5597a55e2dbb72bfe38cd3ddc81e6c3f527ad9b9436b92130223819059964c0519733b664d0264489c6fad23c2fd0b1ad7c8967344fbd4fa2c8304ade93fb841fa2c6c5812ce169995fa9f899b996ec15f315f08855f4983c066b74e9f94b218bc9cb06b3d324ff09bd8987c834e31a6837f8fe44def4d21388c2473ec9ec7ee173c0f23e145b4038772512c518d287bb87d4278a3a3915de84962dd027f8b132f9e14bf690d1d88e547ff4cdae602d1ce980679b1121856f82c302c5d358920a53519ebedcd8089bbe82d8ecdc0b9b23f8bd2c581fee8e9f7c8d6e5fa4916096909a9439e5b19d43c4de66af9ce66f110b64846be19f6b4515f750aeb59be0cb8faaab397a6cc7127340cbfd8a56e8dd1a7c3d7b43dddefe45c64d486516a121aea515"),
			SurjectionProof: h2b("01000172eac6bb8eab6bbccbc2e39559e104575d4833c39eab1d41572d9a034dea2501d63e6ff9838104e29e9536cbf2163e60951488a0504274ebd811aa72d4aa53cf"),
			Address:         "el1qqwxkuf90ne5meuqxzmac2mh44sayll66jzu5msy3uw8s7chsw74r0jt9p9c49f854c249j7af8uh7f22hcngcdtu0jvhry7w7",
			Spent:           false,
			Locked:          false,
			LockedBy:        nil,
			Confirmed:       true,
		},
	}
	marketUnspents = []domain.Unspent{
		{
			TxID:            "1d05e071e1110f40c5fbbae85571725d2ae04d8240becfc121b50b9521ecf64d",
			VOut:            1,
			Value:           100000000,
			AssetHash:       network.Regtest.AssetID,
			ValueCommitment: "0879f93077beca4ecac9f8973ae2e1d5be48db4e570e50b71f8fca15d32ed52103",
			AssetCommitment: "0b4a983148032ab0fd6aa99ae0fa6796c1f23c9e5f9e6021d01f98c97673456be8",
			ScriptPubKey:    h2b("001486db5b499da2c2a8e533c2074de43162202e58bf"),
			Nonce:           h2b("02fd097834303f2a57f9a0cf79b67ba88adcf0eaf19009b826a0eadf438ff13728"),
			RangeProof:      h2b("603300000000000000017e173f00963b3e92b067a10de3e2b0495ef1c71a12d6f363b75e9f02ee4dcc830b6a01e6f07abc9061c86b514c1473a70b8eca30e9ba28a7704d5abd9c4649e8d2573fa3e407f9ddd54172f9047bcf01152d668cb97cbc5cbcaf6706c963f5f738e9b2d011742bdb91ebbc5cd36fdf1926efd7e7d1767585f5506e608f8dfc19a60fc2d782095ef17da74ea8c13b34f7bf994364e969312157d504512063ef32387eb7606190a86e466736901c955f63baf8e16002ba66a4ebe6e9b26c172938f145f7585a7c596672b72a5878880d434b9e98ab0ab49a628e0ee5a0ee87c3ec983b024ba49fd2d16c22c93d65e8d213a6351328110ebd012ba46e486fbc347272a93d9b86ce1d24faa118dd8d83e71a941929c4856a9608d88850dc89293b601371089b7fb24f53cbd54724029f41b636630554def082902b0df4807b116c122db2f2253402715b9fe62b4dc7e84b16ef45eb22db339420921bd8a9a1126669478fad04f18ae74463eb991c9a5df6cd7afef3d209a15ae9583d583ad7050c3251c6a6d4b1035bd9f12ce8ac6bbb3deba58815ea638feaeeb130184063dfbcbd7e6d3c968f75be8cfe3eb4c985d8c64aeb023b163fcdf08e4849e9161c95c61c49a3ca538640afefe88a09b1dc4a358ee9efcf720b6d072badb55f16ae4ab0337695261a0774cf0a4c43b08fc2a97783efb8605a63424d4150132119cd0e68496e66c976a6282da30f061b1040f0e113f5554d68dd3b08c95dd4e7c860988f6d8b869447122aec46563bdcfb0afe3cbc4e72e6a4bfe2c9c851f09161bcd443f4aefae73c635d30ae9d4b48d0e34530e9168c60bc13a74c95cdab3c2b2a55a9be59e5a75fe1862fc101b1ac5bf97b7ad156f36422ff1b9d1297aee7258fb7593852d1215f84957f3d7a21fefbb06e7735f29eaabdd6ac4cbc777c23f2e5ecd5bd2cd313f30442e08337c4624c3289ccfb322a7c500ba122959a32d4481a622e285ad5bf9bd66f78aa5e680a138d8779083811dac48252ee60923d4eb67e29d8b925090f8c303755916c032e10265ae80ebfdba1af3a91a4d02f84569c9b92c2c0bb76edcf386f0b86d08aa1707c560479d8d6daa8299ff1ebf6356dd53579fc5607b42dbd2541e29f259ae735a74df5bae0c53856853ab9caf491eaef535581018b21cf7aba7372abb0e11962bbd5b17de6674fec71565376934b7ad8ff3fcd922f65d3affa7ccc7e2badc20ff06b9ebf0a4349693d001c49c85e788533c34862d8695a1be3b3ca55930c0734c1d5a876aa825ef080de232c845a18dfa5839d50380de02fda4e5c2b6832ea49b70c972b83e62a85b5d6b78b089472064f84dc66b3c7eef2ea565ae7c81562c33a77118bb52851f3c90a7ff4c07cc3390aa0d6be0719fe9dace347fd282aa6c1bf1a6e5deb7195ed1062d2a7302c5f7f20af98fca4c957e7a37ef9b418a26d4557a2b4bd0435bc696cc844a9262b6cfb2eafea3c560f7260e5c3b535cf8d184af53e54489b133a4d3804a5ef2362deaac78342f520d18ef0f0fd49d6994f7c5bad328dd6ebaf7101facf3af331a9669e71a0b78d72b1d12013954e757f7fd212b6dca62a80915568bc59625977986eb73e9c7e11bde8b2dc8f986e90a68443dee482bbec1bf73968c3b919db2a36477449676dfa57a980c4ef7867e13de5af32b8282fcf2b55babf8d5dd484e7e80761d9c1eba5d757095014a8c36b1232ff76b316d6c4888e73ced546925fa5a51af5b96d025f39596732b3073748b098354a9dde93a9341f726083d96692ece735452ff6a568d46ef1277225fc996470e621cf01548d5b05bc9a4b92380bbc60fca27b9706cc6b766e5fa6206572983e620f9ea570e21d6a71b8884b4529ce607862762e11e0213e90a874458759debe6172a92363264f63c7eaf2f2785cfc55c57207a525e02ef02f2b29ceab00c1184ac75e6578a7e5de29cc0646fba794ca0b2e03e6c4cddf51c0e9b1faccbcb31a71ebe57f0af09ba871f61688e849d873944920968cde18ccbaaacc808286347cb130c33b0aae7e0cd2e0cecdee6675ce47301797456f0f777c46d9fe8760d879de7aa8332066f2c9815314997e1b7c84c3262db8ce7cd4a8ec9c671a22110ced2b83006a18915d0ce5ed52f43e8f93cb76053422c422b10e3ce53507aa8214c80b631e388b98abcd0032622da5f8c70680f4acbefe284c7affe4f780b636b2f26893b555266c12048372ef967f4553d72c0c4daa9d92d75c26c7c58a196400b00a9cda8f8d974a048efdb33f0f8f3189418ac22d37d9be54406377e197c37b74daa93575de857333080e018d43db29bbaf6078d62b968237bc51477ba5b48a397eed833f125c0ab7ea26be84302171591fc426189e1a524863ccee1784f43b6bce610628100eb078f3b3afed34b8ca3cceafc69d6a879d8e538315cfe4bec42d4b87ecc74d03ae31de0cc81bf0f5d9530e8b793588da72fd7f3ae132ff9ddfee38bea11df7b2be596c042ed37869e073b6c10d8af82ea3dbb5a6e0bb600403e9a2a324b7a7de4ae071bf3ab5d53d9b11af047b40f4e379e5478ef7543ee61e566cc7dfc63301a675eec04d02347ebf4c105c789584c714b2721dbd2d63ab9ad0cd4880e629aaff4bc2f828faf063fc35a8d93132ecc95f063798680702aaadabcaf2826f1880c133da221392cf51a85637a85233468cdbd993f9c41f3d9c045afb12f1ba5c287af69a487dc60a83aece70c39c50ec9bd015e5ee3959ef390579fb80ca0ee2fa24c17063ff65f4f879338360c8c23308f3d99094b723a07a5b53b4c1a5e73627e016ca9f25156acea0eb7ba8932e5011c8f902f44d682cb3efe5db259ea2a9290d86d9792de3f889cf800a074c2fc670d0391947cff4f937b5f83e3df775a6a98c183e55bc58c96109d6a80292fe306f987d084ce34bf763e6c146090ff14ade1a2cd515d2e17cb2aef45b1f3f48e3fa95a148b12c687198490b98d355da24de15a840fd99a4e688944a9d848446d70969f6103ca956759f9f503ed69da2b783fa0623b0ec3b69b92c0bda8f8eddf58c55b004f3dbf80e24270f037921e6f58f597445f5fa434e8afa3868fb6a568d29de6c1987d7541638bac2b7fd9c016cabed20caf4594f1f3b1be72e6348972f89fde6bdc96bc68e257828810aa6ca7d9d52a04f89cc63d144ea79d5841827b556a1d4a73730338ca217fb513c0a228402742bfe72a5e4aabf9d9a5d529c2c32eb32c893d624b505c40a3dd8f4c19572f55cda16982620eb559a7398d57e583406d3c2c899c4a435bfb9c876fdae8e5e5d4b166a3aa0fdacfce0094fd3e7edc038b020ad30b59f5d1c589aeb427fb9b5ae0e4c33ef0946f05fd4b6d0ff6b3c5093d1b91695d39ca292fccfacd081ea4f597fe863005c94ba3eef2842d22e19a3f413f83888e57f6b566741a4a2360dfe4c428e609d3ad73938eae701990ce6cd63dd535fb44463fa10c05285540b3e23f0810713aaba9d9b2083bd142a9e7bdf532fbd218e87b9ce98510cfe4faa2dc33775cf5090d768a67d1d5bd86c5874a2497034505996fbc9377dd4bb1869c5ba08fc70df1155f691c79f6e8511372e721b06da1e75620e283c76bfc88bf5beebc1732d0424d92c939e01f10c8630f0e880ccf82b29221beb2a9566db4ce6d1943bb10168ff6bcee13cad5bc1af3ff685d37111385309fa883607274cd549c82e866e3d14b9f64a39ef5adb4adc4b88b34ad49c53170d064128c3a9a33e99679a5829ae2c02bfed40158190a546917d057aa90d992598d019df9d4619653b2cec16c60bd38944bf8d1a03524cc1a264044c70564389cd633d278024265cab5c01796dc60ba251837f458e75c2e70e32fb26e8574f42e48379476fd201deb1238c3ec0c1f46945bafd0ad43ae708d42d51fd7dc287b46850cc559790321941babf6c148e07f21575a14782fe1c29b1a6c978e74d83efbe82c7077b636395d6fb4ff61bfe6559bfb01d2b72fa4b373423c8cffd3ba54b9f5456086f6d34de8fea8cce215e82708a997ca724262dd09cbdc4dbedac27f6cd58530d87202ea1dc0641e4539ad9bf8f88030d74969ef8de6dd4a6dae2581a294e46445907e16ad68ac15e2327aa1ba57917736c49edd3d2ff9b715064bbaed17d286da737a5bd72e862c9fb737f647c6b58b0509abb6eae194e75154221e6d4975bc42d678bd9136c4bd79eda851e60554fad822445cfa76bcfb8f7aa3c3171bbb49d47538cdda9dd9cfcd21020e53ea5145432bf7a24f317dcb55821e20278dd0f93e0ee8a82dd8723910ff538b7c1f78a005c7999e7e566a47a6e91dd8cd0752d0538cdd911c12ac2b687a40310c62518c8fa5dc0446f9d642bb9d53caab997cc307ba48aeef195bcb5fb5fe69cf917a1365c93c15b0635b95eb672e6f38f47ad0311a69ae690ba2160d639397a77639ea036f752e395380cf522de14ae0d14186896cb8e5763122e270d93d07b5944be29f2eb588040e0656752e05499c1f993742932709d78f4dfd99a179be250f54670c465dd8b9f26d4bf2b4a2361a0bf24c8aa806cb868b3a856ae858cee9e930aac1313b9ad57239067fcf3d2278df5ccef96be9eaeb17c98b7328804424f2fac88b9dab0538ebfde3440fa42bd6f7a86867c5a36bd7b114fa9a518bb0dfe3d8f3f7114464bd4c98a172626854ff73e9548c48177e11429a1657e7ccc8999c31328ffab31c3b35ce7029bb4a1f0ffa28b403941c76212817ce2def6f69debfe64800e2c55a651d34254f92a3479a6520951adb023a2a17c22cdf4ff986612f27c58d2209551037639b6892f8b8d8a803bf36e2f7d9b23ea12123744d0b2e2fbc2e664f5d6a3cc911fa556c602efda9e6671936682274047037e0196e9bb873695d6b310c64fe9c39fd791ac51cb5b64370c39d6b482df7da709bfc09115bf5f074f836442e3c6a9d8890a50e1b6cf0f89f467c7dac561cc828c0ab145936c93bba338bba6565759a0299e91d9e83c6d9ae22f397b7fa4a12f99c6bf18cf6ac50a0d98e458377c54797f4547b45b9e3488761ab999ed634802b92ab5305b0d1a6225dd066b545a8da66408331d4fc1646b694dd3cae2eb4b000872508cf02e7b872631fadbf6381d4b690ffac2ac30fa89d0627003fa419b35a2fe73a89da91cf21a8e22f1cbea618f7ae8814bc7ea478264a62399df2ebda2a9cde8982dfe116464f8b280ae290ad8ae44664568a9b6ca9b43d2a7d9ac7b3c63feeff47f02b64992514d57d1f7e415358084e176fc25e581f0338a1d60b6ad62b676bd0bb2dc0dcb4384d9b2e3cab2d406a902c008f411cdf01095f18c0012e8c0f4798c38b1fc83c8ee53c3495c7c345498ea9b914cd1d476fb906bf90beabca13efd23ad203fd17dab47fa547df67fa8fc98fec82054ee796183d27745469d32c3e606ecdf0e8138803c7e9299711df3b0d338742bf9498a23fc36a769a5fca1c8a0eb57eac3ddf6ecad256c286222192788ab54254b627d68dd6c98fa51a76f9ae08b72c36753d62949bde5ce67e87839f5ff977ac868d40d8bcaa5fc6f34a13e67255e65fcf993ea9cc9d2d9f6bd89925ef504105b8baed371fccefcfa0bde27249b0931c636a13ae8249e95fe0b2e73f483ae790369b46eb568f8f9b8f52c8a42e0901ef14f867da0e7efd2c47b89db67499c2150f9742e527d2e2ce5fb7a7cac07ae5f26b2ba5efbad32761972e5dd52d16d1aa85984b0880788609e26d389231dcaa367f78e86b80555cbb1a18455c13a52bf05434ff52e3d7690114bd7f70376ba76557f587adbb0cf6e4ee23484b62e3cfc671d92b146d753764a5f43ef9b4f8209782947029bcec5d44fcfdfd91af5"),
			SurjectionProof: h2b("0100019e41b0ac7018972131f6500c5441c1c68414f5a1eabd7be85912cd5bb7ddad9d7a79f4189f8bc60315264a04abaaf263ac3bd7f60712ff2e55b8bdfc32e4792c"),
			Address:         "el1qqvead5fpxkjyyl3zwukr7twqrnag40ls0y052s547smxdyeus209ppkmtdyemgkz4rjn8ss8fhjrzc3q9evt7atrgtpff2thf",
			Spent:           false,
			Locked:          false,
			LockedBy:        nil,
			Confirmed:       true,
		},
		{
			TxID:            "90466cdea09cedf59d168d2f0a5a6c00dc1d5788fb68c34084c2b7ccda5f2441",
			VOut:            1,
			Value:           650000000000,
			AssetHash:       "d090c403610fe8a9e31967355929833bc8a8fe08429e630162d1ecbf29fdf28b",
			ValueCommitment: "085eba23052d6230de120960bd4292df25354169e16f6b356246e9abbbaf72877c",
			AssetCommitment: "0ab7227c3e41691d75345d0fd8c3e032c512574f43762cabe0618230cdaf077273",
			ScriptPubKey:    h2b("001486db5b499da2c2a8e533c2074de43162202e58bf"),
			Nonce:           h2b("035582fe1c32fc3f7bdcc9bd755b88316a27d1b229a242c844cabb872364c20b3e"),
			RangeProof:      h2b("60330000000000000001b0c4ba00cf7c374cf1138be89b0b16bad1cfd86e95081f2e0e1fcdafaaff5beac687be88ac03bf855c5b74ddedf6de66d6a41e41944cbf0239c2a6b532106b94f6b0598d4ec6e05817108932e949f4e91ae6d2dd6e339afc18515b47389f49977f4c64774f4c807adc0f6ce1731fad232801330de69346e424f808605afc7c3b66423efff3879afd529bdabfd07c1a40882d01495a76782c372816b9ae08cd7128c009210867faa649133e7dfa35b07578f8642a620899f074496e140a6885dce940fdf4ec063f01ce868591ae42bc0cdca812996168dc5d01dabc06fe8a3dc8090d6595c4e053aad784135d7dd592bb0a44a6752826443e80572ff1b1249769e206632db6b6cc466ca1cb00246fe33619ad48fd1f650c0f24e6e2dbf218bc711b94956d09459748f00e2d80d217cfa073e161d54a656f78144fb3489264ea0cfbe16a19963dcc4a93d0be83d7bd3552d3d0beccd76fceff3fb1008bdbf3f4cb6dcb2b7eddc9c5eb99795435a9ff0c1e557714dea7b3d912ad990c79993816bf8ba4a3860aa95f11a2f0287d3f56d09cbcac6bc02f98ac905c73b234e73f70e298f3ecfc211613d9a0c5de786b802d525fb350d16e9d972c00d5476eb79cb3ed121a0442fff5ec89735490cba983cc95523c91686f3a7142403b1daf01c6bf5f85ed80578b7437e66289b13f7afeafe99d642e5c13915f671aee8ce71763a7c353490533b80cab76dd8f95d6855edcf1fa9c514244123c0c6e3744c7ef6900b1ff520aa6258e92acf1ee98e8843a9e6a77b043dd68d779c6f72aea5925927e03e9f6249ab98bd1846cd182f956f14e8705143214f4e63b7440705acd08c3cac5c3600b1a482da1459705f9451c85545cf87424bd41911f93493750ec0e710c89b6d5f945f40f76c04054adc055aedf530bf5a3054b7d1c860f1b49b6661536b8f8b84d627c55d6366179206eee43880437586dd22d96d61e75cacbdf3d5bab7e6382554fe3c3af8e2f5627044fc87efaca0358a8f5368e595b7ce2e107beb98d8a39eeb52c3f8c4df9ed437d156be8417959cb31305b6034a7224ffb92cd74ffa3031f5173ac09564ab10157d156ad7ddfd288fcded894cd3e6b84c2f3096aae473cc8f42a8e7a2e6d8e7aaf1a4bab81b52be4487ad4a6a11841202a2c64cb03e34e41c23eead831cf2cd95e0413c4d7790b9c64d215c617e7b6fc51799dc8587598eaba0b2854acbb518f3ccf3d720d792076bfe22235a01c4ca8af60cdfe3ae1d6b84e24bbf49dc4513048dd4dd88156124112d632fa19d641640fed865ac266582b6634d2be921ce5341c82f867310772bb93156bac9c0277f2a8b075675c008f2dd5873f122254620e6ff39041458e0bd31c82dbf0b0a6bb0c195431d8627731e8c29c3ae8de245ee371770812f7f5b572da0d54ada528a15ab92e9958f7ea8cab2dc081b044170619e01626776a8f1a0a7694e1bb81c6f83ff7c7eac957c2ef95f2e825a03214034025dc1d9f55b619b07d260a8ff80a5fe9161037ce33291e04fbcf9fc3a7a2744e16b16ee3811c265cf678935287decfb3b051efd8118e39ed6d988f2152ed796b75d58496a6aae9479d0eed96093f4e816fa9a28e790c2f9ecea13ef881a2c50606c4c61fc25b0b03610c589437bb5d0f84f6a09f75e1396a62435f5cb16e1a3026cc0ce78a4e6d6931f6272520859893af5fe48a9ac09875a303d07c1936514e49bead10424a7ed9106a98e53ab6e6e4530443c685ab70c359faf7fb56e5dd5240b9390e7cce4e6c1bca86c895a44d3d028946f9dda89b5238cca11572bf89fd3176eb57e4f2439019b2c2212941f105d174c1fa5ea5318131241abf503077853e44fa0b80502e7524597886ce088cefadd5ef793fa0b25340ce5723d67cb1019053ced7d5aa2855d1ebb926c8639008363b771dbe4c2ffb4c37df92c3253bef7dcda305bf9c9695b2a5dc0f24c6044a88db5e20bacc376317ddb64bc241f19ae01f3889f7afbc2d278b7d1c2808a24ad1b5297b3e87a1daa4faec1d8bbaa034ec15823e75856b26133c8d8bda3153a26f19b5ab126dee49e142facc6c99bc02d0c9ac58b0fa2e16e584e5e3050fa1abdd4d38c95a0199bab0ad3bf98c510b878ac27932ae6558d5264c8da13ee9fc183300a68eaed99fd8d659e5522ea8bfb949e80ceddc4074dccf136346f669499d4fb4293e5c4a723bb24d24ddfa04c6003f3baefee373c246a44d54c4079781ef465f0427972296f32ecb5e93bc6dcc7e0b4f7a538f4baf274003d452b6d166baaaf0a7c7d7384aa79dd147e6bc412777a5a5869603b33b8c6dafed67d75ec629a7bbbaf1a3b7aa83ffe8c1e5e3f6b38b663d42d55aa436b04a7ec970cd19786caea85c1c01c75af1aa651bcfc2c5d0d313295d833c6dd24332cd0be0c7e91cade37f628ad9c096a72442aac3471958b939636adbf645718d781836c459392dbb660e1726c7a733a021f9d1e532e342f9ff95d156028bca0096401fe63ddf427d04951832b7864f35a372b4e7f356efd99b4b49df0f86ae4d560dcb84a1128388ecf89089bdad2b7dc8a07eff5eb032cad102e76a8294886ddbbb9cb3d989e59e375c814b58616d734c858fcb697d07240baf43b24f169014ca20841de9af59299043b30116482cbeb1e9517b33664e8609a28f483fa339e31b1ae2ea9f8b493c5876ce4aa4f924c7ed065edac5e8981ee4556e1e9054be7e3b0b72ff6207bf726d0ac6c9a3e8803df166df015854db2179a7579c3f53651b7b056215bd2cb5d0d525eaca70e2497f1f04749600a10c737b1d3f700f684702070995e8384b7ea9b58e86ec13299fb584382bede83ff0ed04d5254608c66cef25e9e8436bb7d1d9bcb4480c56c900bdc9964261bea62a713a48143d92df49975e1d4c54d46d012b3de46541714e72b49ec086a7e24bbda35e8e30b05227a5e593282517e6be41a39206a61a13f67f4d3dfa217610e4da066ef11649a2674c7c203bcd2f9d95504e8a01231bb5feeff496f0358e1419bde3a05fb363685b80b9b74a792c898093f86e7229f4a6b74899172269fadf63ee2cce78ab9a8bed0f1063360c03d1c01925a8b006c12a79da6dcab90a704bde016ab85f15fb4157dff35506dbe7a1bfac92e5d65b866e339b39577a3afcdf05727f8eed9e1b6e9ec6c2a40d451dbf238a8b7bcd632f67ed1c745cb460276727ed6b7a450ff0610a80a849dea5344bd8d17d805ec0897def769f6c4ed53d578d6c2f688cfcba7c5e87c87242b41b22f94a3aef339308552953470191cf3d00d4b5b82c169aa9e109c762af306d503ec9f8208a432d9c41a462906bfaccb3acee2b229e208bf35e8d00f9734eb8497499b283ddd414be10aef2b4a7a888400d23b38c37db846f1986a628baaa532184c40bd887a5adde63d5721de569496a36dc2777c8402bee99ecb49af37909d575c2ced58fa9f7a56eba52a6ecf7cb2c5cb20285c55774c468d1e3f8b907f1ad08a2313b8f7a08149b73718799d4ff81cf5a48a8f307015ecda8c8eafc75411c09cc2ea8a7edc7827be403c38363a14b741036c272fdc15d53ae01f4b2af11127fc16dfd8aa48cce6c32f4ae21fe4842263a4fe1f3faf97b30776a4ba5af6b7e6a251d1a842d30b4455659b59441248a1bcfa73429b3ba23e37c5e0f1be1423804229c6cb70b72a5f83568ababe6793153291f4b50dc677d9d3473de0643296f14f50a2f16f2026880c7d3000547227695e094f8658e410d45f5cb54756ac104a38fbd962d2ef28d9e4b5db3cf92d55f22ff644a7d51023efaad12263672140d403e8a11ab23cc7038b2f442d0e146940ab6025dfa2488ae115981cf7460dc14675623f54d750a2b68bcd2d957f0df6806eb774ba9c218760ac3367296c076a77be5a95c77b63d98c79a323e650e7b231f53fb88fa388085b4781d827a392de5d7416120f2feeb55ce76bac8ccbcc481c65a454962c042d8cd6795e1a6a101b2e8c956140caa633937eaa42ebe69edc400c44fa3a64bd5f99725c9516a6dc7d27ea07bcb9ded7ee9b982ce9a8de567b3875993f931631f13303e19aca5a893566380ca73e365f527d9247d8e30ac2f7d61aae30ca41c9c207405eb9725f49e706aafcbd75f7577aee05373f6cd0c45e436652a189b59d31421c853917cec7feab41d1ac64c274b0a49883f687d977b0f953ed30edaa94c6b3cc214608911f0f19a745f187d672c2b380f7440838c06632e35e84d510b42b52ae854a356ca78da7c7240995f5baee70e5e5049368ac127e2be429ee0400f317d7118e69246f28f84994636e5befe2e938cef537a1e2cd04c31729aaa3722f5a9136b5610da776d9d1171eff8025cff5271e1ce6acf007d589d2fffd9678136c456300b1a08a1d6330c819532ad500f2e34be33acf9e49e67bd82c251a3a37fac04a6f30e861abe93b1956d555bd0d7bac508f5a0e2730b453bfc5123dd2fcd825c14cff534f87088a9a214fab58873c8ffa7e48b11f57f53ff9ccb0b7ff07fa50a2069d3cf56ca91d8dcdc52357ff1d993fb28a1dc64bce0db1f8fd1686ce7ebc66d83b9c60499e356ad42065723883550f08e076ea59017459c92c7abd341f342273277df7019e60c3ef4db42fc13c5344fcaeafc2fcdf12ce08f10251d3dfe3393d52a56da5d6a163e16bde4e2b10732239169f59bb017521c55d6afcf38774ce897dd408964256a33dfeb0ff1b0e13c90b7deaa60a35f37aedd404c7b78a47eacf8f2e6f5ad6f806ddfcad320ff01ce60a8b50f01d7a6ac5a6fc4755cfff9308476c45e2022798c0eca0da618dd8cb919d4a92ee5ed797975781bcb3b759f0fa1a514996d28c18fb2c26717dc175ecf5d5c00bae5871eb73ed6d1984c7c6e00ff146dbf58b94f066039b7401cb74af7777d2c4cddcf8c27ba4425ed2535bced2a512525c09884e1b457c1400f01ee786d28cf57946bd9b8ae3cec57c488e045e7bf08d2d78335b7a0b93d7a96cd1358bbaa09cc5079d5cada1303dd3341519ede49ccd7c05d8e841ba51a506afe29745eeb6498f9383d9641845a710716eaca418b9d309cec359781521830455c77b8d0cd721fb8add10f04a14ce70af210bbba5a3c5587fdb8a4370ebcdbbcbf08881b0dc1035f16a36a17ddd986b3d404872cf17b2c2caccd56f3ef3e10d216da6e76577a99d5d229fe741f617509cd6995c397bade55ba4b250a7efdcc5d9851a58f8a1f5012dd8de587265f91b56460ed941d0d9dbe848950c75ac20d86b450fc5213e7ab1c21209668d7293f4694d6ea094f54055672115e6c7e7bee9f619449a8d7a7e4c8cd7d25849c9b035a224b81b7b7bc9694676c8b06c21d6c299a80e7b648f944109966d6b168e761657ed42a8bb125ee5db0e6e32b7917c15cd9ade0461ce2c15a6ba424e2db8a9986149856e00b13fce3655a65f3c091b08ce078192b95bd9dee938978f923b09438e33fbaab69438cf0b95bbd7035056f81eae14b23b3f7b11f42725f2dff79aa368d7529400c5685547c3f5cbcb99bb291601a37e3f70b5971032e3b48871074efa1ddfd28c0791c47fd16cb0d35d9b864dffaa811d8a3021bdc5419c2fd8acd4525378b6d2a7cb700bbf3eb83dca5285c640f1816727ff09b466129e2b52cd5b9c7b6f86efdee46ba6ce34c7e949dac0021fef0cbb9cee995108e839eec233cb5515a931bfc8e278a891d441b8e7dad7c94fb4d3b7d0a5a26ea40649c67117684fdbb4ca6d442cbbe3cfd90fbb055de2ba1151942d57d762ba89499eea08a2e2a2a5116f009d06b641020c8d4fea9bf56db7e7b4ba0318cb399f434ea3be6ee1d7868ea36ad6c72e6b49d9c1aa3f36dca72ceb5f35e7b"),
			SurjectionProof: h2b("020003eab2daababfc65a4458a8861d641198ce984ca33c94b62b0a923fd3053ef3735507b8d1f8d5cb05a3d9cfe85f369fce0f55d33022054862086c452dd87cc8574a6ba31d50e25de1038b971620544b7e636ef6d60aac151eb04f708aae4b88599"),
			Address:         "el1qqvead5fpxkjyyl3zwukr7twqrnag40ls0y052s547smxdyeus209ppkmtdyemgkz4rjn8ss8fhjrzc3q9evt7atrgtpff2thf",
			Spent:           false,
			Locked:          false,
			LockedBy:        nil,
			Confirmed:       true,
		},
	}
)

func mockDb() (*dbbadger.DbManager, error) {
	passHash, _ := hex.DecodeString("5321bd178a39c442833d1f8327b567b46cdffd86")
	bk1, _ := hex.DecodeString("0da00604f2f3fdf33d3dae4bf5edb755218c72e904175f4981a8ad8271b6207b")
	bk2, _ := hex.DecodeString("4c4a998378accfac4bce02a3404b55f1ec2e1b3b26309958c697b097f1c7e216")
	v := domain.Vault{
		Mnemonic: []string{"gun",
			"transfer",
			"capable",
			"insane",
			"daring",
			"october",
			"dwarf",
			"useless",
			"dizzy",
			"infant",
			"green",
			"person",
			"resemble",
			"exchange",
			"bleak",
			"latin",
			"jacket",
			"hint",
			"oil",
			"lens",
			"demand",
			"feature",
			"caught",
			"tunnel",
		},
		EncryptedMnemonic: "htT+VEkwzI7hiehnxK09rQ6vIC0yc5VuDcVW/QVNwaAjApzoiGQBXIHRafMNVv2ih+8NluGCH3ePBqAQaoSVhBaXjXCT3TgOKf8lv1xw9amQQYgk7JKp5/8AmH4xjsCFPRzfXAaEZjJsU4yXoGZxXABDvSTtf9TPmcL2n3EmRUnZhI6LqJPvNb4LurqblZbBN5W0KhaRzS9kKEUh8rEEO8o2U7/qhzl0gSIPAu4pMso3acTjEg7SBhb6V/ZwyF+d/FTrNVDy6AxMwFtlIYzv8ITeSLP8e3jAxxNNOhVt",
		PassphraseHash:    passHash,
		Accounts: map[int]*domain.Account{
			0: {
				AccountIndex:      0,
				LastExternalIndex: 1,
				LastInternalIndex: 0,
				DerivationPathByScript: map[string]string{
					"0014bad43a13c57b103ad2b6800f38812f61d6bbf956": "0'/0/0",
				},
			},
			1: {
				AccountIndex:      1,
				LastExternalIndex: 0,
				LastInternalIndex: 0,
			},
			5: {
				AccountIndex:      5,
				LastExternalIndex: 1,
				LastInternalIndex: 0,
				DerivationPathByScript: map[string]string{
					"0014aa966270d72e937e73dc80c759b7c04eb4ba9aa0": "5'/0/0",
				},
			},
		},
		AccountAndKeyByAddress: map[string]domain.AccountAndKey{
			"el1qq0kzftpk5pjemzpkrhgq858zqux34v37tl54hys47rmqfcdfdyhs8wk58gfu27cs8tftdqq08zqj7cwkh0u4vjkpawf6fvhft": {
				AccountIndex: 0,
				BlindingKey:  bk1,
			},
			"el1qqfl68rn7lsn93jmfghaln3ynj3gkwuwm56fekyne9zr97ytwu5uaa25kvfcdwt5n0eeaeqx8txmuqn45h2d2qj28epp564py6": {
				AccountIndex: 5,
				BlindingKey:  bk2,
			},
		},
	}

	feeUnsp := domain.Unspent{
		TxID:            "cb304abf4c5ce39a439f7568e149442a5bf3a58a4f12d13205d61ad1ac1480d0",
		VOut:            0,
		Value:           100000000,
		AssetHash:       network.Regtest.AssetID,
		ValueCommitment: "09331809f192905a49af2d32e2580ca721118dd76c3ad36e725a85312ea8efd979",
		AssetCommitment: "0a9aaf707a4275f3f116dc78fd22a69a300ce3f3b2f87e4724d5261c3487fd316d",
		ScriptPubKey:    h2b("0014bad43a13c57b103ad2b6800f38812f61d6bbf956"),
		Nonce:           h2b("02e38611338433db2e16d10dd2d136769e8f261414fe47d087167548c75ef0fdec"),
		RangeProof:      h2b("60330000000000000001a78673011553eb88ff7f3283945096a635563c787ce0796181271e8ce297e1de7dc006ca534af33b687d96d8e7df3123fa0897bd5d31784ed2550040ed020d0fbbd7219e66d1c53dfdaf8442b41c213a75dd261e4222271b31f809b2eee2cbf6cea36e1f09392e6d1ec6b05dcc51ee5994269d95ae56ef593a5dd913dcfe9a99b844b6acb450b5049dcdd71a063488e4d4db767c78a3c8c6ca88c0700021c5931cec4571f05021124089ae9addda0418dc68f9a0c181c13ba466e57477bdb0f49cb20cf5faea7f0cbd64c5031ccf38d3717ef38657bdbb79fc3988892333a59016415731ee17fb7f5ef49f78025a1e5aa366860538f307814f8c65c5979fe90fbec23c2f5c7432c05352f4b3c477898ccd905a51839a541587fe3cb4fe68c1d1b806ac7b844f350111b03990aafba834b02038f8b93ad9ff3c0b0b74e3981a643fe2a930aafbb9c20bb90ae9875a3f33c859122ccc60e86e7af8222a9c730c6a4bbb52f6159c2e48be21a055c8e4dbe5ccde8ba5f1523521843c3c331dfcfea325fcd244620266311fc8df0afcb1f278c3f22c86f1cde74939db9e0b2db94721f2b1b962dbadc8a065dc5022957676ac214fc0a67338a3e41ad2d8c5b38891c184d6ad16252a25bf8b3caa93fdfd9ee591202ff33d50498791df55a51684eddfc5850c74fd36b019423061c8ac5e44fd5e87af4898900166d7b58e45809b192fa1d8897306767cccfb973575247021f7f369a34b2eec91ed8893c4a36ad8fb56224e7a26974d386d8836a29d385c8e01d3e9e2865252cb65d216e1a2cc416d3bfb46cd0687aaff8ee4e984abf68dc40fa53d295c37e1ddd218ce04da6bebaa2b3f7f4d7fa5cf236fd6dea189ddd5641bd84501761efb7c4cfe175dcf2f7f7be7f19c9eb9f49f6ce007eb153b04e23a7b0d1dcc4902e118741a882e2d8c3a1b7b34e3ff6e3fb21a01825df74973e270bf5606291ad8f55bd371520664d584a1f0e46e84deeabbfa59a2d5a1176cb3957cc22e4cb268987f589c32b817d71771969bd80756133e0edef7b92a5783da4d301ec480614e304361b5ac1387889ecb0210154a5106654731762603bd461a5e5213dc3b8d91039d8942eacc93cc788661d31bb45142ea9b55bbafc62f518c3f91841761a68dc42172e86f16414112c1744d9f1f8af7c64dd1ed0333f00a505090900d66c3956e2cf4e9d6613e58af696f0905e29ecbad404865a031d6595244edce2648fe32d666fa3c33c383655ac27711446a63cb895230dbae4634642ce0bf8f6a0e7a91f8589bc378d92514319c6743e63d06c17baa342f2e00c3a3a990061361d15a6f5a3da0bcedbe7310b07f284bb43ebbfb52f5635bca2eb3462b3340f21c51a7b5d981516910e8bd90aa522b1644c19deaeda697d11855c40d684e2b4e040909d1ef1e7399d92dab4ea396bc024d5095defd99c706e5a0e1f6a0aff8acdbdc99e1d15bcef0dcce134eb07fda5de0b29c5dd55b6c1511e49629dc4c80d7cc2b5a92bd412333c34f07d4caa13448199220f2d3bf8bb14290ebf7027a3a89c7d1a9df744e5f96b864694ef9b14641afeb6733717f2c7fddcc71b1e3b0a84881d49be4b63fb41592906373b1d56841d7627a38fec5efba14ae46634fc0966b041ea04a251c8e7ce0ba08d6dd9d27e2bbe5164c2b2aab9e8f5df4ddf3a130caa9eb47c5efa73fb5171c5158de30435a87e10615ede093aa8442bcd6b7631305c7a25f25b271e70a2f0a81e203827e9cbafdbed5c119ce9cbaa3b63fb985d54753cb3ae9cb52d3158661ac9c785f1a2dd92d375202b208c4121c955e3ca231fbcc33a1f3285193dfc6f8dfdbaf3f2daf39f657a12289e0d76837f50ba4f63aeb68035bd9c137945381fb2c28ac270fc84e29f3efdd2ce2dbd32811f9212ec4cc5e42fb2f0f41a837c8eb3875ecfeecce6a09229d37bcf3da8b62a36e6bfc334cfc074ea09c67b592bd767199ca541d305d6ea30451f9c34ac9c1794afeb4a982281b41b56da113b6769b0610bb3923d5a278f88182394d5946943bef5cb09b6d095858089a6211d97f6de2744bca8474b61f5015f127b8910a440e80f78ba2d783465281c5c5b90e91786e42fccc76fb763edddc258c92cecfc816a4ad5d9d60b339cdb60504151f3b16cffee916e7fb5f9de920ed00c241795fae0150511a39024814234ca25e770da4cec6ca39146a2687b6d107398dcb16f7cd8ffca355ed681d5b33367f6949b53d3d597702ba5058a4f49d194f1e61d08b7f499940ddc3e9df9d251a69167f1d72ad5e3505980207a02ea508bae6d5db87e1096b029e85fe9ccfa51f3ccf4d4b2c26f28bc0cd6e61a53f20c46333074b933b26e8541cff7e60ac06e13390e267d653b0418eb3050429071321611778a73bccc6b61a0f832229c633423bcd5e57ab40e1a78052afabe7376e007b80079b4f13f0acd1cb220a550556f4fd65003b4f9507b0378bcdba6dfe6990ecb8280a475d8f0d08376e676c1fad5f46a96a777da4746db029705d00f78dd95da5851078c1b08c2cd057e54b760411bc0c17bee8a2923f52a966e641b80fd81edb8188bd2bf3837e55abe4d75ef0f9e25a679f29c8e8e8daf4baf6adc6c4f43c059fa7c1908bd2fac3abf31ea0ffb6265052394e8affe3d0880f4f2bd6b02e0a2741104e4445a2454099ee1bcdcb1602716d4a0aceab3242e44550283c048c036585a1b189005279c09d6c210400937677e30fa5c89e0df7733743c5b8644ac7e02a6c1f672124a556b30262bb3f1a107a4d664fefee09f8e8eb3743d70be29cc37149a146dd19cc5f9bee4edcc0d33977ff2b3b9012b43d105666134558e96bb02a5670793ec20e88c3b232bce5a79d7722967cae8d81459493b2e376282d6c0f2b06775852334601aaff8736f79735b31287e57bb3969d8454dc9008244aa5fd915c63d44fc852ebfd5d6d3c97f423a46bf623738e8ecdf126be3430aa065a6d09eba40bf3b67d67950090f99b7fd8dab566e404d4c37084900eaacc438bcf3edb5d70ad7eb5c2f07eeb83be520f4919f33e6d62acdb7183d099da05c60526f9fb54fb8802abb07386a58e3e666e361568b4325da0b0e9772367f3dc1e3ff15189d4662f9ef42a468b9818a2219b19f9e0bcc7b0c56d54539f83dd5b13a3b4258c5920cd11ea82720c6c960b85f9d8d7d2da88f3a3a12a993c452e1f46fe15777f721ef4d5c604160457612051737c9ff0c0eb27e96364c56c398d98e7fb4bd06d97439fb5a4fc1a689f60f39de42c4567a6d9cb1468be8d2d54bea45bf390c36128ffebb97e3006ddd72d6934383e23957145cc1257e388ce4f3b2bf54a4c4e5d128aabd99daad2f9d017a59dde0a51fdd3871b28bf01416aca9b4d4369ab38e4ca50a8a1ee946e296898f5459d9268625670fc1981f9a5dde45b7735f20bd9a9042166a7c8444fab77c9fee052e4eb617c4b041363f5098e190a09e8f28cf3e5f782a7a338237c2b03793d638b815a056ada346050789a0e717f0d033a1cd78f0615eec600e02fc550d71e0d4030b6eff7e0cafc8c0659c24539a8c321cf6494db439a9c9f494c86f9426ac8b3272d36fdb33a685e04a794ff7a5fd1a30cbc102fddd9ec2f415726980d1875b10ca9fbaa2b35e9bfb59fa5b42bfaee2e4a93d7ee2d7cf572ba8cc2fefa3331c5177daff7b1b860c67ad051cbe9aaf7bad3e7aada77e88fb411ab68cf60ada2eb7a878c9a41ec0a23310156d16b12dd84d69ee0baf6adf9416f43b1010951e9ec88edba6039ec33314d228c76248fb99621bd9cd9107144edcd1725de4e047eae27f94af302f634b9a8ad76ee849d64e597f10d2e357e6c37897ab4c6f739f59dd3ce30898bfc93b95eb74c5e405b22982e66350d02dd65def2bf169f87b6af36575c77c924828e0d2e73cc3712a329813b5fdebff0cd0bf73bf46abce2e1be1a10c4a3a54ce0e4105b627c41b73e5c06abfcaaecbb0fbc8a68dc764d205dd854e7df25fce3b36cfae9646bb68b51d3186e5895e6c7078568a7c8e037f7daa7f8a6458944f2ea1602d53aeb63cc5f8788cd10e54349cb74a4ccac4a8abb1627615c521edb655b4ed52df8adc2004fab885a7854563bab3448adda90e6d85d154f0b19f5ab25da5ed143f5e89c11b8445277277d8698ac736b28af7f6321df1ddc6e0c3c4461b255dba87c3f2ae9e9b29f93fbc046cc9cf64bcf4bb29a10e26d3d844cbbe35edd0f25302b3e0b02609a0cd432d78325025338d08eaa128829461a403a37c0a52d100ea839fb673f69e8a218e83ae985f87365cb9dcd96564b993465b56024d0231e9da52a1bd122ab757340189498db836876a281ea2c98107fbf4bcd00cbe8d641f0b23329e4c42b968dff0dfe091a0066e15cd86e3ba4bde87dae9bf70e803a06e5585eee69f274a1d897a64d53ca4c40c5d28ba59296272a4d937b7d54c9e4caf66a574b79cc7a05d4831c404f5967d2af3affebf431dd4604751dc5f42ec864ede26856bc0b64c6ca38f031eb6cdb23cf17a4881a1dbf0311070e3895db7118c1591179a93c23ad48c1346a57d2e331bf7e242e300bed38acf2720f114b9d29b3fac5d44cadb7434ef081f8924e0dc95991dcb423cca41593c66f7fac18e6c030002232a1f73e2fd797fd4c3335c7eafb9b51bb8fac60043e72c636d2e7b5ab525e34638d84b6c68c90db0dac883271b7268d374f8b9d6ef9cd5930f5fe88ee32b3b6e49cebb07503ef98cd97f030a852cf1ea761cc8704c318ce79e091695116be53499cd50aec4cf5b53a9fc17de2aaab2dd486f146a4b3a93989212fdebac29a72dbe383215af3776a793bbd831650ed79a7011a7c6be8a0b0bdefa8cc83250dee1c0b1df55f063695826e5cc557555890a71eb946aac91c5301f8526927c97c96614dba1adf4d74f6bef24500bfa5cfd44eaf9a951c6e772651dce84844a8274526934c1fec94a1a9d9b85af581be2cbd33758d0d8194efa0e285eaed132aaab651a7b2f16d98fc580e455903073eba13f9c630c9c33be7b219d010d8554cf0c43318107a003d589875cccde904e86a9c6382cc12dd4df643786bc748f166937436b2025a9397ed12c62f491a9d4ac2bc03a2d9f810f5ac21e4c5e68ca925d46a591354d6a8604a2e7356889b53442fde1623aae1b2ce5e5e6fd964bb95372af4aa9a6e0ba014e8a3de7bb720adb8939e40e3a5b44a4815fa8a7a8044e82acc30846fe8019a383f110df0327f0a8df88831ffbce50dda8e3272d6a071c8da5769ab3bca371bc0c8e1d79729137b291f91d44ca57c5c970ca30ae1de5f6110c3ffea47b0cec5ce0e9fd1ec822dc24f10e183243edbde9a478228d9cef4097b60f19e32277a70c31a480daefb88e0814af496876fcb2ccf9f08c1be961232b0b2c4dbc8bcfb207dc9b79e65ba185276a4564edc93b9001a7e1477282d5379f862c2a96c625814e072589572aa316354eceb4c53f89ca5311fd88e2d446c0efefdd61e89ab3e689edd8ef8b0f21c11ae8329ffc8feb8afcd998c2d81a893ae37946edc3dcc63b1419e81e46de5145290cbf9eb7b5df6d9adade8d853cf6a64a445ca0dd06ba9d19d97119db7e9728e5c23bffc020dd3aa0eb1f72ac2e5c2acac340258124e48f9127aaa7eda7c95d3c0a68dcd9cec396b9a82c0816a90f4e0e4c6f66b1ba28cf6c1863ecf24d3491728cee1d096864065279e67c9d69f375e93166794dc79b99fb388b20ca2b3258b9916821e67389fdea8142b8821e620c26dcadf9fb1bcf8b0dbd68b1d5a48e9d3f66d55b6f1ad3a34e6270f94ef9e445a7d04786c49757e534c6b239d7ac7e589e095d02b0c53d0743266e6bcb15fbf8a564fbd6ae99c83"),
		SurjectionProof: h2b("010001afcfba84b511e8c975eaa61b3459f6772fb598845e93d8a9073bc43bf99efacc2327884ca5368b41a09982a5d9dbeb4da43bbb2cb5781e6c96a11410a8d6ec7f"),
		Address:         "el1qq0kzftpk5pjemzpkrhgq858zqux34v37tl54hys47rmqfcdfdyhs8wk58gfu27cs8tftdqq08zqj7cwkh0u4vjkpawf6fvhft",
		Spent:           false,
		Locked:          false,
		LockedBy:        nil,
		Confirmed:       true,
	}
	marketUnsp1 := domain.Unspent{
		TxID:            "fb29f37ec1a126c9b83217d46e0329369e5cbff74ed61f05f6fc993e205cac77",
		VOut:            0,
		Value:           100000000,
		AssetHash:       network.Regtest.AssetID,
		ValueCommitment: "086ce646eaaf9fc6d905975e6c06f0938ddb12dcb0fdf525a2102ccb3979a4318c",
		AssetCommitment: "0b4dae395d5b4e48a099d4cb89be7b076af58e60204ce1304600765619ab3d729e",
		ScriptPubKey:    h2b("0014aa966270d72e937e73dc80c759b7c04eb4ba9aa0"),
		Nonce:           h2b("02aaf44b64e018b609098b7322a8034b2c6ea9c1a5800a4db8d17f55dd369deb84"),
		RangeProof:      h2b("60330000000000000001fe4a5301163e54b82563ba39a9d5915da54a5aaf587559e85b1ffd6c1dc897094009093c24c5be045291d15d6889c4a0e28d1076d2ff20e4354fd5a71d0f9560a03a2cfd92df609c0a9084821fa704155639cb352a80d6d1a364c41759246f9bb4c93f6fd9d3deacac067bfc52381a9dcc753109989d481740240d8c7ca648a9d09a8932db14def95515908f944ba812e87ed43ee27d060c15a846347a4a338b07a562b0bc4aba1aed7c84b5ed71258fe7a64c479fa70350239cf74ed2bde85b943885dd5d2ffc4a9c81dcf4137a1371e1677cbd4c59103bef64ddbaeb448158407cdea51938963f64570995e68054b77cf8825070b44a14ce9139a42e7f41dbe47b80447a2295d41a3858336d3cd932f3976bce48d7623cefb326a5e4276472ac307a193103e7148eea2f0f63c6346c56679c98e28ecefe4088afe6213e53db76bffb6691f5eb885bd051c2d186fdcd055c92fddaebe73933cfcb057aad15eba0c4851c868c0f06c4019ad941fc48db473fece7d5512aeb35112bf431d628cdcda713bf2a7d3a4c5cf57984fbfa31b7cac4aba8627fc91815e77221a05521345420ec9b498a4b79b170444ca31ad801c77b6f9aa098aa80e7b06b6c5b9a645cfd22b32b0bb6e2e22d034dc6456a135d89e1cb1c717450a77cf436ce1bea52875dc73b707fd199a3433f738c31c524cd8a75ed9f0924b1a12704c7a04c857f3bd44014ef709ecc57dac77d223b2fb8ae3c029617f3c1030a59af1b690af681a04d74357cb3888b1d39812a81c8ebdf3a2ff084784fc04464b6e2428045cad08cc2dd341817befd125d1732ede4c25767a4a206f9b0e404c1c99aff0d92b4e2e566cabfa014632164e7df15198072fa5ae0d6451d0be2f0298ba39ec2aebda1d97911fcb70e7093c9699eeb1accd8d00a1c2d74bba7f00d11b2b0429db3db2c0fd9e4d6b18eddabae951a30af322a482104daf994aaa59e78a69a3d628131af7761d3e4eb043c825f9c92736e41a004a189de9de9f78b7eed86354a5e4f27a9ecdcfc89dc6dbd53137213a9e45fb1da53f8a97ceef2500fee204d7107db9eb9470df169d9660e4ac354a09bbe18371bedc74aa3dd9c13b968bdbd4e5433c20adf190ccb8d5c8d424fc0703d8b82ed30cf893340114fbe5206557bad0c5299f938d85b4e690cbf9bb275d0c5da1cab8340cda1f758af9b5e9468fa3e861166b2de48828abe25bc250060ed71d4056fd6def7d345e74749c5a03fb347a7ab3bcfbfead60644101ba293160a78184e4dc7893a939da7c9583cda0a59b05a236df075b0e5a4964c7b12c355e6dea9ba661e7c4663562fb22dc527bb8cc5295012eb5f2e42350d222ac063e982e5f40c3a073d88b48afdc3b835732c06a7d32d17c85fe32e5d46d115f332c69db7f83e762ed616ada9fd7b816b4f30d317aadabbac0fc0820c21eb7b01c287e9fc897d2928c157c63e0edbe162a83e9c00815a16ed68a199ca5c6ede18720833b95187c79bf90c0e95ba3444e64b7dcc7739984be91682ac2014a05b20a678ba9d9e1f18fe3745091a18ac2b7cbcb0a46f9f02171fed88b4b8a89ba00779f13f0fda1c578f7ff9ed8a5f823fb5bd06ea57a598c8872caca8a0a4b23e18ce7d086f2e4df149e5c463d7dc33c48dee629e0927e3ea2d44f4e3a9a8dde6675ee7de6ab471427053466bf6e4c4c3c773cc44694c94d215db06f6a5d814fb02dd64b2c42bdb1ae980484c227612a98f8499c6455cbd3277c0a55291ed490b351b895adee01176ee0e9fadafa005329ef0687394f0a22dffce81e94a32a218bec1c15040d235f662ade3c147b524a9003d7772aead4748b6abee5e86ca04d2baf1bf9a6baaa807e0ad735e1b1872cde6dabf41cbd5faf92461e5841eea01216115042df20679a9a0c890c83fa296e48e5015dd08b296528d2c03e0ca45ab676c2802bfd9e59fef1f282c1e56176de6b370952c4a911d2caa1dc7f27bcaf9fa5e317ddec151d8126988908434a8b9ed1a0cc9768ddf3daac30567f8a0942b36cb75e74ab274d17e1fd8481bd5cac85e4c7d8b9fabfd534de24ce27a6baedb3d9cf33e15a9b9ca0420653ea6de3c3d1851c43b0204e206336dc0c84b8d34ab49ae75503940b67cdbfa4762ee051d7c560c2b662240e37b02eb31283eb1404895bf4741df5935564ec4159daed1ab13c1ce338a9c78584b367e021dd268805c9f5e98c4e45fd8165da4224d77caccf106035debbfebdc400995ad7a83b1f57c0e69c22891eace268a1e86c4ba7c61d69e9cbe4835b4664b28818c9db7a71c5b5659c6d77111d141e5041399574358fbbfb00ffb90a85b3ead4b3143c41c4f620763e2cdd4e6a438d8f8177b440714cc56347f6e06d62e87a04325ed743b3385bbceb55c46ce2759b4df3479ae57ec4a6d7c851ae8fb4728a791e66b3eca43dc7ec46857c2fe562d16545d8b4fbc1ea09caaae5400256f225dcb93347ed32e20b0ae7a3fc7aefbd5d61ac4b115fec4982d24c30a0667a87dfe0eb5fe55938b093db7dafd7b41eccee582ec4f105b5aacdf6ff7a6ab87c2e1d78a411c3188ca57e5fc853daee8dd1e048e7cd49f429413532b4d30c74cd4c3c3531cdf4b7dc9ac01e9e17ec1ca1bda93836a653393d97a80f3a36615d9d8421ad641641f7d4d0b27d3c308b2cdea018e6f89f98f9db4310a2df0a1b456d01002c1b4efe52f8ce8198caded0f16f3d458f9a4e3ce8f2cf5c5a2e9e6e2a713976538689fba1f4c16cb2e41ff5d8c17042a443df0829cf6fea944274e6e2a3d02423bf5908ea86d2b04f5b020f02968ba7d1bbb8cef223100a64c3512c62d184a16636fc773df75a35311c376446d48f6edcb077676e0e5f57c2e395adbd6d269880fb4282f7b6bbdab915626435784656b013d7d3c466cbf976148c1ef52af109f856e18a79fcd96a023015dec025a2347f1ca80e3904cd83d430e769b97551fd20e30ec14e6e56f733375dd8390a95a1c7288b5e683ae5fec8454390a08ac543a94a93aa2ed24bdaf3f0f573183f40bafd650326659a640e77e1f74f8b2e1a7312ffad445cbc8072499fb0c0a33a373affb1ba19933eebdf651c703b31c9bc4bb36a70304963eda877ced3f193d0fc524347958496fd954234818c54e033606b4bf7d96f65d5702defc5d07c14cab1acab858503b64ae42684c2fcd2e4a0c0857ffed6fd68daf90edebbfea774d121cfb3f689a6e4a43b93654289236042744ec141446aeff945610b1e1a8b4ebd45a8050b6cbd5ec44dcb2a5f7cd27ed470821616a509409c1639573ff2f0be790e2d5748332c94ddee014817084c25925be6b677c8f921784edc962c0551b78b98965917a7235c7f6e2551df99986bc30ce93c32405b721154c12ca2e06fa5207a47bda23fb77c902636318e227eeaf6dc6be8ade5353722a6804285834c062e56a73f29ed3afbcd08d75fd5c39421b49e6212c2600538cae5f0505bc932b2cd90a3e7eddca16a1a8e29c6543c4a376b8a26586373018a81f419aea306fff8f5801a2e6f524afde2783087ea222ab0726425c166acc6810341ee292d72398f561b6302c3d977a92286ff18fb9d592c1dab761b719c1991d3c218e5b9a4984f0906cfc3e09abb3fade5609bb4f53ee598eab64f791a1eef8de09b5bf417a56af230d5029152bce84bb6086dc5cf04a6a30d29e4746db85165828000fbea75ad9ca419ffe3e180b7ebfe272a08662ef3d1233594d05e7c0357c2b57b50163a6318fe32645384859216daba4e92ea43c641fb91ece76a146e3d71cb2a4ea28b274a7ba2ec45c80911436536fe06fef7a3c6aee41d5e0542a190d837179ace31f4e1385fccd8c730cc4816b2bf4179ff0912754bf55a44d069c1dacfda8cf8214972bc5471c0afe544fc86f3b07a8685de0128161d49730ca25a03e1571f46aa1eb666bb4380eb7608cf51ddb03c0140d0e8b59b6b6841160e607f4822f7d8d483826bc1cd67d395bf1755a92f66f974b408077e8734d24db5c3a39ac5e3c3fcb96eaa3f122a5a8beb4ec3d91ddeba98eafb54adb12c43ce50600b1c72c9581af065458d326cc16a0b7ac8a401e01be2d811f14531816a542332e5eef7b1b67294cfdcade7c0952f4e43eae4de7619efca76069286deb852c43dd4a5c428e20903675556e8eb4c09fe3a17b1e9b316c2b336cf3e48726b0073c1f522c1dc3977374d4cd5ae534a58e12a4b7c065c3af0f44c202474352ad2a8d1ec2083ac3f247a5f8f052aec42fe0797ef4ce15ad8428b4bb92f169b908396803444d7a4c5db6e502ccc74baf99acf92953c51f7c19c5d5e9ec4e8e48603f4fb6e4ea15f881e4ada9c6e261b2931476d1cf871dead79b6836ca7b6f9e923d333dfad21cca3c9e96dc0176be7f234457300a150c5f3563286fe05b511ce421983258d8b3328d8dc1c4a6cb3d9316fa98f1a2df3af297052eb9995f08f3a530a22a7775c68a8a370de1c77f4a78dadf5918abb453fcaac1b1baaebc53037fe60101b078d5c69fb4c754f35f2aaed9166f747c40f103c329624f3c29ac7b89d8e99a2811c04bf183f207213a4e32c9cfec59c64b1a56d3e836b2c4691706bb3dda87c03e2d6bf33faa49f9a9e5d1f8724b671a69e6a6c562b9eb7f5f23285581125fc6afe7883ca6b542e47808f474585a5077edf54de4c9671399db22699195ecd062d7cea9b554b9d57dbdab5906e432d4e81b494a83e7732a2f5daf64a6d11c2d0235990e82b9a5c51a24b3c6b820d78c3e4c0d20cbae3825f1cb71121c55fb3b24275106ced77709e6cba326bc7419823b5a0de02fa14204b73cf31fdb86500102c2ef24ff13c0fe2dc5ec26b27be75a759d4313f549d8921df86737aa551a140ac7be31aec825af68882fded4b7007c422efdba557afb769dcf146d78407e89e9386c982d6e2a4d110c1bfd1724e6e4827f04873a7bc7172ff65a66adea0d0dd9503f13c7efeaf0cd101d3c517b818fc366bda7fc1e456b284b8eef8446e8e0dd5bf860bdeed2dafe583f44bbba14879595dc005ca02c49c142fbeaa157018be2e927bb92b71650d0fcde94f30c0dce8fc864319c633fabbd2fcf8ed938678bf2c5e415e22c35103db9a5f07591e6e2622cb1272206dd1f34f00dd4713ce5b5c259e3a1b2c306ad57760b94a890e90bf94e8a33b72cc2b46e39ab383c98792cf83669d3241bb19c3a2bf193b9907cec14a5c45705c587de6d03eb219431150fc344c8adf70c8b5960d51fba62d235fd1b163aa60bf3f7097d71abad629cbb7496be7349a47917ced92da0f71510353f9ef416a3d5b8f3dbbde1563dd9a6dfbecb58266e669596cd94d2acdb65e1af7a2ee8a75d165b0c3f0658592df7cfd5c78078721a3ad0c2294f3efb1ee90792cdcb5a90d7fda35db85e4f424be545f61654b4a2f79bd868fb559bd7305584b5c1ce2016714f70bb283739b197a1f12982926c3181c5508a925cb969caaf3c17c7aee25bed7123dc8b7ee9cd981519ec16bc54dece587f4c4a1c4dcdfcc96ddd8fb32d4dda51f704390b61aeb9d3dab0613cabfef9b1046c32465ca7c1558cff2c4a80592125ed4f17470fc2644d0be8db13ecf02a654b86ebd00efc9cb0b8cd82a0ad76c8916b7e5ddad9e3591a43bf1cdc447351ee2f906c7bd74c45cdc7a60bbdcb205381aafebf5dd6b22a7a14acfa0ae72f8ff43beb365a1144b6a720bb716f6a7099fa51df59b0fb77864578e9a151448f35a3560e92a071f630940a55a6cc725ca6ba5d591842b07a282696221802913b61da4aff73326d4a66b42bd0f29c486646fa04873325cfb11cc04c1c646b34987cdcfe686c46e2bd4662d3318ac613ffe8b9d789c7aa43c63c4cc68d0a"),
		SurjectionProof: h2b("010001a6ab26512a782a618016356d157c4172afef3ab980377051da05a7abb59d1f6c8ec810e726d62e8b78cd91b144d7b77765c4b92f7eaac142d277774db1b559bf"),
		Address:         "el1qqfl68rn7lsn93jmfghaln3ynj3gkwuwm56fekyne9zr97ytwu5uaa25kvfcdwt5n0eeaeqx8txmuqn45h2d2qj28epp564py6",
		Spent:           false,
		Locked:          false,
		LockedBy:        nil,
		Confirmed:       true,
	}
	marketUnsp2 := domain.Unspent{
		TxID:            "5de92a70bae806970c44428f79cfce3a2ff03dfe26ee74410728d73bc65c5601",
		VOut:            0,
		Value:           599000000000,
		AssetHash:       "d73f5cd0954c1bf325f85d7a7ff43a6eb3ea3b516fd57064b85306d43bc1c9ff",
		ValueCommitment: "090518c39d53f4b1de7b21d0bc070f9605bc5337bf70873938156171e7d1a371bb",
		AssetCommitment: "0a137666c00f53df8d8a626c21660b434c6d6fe185bcaaa50db6f0ec40b09435ca",
		ScriptPubKey:    h2b("0014aa966270d72e937e73dc80c759b7c04eb4ba9aa0"),
		Nonce:           h2b("038cdc3fa8d8f8cc4de8e543dd21bf05ea168f219ee6b6d6b90af17934fb5c178f"),
		RangeProof:      h2b("60330000000000000001b92b87009b5a61de04fe8a23c43cac425364aac9ca548311eafc931c5c8edbff4f7994ec1d5821fa05ede69e4092b6d9f1dd29b7beeac1c8543f574b6668162d5043d23fda8ad4953caf8221603c5a955c6d25dea4bcc4f2ce98eeb8d9b222c1b0c6076c181bbef2f1c1b3f57ddf6f6ec2e4b22d49141e5241678cc2ea35d7924e0077e00ba9e30a1dddcc0587e4688efcfc81b3f4768eb3098411245e61af28c1c7bf76c5d593ac15bec17c5aab96f3f8cbf786e98156f2ae3c3c294be4b6a1da9202356e69c3d4a57f8a8a71bb80fb76b61e7a567af8aa606b04e0b3e7993fa50b1c07326adfd1fe59d4fe63bff9b2fbf338c5939d76c4c7bdcfebc34ed9016022a61eb5846c2333fd8ddf708aa9526f220becfe90fa1c76ede4bf96d46782dc79a06a7edda1041b69524e3719516a313bda81f03d65e223e60a5021401a96714e963a0219eaf791e5e750cc475720eaaa558fa4c9533c041eaed5e21147620edfb9f8f02f7a9fc32efcc6b2b8576f69d7b28485cad714470cc8f2b1be9d4b2c863348d0f01c2a1a4192327716bccf0c3608ce1814084fddb0ebf720257dde51a9c290470369cbeac4b87b682443d9388e2413733629e1b69e5bed6fd0c3065e7e6e8302a06124da1c23d427dc004ad6219a1b01b7868240beb208fc9cfaa065c63bbaa2dd96654c01e6900d855171628e9e658cd422c734f29d1dd859116b5fa694a1093387e2fd2ebae9de61da49cec09c4e5d800943a50a09020a67953b553795705fc1dbc9714a297f0eb0d06aad2d9d6e9ecec032a86b02c550949ddcd15529e7e822e8a2d3edf2dc65a78f94800ad2ba41cdc995fcdc6c20f393370392093ef221a815f4d56f21c4f14ff039adfcde79b44672401fbcd96d3dd1b5ada24e85de86bf47ee332864380efa5990c5f609cc23bd4f1e46ebf5330bc38c59b582db951690de72776b8cce45ddbf3ea3ebb080398603ea1f9e3f6fc75fde64f134cd927884befed79d95dd33a38568e8e638fc7fc4ff7eb3ed213bd639d3363225ad1c29766817033adf16990d515341b905cb7fad22d529bbf605d6d94a2271fda65a2e898efcc7e0686e3c545ed9f6c499afba6c0e7375182858da8476d9198be8a87b0e9aeba43da380188cf942c7adc2574e89983ceb9b6358e611c7dfc0a98e68a0d93f4c9679cf4cefca4e6698bc889aa976d03d3d6340b37ff54b5e64ea8cf6f59abbd0ba75729f03bd4cd7518e47814fb9d8cdca357bda84346a98968c78cb7f627c6d8bf5a75c621b187f95fbcea5edf31a39a1892f8eb18d40917fa566bb06de61a596484578f982434f9289eed81cb40cfb0d2032ab152f91a29a0e86734efeecece2e990f011dabe5de58a0b8044d7d449a167e1f94b5b3453f9f5d0d786df37ecefaa50db3bffc812d415160f202bbd3a45654d366544c7a8034eb1df219c15d1a4102fa52551e539c80d4c50c70386f0b3358e4e4780588934031c99d4c042432b136a1223fb72a150205195eb829c2cd90382098a618597ec6a354c19f0c9b23a13cc3efb76eb2e3973dfe7c959d52c19db41a39bc0009a0148de4bc55b9631b3389e03849b16b99b663d906f9293c494dded02dce332bb422db041c3f9b2bd6d9009061b2a3b0f69f900c3bec62125680ef536441f0a2ce95a52cdbd4baf98506ab44c2c109e6e726c27f51bbc875fa408c40d1239d86b299af9adb63460dd97f5bedfe9ad9725d6f5bd34097468e936f306fe18b66a91b2e35251eac5bbef559968901f2d6eb172c77a98292f74adc90256d255409f2fa7c28b0ee5bab139a15b6e1b4f6fbd05eb0fb3dc574a14dd4ce2683ff6a313809a2eff8d526ecddab0e915262be020f5c4909f95bdbdd13556a6ed3342e3efcdf47734bf3198d18a3f3d884641142d107204cf1797c6404f2287b27de32880729c58af9309df8835b47a6641fbcf3297ac4c33bbfd230e77620bd04384c525d45d149f0c50ecbfb6e13287d23b00ad00afc5d7cfb2d15e415e479e9d7c7499c92a0dc90e56799e2bdda404003a9cda6551f38262a665337654020325e1e35bb6fe07c3cf86751c440ef71c7a7477241902298b9af494337ea62cc0e49767fba3312db18290ffd05fdca01eec7720838edec0d6ee633dc23e6ffd0c134e3a012634f567c8032b926475dd3a10ba66c5356ed2cc2a6eb242e051008148a814bebcd0dd6e5d241fd54566c4bef895264f892945dac3ecc7aa83c4b82e2c1aec5162ee523949db622b0007e1ba5717422593d1bc3b2d94962ad9afa98c39e7c2c3be5d920fa3987101df45a89cf008ba32eb35d0cb2cb0aea40c2de1ad2ea75d61983d20fcfaa137c51057d3b3e84e508ff7a6abc2422d29faee7fed8602a44b3a10d58cf931f47a51ef8cf7050ce7bed1eaa93f507ed4635d07d35d189ec32921138423297d71eb700923406c6c1f6de0bfdf031d838a0b52bc644f85fc5a982d2c722235c44390a84e3e286de2e92ea1bd015c4fe8192e0c86d5a1fb2972cc7af2770dec5a5bb424a520dc3c7d8f6275be4ceec99df917b6efd7c746ed7c4fcb372804a03f68dcc68f9b86254ee2d2512caee6ecb9c2403941d7a89fba89a7d3f3429f7970e320181f6d96f6ca53078b58cf595593502dacb74862765046bd386e3c02a630d2f0ae7ebab630bd8b96ce5dad086fc9adeb4482336d2db8067e369d5943d6135493c860d34151c41b0e95ef712811f778ef7eea8bb94c954f245663aa933c163b15e3495083024adab86227eaafc99bcf63b6757a5594a5f3406ea486fe509dbcc1df721a5c714f3557ad1a19f3e700999c9fdad5a4f2bd20001f0c44e244848e5369fe5550a05fd284c4216cf817da265515664518652805864dd9200226abe3e27335d8597031c00afe24cddada699d1d92a26a7a4be4943962ff75ad75704581bd4152e5d7932e3fb5abf9b72d8b2e78db92e5b857fdf06cd983004394b73a84a392b2b2ba927cff779947c33afed343726a7f406e77d65cff4b04ca6d82fb6f14d9ffbae87a5cfa9554566578e916bc26e04366ffe374b5b8723955f197866519956dc0c326cf12fb557bb3853c6bddadff0382e231d116729b6791e33b8d479c679d49511a2417ceb050af3c2cc1c04a1eff1de1f60314d3ad0a6faa08700b1074e5f9e16d33ae96edab232ed40961ac9c63e917007b397f5fb25f281558cfaf5642b57fdba493e8176f00723e548ddd0d7a61d6e72f71984f777ed46e9c3d723c0ff3f164913198d106341d86d164c07345d8e1dbbc08073daeb9acb068166eccea93b4030f18a594c4dae99bc0635bb448285084f21774e45f1bbecb6ec54b7b053c16f38d8e7071988594c56a59432b49f8635bbc4b6c382071ca6c32a4b6cbe9249f87b294d0698fe35dbf1e591d9445c6cf4a42e80063283473b1a016c46e5a32b53e5294578ab61140f2b721de1ee9cb710574312e491ca67cc15709b582d3b74d7930f93321cfb5feaea6f28c29c7e17dd7291249ebac2058591658524e7d68b7c100c9be818839e115088de7ac234391819cb323f2c27fe76e5d633ad4faa617acf179634469f0a6ba587726290661edc7a40e72c48e893118502b9eca81022c38fdb14e07e5ccc85bb37b75b96ca9d1b47eb55e7be40f38acb81547ac0495fdd3dfc83b30475fabc06eadbdb07c099b78f6ecb8607381cc92cd723b3148fca8599303c9dc229216f0c974664e2b4a8b780d3f1979b3ae6cb706f0ff3a75353be416dc431d9b3d4e872a08590bc5d24ed273106ecae6dfff1bb4bafb0441ddd409ec3d87e4c547d2ff0c390e99b970937810569c7a3aadc2de4bfc2b037eb5f76b890799dadf38c7ccb57d87aa38a1fc2725b05c0a37a9aba87423b0f482b13c18acb7fa34b12a91370c02ec25237bfeadaf6ddcf4c43597400c52e5420efcf763d70967ca254c8266cff3f571f251925286c09c8864557d5fcab6889770550ed02d9cce366ba3f3771fa4d60ab3236e1f9b1e89b6fe1ac64e0d597960d5e1927204fb672cf8e668662b505aab9100ae1cac39c5e1485f0df164513d74493d9c5b8cd9f10d2ce1e73862bad1a2d31b471939123c1f494020d16fd80ea6722da3b0aaa2940d98e67cf19652cc4b6531ae8ba8e8593e733a94757fc0148165502892c1609a4feb6d8f1c6f4461b336cf9d11f73be93055e862bdc921042d2b5ebc022cc46904f5918d203a872d27e1c7623711019c582a21006947ef369737f5f4c960d8f2ce981c9ede6007eea4033c6153a95954d31bddaf2e0c5b07a21786507cd2f5a46f26be1a147c83afaea472a7bd8a3db42547053726c2c1196a1fd97162246ae61c07270e2e91c03ea45719eb9ed9e263022bdb7c76238342313e5e66ac06536e085c247f89e798d86b7662785d6e9df05e8ad30b55a98606c124c82aa27a6e24196cdb66a8557437c52ca99615db6b755cbeb8f429bb663ac29677f057e615890ee1c270a63e21c9cadc1e65ec3616c6b5d933c8d8b450c3265bb1c2e20b9f09340449bddbdd839fd9b09f15b02ac4d0b24ab4ee109171fa99e494eb603853c14d7ab156d00a4461730eda0f34e9ea8b5ac3b8edb7620e9144198aba74e6488096d18ac3fc7c10a8342463e03336a186fb6ea4418b1dd47abb688cf5e13fe1070c35d4507cdc5f42c2e8ff316eb9d98bfffbcf40b637e2496b41cad65dbce00de32e55898fec9f36d302d8c6c24de74a55876b52a6cd8ae56aa74235eb688c8292054031a304892d9185abfe1456c08fa76082eafcc108a04044e1614cac376081488873d6258311481ec0a070e8db5e1c44e26b13015ede6fd498f70e0db6d1d39766e6decbc98e8d6c9a6d89c62767a76ab12abf5608035748433e027b0fc7491cf0e84c1a74cb2e61656bbe0209e480f9d1ce9e52361609efcb488cd2c267374e7e697521b88c56bfc5b1b0a1e8a028bf9f2a9c38d7ec2ebf1ea009a3f91ded7f44f09ae7653eaa9c6a8705349874598b988dc1e31b29e1c9848c405a93e2268ccac967380fe6e371e60fc854c164f7fd12aff6956427406a7164d14f8bb673e461cfac4dc5eefb9fcee08b876121c5ba35c3d724058e7c52f4f21d846a3d2b07a6f86dcc94aff2045da034db4470fdab2750c23e35cab24b7a4ed4776c18d4a234b24de8691e76588b474d2b027ea868feb31673d87086e8df6ef7bf7e88e7549b5517e1c0ce20d1893c1635370fd75e426263d389c073d330b6a1e000aeab8cbd9bfae1e53630361ee5ad63a183a8572e341ca2ebf0e8a1a9b21919a7b5624f4d516b6427df646be91d2ed66f78b5a25f3c3b3966a84be40d426b6e3f402fd19e9da93c529dd33a58383036ca04e1d782b5eaac63e324c9ca7d58a18b52ed98f505fdb06470f18789093e0988e1a5c9b99c1b553fbadfd8b48547872a8512266ecc0270bde8ceca34b91ef244c434c79003ee17520ad4144de1de46b07a171e2836586b9f7342c31c9b6d01f6fc983084cec955e3a3c2eff6c3ef4cdf8f7e74692fa105c08d8fb610f027644264f503c0a5f75ade091794ca447063f81b5da6d7f450686a7b975c56b0f5239c9577ee0428d3068b8c7e6a94e73431b482e444d2b4ee4c8c5b206c01f4543e1bd792e737eeac672bb98c0c34685f621f41feac7f0c4a43582a2e907d3b443b688f89e2d077ea2d163585a4f2d9cc300316b669d8e3f3982982d5836d8876951865237137a50aa9b1e721ba15c939d37602f4da72995b7bbfd4a362ac93d12b02aeca20fff4ec964ac2716972272e5fcec2f4462c8e53593c98f1af9b66f870c4cbb22bdbd3e8c0f00883540fa569cd156e3fb1882c7355bee543a9f4e065b47019a59f808847"),
		SurjectionProof: h2b("03000760350e908975fd14f5bbd5ccfd0f6965fac2d6694b68d0068c4a3cd4bd859ea370ccf9dfd73e8d260b1bad7320e042d97d610b967137db1bc6abc9ff6350a95acaced61c5958b2b2ce8eb79e6626bbfed7d93ffdd9a500e32bced0c474d9006c94e082cc5cd39c6d09d773ed2e99aa6d6dd03fc853ebaa43232c5dae97feee6b"),
		Address:         "el1qqfl68rn7lsn93jmfghaln3ynj3gkwuwm56fekyne9zr97ytwu5uaa25kvfcdwt5n0eeaeqx8txmuqn45h2d2qj28epp564py6",
		Spent:           false,
		Locked:          false,
		LockedBy:        nil,
		Confirmed:       true,
	}
	unsp := []domain.Unspent{
		feeUnsp,
		marketUnsp1,
		marketUnsp2,
	}

	market := domain.Market{
		AccountIndex: 5,
		BaseAsset:    "5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225",
		QuoteAsset:   "d73f5cd0954c1bf325f85d7a7ff43a6eb3ea3b516fd57064b85306d43bc1c9ff",
		Fee:          25,
		FeeAsset:     "5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225",
		Tradable:     false,
		Strategy: mm.MakingStrategy{
			Type: 1,
		},
		Price: domain.Prices{},
	}

	if _, err := os.Stat(testDir); os.IsNotExist(err) {
		os.Mkdir(testDir, os.ModePerm)
	}
	dbManager, err := dbbadger.NewDbManager(testDir, nil)
	if err != nil {
		panic(err)
	}
	err = dbManager.Store.Insert("vault", &v)
	if err != nil {
		return nil, err
	}

	err = dbManager.PriceStore.Insert(market.AccountIndex, &domain.Prices{
		BasePrice:  decimal.Decimal{},
		QuotePrice: decimal.Decimal{},
	})
	if err != nil {
		return nil, err
	}

	for _, val := range unsp {
		err = dbManager.UnspentStore.Insert(val.Key(), &val)
		if err != nil {
			return nil, err
		}
	}

	err = dbManager.Store.Insert(market.AccountIndex, &market)
	if err != nil {
		return nil, err
	}

	return dbManager, nil
}
