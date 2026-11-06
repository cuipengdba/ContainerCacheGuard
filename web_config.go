package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

func validateWebListen(address, tokenFile string) error {
	if address == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("web_listen 必须是 host:port: %w", err)
	}
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if !loopback && tokenFile == "" {
		return errors.New("非回环 web_listen 必须设置 web_token_file")
	}
	return nil
}
