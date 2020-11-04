package grpchandler

import (
	"context"
	"log"
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tdex-network/tdex-daemon/internal/core/application"
	dbbadger "github.com/tdex-network/tdex-daemon/internal/infrastructure/storage/db/badger"
	"github.com/tdex-network/tdex-daemon/pkg/explorer"
	pb "github.com/tdex-network/tdex-protobuf/generated/go/trade"
	"github.com/tdex-network/tdex-protobuf/generated/go/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

const RegtestExplorerAPI = "http://127.0.0.1:3001"
const testDir = "testDatadir"

const (
	LBTC = "5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225"
	USDT = "7151f4f38f546c3084afa957c5a0b914b4af5726065b450edad1fc11b8dbe900"
)

func newTraderHandler() (pb.TradeServer, context.Context, func()) {
	if _, err := os.Stat(testDir); os.IsNotExist(err) {
		os.Mkdir(testDir, os.ModePerm)
	}

	dbManager, err := dbbadger.NewDbManager(testDir, nil)
	if err != nil {
		panic(err)
	}
	
	tx := dbManager.NewTransaction()
	ctx := context.WithValue(context.Background(), "tx", tx)

	explorerSvc := explorer.NewService(RegtestExplorerAPI)
	tradeSvc := application.NewTradeService(
		dbbadger.NewMarketRepositoryImpl(dbManager), 
		dbbadger.NewTradeRepositoryImpl(dbManager), 
		dbbadger.NewVaultRepositoryImpl(dbManager), 
		dbbadger.NewUnspentRepositoryImpl(dbManager), 
		explorerSvc,
	)

	tradeHandler := NewTraderHandler(tradeSvc)
	close := func() {
		dbManager.Store.Close()
		dbManager.UnspentStore.Close()
		os.RemoveAll(testDir)
	}
	return tradeHandler, ctx, close
}

func dialer() (func(context.Context, string) (net.Conn, error), context.Context, func()) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()

	traderHandler, ctx, close := newTraderHandler()
	pb.RegisterTradeServer(server, traderHandler)
 
	go func() {
		if err := server.Serve(listener); err != nil {
			log.Fatal(err)
		}
	}()
 
	return func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}, ctx, close
}

func initClient() (pb.TradeClient, context.Context, func()) {
	dialer, ctx, closeDb := dialer()
	conn, err := grpc.DialContext(ctx, "", grpc.WithInsecure(), grpc.WithContextDialer(dialer))
	if err != nil {
		log.Fatal(err)
	}
	client := pb.NewTradeClient(conn)

	closeFn := func() {
		conn.Close()
		closeDb()
	}

	return client, ctx, closeFn
}

func TestTraderServer_Markets(t *testing.T) {
	client, ctx, close := initClient()
	defer close()

	t.Run("Markets should return the markets", func (t *testing.T) {
		request := &pb.MarketsRequest{}
		reply, err := client.Markets(ctx, request)
		assert.Equal(t, nil, err)
		markets := reply.Markets
		assert.Equal(t, 0, len(markets))
	})
}

func TestTraderServer_Balances(t *testing.T) {
	client, ctx, close := initClient()
	defer close()

	t.Run("Balances shoud return error if market does not exist", func (t *testing.T) {
		request := &pb.BalancesRequest{ Market: &types.Market{BaseAsset: LBTC, QuoteAsset: USDT}}
		_, err := client.Balances(ctx, request)
		assert.NotEqual(t, nil, err)
		// balances := reply.Balances
		// assert.Equal(t, 0, len(balances))
	})

}