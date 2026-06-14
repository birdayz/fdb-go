package fdb

// Compile-time interface checks.
var (
	_ Transactor          = Database{}
	_ Transactor          = Transaction{}
	_ ReadTransactor      = Database{}
	_ ReadTransactor      = Transaction{}
	_ ReadTransactor      = Snapshot{}
	_ ReadTransaction     = Transaction{}
	_ ReadTransaction     = Snapshot{}
	_ WritableTransaction = Transaction{}
	_ TransactionOptions  = goTransactionOptions{}
	_ KeyConvertible      = Key{}
	_ ExactRange          = KeyRange{}
	_ Range               = KeyRange{}
	_ Range               = SelectorRange{}
	_ Selectable          = KeySelector{}
)
