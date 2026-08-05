package drawer

import (
	"testing"

	"go.viam.com/test"
)

func TestConfigValidate(t *testing.T) {
	cfg := &Config{}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldBeEmpty)
}
