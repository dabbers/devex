package config

import (
	"fmt"
	"strings"

	"github.com/dabbers/devex/internal/auth"
)

// AuthKind selects how requests are identified.
type AuthKind string

const (
	// AuthClerk verifies session tokens from a Clerk instance.
	AuthClerk AuthKind = "clerk"
	// AuthSingle treats every request as one fixed person. It authenticates
	// nothing, so it is for a daemon on a loopback address and nowhere else.
	AuthSingle AuthKind = "single"
)

// AuthConfig configures the identity boundary.
type AuthConfig struct {
	// Kind is required. There is no default: a deployment that forgot to
	// configure authentication must fail to start rather than come up open.
	Kind  AuthKind         `yaml:"kind" json:"kind"`
	Clerk auth.ClerkConfig `yaml:"clerk" json:"clerk"`
}

// Validate checks the configuration.
func (c AuthConfig) Validate() error {
	switch c.Kind {
	case AuthClerk:
		return c.Clerk.Validate()
	case AuthSingle:
		return nil
	case "":
		return fmt.Errorf("auth.kind is required; set it to clerk, or to single for local development")
	default:
		return fmt.Errorf("auth.kind %q is not one of clerk, single", c.Kind)
	}
}

// Authenticator builds the configured authenticator.
func (c *Config) Authenticator() (auth.Authenticator, error) {
	switch c.Auth.Kind {
	case AuthClerk:
		clerk := c.Auth.Clerk
		if clerk.SecretKey == "" {
			clerk.SecretKey = strings.TrimSpace(c.Env(EnvClerkSecret))
		}
		if clerk.PublishableKey == "" {
			clerk.PublishableKey = strings.TrimSpace(c.Env(EnvClerkPublishable))
		}
		return auth.NewClerk(clerk)
	default:
		// The owner from the configuration file becomes the one person every
		// request is attributed to.
		return auth.Single{Subject: "single-user:" + c.Owner, Email: c.Owner}, nil
	}
}
