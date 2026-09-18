//go:build linux

package main

import (
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func probeBatchedSupported() error { return nil }

// enableBatching sets UDP_SEGMENT so that one write() of dg*batch bytes is
// segmented by the kernel into `batch` datagrams of dg bytes each (UDP GSO).
// For the sendmmsg path there is nothing to enable.
func enableBatching(conn *net.UDPConn, dg, _ int, mode string) error {
	if mode != "gso" {
		return nil
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		// UDP_SEGMENT == 103 on Linux (include/uapi/linux/udp.h)
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_UDP, 103, dg)
	}); err != nil {
		return err
	}
	if serr != nil {
		return fmt.Errorf("UDP_SEGMENT: %w", serr)
	}
	return nil
}

func sendBatch(conn *net.UDPConn, addr *net.UDPAddr, payload []byte, dg, batch int) error {
	if gMode == "gso" {
		// One syscall; the kernel splits the buffer into `batch` segments of
		// dg bytes each.
		_, err := conn.WriteToUDP(payload[:dg*batch], addr)
		return err
	}
	// sendmmsg: `batch` datagrams in one syscall, via x/net/ipv4's WriteBatch
	// (x/sys/unix does not export Mmsghdr, so this is the supported route).
	p := ipv4.NewPacketConn(conn)
	ms := make([]ipv4.Message, batch)
	for i := range ms {
		ms[i].Buffers = [][]byte{payload[i*dg : (i+1)*dg]}
		ms[i].Addr = addr
	}
	n, err := p.WriteBatch(ms, 0)
	if err != nil {
		return err
	}
	if n != batch {
		return fmt.Errorf("sendmmsg sent %d of %d", n, batch)
	}
	return nil
}
