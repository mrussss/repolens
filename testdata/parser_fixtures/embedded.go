package fixture

// BaseEntity holds common fields.
type BaseEntity struct {
	CreatedAt int64
	UpdatedAt int64
}

// Account embeds BaseEntity and adds account details.
type Account struct {
	BaseEntity
	AccountNo string
	Balance   int64
}

// Deposit credits the account balance.
func (a *Account) Deposit(amount int64) {
	a.Balance += amount
}
