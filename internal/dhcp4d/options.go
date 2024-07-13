package dhcp4d

import "net"

type options struct {
	conn net.PacketConn
}

type Option interface {
	set(*options)
}

type connOption struct {
	conn net.PacketConn
}

func (c *connOption) set(o *options) {
	o.conn = c.conn
}

func WithConn(conn net.PacketConn) Option {
	return &connOption{conn: conn}
}
