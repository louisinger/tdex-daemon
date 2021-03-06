package application

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/tdex-network/tdex-daemon/config"
	"github.com/tdex-network/tdex-daemon/internal/core/domain"
	"github.com/tdex-network/tdex-daemon/internal/infrastructure/storage/db/inmemory"
	"github.com/tdex-network/tdex-daemon/pkg/explorer"
	"github.com/tdex-network/tdex-daemon/pkg/trade"
	"github.com/vulpemventures/go-elements/network"
)

const (
	marketRepoIsEmpty  = true
	tradeRepoIsEmpty   = true
	vaultRepoIsEmpty   = true
	unspentRepoIsEmpty = true
	marketPluggable    = true
)

var baseAsset = config.GetString(config.BaseAssetKey)

func TestListMarket(t *testing.T) {
	t.Run("ListMarket should return an empty list and a nil error if market repository is empty", func(t *testing.T) {
		operatorService, ctx, close := newTestOperator(marketRepoIsEmpty, tradeRepoIsEmpty, vaultRepoIsEmpty)
		defer close()
		marketInfos, err := operatorService.ListMarket(ctx)
		assert.Equal(t, nil, err)
		assert.Equal(t, 0, len(marketInfos))
	})

	t.Run("ListMarket should return the number of markets in the market repository", func(t *testing.T) {
		operatorService, ctx, close := newTestOperator(!marketRepoIsEmpty, tradeRepoIsEmpty, vaultRepoIsEmpty)
		defer close()
		marketInfos, err := operatorService.ListMarket(ctx)
		assert.Equal(t, nil, err)
		assert.Equal(t, 2, len(marketInfos))
	})
}

func TestDepositMarket(t *testing.T) {
	operatorService, ctx, close := newTestOperator(marketRepoIsEmpty, tradeRepoIsEmpty, vaultRepoIsEmpty)

	config.Set(config.MnemonicKey, strings.Join(newTradeWallet().mnemonic, " "))

	t.Run("DepositMarket with new market", func(t *testing.T) {
		address, err := operatorService.DepositMarket(ctx, "", "")
		assert.Equal(t, nil, err)

		assert.Equal(
			t,
			"el1qqvead5fpxkjyyl3zwukr7twqrnag40ls0y052s547smxdyeus209ppkmtdyemgkz4rjn8ss8fhjrzc3q9evt7atrgtpff2thf",
			address,
		)
	})

	t.Run("DepositMarket with invalid base asset", func(t *testing.T) {
		validQuoteAsset := "5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225"
		emptyAddress, err := operatorService.DepositMarket(ctx, "", validQuoteAsset)
		assert.Equal(t, domain.ErrInvalidBaseAsset, err)
		assert.Equal(
			t,
			"",
			emptyAddress,
		)
	})

	t.Run("DepositMarket with valid base asset and empty quote asset", func(t *testing.T) {
		emptyAddress, err := operatorService.DepositMarket(ctx, baseAsset, "")
		assert.Equal(t, domain.ErrInvalidQuoteAsset, err)
		assert.Equal(
			t,
			"",
			emptyAddress,
		)
	})

	t.Run("DepositMarket with valid base asset and invalid quote asset", func(t *testing.T) {
		emptyAddress, err := operatorService.DepositMarket(ctx, baseAsset, "ldjbwjkbfjksdbjkvcsbdjkbcdsjkb")
		assert.Equal(t, domain.ErrInvalidQuoteAsset, err)
		assert.Equal(
			t,
			"",
			emptyAddress,
		)
	})

	t.Cleanup(close)
}

func TestDepositMarketWithCrawler(t *testing.T) {

	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	t.Run("Get address to deposit, fund market and get next address for the market", func(t *testing.T) {
		operatorService, ctx, close := newTestOperator(marketRepoIsEmpty, tradeRepoIsEmpty, !vaultRepoIsEmpty)

		t.Cleanup(func() {
			close()
		})

		address, err := operatorService.DepositMarket(ctx, "", "")
		assert.Equal(t, nil, err)

		assert.Equal(t, nil, err)
		assert.Equal(
			t,
			"el1qqvead5fpxkjyyl3zwukr7twqrnag40ls0y052s547smxdyeus209ppkmtdyemgkz4rjn8ss8fhjrzc3q9evt7atrgtpff2thf",
			address,
		)

		// Let's depsoit both assets on the same address
		explorerSvc := explorer.NewService(RegtestExplorerAPI)
		_, err = explorerSvc.Faucet(address)
		assert.Equal(t, nil, err)
		time.Sleep(1500 * time.Millisecond)

		_, quoteAsset, err := explorerSvc.Mint(address, 5)
		assert.Equal(t, nil, err)
		time.Sleep(1500 * time.Millisecond)

		// we try to get a child address for the quote asset. Since is not being expicitly initialized, should return ErrMarketNotExist
		failToGetChildAddress, err := operatorService.DepositMarket(ctx, baseAsset, quoteAsset)
		assert.Equal(t, domain.ErrMarketNotExist, err)
		assert.Equal(
			t,
			"",
			failToGetChildAddress,
		)

		// Now we try to intialize (ie. fund) the market by opening it
		err = operatorService.OpenMarket(ctx, baseAsset, quoteAsset)
		assert.Equal(t, nil, err)

		// Now we can derive a childAddress
		childAddress, err := operatorService.DepositMarket(ctx, baseAsset, quoteAsset)
		assert.Equal(t, nil, err)
		assert.Equal(
			t,
			"el1qqfzjp0y057j60avxqgmj9aycqhlq7ke20v20c8dkml68jjs0fu09u9sn55uduay46yyt25tcny0rfqejly5x6dgjw44uk9p8r",
			childAddress,
		)

	})
}

func TestUpdateMarketPrice(t *testing.T) {
	const getPriceAfterRequest = true

	market := Market{
		BaseAsset:  marketUnspents[0].AssetHash,
		QuoteAsset: marketUnspents[1].AssetHash,
	}

	// update price function
	updateMarketPriceRequest := func(basePrice float64, quotePrice float64, getPrice bool) (*Price, error) {
		const initMarketAsPluggable = true

		operatorService, tradeService, _, ctx, close, _ := newMockServices(
			!marketRepoIsEmpty,
			tradeRepoIsEmpty,
			!vaultRepoIsEmpty,
			!unspentRepoIsEmpty,
			initMarketAsPluggable,
		)

		t.Cleanup(close)
		args := MarketWithPrice{
			Market: market,
			Price: Price{
				BasePrice:  decimal.NewFromFloat(basePrice),
				QuotePrice: decimal.NewFromFloat(quotePrice),
			},
		}

		// update the price
		err := operatorService.UpdateMarketPrice(ctx, args)
		if err != nil {
			return nil, err
		}

		// get the price if flag is specified
		if getPrice {
			priceWithFee, err := tradeService.GetMarketPrice(ctx, market, 1, 1)
			if err != nil {
				panic(err)
			}
			return &priceWithFee.Price, nil
		}

		return nil, nil
	}

	t.Run("should not return an error if the price is valid and market is found", func(t *testing.T) {
		priceAfter, err := updateMarketPriceRequest(10.01, 1000, getPriceAfterRequest)
		assert.Equal(t, nil, err)
		basePrice, _ := priceAfter.BasePrice.Float64()
		quotePrice, _ := priceAfter.QuotePrice.Float64()
		assert.Equal(t, basePrice, float64(10.01))
		assert.Equal(t, quotePrice, float64(1000))
	})

	t.Run("shoud not return an error if the price is valid and > 0 && < 1", func(t *testing.T) {
		_, err := updateMarketPriceRequest(0.4, 1, !getPriceAfterRequest)
		assert.Equal(t, nil, err)
	})

	t.Run("should return an error if the prices are <= 0", func(t *testing.T) {
		_, err := updateMarketPriceRequest(-1, 10000, !getPriceAfterRequest)
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return an error if the prices are greater than 2099999997690000", func(t *testing.T) {
		_, err := updateMarketPriceRequest(1, 2099999997690000+1, !getPriceAfterRequest)
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return an error if one of the prices are equal to zero", func(t *testing.T) {
		_, err := updateMarketPriceRequest(102.1293, 0, !getPriceAfterRequest)
		assert.NotEqual(t, nil, err)
	})

}
func TestListSwap(t *testing.T) {
	t.Run("ListSwap should return an empty list and a nil error if there is not trades in TradeRepository", func(t *testing.T) {
		operatorService, ctx, close := newTestOperator(marketRepoIsEmpty, tradeRepoIsEmpty, vaultRepoIsEmpty)
		defer close()

		swapInfos, err := operatorService.ListSwaps(ctx)
		assert.Equal(t, nil, err)
		assert.Equal(t, 0, len(swapInfos))
	})

	t.Run("ListSwap should return the SwapInfo according to the number of trades in the TradeRepository", func(t *testing.T) {
		operatorService, ctx, close := newTestOperator(!marketRepoIsEmpty, !tradeRepoIsEmpty, vaultRepoIsEmpty)
		defer close()

		swapInfos, err := operatorService.ListSwaps(ctx)
		assert.Equal(t, nil, err)
		assert.Equal(t, 1, len(swapInfos))
	})
}

func TestWithdrawMarket(t *testing.T) {
	operatorService, _, walletService, ctx, close, _ := newMockServices(
		!marketRepoIsEmpty,
		!tradeRepoIsEmpty,
		!vaultRepoIsEmpty,
		!unspentRepoIsEmpty,
		false,
	)

	err := walletService.UnlockWallet(ctx, "Sup3rS3cr3tP4ssw0rd!")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(close)

	t.Run(
		"WithdrawMarketFunds should return raw transaction",
		func(t *testing.T) {
			rawTx, err := operatorService.WithdrawMarketFunds(ctx, WithdrawMarketReq{
				Market: Market{
					BaseAsset:  marketUnspents[0].AssetHash,
					QuoteAsset: marketUnspents[1].AssetHash,
				},
				BalanceToWithdraw: Balance{
					BaseAmount:  4200,
					QuoteAmount: 2300,
				},
				MillisatPerByte: 20,
				Address:         "el1qq22f83p6asdy7jsp4tuke0d9emvxhcenqee5umsn88fsn8gggzlrx0md4hp38rnwcnu9lusmzhmktlt3h5q0gecfpfvx6uac2",
				Push:            false,
			})
			assert.NoError(t, err)
			assert.Equal(t, true, len(rawTx) > 0)
		},
	)

	t.Run(
		"WithdrawMarketFunds should return error for wrong base asset",
		func(t *testing.T) {
			_, err := operatorService.WithdrawMarketFunds(ctx,
				WithdrawMarketReq{
					Market: Market{
						BaseAsset:  "4144",
						QuoteAsset: "d73f5cd0954c1bf325f85d7a7ff43a6eb3ea3b516fd57064b85306d43bc1c9ff",
					},
					BalanceToWithdraw: Balance{
						BaseAmount:  4200,
						QuoteAmount: 2300,
					},
					MillisatPerByte: 20,
					Address:         "el1qq22f83p6asdy7jsp4tuke0d9emvxhcenqee5umsn88fsn8gggzlrx0md4hp38rnwcnu9lusmzhmktlt3h5q0gecfpfvx6uac2",
					Push:            false,
				})
			assert.Error(t, err)
			assert.Equal(t, err, domain.ErrInvalidBaseAsset)
		},
	)

	t.Run(
		"WithdrawMarketFunds should return error for wrong qoute asset",
		func(t *testing.T) {
			_, err := operatorService.WithdrawMarketFunds(ctx,
				WithdrawMarketReq{
					Market: Market{
						BaseAsset:  "5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225",
						QuoteAsset: "eqwqw",
					},
					BalanceToWithdraw: Balance{
						BaseAmount:  4200,
						QuoteAmount: 2300,
					},
					MillisatPerByte: 20,
					Address:         "el1qq22f83p6asdy7jsp4tuke0d9emvxhcenqee5umsn88fsn8gggzlrx0md4hp38rnwcnu9lusmzhmktlt3h5q0gecfpfvx6uac2",
					Push:            false,
				})
			assert.Error(t, err)
			assert.Equal(t, err, domain.ErrMarketNotExist)
		},
	)

	t.Run(
		"WithdrawMarketFunds should return error, not enough money",
		func(t *testing.T) {
			_, err := operatorService.WithdrawMarketFunds(ctx,
				WithdrawMarketReq{
					Market: Market{
						BaseAsset:  "5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225",
						QuoteAsset: "d73f5cd0954c1bf325f85d7a7ff43a6eb3ea3b516fd57064b85306d43bc1c9ff",
					},
					BalanceToWithdraw: Balance{
						BaseAmount:  1000000000000,
						QuoteAmount: 2300,
					},
					MillisatPerByte: 20,
					Address:         "el1qq22f83p6asdy7jsp4tuke0d9emvxhcenqee5umsn88fsn8gggzlrx0md4hp38rnwcnu9lusmzhmktlt3h5q0gecfpfvx6uac2",
					Push:            false,
				})
			assert.Error(t, err)
		},
	)
}

func TestBalanceFeeAccount(t *testing.T) {
	operatorService, _, walletSvc, ctx, close, _ := newMockServices(
		!marketRepoIsEmpty,
		tradeRepoIsEmpty,
		!vaultRepoIsEmpty,
		!unspentRepoIsEmpty,
		!marketPluggable,
	)
	t.Cleanup(close)

	err := walletSvc.UnlockWallet(ctx, "Sup3rS3cr3tP4ssw0rd!")
	if err != nil {
		t.Fatal(err)
	}

	t.Run(
		"FeeAccountBalance should return fee account balance",
		func(t *testing.T) {
			balance, err := operatorService.FeeAccountBalance(ctx)
			assert.NoError(t, err)
			assert.Equal(t, int64(100000000), balance)
		},
	)
}

func TestGetCollectedMarketFee(t *testing.T) {
	operatorService, traderSvc, _, ctx, _, dbManager := newMockServices(
		!marketRepoIsEmpty,
		!tradeRepoIsEmpty,
		!vaultRepoIsEmpty,
		!unspentRepoIsEmpty,
		!marketPluggable,
	)

	markets, err := traderSvc.GetTradableMarkets(ctx)
	if err != nil {
		t.Fatal(err)
	}

	market := markets[0].Market
	preview, err := traderSvc.GetMarketPrice(ctx, market, TradeSell, 30000000)
	if err != nil {
		t.Fatal(err)
	}

	proposerWallet, err := trade.NewRandomWallet(&network.Regtest)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("GetCollectedMarketFee", func(t *testing.T) {
		swapRequest, err := newSwapRequest(
			proposerWallet,
			market.BaseAsset, 30000000,
			market.QuoteAsset, preview.Amount,
		)
		if err != nil {
			t.Fatal(err)
		}

		_, swapFail, _, err := traderSvc.TradePropose(
			ctx,
			market,
			TradeSell,
			swapRequest,
		)
		if err != nil {
			t.Fatal(err)
		}
		if swapFail != nil {
			t.Fatal(swapFail.GetFailureMessage())
		}

		fee, err := operatorService.GetCollectedMarketFee(ctx, market)
		if err != nil {
			t.Fatal(err)
		}

		assert.Equal(t, 0, len(fee.CollectedFees))

		tradeRepo := inmemory.NewTradeRepositoryImpl(dbManager)
		trades, err := tradeRepo.GetAllTradesByMarket(ctx, market.QuoteAsset)
		if err != nil {
			t.Fatal(err)
		}

		tr := trades[0]
		err = tradeRepo.UpdateTrade(
			ctx,
			&tr.ID,
			func(trade *domain.Trade) (*domain.Trade, error) {
				trade.Status = domain.CompletedStatus
				return trade, nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		fee, err = operatorService.GetCollectedMarketFee(ctx, market)
		if err != nil {
			t.Fatal(err)
		}

		assert.Equal(t, 1, len(fee.CollectedFees))
	})

}

func TestListMarketExternalAddresses(t *testing.T) {
	const (
		validQuoteAsset             = "d090c403610fe8a9e31967355929833bc8a8fe08429e630162d1ecbf29fdf28b"
		validBaseAsset              = "5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225"
		validQuoteAssetWithNoMarket = "0ddfa690c7b2ba3b8ecee8200da2420fc502f57f8312c83d466b6f8dced70441"
		invalidAsset                = "aaa001zzzDL"
	)

	const (
		vaultIsEmpty    = true
		vaultIsNotEmpty = false
	)

	listMarketExternalRequest := func(
		baseAsset string,
		quoteAsset string,
		repoIsEmpty bool,
	) ([]string, error) {
		operatorService, ctx, close := newTestOperator(!marketRepoIsEmpty, tradeRepoIsEmpty, repoIsEmpty)
		t.Cleanup(close)
		market := Market{
			QuoteAsset: quoteAsset,
			BaseAsset:  baseAsset,
		}
		return operatorService.ListMarketExternalAddresses(ctx, market)
	}

	t.Run("should return error if baseAsset is an invalid asset string", func(t *testing.T) {
		_, err := listMarketExternalRequest(invalidAsset, validQuoteAsset, vaultIsNotEmpty)
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return error if quoteAsset is an invalid asset string", func(t *testing.T) {
		_, err := listMarketExternalRequest(validBaseAsset, invalidAsset, vaultIsNotEmpty)
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return error if market is not found for the given quoteAsset", func(t *testing.T) {
		_, err := listMarketExternalRequest(validBaseAsset, validQuoteAssetWithNoMarket, vaultIsNotEmpty)
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return an error if the Vault repository is empty", func(t *testing.T) {
		_, err := listMarketExternalRequest(validBaseAsset, validQuoteAsset, vaultIsEmpty)
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return a list of addresses and a nil error if the market argument is valid", func(t *testing.T) {
		addresses, err := listMarketExternalRequest(validBaseAsset, validQuoteAsset, vaultIsNotEmpty)
		assert.Equal(t, nil, err)
		assert.NotEqual(t, nil, addresses)
		assert.Equal(t, 1, len(addresses))
	})
}

func TestOpenMarket(t *testing.T) {
	const depositFeeAccount = true

	validBaseAsset := marketUnspents[0].AssetHash
	validQuoteAsset := marketUnspents[1].AssetHash

	const (
		validQuoteAssetWithNoMarket = "0ddfa690c7b2ba3b8ecee8200da2420fc502f57f8312c83d466b6f8dced70441"
		invalidAsset                = "allezlafrance"
	)

	openMarketRequest := func(
		baseAsset string,
		quoteAsset string,
		depositFeeAccountBefore bool,
	) (error, error, func()) {
		operatorService, ctx, close := newTestOperator(!marketRepoIsEmpty, tradeRepoIsEmpty, vaultRepoIsEmpty)
		if depositFeeAccountBefore {
			_, _, err := operatorService.DepositFeeAccount(ctx)
			if err != nil {
				return err, nil, close
			}
		}

		return nil, operatorService.OpenMarket(ctx, baseAsset, quoteAsset), close
	}

	t.Run("should return an error if the crawler does not observe any fee account addresses", func(t *testing.T) {
		failErr, err, close := openMarketRequest(validBaseAsset, validQuoteAsset, !depositFeeAccount)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
		close()
	})

	t.Run("should return an error if the base asset is not valid", func(t *testing.T) {
		failErr, err, close := openMarketRequest(invalidAsset, validQuoteAsset, depositFeeAccount)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
		close()
	})

	t.Run("should return an error if the quote asset is not valid", func(t *testing.T) {
		failErr, err, close := openMarketRequest(validBaseAsset, invalidAsset, depositFeeAccount)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
		close()
	})

	t.Run("should return an error if the market is not found", func(t *testing.T) {
		failErr, err, close := openMarketRequest(validBaseAsset, validQuoteAssetWithNoMarket, depositFeeAccount)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
		close()
	})

	t.Run("should NOT return an error if someone have deposited an address and assets string are valid", func(t *testing.T) {
		failErr, err, close := openMarketRequest(validBaseAsset, validQuoteAsset, depositFeeAccount)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.Equal(t, nil, err)
		close()
	})
}
func TestUpdateMarketStrategy(t *testing.T) {
	const (
		validQuoteAssetWithNoMarket = "0ddfa690c7b2ba3b8ecee8200da2420fc502f57f8312c83d466b6f8dced8a441"
		invalidAsset                = "allezlesbleus"
	)

	const (
		letsCloseTheMarketBefore = true
		dontCloseTheMarketBefore = false
	)

	operatorService, ctx, close := newTestOperator(!marketRepoIsEmpty, tradeRepoIsEmpty, !vaultRepoIsEmpty)
	defer close()

	validMarket := Market{
		BaseAsset:  marketUnspents[0].AssetHash,
		QuoteAsset: marketUnspents[1].AssetHash,
	}

	// update price function
	updateMarketStrategy := func(strategy domain.StrategyType, market Market, closeTheMarket bool) (error, error) {
		// close the market
		if closeTheMarket {
			err := operatorService.CloseMarket(ctx, market.BaseAsset, market.QuoteAsset)
			if err != nil {
				return nil, err
			}
		}

		// update the strategy
		err := operatorService.UpdateMarketStrategy(
			ctx,
			MarketStrategy{
				Market:   market,
				Strategy: strategy,
			},
		)

		if err != nil {
			return err, nil
		}

		// if pluggable set prices
		if strategy == domain.StrategyTypePluggable {
			err := operatorService.UpdateMarketPrice(ctx,
				MarketWithPrice{
					Market: market,
					Price: Price{
						BasePrice:  decimal.NewFromFloat(0.2),
						QuotePrice: decimal.NewFromInt(1),
					},
				},
			)

			if err != nil {
				return nil, err
			}
		}

		// reopen the market
		_, _, err = operatorService.DepositFeeAccount(ctx)
		if err != nil {
			return nil, err
		}

		err = operatorService.OpenMarket(ctx, market.BaseAsset, market.QuoteAsset)
		if err != nil {
			return nil, err
		}

		return nil, nil
	}

	// get market price function
	getMarketStrategy := func() (domain.StrategyType, error) {
		marketsInfos, err := operatorService.ListMarket(ctx)
		if err != nil {
			return -1, err
		}

		for _, marketInfo := range marketsInfos {
			if marketInfo.Market.BaseAsset == validMarket.BaseAsset && marketInfo.Market.QuoteAsset == validMarket.QuoteAsset {
				return domain.StrategyType(marketInfo.StrategyType), err
			}
		}

		err = errors.New("market not found")
		return -1, err
	}

	t.Run("should update the strategy to PLUGGABLE", func(t *testing.T) {
		err, failErr := updateMarketStrategy(domain.StrategyTypePluggable, validMarket, letsCloseTheMarketBefore)
		if failErr != nil {
			t.Error(failErr)
		}
		strategy, failErr := getMarketStrategy()
		if failErr != nil {
			t.Error(failErr)
		}
		assert.Equal(t, nil, err)
		assert.Equal(t, domain.StrategyTypePluggable, strategy)
	})

	t.Run("should update the strategy to BALANCED", func(t *testing.T) {
		err, failErr := updateMarketStrategy(domain.StrategyTypeBalanced, validMarket, letsCloseTheMarketBefore)
		if failErr != nil {
			t.Error(failErr)
		}
		strategy, failErr := getMarketStrategy()
		if failErr != nil {
			t.Error(failErr)
		}
		assert.Equal(t, nil, err)
		assert.Equal(t, domain.StrategyTypeBalanced, strategy)
	})

	t.Run("should return an error if the new strategy is not supported", func(t *testing.T) {
		err, failErr := updateMarketStrategy(domain.StrategyTypeUnbalanced, validMarket, letsCloseTheMarketBefore)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return an error if the market quote asset is invalid", func(t *testing.T) {
		err, failErr := updateMarketStrategy(
			domain.StrategyTypePluggable,
			Market{
				BaseAsset:  validMarket.BaseAsset,
				QuoteAsset: invalidAsset,
			},
			dontCloseTheMarketBefore,
		)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return an error if the market base asset is invalid", func(t *testing.T) {
		err, failErr := updateMarketStrategy(
			domain.StrategyTypePluggable,
			Market{
				BaseAsset:  invalidAsset,
				QuoteAsset: validMarket.QuoteAsset,
			},
			dontCloseTheMarketBefore,
		)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return an error if the market does not exist", func(t *testing.T) {
		err, failErr := updateMarketStrategy(
			domain.StrategyTypePluggable,
			Market{
				BaseAsset:  validMarket.BaseAsset,
				QuoteAsset: validQuoteAssetWithNoMarket,
			},
			dontCloseTheMarketBefore,
		)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
	})

	t.Run("should return an error if the market is not closed", func(t *testing.T) {
		err, failErr := updateMarketStrategy(
			domain.StrategyTypePluggable,
			Market{
				BaseAsset:  validMarket.BaseAsset,
				QuoteAsset: validQuoteAssetWithNoMarket,
			},
			dontCloseTheMarketBefore,
		)
		if failErr != nil {
			t.Error(failErr)
		}
		assert.NotEqual(t, nil, err)
	})
}
