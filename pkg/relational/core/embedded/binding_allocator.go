package embedded

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// bindingAllocator belongs to one query construction and is shared by all of
// its child/CTE owners. No namespace restart or process-global planning history
// can give a sibling a previously allocated identity.
type bindingAllocator struct {
	ids      values.CorrelationIdentifierAllocator
	reserved map[string]struct{}
}

func (a *bindingAllocator) reserve(name string) {
	if a.reserved == nil {
		a.reserved = make(map[string]struct{})
	}
	if name != "" {
		a.reserved[strings.ToUpper(name)] = struct{}{}
	}
}

func (a *bindingAllocator) mint() values.CorrelationIdentifier {
	for {
		id := a.ids.Next()
		if _, occupied := a.reserved[strings.ToUpper(id.Name())]; occupied {
			continue
		}
		a.reserve(id.Name())
		return id
	}
}
