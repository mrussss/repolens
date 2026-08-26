package fixture

import (
	"github.com/external/missing"
)

// CallExternal invokes an external unresolved package.
func CallExternal() string {
	return missing.DoSomething("arg")
}
