package reviewrequest

import "context"

// DenyAllProducers is a ProducerPolicy that authorizes no principal to publish
// observations. It is the policy of deployments that expose only the public
// surface: execution observations arrive over the internal surface, which is
// enabled separately together with its service principals.
type DenyAllProducers struct{}

// MayPublish implements ProducerPolicy.
func (DenyAllProducers) MayPublish(context.Context, string, string) bool { return false }
