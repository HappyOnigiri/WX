package main

import (
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

func rpcClient() (rpc.Client, error) {
	socket, err := config.SocketPath()
	return rpc.Client{Socket: socket, Timeout: 5 * time.Second}, err
}
