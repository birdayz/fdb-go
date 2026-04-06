package fdb

// Tenant is a handle to a FoundationDB tenant.
// Tenant operations are not yet implemented in the pure Go client.
type Tenant struct {
	db Database
}

func (t Tenant) Transact(f func(Transaction) (any, error)) (any, error) {
	return nil, Error{Code: 2051}
}

func (t Tenant) ReadTransact(f func(ReadTransaction) (any, error)) (any, error) {
	return nil, Error{Code: 2051}
}

func (t Tenant) CreateTransaction() (Transaction, error) {
	return Transaction{}, Error{Code: 2051}
}
