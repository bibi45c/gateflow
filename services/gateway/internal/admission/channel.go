package admission

import (
	"errors"
	"regexp"
	"strings"

	"github.com/bibi45c/gateflow/services/gateway/internal/accessclient"
)

// channelProtocol is the only protocol a channel may speak in S1.
const channelProtocol = "anthropic-messages"

// The channel rules of #18, with the patterns of api/access.yaml.
var (
	baseURLPattern       = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	credentialEnvPattern = regexp.MustCompile(`^UPSTREAM_CREDENTIAL_[A-Z0-9_]+$`)
)

// checkChannel fails when a channel breaks the rules of #18: its base URL must be
// http or https, a host and an optional port, with no path, query or trailing slash;
// its credential_env must be empty, or name a variable matching
// UPSTREAM_CREDENTIAL_[A-Z0-9_]+ that getenv finds set, on an https base URL. The
// reasons are fixed strings and never hold a value of the channel.
func checkChannel(channel accessclient.Channel, getenv func(string) string) error {
	switch {
	case channel.Protocol != channelProtocol:
		return errors.New("protocol is not " + channelProtocol)
	case !baseURLPattern.MatchString(channel.BaseURL):
		return errors.New("base_url is not http(s)://host[:port]")
	case channel.CredentialEnv == "":
		return nil
	case !credentialEnvPattern.MatchString(channel.CredentialEnv):
		return errors.New("credential_env does not match UPSTREAM_CREDENTIAL_[A-Z0-9_]+")
	case !strings.HasPrefix(channel.BaseURL, "https://"):
		return errors.New("base_url is not https although credential_env is set")
	case getenv(channel.CredentialEnv) == "":
		return errors.New("the variable credential_env names is unset")
	}
	return nil
}
