//go:build !linux

package transport

const (
	netgEnabled = false
	netgWriter  = false
)

type ngConn struct{}

func netgRegister(*Conn) bool { return false }

func (c *Conn) ngSend(UID, []byte) error { panic("netgoroutine: linux only") }

func (c *Conn) ngClose() {}
