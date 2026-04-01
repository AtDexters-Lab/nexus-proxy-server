//go:build linux

package client

import (
	"log"
	"syscall"
	"unsafe"
)

const (
	// Netlink constants for subscribing to link state changes
	rtmgrpLink = 0x1

	// Netlink message types
	rtmNewlink = 16

	// Interface flags
	iffUp      = 0x1
	iffRunning = 0x40
)

// ifInfoMsg is the Linux interface info message structure
type ifInfoMsg struct {
	Family uint8
	_      uint8
	Type   uint16
	Index  int32
	Flags  uint32
	Change uint32
}

// watchNetworkState monitors network interface state changes using netlink.
// Returns a channel that receives a signal when a network interface comes up.
// This is best-effort: if netlink setup fails, returns nil (falls back to timer-based backoff).
func (c *Client) watchNetworkState() <-chan struct{} {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		log.Printf("DEBUG: [%s] Network state monitoring unavailable: %v (using timer-based backoff)", c.config.Name, err)
		return nil
	}

	// Bind to link state change events
	addr := &syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: rtmgrpLink,
	}
	if err := syscall.Bind(fd, addr); err != nil {
		syscall.Close(fd)
		log.Printf("DEBUG: [%s] Failed to bind netlink socket: %v (using timer-based backoff)", c.config.Name, err)
		return nil
	}

	wakeup := make(chan struct{}, 1)

	go func() {
		defer syscall.Close(fd)
		defer close(wakeup)

		buf := make([]byte, 4096)
		for {
			// Check if context is done before blocking on read
			select {
			case <-c.ctx.Done():
				return
			default:
			}

			// Set a read timeout so we can periodically check ctx.Done()
			tv := syscall.Timeval{Sec: 1, Usec: 0}
			syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
					// Timeout, check context and continue
					continue
				}
				// Real error, exit the goroutine
				log.Printf("DEBUG: [%s] Netlink read error: %v", c.config.Name, err)
				return
			}

			// Parse netlink messages
			if linkUp := parseNetlinkMessages(buf[:n]); linkUp {
				// Non-blocking send to wakeup channel
				select {
				case wakeup <- struct{}{}:
					log.Printf("DEBUG: [%s] Network link up detected, signaling reconnect", c.config.Name)
				default:
					// Channel already has a pending signal
				}
			}
		}
	}()

	log.Printf("DEBUG: [%s] Network state monitoring enabled (netlink)", c.config.Name)
	return wakeup
}

// parseNetlinkMessages parses raw netlink data and returns true if a link came up.
func parseNetlinkMessages(data []byte) bool {
	for len(data) >= syscall.NLMSG_HDRLEN {
		// Parse netlink message header
		hdr := (*syscall.NlMsghdr)(unsafe.Pointer(&data[0]))
		if hdr.Len < syscall.NLMSG_HDRLEN || int(hdr.Len) > len(data) {
			break
		}

		// Check for new link message
		if hdr.Type == rtmNewlink {
			// Parse interface info
			if int(hdr.Len) >= syscall.NLMSG_HDRLEN+int(unsafe.Sizeof(ifInfoMsg{})) {
				ifInfo := (*ifInfoMsg)(unsafe.Pointer(&data[syscall.NLMSG_HDRLEN]))
				// Check if interface is up and running
				if ifInfo.Flags&iffUp != 0 && ifInfo.Flags&iffRunning != 0 {
					return true
				}
			}
		}

		// Move to next message (aligned)
		msgLen := alignNlMsg(int(hdr.Len))
		if msgLen > len(data) {
			break
		}
		data = data[msgLen:]
	}
	return false
}

// alignNlMsg aligns netlink message length to 4-byte boundary
func alignNlMsg(len int) int {
	return (len + syscall.NLMSG_ALIGNTO - 1) & ^(syscall.NLMSG_ALIGNTO - 1)
}
