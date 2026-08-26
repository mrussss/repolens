package fixture

// User represents a user entity.
type User struct {
	ID   int64
	Name string
}

// UserID is a type definition for int64.
type UserID int64

// UserAlias is a type alias for User.
type UserAlias = User

// Status enum type.
type Status string

const (
	StatusActive  Status = "ACTIVE"
	StatusPending Status = "PENDING"
)
