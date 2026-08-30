package app

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

const systemdReadyState = "READY=1\nSTATUS=ResMan is ready"

// notifySystemdReady reports that bootstrap completed to a Type=notify service
// manager. A process started outside systemd has no NOTIFY_SOCKET and needs no
// acknowledgement.
func notifySystemdReady() (resultErr error) {
	socket, present := os.LookupEnv("NOTIFY_SOCKET")
	if !present || socket == "" {
		return nil
	}
	if !strings.HasPrefix(socket, "/") && !strings.HasPrefix(socket, "@") {
		return fmt.Errorf("NOTIFY_SOCKET must be an absolute or abstract Unix socket, got %q", socket)
	}

	address := &net.UnixAddr{Name: socket, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, address)
	if err != nil {
		return fmt.Errorf("connect to systemd notification socket: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close systemd notification socket: %w", err))
		}
	}()

	if _, err := conn.Write([]byte(systemdReadyState)); err != nil {
		return fmt.Errorf("write systemd readiness notification: %w", err)
	}
	return nil
}
