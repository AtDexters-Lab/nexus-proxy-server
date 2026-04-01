//go:build !linux

package client

// watchNetworkState returns nil on non-Linux platforms.
// This is a no-op stub; the reconnection loop will use timer-based backoff only.
func (c *Client) watchNetworkState() <-chan struct{} {
	return nil
}
