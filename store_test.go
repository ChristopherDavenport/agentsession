package agentsession_test

import (
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/storetest"
)

func TestMemoryStore(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store { return agentsession.NewMemoryStore() },
	})
}
