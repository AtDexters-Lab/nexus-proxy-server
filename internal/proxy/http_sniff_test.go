package proxy

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPeekHTTPHostAndPreludeEnforcesLimit(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer server.Close()

	limit := 128
	go func() {
		defer client.Close()
		filler := strings.Repeat("a", limit+32)
		fmt.Fprintf(client, "GET / HTTP/1.1\r\nHost: example.com\r\nX-Fill: %s\r\n\r\n", filler)
	}()

	_, _, _, err := PeekHTTPHostAndPrelude(server, PeekTimeouts{
		FirstByte:     time.Second,
		IdleExtension: 500 * time.Millisecond,
		AbsDeadline:   time.Now().Add(time.Second),
	}, limit)
	require.ErrorIs(t, err, ErrHTTPPreludeTooLarge)
}
