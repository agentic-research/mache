// Package leyline consumes LLO's published daemon JSON wire contract.
package leyline

import daemonwire "github.com/agentic-research/ley-line-open/clients/go/leyline-schema/daemon/wire"

// These aliases preserve mache's internal call sites while making the LLO
// schema module the single definition of the daemon response contract.
type (
	Node                 = daemonwire.Node
	Ref                  = daemonwire.Ref
	GetNodeResponse      = daemonwire.GetNodeResponse
	ListChildrenResponse = daemonwire.ListChildrenResponse
	ReadContentResponse  = daemonwire.ReadContentResponse
	FindCallersResponse  = daemonwire.FindCallersResponse
	FindCalleesResponse  = daemonwire.FindCalleesResponse
)
