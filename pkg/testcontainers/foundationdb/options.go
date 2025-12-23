package foundationdb

import (
	"github.com/testcontainers/testcontainers-go"
)

type configCustomizer struct {
	fn func(*Config)
}

func (c *configCustomizer) Customize(req *testcontainers.GenericContainerRequest) error {
	// This customizer modifies the internal config, not the container request
	return nil
}

func (c *configCustomizer) customize(config *Config) {
	c.fn(config)
}

// WithDatabase sets the database name for the FoundationDB container
func WithDatabase(database string) testcontainers.ContainerCustomizer {
	return &configCustomizer{
		fn: func(config *Config) {
			config.Database = database
		},
	}
}

// WithAPIVersion sets the FDB API version
func WithAPIVersion(version int) testcontainers.ContainerCustomizer {
	return &configCustomizer{
		fn: func(config *Config) {
			config.APIVersion = version
		},
	}
}

// WithMemory sets the memory limit for the FDB process
func WithMemory(memory string) testcontainers.ContainerCustomizer {
	return &configCustomizer{
		fn: func(config *Config) {
			config.Memory = memory
		},
	}
}

// WithVersion sets the FoundationDB version (Docker tag)
func WithVersion(version string) testcontainers.ContainerCustomizer {
	return &configCustomizer{
		fn: func(config *Config) {
			config.Version = version
		},
	}
}

// Note: Advanced FoundationDB configuration options like custom cluster files,
// networking modes, datacenter settings, etc. are not supported in this testcontainer
// implementation due to the socat proxy networking approach. This module is designed
// for simple single-node testing scenarios.
