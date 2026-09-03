package main

import (
	"fmt"
	"io"
	"net"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: silent-smtp-server ADDRESS_FILE ACCEPTED_FILE")
		os.Exit(2)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()
	if err := os.WriteFile(os.Args[1], []byte(listener.Addr().String()), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "publish address: %v\n", err)
		os.Exit(1)
	}

	connection, err := listener.Accept()
	if err != nil {
		fmt.Fprintf(os.Stderr, "accept: %v\n", err)
		os.Exit(1)
	}
	defer connection.Close()
	if err := os.WriteFile(os.Args[2], nil, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "publish accepted connection: %v\n", err)
		os.Exit(1)
	}
	if _, err := io.Copy(io.Discard, connection); err != nil {
		fmt.Fprintf(os.Stderr, "wait for client close: %v\n", err)
		os.Exit(1)
	}
}
