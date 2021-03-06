package ports

import "context"

// DbManager interface defines the methods for swap, price and unspent.
type DbManager interface {
	NewTransaction() Transaction
	NewPricesTransaction() Transaction
	NewUnspentsTransaction() Transaction
	RunTransaction(
		ctx context.Context,
		readOnly bool,
		handler func(ctx context.Context) (interface{}, error),
	) (interface{}, error)
	RunUnspentsTransaction(
		ctx context.Context,
		readOnly bool,
		handler func(ctx context.Context) (interface{}, error),
	) (interface{}, error)
	RunPricesTransaction(
		ctx context.Context,
		readOnly bool,
		handler func(ctx context.Context) (interface{}, error),
	) (interface{}, error)
}

// Transaction interface defines the method to commit or discard a database transaction.
type Transaction interface {
	Commit() error
	Discard()
}
