package graph

import (
	"github.com/djism/conduit/config"
	"github.com/djism/conduit/internal/dlq"
	"github.com/djism/conduit/internal/kafka"
	"github.com/djism/conduit/internal/lag"
)

// Resolver is the root resolver — injected into every query,
// mutation, and subscription resolver via the embedded pointer.
//
// All services are injected here at startup in cmd/server/main.go.
// Resolver methods never construct services themselves — they only
// use what's injected. This makes resolvers fully testable with mocks.
type Resolver struct {
	Config   *config.Config
	DLQ      *dlq.Store
	Schemas  *kafka.SchemaRegistry
	Tracker  *lag.Tracker
	Admin    *kafka.AdminClient
	Producer *kafka.Producer
}
