package client

import "fdb.dev/pkg/fdbgo/transport"

// replyGuard owns a registration until it receives the reply or transfers it to
// another owner. Its deferred close also unregisters a request whose builder or
// sender panics, like C++ ReplyPromise/FlowReceiver destruction (fdbrpc.h:48-53).
// A retry loop reuses one guard, releasing each attempt before registering another.
type replyGuard struct {
	handle *transport.ReplyHandle
}

func (g *replyGuard) close() {
	if g.handle != nil {
		g.handle.Cancel()
		g.release()
	}
}

// release follows a receive or Cancel; the registration can no longer publish.
func (g *replyGuard) release() {
	h := g.handle
	g.handle = nil
	h.Release()
}

func (g *replyGuard) transfer() { g.handle = nil }
